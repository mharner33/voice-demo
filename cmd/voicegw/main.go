// Command voicegw is the media gateway: it receives RTP/G.711, runs each stream
// through a jitter buffer, and feeds the result to the STT → LLM → TTS pipeline.
//
// Phase 4 scope. Synthesized audio is counted but not yet sent back to the
// caller, because establishing the return media path is the control plane's job
// in phase 5. Datadog instrumentation also arrives in phase 5. See docs/plan.md.
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

	if err := run(*addr, *interval, *idle, jbCfg, providers); err != nil {
		log.Fatalf("voicegw: %v", err)
	}
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

	mu       sync.Mutex
	lastSeen time.Time
	reported bool
	result   call.Result
	runErr   error
	haveRes  bool

	stop     chan struct{}
	played   chan struct{} // closed when the playout loop exits
	piped    chan struct{} // closed when the pipeline returns
	stopOnce sync.Once
}

func newActiveCall(ssrc uint32, sess *rtp.Session, jbCfg jbuf.Config, p providers, now time.Time) (*activeCall, error) {
	jb, err := jbuf.New(jbCfg)
	if err != nil {
		return nil, err
	}

	c := &activeCall{
		id:       fmt.Sprintf("c-%08x", ssrc),
		ssrc:     ssrc,
		sess:     sess,
		jb:       jb,
		audio:    make(chan stt.Audio, pipelineQueue),
		lastSeen: now,
		stop:     make(chan struct{}),
		played:   make(chan struct{}),
		piped:    make(chan struct{}),
	}

	// OnAudio is deliberately nil: phase 5's control plane establishes the
	// return media path, and the pipeline already counts synthesized frames in
	// Result.FramesOut, so there is nothing useful to do with the audio yet.
	session, err := call.New(call.Config{
		CallID: c.id,
		STT:    p.stt,
		LLM:    p.llm,
		TTS:    p.tts,
		Tools:  p.tools,
	})
	if err != nil {
		return nil, err
	}

	go c.playout()
	go func() {
		defer close(c.piped)
		res, err := session.Run(context.Background(), c.audio)

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

// close stops playout and waits for the pipeline to finish the call.
func (c *activeCall) close() {
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.played
	<-c.piped
}

func (c *activeCall) snapshot() (call.Result, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.runErr, c.haveRes
}

func run(addr string, reportInterval, idleTimeout time.Duration, jbCfg jbuf.Config, p providers) error {
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
			c, err = newActiveCall(sess.SSRC(), sess, jbCfg, p, arrival)
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
