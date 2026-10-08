// Command voicegw is the media gateway: it receives RTP/G.711, runs each stream
// through a jitter buffer, and feeds the result to the STT → LLM → TTS pipeline.
//
// Phase 5 scope: fully instrumented for Datadog APM, Agent Observability,
// DogStatsD and the call log. Synthesized audio is still counted rather than
// sent back to the caller, because establishing the return media path needs the
// control plane. See docs/plan.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/call"
	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
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
		interval = flag.Duration("report-interval", 5*time.Second, "how often to report live stream stats")
		idle     = flag.Duration("idle-timeout", 3*time.Second, "silence after which a stream is treated as ended")

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

	providers, err := buildProviders(pv, *seed)
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

	if err := run(*addr, *interval, *idle, jbCfg, providers, tel); err != nil {
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

// pipelineQueue is how much audio may wait for the pipeline. Two seconds is
// enough to ride out a slow agent turn without the queue becoming a second,
// invisible jitter buffer.
const pipelineQueue = 100

// playoutHangover is how many consecutive starved slots the playout loop will
// fill before going idle to wait for more audio.
//
// Without this bound the loop would emit concealment frames forever once a call
// went quiet, and since every one counts as a concealed slot, the conceal rate
// would drift toward 100% and stop describing the call. The threshold has to
// exceed the longest plausible mid-call gap or real holes would be cut short:
// the lossy-wan profile bursts about 4 frames, so 10 frames (200 ms) leaves
// ample margin.
//
// This is a stopgap. Phase 5's control plane carries an explicit end-of-call
// message, which ends playout exactly instead of inferring it from silence.
const playoutHangover = 10

// activeCall is one inbound stream: its measurements, its jitter buffer, and
// the pipeline consuming its audio.
type activeCall struct {
	id   string
	ssrc uint32
	sess *rtp.Session
	jb   *jbuf.Buffer

	audio chan stt.Audio
	// dropped counts frames the pipeline could not keep up with. Atomic because
	// the playout goroutine writes it while the reporter reads it.
	dropped atomic.Uint64

	tel   telemetry
	tags  obs.CallTags
	start time.Time
	// root is the call's APM span. The pipeline's Agent Observability workflow
	// span is parented to it, which is what puts the transport stages and the
	// AI stages on one trace.
	root        *obs.Span
	rootContext context.Context

	mu       sync.Mutex
	lastSeen time.Time
	reported bool
	result   call.Result
	runErr   error
	haveRes  bool

	stop       chan struct{}
	played     chan struct{} // closed when the playout loop exits
	piped      chan struct{} // closed when the pipeline returns
	stopOnce   sync.Once
	reportOnce sync.Once
}

func newActiveCall(ssrc uint32, sess *rtp.Session, jbCfg jbuf.Config, p providers,
	tel telemetry, now time.Time) (*activeCall, error) {

	jb, err := jbuf.New(jbCfg)
	if err != nil {
		return nil, err
	}

	id := fmt.Sprintf("c-%08x", ssrc)
	tags := obs.CallTags{
		CallID:          id,
		Codec:           string(jbCfg.Codec),
		ProviderProfile: tel.profile,
		STTProvider:     p.stt.Info().Provider,
		LLMProvider:     p.llm.Info().Provider,
		TTSProvider:     p.tts.Info().Provider,
	}

	c := &activeCall{
		id:       id,
		ssrc:     ssrc,
		sess:     sess,
		jb:       jb,
		audio:    make(chan stt.Audio, pipelineQueue),
		lastSeen: now,
		tel:      tel,
		tags:     tags,
		start:    now,
		stop:     make(chan struct{}),
		played:   make(chan struct{}),
		piped:    make(chan struct{}),
	}

	// The APM root span for the call. Everything else hangs off its context,
	// including the pipeline's Agent Observability workflow span, so one trace
	// shows the whole call from packets to reply.
	root, ctx := tel.tracer.StartAPM(context.Background(), "voice.call", map[string]string{
		"call_id":          id,
		"ssrc":             fmt.Sprintf("%#08x", ssrc),
		"codec":            tags.Codec,
		"provider_profile": tel.profile,
	})
	c.root, c.rootContext = root, ctx

	// OnAudio is deliberately nil: establishing the return media path needs the
	// control plane, and the pipeline already counts synthesized frames in
	// Result.FramesOut, so there is nothing useful to do with the audio yet.
	session, err := call.New(call.Config{
		CallID: c.id,
		STT:    p.stt,
		LLM:    p.llm,
		TTS:    p.tts,
		Tools:  p.tools,
		Obs:    tel.tracer,
		Tags:   tags,
	})
	if err != nil {
		root.Finish(err)
		return nil, err
	}

	go c.playout()
	go func() {
		defer close(c.piped)
		res, err := session.Run(ctx, c.audio)

		c.mu.Lock()
		defer c.mu.Unlock()
		c.result, c.runErr, c.haveRes = res, err, true
	}()

	return c, nil
}

// playout drains the jitter buffer at the packetization interval, as an audio
// device's callback would, and hands each frame to the pipeline.
func (c *activeCall) playout() {
	defer close(c.played)
	defer close(c.audio)

	ticker := time.NewTicker(codec.FrameDuration)
	defer ticker.Stop()

	starved := 0
	for {
		select {
		case <-c.stop:
			for _, f := range c.jb.Drain() {
				c.forward(f)
			}
			return
		case <-ticker.C:
			if starved >= playoutHangover {
				// Idle until audio arrives again, rather than emitting filler.
				if c.jb.Stats().CurrentDepth == 0 {
					continue
				}
				starved = 0
			}

			f, ok := c.jb.Pop()
			if !ok {
				continue // still prebuffering
			}

			// A concealed slot with an empty buffer behind it means the far end
			// has stopped sending, not that a packet went missing mid-call.
			if f.Concealed && c.jb.Stats().CurrentDepth == 0 {
				starved++
				// Do not feed this to the recognizer. With nothing buffered,
				// the frame is entirely invented, and inventing input for a
				// speech recognizer is worse than briefly pausing the stream:
				// it drags the utterance's confidence down and, at end of call,
				// made the final turn report zero confidence even though its
				// speech was recognized perfectly well.
				//
				// Mid-call holes with audio queued behind them are still
				// forwarded, since those do need to keep the timeline intact.
				continue
			}
			starved = 0
			c.forward(f)
		}
	}
}

// forward hands a frame to the pipeline, dropping it if the pipeline has fallen
// behind. Blocking here would stall playout, and a caller's voice cannot be
// paused — so the honest response to a slow pipeline is to drop audio and say
// so, rather than to accumulate unbounded hidden latency.
func (c *activeCall) forward(f jbuf.Frame) {
	select {
	case c.audio <- stt.Audio{PCM: f.PCM, Concealed: f.Concealed}:
	default:
		c.dropped.Add(1)
	}
}

func (c *activeCall) touch(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = t
	c.reported = false
}

// expired reports whether the stream has gone silent long enough to be treated
// as ended, and claims the one-time end-of-call report.
func (c *activeCall) expired(now time.Time, idle time.Duration) (ended, alreadyReported bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ended = now.Sub(c.lastSeen) > idle
	alreadyReported = c.reported
	if ended {
		c.reported = true
	}
	return ended, alreadyReported
}

// close stops playout, waits for the pipeline, then reports the call.
//
// All the reporting happens here rather than during the call because the
// transport figures are cumulative: loss, jitter and buffer behavior are only
// meaningful once the stream has ended.
func (c *activeCall) close() {
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.played
	<-c.piped
	c.reportOnce.Do(c.emitTelemetry)
}

// emitTelemetry writes the call's spans, metrics, and log record.
func (c *activeCall) emitTelemetry() {
	ns := c.sess.Stats()
	jb := c.jb.Stats()
	res, runErr, haveRes := c.snapshot()

	// The transport stages ran for the whole call rather than as a nested call
	// stack, so their spans are created now and backdated to cover it. Their
	// measurements go on as tags: this is the APM half of the demo, where
	// packet loss and buffer depth belong.
	if c.root != nil {
		ingest, _ := c.tel.tracer.StartAPMAt(c.rootCtx(), "voice.rtp.ingest", c.start,
			map[string]string{"codec": c.tags.Codec})
		ingest.SetAPMTag("rtp.packets_received", int64(ns.Received))
		ingest.SetAPMTag("rtp.packets_expected", int64(ns.Expected))
		ingest.SetAPMTag("rtp.packets_lost", int64(ns.Lost))
		ingest.SetAPMTag("rtp.loss_pct", ns.LossPct)
		ingest.SetAPMTag("rtp.reordered", int64(ns.Reordered))
		ingest.SetAPMTag("rtp.duplicated", int64(ns.Duplicated))
		ingest.SetAPMTag("rtp.jitter_ms", ns.JitterMs)
		ingest.SetAPMTag("rtp.max_jitter_ms", ns.MaxJitterMs)
		ingest.SetAPMTag("mos", ns.MOS())
		ingest.Finish(nil)

		buffer, _ := c.tel.tracer.StartAPMAt(c.rootCtx(), "voice.jitter_buffer", c.start, nil)
		buffer.SetAPMTag("jbuf.frames_played", int64(jb.Popped))
		buffer.SetAPMTag("jbuf.concealed", int64(jb.Concealed))
		buffer.SetAPMTag("jbuf.conceal_pct", jb.ConcealRate()*100)
		buffer.SetAPMTag("jbuf.late_drops", int64(jb.Late))
		buffer.SetAPMTag("jbuf.evicted", int64(jb.Evicted))
		buffer.SetAPMTag("jbuf.underruns", int64(jb.Starved))
		buffer.SetAPMTag("jbuf.target_depth_ms", jb.TargetDepthMs())
		buffer.SetAPMTag("jbuf.peak_depth_frames", int64(jb.MaxObservedDepth))
		buffer.Finish(nil)

		c.root.SetAPMTag("call.turns", len(res.Turns))
		c.root.SetAPMTag("call.tool_calls", res.ToolCalls)
		c.root.SetAPMTag("call.errors", res.Errors)
		c.root.SetAPMTag("call.pipeline_dropped", int64(c.dropped.Load()))
		c.root.Finish(runErr)
	}

	// Metrics. Transport and call-level figures only; token counts live on the
	// Agent Observability spans, from which Datadog derives its own.
	c.tel.tracer.RecordNetwork(obs.NetworkStats{
		Received:   ns.Received,
		Lost:       ns.Lost,
		Reordered:  ns.Reordered,
		Duplicated: ns.Duplicated,
		LossPct:    ns.LossPct,
		JitterMs:   ns.JitterMs,
		MOS:        ns.MOS(),
		Duration:   ns.Duration,
	}, c.tags)

	c.tel.tracer.RecordBuffer(obs.BufferStats{
		Popped:        jb.Popped,
		Concealed:     jb.Concealed,
		Starved:       jb.Starved,
		Late:          jb.Late,
		Evicted:       jb.Evicted,
		DepthMs:       jb.DepthMs(),
		TargetDepthMs: jb.TargetDepthMs(),
		ConcealPct:    jb.ConcealRate() * 100,
	}, c.tags)

	if haveRes {
		c.tel.tracer.RecordCall(obs.PipelineStats{
			Turns:          len(res.Turns),
			ToolCalls:      res.ToolCalls,
			Errors:         res.Errors,
			AudioIn:        res.AudioInDuration(),
			ConcealedPct:   res.ConcealedFraction() * 100,
			DroppedFrames:  c.dropped.Load(),
			FirstPartialAt: res.FirstPartialAt,
			HavePartial:    res.HavePartial,
		}, ns.Duration, c.tags)
	}

	c.writeCallLog(ns, jb, res, runErr, haveRes)
}

// rootCtx returns a context carrying the call's APM root span, so the transport
// spans attach to it rather than starting traces of their own.
func (c *activeCall) rootCtx() context.Context {
	if c.rootContext != nil {
		return c.rootContext
	}
	return context.Background()
}

// writeCallLog emits the one line that ties all three layers together.
func (c *activeCall) writeCallLog(ns rtp.Stats, jb jbuf.Stats,
	res call.Result, runErr error, haveRes bool) {

	rec := obs.CallRecord{
		CallID:  c.id,
		SSRC:    fmt.Sprintf("%#08x", c.ssrc),
		Codec:   c.tags.Codec,
		Profile: c.tel.profile,

		PacketsRx:   ns.Received,
		PacketsLost: ns.Lost,
		LossPct:     ns.LossPct,
		Reordered:   ns.Reordered,
		Duplicated:  ns.Duplicated,
		JitterMs:    ns.JitterMs,
		MOS:         ns.MOS(),
		DurationMs:  ns.Duration.Milliseconds(),

		Played:        jb.Popped,
		Concealed:     jb.Concealed,
		ConcealPct:    jb.ConcealRate() * 100,
		LateDrops:     jb.Late,
		Evicted:       jb.Evicted,
		Underruns:     jb.Starved,
		TargetDepthMs: jb.TargetDepthMs(),

		STTProvider: c.tags.STTProvider,
		LLMProvider: c.tags.LLMProvider,
		TTSProvider: c.tags.TTSProvider,

		DroppedFrames: c.dropped.Load(),
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		rec.Event = "call.failed"
		rec.Error = runErr.Error()
	}

	if haveRes {
		rec.TurnCount = len(res.Turns)
		rec.ToolCalls = res.ToolCalls
		rec.PipelineErrors = res.Errors
		rec.InputTokens = res.InputTokens
		rec.OutputTokens = res.OutputTokens
		rec.ConcealedInPct = res.ConcealedFraction() * 100
		rec.FramesOut = res.FramesOut
		if res.HavePartial {
			rec.FirstPartialMs = res.FirstPartialAt.Milliseconds()
		}
		for _, turn := range res.Turns {
			tr := obs.TurnRecord{
				Index:        turn.Index,
				Transcript:   turn.Transcript,
				Confidence:   turn.Confidence,
				Reply:        turn.Reply,
				ToolCalls:    turn.ToolCalls,
				ToolRounds:   turn.ToolRounds,
				InputTokens:  turn.InputTokens,
				OutputTokens: turn.OutputTokens,
				FinalMs:      turn.FinalAt.Milliseconds(),
				LLMMs:        turn.LLMLatency.Milliseconds(),
				TTSFirstMs:   turn.TTSFirstByte.Milliseconds(),
				TTSAudioMs:   turn.TTSAudio.Milliseconds(),
			}
			if turn.Err != nil {
				tr.Error = turn.Err.Error()
			}
			rec.Turns = append(rec.Turns, tr)
		}
	}

	rec.Correlate(c.root, c.tel.tracer)

	if err := c.tel.callLog.Write(rec); err != nil {
		log.Printf("call %s: writing the call log: %v", c.id, err)
	}
}

func (c *activeCall) snapshot() (call.Result, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.runErr, c.haveRes
}

func run(addr string, reportInterval, idleTimeout time.Duration, jbCfg jbuf.Config,
	p providers, tel telemetry) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer conn.Close()

	log.Printf("listening for RTP on %s", conn.LocalAddr())
	log.Printf("jitter buffer: target %d frames (%dms), max %d frames (%dms), conceal=%s, adaptive=%v",
		jbCfg.TargetDepth, jbCfg.TargetDepth*20, jbCfg.MaxDepth, jbCfg.MaxDepth*20,
		jbCfg.Conceal, jbCfg.Adaptive)
	log.Printf("providers: stt=%s/%s llm=%s/%s tts=%s/%s",
		p.stt.Info().Provider, p.stt.Info().Model,
		p.llm.Info().Provider, p.llm.Info().Model,
		p.tts.Info().Provider, p.tts.Info().Model)
	log.Print(tel.tracer.Describe())

	recv := rtp.NewReceiver(conn, codec.SampleRate8k)

	var (
		mu    sync.Mutex
		calls = make(map[uint32]*activeCall)
	)

	recv.OnPacket(func(sess *rtp.Session, pkt *pionrtp.Packet, arrival time.Time) {
		mu.Lock()
		c, known := calls[sess.SSRC()]
		if !known {
			var err error
			c, err = newActiveCall(sess.SSRC(), sess, jbCfg, p, tel, arrival)
			if err != nil {
				mu.Unlock()
				log.Printf("stream %#08x: cannot start: %v", sess.SSRC(), err)
				return
			}
			calls[sess.SSRC()] = c
			log.Printf("call %s: started (ssrc %#08x)", c.id, sess.SSRC())
		}
		mu.Unlock()

		c.touch(arrival)

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
		ticker := time.NewTicker(reportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				mu.Lock()
				snapshot := make([]*activeCall, 0, len(calls))
				for _, c := range calls {
					snapshot = append(snapshot, c)
				}
				mu.Unlock()

				for _, c := range snapshot {
					ended, already := c.expired(now, idleTimeout)
					switch {
					case ended && !already:
						c.close()
						report("ended", c)
					case !ended:
						report("live", c)
					}
				}
			}
		}
	}()

	serveErr := recv.Serve(ctx)
	stop()
	wg.Wait()

	mu.Lock()
	remaining := make([]*activeCall, 0, len(calls))
	for _, c := range calls {
		remaining = append(remaining, c)
	}
	mu.Unlock()

	for _, c := range remaining {
		c.close()
		report("final", c)
	}
	if n := recv.Malformed(); n > 0 {
		log.Printf("ignored %d malformed packets", n)
	}

	// A process that exits immediately would drop whatever is still buffered,
	// which for a short demo run is most of it.
	tel.tracer.Flush()

	if serveErr != nil {
		return serveErr
	}
	log.Print("shutdown complete")
	return nil
}

func report(phase string, c *activeCall) {
	// Named ns rather than net, which would shadow the net package.
	ns := c.sess.Stats()
	if ns.Received == 0 {
		return
	}
	jb := c.jb.Stats()

	fmt.Fprintf(os.Stdout,
		"[%s] %s | net: recv=%d lost=%d (%.2f%%) reorder=%d jitter=%.1fms mos=%.2f dur=%s"+
			" | jbuf: played=%d concealed=%d (%.2f%%) late=%d depth=%.0f/%.0fms"+
			" | pipe: dropped=%d\n",
		phase, c.id,
		ns.Received, ns.Lost, ns.LossPct, ns.Reordered, ns.JitterMs, ns.MOS(),
		ns.Duration.Round(time.Millisecond),
		jb.Popped, jb.Concealed, jb.ConcealRate()*100, jb.Late,
		jb.DepthMs(), jb.TargetDepthMs(),
		c.dropped.Load())

	res, runErr, have := c.snapshot()
	if !have {
		return
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		fmt.Fprintf(os.Stdout, "       %s | pipeline failed: %v\n", c.id, runErr)
		return
	}

	fmt.Fprintf(os.Stdout,
		"       %s | ai: turns=%d tools=%d errors=%d tokens=%d/%d"+
			" first_partial=%s concealed_in=%.1f%% frames_out=%d\n",
		c.id, len(res.Turns), res.ToolCalls, res.Errors,
		res.InputTokens, res.OutputTokens,
		res.FirstPartialAt.Round(time.Millisecond),
		res.ConcealedFraction()*100, res.FramesOut)

	for _, turn := range res.Turns {
		if turn.Err != nil {
			fmt.Fprintf(os.Stdout, "       %s | turn %d failed: %v\n", c.id, turn.Index, turn.Err)
			continue
		}
		fmt.Fprintf(os.Stdout,
			"       %s | turn %d: %q (conf %.2f) -> %q [tools=%v llm=%s tts_fb=%s]\n",
			c.id, turn.Index, turn.Transcript, turn.Confidence, turn.Reply,
			turn.ToolCalls,
			turn.LLMLatency.Round(time.Millisecond),
			turn.TTSFirstByte.Round(time.Millisecond))
	}
}
