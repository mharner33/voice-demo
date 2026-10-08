package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mharner33/voice-demo/internal/call"
	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/control"
	"github.com/mharner33/voice-demo/internal/jbuf"
	"github.com/mharner33/voice-demo/internal/obs"
	"github.com/mharner33/voice-demo/internal/rtp"
	"github.com/mharner33/voice-demo/internal/stt"
)

// pipelineQueue is how much audio may wait for the pipeline. Two seconds is
// enough to ride out a slow agent turn without the queue becoming a second,
// invisible jitter buffer.
const pipelineQueue = 100

// playoutHangover bounds how many consecutive starved slots the playout loop
// will fill before idling.
//
// A signaled call does not need it: EndCall says exactly when the caller
// stopped, so playout stops there. It remains for unsignaled calls, where the
// end has to be inferred from silence, and for mid-call gaps. Without a bound
// the loop would emit concealment forever once a call went quiet and the
// conceal rate would drift toward 100%.
const playoutHangover = 10

// activeCall is one call: its negotiated parameters, its jitter buffer, the
// pipeline consuming its audio, and the telemetry it produces.
//
// A signaled call is created by StartCall, before any media arrives, and bound
// to its RTP stream when the first packet shows up. An unsignaled call is
// created by that first packet instead.
type activeCall struct {
	id     string
	params control.CallParams
	tel    telemetry
	tags   obs.CallTags
	start  time.Time

	jb    *jbuf.Buffer
	audio chan stt.Audio

	// dropped counts frames the pipeline could not keep up with. Atomic
	// because the playout goroutine writes it while the reporter reads it.
	dropped atomic.Uint64

	// sess is the RTP statistics tracker, set when the stream binds. It is nil
	// between StartCall and the first packet.
	sess atomic.Pointer[rtp.Session]

	// egress sends synthesized audio back to the caller. It is nil until the
	// stream binds, because its destination is the address the media came from.
	egress    atomic.Pointer[rtp.Sender]
	egressFwd atomic.Uint64
	bindOnce  sync.Once

	root        *obs.Span
	rootContext context.Context

	mu       sync.Mutex
	lastSeen time.Time
	result   call.Result
	runErr   error
	haveRes  bool

	stop       chan struct{}
	played     chan struct{}
	piped      chan struct{}
	stopOnce   sync.Once
	reportOnce sync.Once

	// signaled records whether the call came through the control plane. An
	// unsignaled call cannot report trailing loss or send audio back, and the
	// call log says so rather than leaving it ambiguous.
	signaled bool

	// senderReport is the caller's own account of what it transmitted,
	// delivered at teardown. Holding it lets the call log carry both sides'
	// numbers, which is what makes the loss accounting checkable.
	senderReport atomic.Pointer[control.SenderReport]
}

func newActiveCall(params control.CallParams, jbCfg jbuf.Config, p providers,
	tel telemetry, signaled bool) (*activeCall, error) {

	// The buffer must decode the codec the call negotiated, not the gateway's
	// default, or an A-law call would be expanded with the wrong table.
	jbCfg.Codec = params.Codec
	jb, err := jbuf.New(jbCfg)
	if err != nil {
		return nil, err
	}

	// A signaled call learns its network profile from the caller. An
	// unsignaled one cannot: nothing in an RTP stream says how it was
	// degraded, so the field stays empty rather than claiming "clean".
	tags := obs.CallTags{
		CallID:          params.CallID,
		Codec:           string(params.Codec),
		ProviderProfile: tel.profile,
		STTProvider:     p.stt.Info().Provider,
		LLMProvider:     p.llm.Info().Provider,
		TTSProvider:     p.tts.Info().Provider,
	}

	c := &activeCall{
		id:       params.CallID,
		params:   params,
		tel:      tel,
		tags:     tags,
		start:    params.StartedAt,
		jb:       jb,
		audio:    make(chan stt.Audio, pipelineQueue),
		lastSeen: params.StartedAt,
		stop:     make(chan struct{}),
		played:   make(chan struct{}),
		piped:    make(chan struct{}),
		signaled: signaled,
	}

	rootTags := map[string]string{
		"call_id":          params.CallID,
		"ssrc":             fmt.Sprintf("%#08x", params.SSRC),
		"codec":            string(params.Codec),
		"provider_profile": tel.profile,
		"signaled":         fmt.Sprintf("%v", signaled),
	}
	if params.NetworkProfile != "" {
		rootTags["network_profile"] = params.NetworkProfile
	}
	if params.From != "" {
		rootTags["from"] = params.From
	}
	if params.To != "" {
		rootTags["to"] = params.To
	}

	root, ctx := tel.tracer.StartAPMAt(context.Background(), "voice.call", c.start, rootTags)
	c.root, c.rootContext = root, ctx

	session, err := call.New(call.Config{
		CallID:  params.CallID,
		STT:     p.stt,
		LLM:     p.llm,
		TTS:     p.tts,
		Tools:   p.tools,
		Obs:     tel.tracer,
		Tags:    tags,
		OnAudio: c.sendBack,
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

// bind attaches the call to its RTP stream, which is also the moment the return
// media path becomes possible: the destination is the address the media came
// from, combined with the port the caller declared at setup. That pairing is
// what makes the return path work from behind NAT, and is what SDP plus RTP
// does in practice.
func (c *activeCall) bind(sess *rtp.Session, src net.Addr) {
	c.bindOnce.Do(func() {
		c.sess.Store(sess)

		if !c.params.WantsReturnAudio() {
			return
		}
		udp, ok := src.(*net.UDPAddr)
		if !ok {
			log.Printf("call %s: cannot send audio back to a %T source address", c.id, src)
			return
		}

		dst := &net.UDPAddr{IP: udp.IP, Port: c.params.ReturnPort}
		conn, err := net.DialUDP("udp", nil, dst)
		if err != nil {
			log.Printf("call %s: cannot open the return path to %s: %v", c.id, dst, err)
			return
		}
		ssrc, err := rtp.NewSSRC()
		if err != nil {
			conn.Close()
			log.Printf("call %s: cannot generate a return SSRC: %v", c.id, err)
			return
		}
		sender, err := rtp.NewSender(conn, c.params.Codec, ssrc, nil)
		if err != nil {
			conn.Close()
			log.Printf("call %s: cannot start the return sender: %v", c.id, err)
			return
		}
		c.egress.Store(sender)
		log.Printf("call %s: sending replies to %s as ssrc=%#08x", c.id, dst, ssrc)
	})
}

// sendBack transmits one frame of synthesized audio to the caller. It is the
// pipeline's OnAudio hook, so it runs on the pipeline goroutine and must not
// block for long; a UDP write does not.
func (c *activeCall) sendBack(pcm []int16) {
	sender := c.egress.Load()
	if sender == nil {
		return
	}
	if err := sender.Send(c.params.Codec.Encode(pcm)); err != nil {
		return
	}
	c.egressFwd.Add(1)
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
			// has stopped sending, not that a packet went missing mid-call. Do
			// not feed it to the recognizer: with nothing buffered the frame is
			// entirely invented, and inventing input for a speech recognizer
			// drags the utterance's confidence down for no reason.
			//
			// Mid-call holes with audio queued behind them are still forwarded,
			// since those do need to keep the timeline intact.
			if f.Concealed && c.jb.Stats().CurrentDepth == 0 {
				starved++
				continue
			}
			starved = 0
			c.forward(f)
		}
	}
}

// forward hands a frame to the pipeline, dropping it if the pipeline has fallen
// behind. Blocking here would stall playout, and a caller's voice cannot be
// paused, so the honest response to a slow pipeline is to drop audio and say so.
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
}

// idle reports whether the stream has gone silent long enough to be treated as
// ended. It is only consulted for calls that never sent EndCall.
func (c *activeCall) idle(now time.Time, timeout time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Sub(c.lastSeen) > timeout
}

func (c *activeCall) snapshot() (call.Result, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result, c.runErr, c.haveRes
}

// stats returns the RTP measurements, or a zero value if the stream never bound.
func (c *activeCall) stats() rtp.Stats {
	if sess := c.sess.Load(); sess != nil {
		return sess.Stats()
	}
	return rtp.Stats{}
}

// finish stops the call and reports it. Safe to call more than once; only the
// first call does the work, so a teardown racing the idle reaper cannot report
// the same call twice.
func (c *activeCall) finish(reason string) {
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.played
	<-c.piped

	if sender := c.egress.Load(); sender != nil {
		sender.Drain()
		sender.Close()
	}

	c.reportOnce.Do(func() { c.emitTelemetry(reason) })
}
