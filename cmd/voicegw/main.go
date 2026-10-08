// Command voicegw is the media gateway: it accepts calls over a gRPC control
// plane, receives RTP/G.711, runs each stream through a jitter buffer, feeds
// the result to the STT → LLM → TTS pipeline, and sends the synthesized reply
// back to the caller.
//
// Everything is reported to Datadog: an APM trace per call, Agent
// Observability spans for the AI stages, DogStatsD metrics, and a correlated
// call log. Telemetry is off by default so the demo runs with no agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/control"
	"github.com/mharner33/voice-demo/internal/faults"
	"github.com/mharner33/voice-demo/internal/jbuf"
	"github.com/mharner33/voice-demo/internal/llm"
	"github.com/mharner33/voice-demo/internal/obs"
	"github.com/mharner33/voice-demo/internal/rtp"
	"github.com/mharner33/voice-demo/internal/stt"
	"github.com/mharner33/voice-demo/internal/tts"
)

func main() {
	var (
		addr     = flag.String("addr", ":5004", "UDP address to receive RTP on")
		grpcAddr = flag.String("grpc-addr", ":50051", "address for the gRPC control plane")
		interval = flag.Duration("report-interval", 5*time.Second, "how often to report live stream stats")
		idle     = flag.Duration("idle-timeout", 3*time.Second, "silence after which an un-torn-down stream is treated as ended")

		requireSignaling = flag.Bool("require-signaling", false,
			"reject RTP from an SSRC that never called StartCall")

		target   = flag.Int("jbuf-target", jbuf.DefaultTargetDepth, "jitter buffer prebuffer depth in frames (20ms each)")
		maxDepth = flag.Int("jbuf-max", jbuf.DefaultMaxDepth, "jitter buffer hard cap in frames")
		adaptive = flag.Bool("jbuf-adaptive", false, "let the jitter buffer deepen itself in response to late packets")
		concealF = flag.String("conceal", "repeat", "concealment for lost frames: repeat, silence, or noise")

		profile = flag.String("provider-profile", "clean",
			"provider impairment profile ("+strings.Join(chaos.ProfileNames(), ", ")+")")
		sttLatency = flag.Duration("stt-latency", -1, "override STT added latency")
		sttErrors  = flag.Float64("stt-error-rate", -1, "override STT error rate (0-1)")
		llmLatency = flag.Duration("llm-latency", -1, "override LLM added latency")
		llmErrors  = flag.Float64("llm-error-rate", -1, "override LLM error rate (0-1)")
		ttsLatency = flag.Duration("tts-latency", -1, "override TTS added latency")
		seed       = flag.Int64("fault-seed", 1, "seed for provider fault injection")

		ddEnabled   = flag.Bool("dd", envBool("VOICE_DD_ENABLED", false), "report traces, metrics and logs to Datadog")
		ddLLMObs    = flag.Bool("dd-llmobs", envBool("DD_LLMOBS_ENABLED", true), "report Agent Observability spans")
		ddAgentless = flag.Bool("dd-agentless", envBool("DD_LLMOBS_AGENTLESS_ENABLED", false), "submit LLM Obs data without an agent (needs DD_API_KEY)")
		ddService   = flag.String("dd-service", envOr("DD_SERVICE", "voicegw"), "Datadog service name")
		ddEnv       = flag.String("dd-env", envOr("DD_ENV", "demo"), "Datadog environment")
		ddVersion   = flag.String("dd-version", envOr("DD_VERSION", "0.1.0"), "Datadog service version")
		ddMLApp     = flag.String("dd-ml-app", envOr("DD_LLMOBS_ML_APP", obs.DefaultMLApp), "Agent Observability ML app name")
		ddHost      = flag.String("dd-agent-host", envOr("DD_AGENT_HOST", "localhost"), "Datadog agent host")
		ddTracePort = flag.String("dd-trace-port", envOr("DD_TRACE_AGENT_PORT", "8126"), "Datadog APM port")
		ddStatsPort = flag.String("dd-statsd-port", envOr("DD_DOGSTATSD_PORT", "8125"), "DogStatsD port")
		callLogPath = flag.String("call-log", envOr("VOICE_CALL_LOG", "-"), `call log destination; "-" is stdout`)
	)
	flag.Parse()

	mode, err := parseConcealMode(*concealF)
	if err != nil {
		log.Fatalf("voicegw: %v", err)
	}

	jbCfg := jbuf.Config{
		Codec:       codec.PCMU,
		TargetDepth: *target,
		MaxDepth:    *maxDepth,
		Adaptive:    *adaptive,
		Conceal:     mode,
	}
	// Fail fast on a bad flag rather than per-call once traffic arrives.
	if _, err := jbuf.New(jbCfg); err != nil {
		log.Fatalf("voicegw: %v", err)
	}

	prof, err := chaos.LookupProfile(*profile)
	if err != nil {
		log.Fatalf("voicegw: %v", err)
	}
	pv := prof.Provider
	if *sttLatency >= 0 {
		pv.STTExtraLatencyMs = float64(sttLatency.Milliseconds())
	}
	if *sttErrors >= 0 {
		pv.STTErrorRate = *sttErrors
	}
	if *llmLatency >= 0 {
		pv.LLMExtraLatencyMs = float64(llmLatency.Milliseconds())
	}
	if *llmErrors >= 0 {
		pv.LLMErrorRate = *llmErrors
	}
	if *ttsLatency >= 0 {
		pv.TTSExtraLatencyMs = float64(ttsLatency.Milliseconds())
	}

	p, err := buildProviders(pv, *seed)
	if err != nil {
		log.Fatalf("voicegw: %v", err)
	}

	tracerT, err := obs.Start(obs.Config{
		Enabled:       *ddEnabled,
		Service:       *ddService,
		Env:           *ddEnv,
		Version:       *ddVersion,
		AgentHost:     *ddHost,
		TracePort:     *ddTracePort,
		StatsdPort:    *ddStatsPort,
		LLMObsEnabled: *ddLLMObs,
		MLApp:         *ddMLApp,
		Agentless:     *ddAgentless,
	})
	if err != nil {
		log.Fatalf("voicegw: %v", err)
	}
	defer tracerT.Stop()

	callLog, err := obs.NewCallLog(*callLogPath)
	if err != nil {
		log.Fatalf("voicegw: %v", err)
	}
	defer callLog.Close()

	tel := telemetry{tracer: tracerT, callLog: callLog, profile: *profile}

	if err := run(runConfig{
		mediaAddr:        *addr,
		grpcAddr:         *grpcAddr,
		reportInterval:   *interval,
		idleTimeout:      *idle,
		requireSignaling: *requireSignaling,
		jbCfg:            jbCfg,
		providers:        p,
		tel:              tel,
	}); err != nil {
		log.Fatalf("voicegw: %v", err)
	}
}

// telemetry bundles the reporting handles threaded through a call.
type telemetry struct {
	tracer  *obs.Tracer
	callLog *obs.CallLog
	profile string
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func parseConcealMode(s string) (jbuf.ConcealMode, error) {
	switch strings.ToLower(s) {
	case "repeat":
		return jbuf.ConcealRepeat, nil
	case "silence":
		return jbuf.ConcealSilence, nil
	case "noise":
		return jbuf.ConcealNoise, nil
	default:
		return 0, fmt.Errorf("unknown conceal mode %q; want repeat, silence, or noise", s)
	}
}

// providers bundles the three AI seams. They are built once and shared across
// calls, which is what a real deployment would do with provider clients.
type providers struct {
	stt   stt.Transcriber
	llm   llm.Agent
	tts   tts.Synthesizer
	tools *llm.Registry
}

func buildProviders(pv chaos.Provider, seed int64) (providers, error) {
	ms := func(v float64) time.Duration { return time.Duration(v) * time.Millisecond }

	tools, err := llm.DefaultRegistry()
	if err != nil {
		return providers{}, err
	}

	transcriber, err := stt.NewMock(stt.MockConfig{DegradeOnConcealed: true})
	if err != nil {
		return providers{}, err
	}
	agent, err := llm.NewMock(llm.MockConfig{Registry: tools})
	if err != nil {
		return providers{}, err
	}
	synth, err := tts.NewMock(tts.MockConfig{})
	if err != nil {
		return providers{}, err
	}

	faultySTT, err := stt.WithFaults(transcriber, faults.Config{
		ExtraLatency: ms(pv.STTExtraLatencyMs), ErrorRate: pv.STTErrorRate, Seed: seed,
	})
	if err != nil {
		return providers{}, err
	}
	faultyLLM, err := llm.WithFaults(agent, faults.Config{
		ExtraLatency: ms(pv.LLMExtraLatencyMs), ErrorRate: pv.LLMErrorRate, Seed: seed + 1,
	})
	if err != nil {
		return providers{}, err
	}
	faultyTTS, err := tts.WithFaults(synth, faults.Config{
		ExtraLatency: ms(pv.TTSExtraLatencyMs), Seed: seed + 2,
	})
	if err != nil {
		return providers{}, err
	}

	return providers{stt: faultySTT, llm: faultyLLM, tts: faultyTTS, tools: tools}, nil
}

type runConfig struct {
	mediaAddr        string
	grpcAddr         string
	reportInterval   time.Duration
	idleTimeout      time.Duration
	requireSignaling bool
	jbCfg            jbuf.Config
	providers        providers
	tel              telemetry
}

func run(cfg runConfig) error {
	mediaConn, err := net.ListenPacket("udp", cfg.mediaAddr)
	if err != nil {
		return fmt.Errorf("listening for RTP on %s: %w", cfg.mediaAddr, err)
	}
	defer mediaConn.Close()

	grpcLis, err := net.Listen("tcp", cfg.grpcAddr)
	if err != nil {
		return fmt.Errorf("listening for the control plane on %s: %w", cfg.grpcAddr, err)
	}

	mediaPort := mediaConn.LocalAddr().(*net.UDPAddr).Port

	log.Printf("control plane on %s", grpcLis.Addr())
	log.Printf("listening for RTP on %s", mediaConn.LocalAddr())
	log.Printf("jitter buffer: target %d frames (%dms), max %d frames (%dms), conceal=%s, adaptive=%v",
		cfg.jbCfg.TargetDepth, cfg.jbCfg.TargetDepth*20, cfg.jbCfg.MaxDepth,
		cfg.jbCfg.MaxDepth*20, cfg.jbCfg.Conceal, cfg.jbCfg.Adaptive)
	log.Printf("providers: stt=%s/%s llm=%s/%s tts=%s/%s",
		cfg.providers.stt.Info().Provider, cfg.providers.stt.Info().Model,
		cfg.providers.llm.Info().Provider, cfg.providers.llm.Info().Model,
		cfg.providers.tts.Info().Provider, cfg.providers.tts.Info().Model)
	log.Print(cfg.tel.tracer.Describe())
	if cfg.requireSignaling {
		log.Print("requiring StartCall before media")
	}

	gw := newGateway(cfg.jbCfg, cfg.providers, cfg.tel, mediaPort, cfg.requireSignaling)
	recv := rtp.NewReceiver(mediaConn, codec.SampleRate8k)

	recv.OnPacket(func(sess *rtp.Session, pkt *pionrtp.Packet, src net.Addr, arrival time.Time) {
		c := gw.onPacket(sess, pkt.SSRC, src, arrival)
		if c == nil {
			return
		}
		switch res := c.jb.Push(pkt); res {
		case jbuf.PushAccepted, jbuf.PushDuplicate, jbuf.PushLate:
			// Counted in the buffer's own stats.
		default:
			log.Printf("call %s: seq=%d rejected: %s", c.id, pkt.SequenceNumber, res)
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := control.Serve(ctx, grpcLis, gw); err != nil {
			log.Printf("control plane: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(cfg.reportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				// A torn-down call has already reported itself; this only
				// reaps the ones that stopped without a teardown message.
				gw.reapIdle(now, cfg.idleTimeout)
				for _, c := range gw.live() {
					report("live", c)
				}
			}
		}
	}()

	serveErr := recv.Serve(ctx)
	stop()
	wg.Wait()

	for _, c := range gw.drain("shutdown") {
		report("final", c)
	}
	if n := recv.Malformed(); n > 0 {
		log.Printf("ignored %d malformed packets", n)
	}

	// A process that exits immediately would drop whatever is still buffered,
	// which for a short demo run is most of it.
	cfg.tel.tracer.Flush()

	if serveErr != nil {
		return serveErr
	}
	log.Print("shutdown complete")
	return nil
}
