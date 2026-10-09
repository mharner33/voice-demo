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
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
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
		sttDropout = flag.Duration("stt-fail-after", -1,
			"kill the recognition stream once a call has carried this much audio (0 disables)")
		seed = flag.Int64("fault-seed", 1, "seed for provider fault injection")

		realSTT = flag.Bool("real-stt", envBool("VOICE_REAL_STT", false),
			"use Google Speech-to-Text v2 for recognition instead of the scripted mock")
		realTTS = flag.Bool("real-tts", envBool("VOICE_REAL_TTS", false),
			"use Google Cloud Text-to-Speech for synthesis instead of the tone mock")
		googleProject = flag.String("google-project", envOr("GOOGLE_CLOUD_PROJECT", ""),
			"Google Cloud project billed for -real-stt and -real-tts")
		googleLocation = flag.String("google-location", envOr("GOOGLE_CLOUD_LOCATION", stt.DefaultGoogleLocation),
			"Google Cloud location for -real-stt (a non-global one needs a regional endpoint)")
		sttModel = flag.String("stt-model", envOr("VOICE_STT_MODEL", stt.DefaultGoogleModel),
			"Google recognition model for -real-stt: telephony, telephony_short, long, short")
		sttLanguage = flag.String("stt-language", envOr("VOICE_STT_LANGUAGE", stt.DefaultGoogleLanguage),
			"BCP-47 language tag for -real-stt")
		ttsVoice = flag.String("tts-voice", envOr("VOICE_TTS_VOICE", tts.DefaultGoogleVoice),
			"Google voice name for -real-tts")
		ttsLanguage = flag.String("tts-language", envOr("VOICE_TTS_LANGUAGE", tts.DefaultGoogleLanguage),
			"BCP-47 language tag for -real-tts; must match the voice")

		realLLM = flag.Bool("real-llm", envBool("VOICE_REAL_LLM", false),
			"use the Anthropic API for the agent turn instead of the scripted mock")
		llmModel = flag.String("llm-model", envOr("VOICE_LLM_MODEL", llm.DefaultAnthropicModel),
			"model to use with -real-llm")
		llmEffort = flag.String("llm-effort", envOr("VOICE_LLM_EFFORT", "low"),
			"reasoning effort for -real-llm: low, medium, high, xhigh, or max")

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
		httpAddr    = flag.String("http-addr", envOr("VOICE_HTTP_ADDR", ":8080"),
			`address for /chaos and /healthz; "" disables it`)
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
	if *sttDropout >= 0 {
		pv.STTFailAfterMs = float64(sttDropout.Milliseconds())
	}

	p, err := buildProviders(context.Background(), pv, *seed, providerChoice{
		agent: agentConfig{
			real:   *realLLM,
			model:  *llmModel,
			effort: *llmEffort,
		},
		speech: speechConfig{
			realSTT:  *realSTT,
			realTTS:  *realTTS,
			project:  *googleProject,
			location: *googleLocation,
			model:    *sttModel,
			sttLang:  *sttLanguage,
			voice:    *ttsVoice,
			ttsLang:  *ttsLanguage,
		},
	})
	if err != nil {
		log.Fatalf("voicegw: %v", err)
	}
	defer p.close()

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

	// The profile is a live value rather than a constant: /chaos changes it,
	// and every metric tagged with it has to follow, or a beat's latency would
	// be attributed to the profile that was in effect before it.
	tel := telemetry{tracer: tracerT, callLog: callLog, profile: newProviderProfile(*profile)}

	if err := run(runConfig{
		mediaAddr:        *addr,
		httpAddr:         *httpAddr,
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
	profile *providerProfile
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

	// The descriptions are logged at startup. Which providers are serving a
	// run changes what it costs and how reproducible it is, so none of it
	// should ever be a guess.
	sttDescription string
	llmDescription string
	ttsDescription string

	// forget releases a call's state at teardown, for an agent that keeps
	// conversation history. It is nil for the stateless mock.
	forget func(callID string)

	// closers release the cloud clients at shutdown. Empty for the mocks,
	// which hold no connections.
	closers []func() error

	// faults are the provider impairment injectors, kept so the live /chaos
	// endpoint can retune them mid-call. They exist even on a clean run, which
	// is what Tunable buys.
	faults providerFaults
}

// providerFaults bundles what the live endpoint retunes: the three open-time
// injectors, and the recognizer's mid-stream fault, which is a different kind
// of failure and so a different mechanism (see internal/stt/dropout.go).
type providerFaults struct {
	stt *faults.Injector
	llm *faults.Injector
	tts *faults.Injector

	sttStream *stt.StreamFault
}

// tunableProvider is implemented by the fault-injecting wrappers. The three
// packages each have their own unexported wrapper type, so this is how the
// gateway reaches the injector without any of them exporting it.
type tunableProvider interface {
	Injector() *faults.Injector
}

// injectorOf extracts a wrapped provider's injector. It returns nil for an
// unwrapped provider, which with Tunable set should not happen — so the caller
// treats nil as a programming error rather than papering over it.
func injectorOf(v any) *faults.Injector {
	if t, ok := v.(tunableProvider); ok {
		return t.Injector()
	}
	return nil
}

// close releases every provider's resources, reporting failures rather than
// discarding them: a client that will not close is usually a client that was
// in a worse state than this process noticed.
func (p providers) close() {
	for _, c := range p.closers {
		if err := c(); err != nil {
			log.Printf("closing a provider: %v", err)
		}
	}
}

// providerChoice selects the implementations for a run.
type providerChoice struct {
	agent  agentConfig
	speech speechConfig
}

// agentConfig selects the agent implementation.
type agentConfig struct {
	real   bool
	model  string
	effort string
}

// speechConfig selects the recognizer and synthesizer implementations.
//
// The two are independent flags rather than one "-real" switch, because the
// interesting comparisons run them separately: real recognition with the mock
// synthesizer keeps the reply audio verifiable by frequency while proving the
// transcript is genuine, and real synthesis with the mock recognizer gives an
// audible demo with a fixed transcript.
type speechConfig struct {
	realSTT  bool
	realTTS  bool
	project  string
	location string
	model    string
	sttLang  string
	voice    string
	ttsLang  string
}

// buildAgent returns the agent for this run, plus a description and a teardown
// hook.
//
// The mock is the default deliberately. It keeps `make test` and an offline
// demo working with no credentials and no spend, and it is what makes the
// transport story reproducible: a scripted agent returns the same reply for the
// same transcript, so a change in the dashboards is a change in the network
// rather than a change in the model's mood.
func buildAgent(cfg agentConfig, tools *llm.Registry) (llm.Agent, string, func(string), error) {
	if !cfg.real {
		agent, err := llm.NewMock(llm.MockConfig{Registry: tools})
		if err != nil {
			return nil, "", nil, err
		}
		return agent, "mock (scripted; pass -real-llm for the Anthropic API)", nil, nil
	}

	agent, err := llm.NewAnthropic(llm.AnthropicConfig{
		Model:  cfg.model,
		Effort: cfg.effort,
		// The API key is deliberately not a flag. The SDK resolves credentials
		// from the environment, which keeps a key out of this program's command
		// line, its logs, and anyone's shell history.
	})
	if err != nil {
		return nil, "", nil, err
	}

	info := agent.Info()
	desc := fmt.Sprintf("anthropic %s at %s effort", info.Model, cfg.effort)
	if info.Pricing.Free() {
		desc += " (no published price for this model; cost metrics will be absent)"
	}
	return agent, desc, agent.Forget, nil
}

// buildTranscriber returns the recognizer for this run, plus a description and
// a closer for the client it may hold.
//
// The mock remains the default for the same reason the mock agent does: it is
// a function of how much audio it consumed, so the same call transcribes
// identically every time and a change in the dashboards between two runs is a
// change in the network rather than in the weather over a datacentre.
func buildTranscriber(ctx context.Context, cfg speechConfig) (stt.Transcriber, string, func() error, error) {
	if !cfg.realSTT {
		m, err := stt.NewMock(stt.MockConfig{DegradeOnConcealed: true})
		if err != nil {
			return nil, "", nil, err
		}
		return m, "mock (scripted; pass -real-stt for Google Speech-to-Text)", nil, nil
	}

	g, err := stt.NewGoogle(ctx, stt.GoogleConfig{
		Project:  cfg.project,
		Location: cfg.location,
		Model:    cfg.model,
		Language: cfg.sttLang,
		// Credentials come from Application Default Credentials, so no key
		// passes through this program's flags or logs.
	})
	if err != nil {
		return nil, "", nil, err
	}

	info := g.Info()
	desc := fmt.Sprintf("google %s, %s, %d Hz, project %s in %s",
		info.Model, cfg.sttLang, info.SampleRate, cfg.project, cfg.location)
	return g, desc, g.Close, nil
}

// buildSynthesizer returns the synthesizer for this run, plus a description
// and a closer.
//
// The mock's tone is derived from the reply text, which is what lets a test
// prove the agent's specific words reached the caller as audio. The real voice
// is what lets a person listen to the call. Both are useful; which one a run
// wants depends on whether it is being asserted on or demonstrated.
func buildSynthesizer(ctx context.Context, cfg speechConfig) (tts.Synthesizer, string, func() error, error) {
	if !cfg.realTTS {
		m, err := tts.NewMock(tts.MockConfig{})
		if err != nil {
			return nil, "", nil, err
		}
		return m, "mock (tone; pass -real-tts for Google Text-to-Speech)", nil, nil
	}

	g, err := tts.NewGoogle(ctx, tts.GoogleConfig{
		Project:  cfg.project,
		Voice:    cfg.voice,
		Language: cfg.ttsLang,
	})
	if err != nil {
		return nil, "", nil, err
	}

	info := g.Info()
	desc := fmt.Sprintf("google %s, %s, %d Hz", info.Model, cfg.ttsLang, info.SampleRate)
	return g, desc, g.Close, nil
}

func buildProviders(ctx context.Context, pv chaos.Provider, seed int64,
	choice providerChoice) (providers, error) {

	ms := func(v float64) time.Duration { return time.Duration(v) * time.Millisecond }

	tools, err := llm.DefaultRegistry()
	if err != nil {
		return providers{}, err
	}

	var closers []func() error

	transcriber, sttDesc, closeSTT, err := buildTranscriber(ctx, choice.speech)
	if err != nil {
		return providers{}, err
	}
	if closeSTT != nil {
		closers = append(closers, closeSTT)
	}

	agent, agentDesc, forget, err := buildAgent(choice.agent, tools)
	if err != nil {
		return providers{}, err
	}

	synth, ttsDesc, closeTTS, err := buildSynthesizer(ctx, choice.speech)
	if err != nil {
		return providers{}, err
	}
	if closeTTS != nil {
		closers = append(closers, closeTTS)
	}

	// The return path companders synthesized audio straight into G.711 and
	// paces it at one frame per packetization interval, so anything but the
	// wire rate would be played back to the caller at the wrong speed. Caught
	// here rather than heard later.
	if r := synth.Info().SampleRate; r != codec.SampleRate8k {
		return providers{}, fmt.Errorf(
			"the synthesizer produces %d Hz audio but the return RTP stream carries %d Hz",
			r, codec.SampleRate8k)
	}

	// Tunable on all three: the wrappers stay in place even when the profile
	// is clean, so /chaos can turn conditions on mid-demo.
	faultySTT, err := stt.WithFaults(transcriber, faults.Config{
		ExtraLatency: ms(pv.STTExtraLatencyMs), ErrorRate: pv.STTErrorRate,
		Seed: seed, Tunable: true,
	})
	if err != nil {
		return providers{}, err
	}
	// The mid-stream fault wraps the open-time one, so a call can be failed at
	// open or part way through, and the wrapper is always present — it is
	// inert until armed, and /chaos needs something to arm.
	dropout := stt.WithStreamFault(faultySTT)
	dropout.SetFailAfter(ms(pv.STTFailAfterMs))

	faultyLLM, err := llm.WithFaults(agent, faults.Config{
		ExtraLatency: ms(pv.LLMExtraLatencyMs), ErrorRate: pv.LLMErrorRate,
		Seed: seed + 1, Tunable: true,
	})
	if err != nil {
		return providers{}, err
	}
	faultyTTS, err := tts.WithFaults(synth, faults.Config{
		ExtraLatency: ms(pv.TTSExtraLatencyMs), Seed: seed + 2, Tunable: true,
	})
	if err != nil {
		return providers{}, err
	}

	p := providers{
		stt: dropout, llm: faultyLLM, tts: faultyTTS, tools: tools,
		sttDescription: sttDesc,
		llmDescription: agentDesc,
		ttsDescription: ttsDesc,
		forget:         forget,
		closers:        closers,
		faults: providerFaults{
			stt:       injectorOf(faultySTT),
			llm:       injectorOf(faultyLLM),
			tts:       injectorOf(faultyTTS),
			sttStream: dropout,
		},
	}
	if p.faults.stt == nil || p.faults.llm == nil || p.faults.tts == nil {
		// Tunable was supposed to guarantee a wrapper on every provider. If one
		// is missing, /chaos would silently do nothing for that stage, which is
		// worse during a demo than refusing to start.
		return providers{}, fmt.Errorf(
			"a provider is not impairable: stt=%v llm=%v tts=%v",
			p.faults.stt != nil, p.faults.llm != nil, p.faults.tts != nil)
	}
	return p, nil
}

type runConfig struct {
	mediaAddr        string
	httpAddr         string
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
	// One line per provider, because which implementation is serving a run
	// changes what the demo costs and how reproducible it is. None of it
	// should have to be inferred from the dashboards afterwards.
	log.Printf("stt: %s", cfg.providers.sttDescription)
	log.Printf("agent: %s", cfg.providers.llmDescription)
	log.Printf("tts: %s", cfg.providers.ttsDescription)
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

	// The live impairment endpoint. A failure to bind is logged rather than
	// fatal: a gateway that can serve calls but not /chaos is still a working
	// gateway, and killing a demo over a port collision would be worse than
	// losing the ability to change beats without a restart.
	if cfg.httpAddr != "" {
		api := &chaosAPI{
			faults:    cfg.providers.faults,
			profile:   cfg.tel.profile,
			liveCalls: func() int { return len(gw.live()) },
		}
		srv := &http.Server{
			Addr:              cfg.httpAddr,
			Handler:           api.routes(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		httpLis, err := net.Listen("tcp", cfg.httpAddr)
		if err != nil {
			log.Printf("not serving /chaos: %v", err)
		} else {
			log.Printf("chaos endpoint on http://%s/chaos", httpLis.Addr())
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := srv.Serve(httpLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("chaos endpoint: %v", err)
				}
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}()
		}
	}

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
