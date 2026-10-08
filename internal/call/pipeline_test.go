package call

import (
	"context"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/jbuf"
	"github.com/mharner33/voice-demo/internal/llm"
	"github.com/mharner33/voice-demo/internal/rtp"
	"github.com/mharner33/voice-demo/internal/stt"
	"github.com/mharner33/voice-demo/internal/tts"
)

// This file holds the phase-4 capstone: every production component from the WAV
// file to the RTP packets going back out, with no stubs in between and no cloud
// dependency. It is the regression test every later phase builds on, so it
// asserts the whole chain rather than any single layer.

// inboundFrames runs audio through the real packetizer, the real impairment
// model, and the real jitter buffer, returning the frames a recognizer would
// actually receive.
//
// The impairer's delays are applied as an ordering rather than as wall-clock
// sleeps, which keeps the test instant and exactly reproducible while still
// exercising the production code paths.
func inboundFrames(t *testing.T, audio codec.Audio, c codec.Codec, netCfg chaos.Network, seed int64) ([]jbuf.Frame, chaos.SendStats, jbuf.Stats) {
	t.Helper()

	imp, err := chaos.NewImpairer(netCfg, seed)
	if err != nil {
		t.Fatalf("NewImpairer: %v", err)
	}
	pktz := rtp.NewPacketizerAt(0x5EED1234, c, 2000, 7_000_000)

	type arrival struct {
		pkt *pionrtp.Packet
		at  time.Duration
	}
	var arrivals []arrival
	for i, frame := range codec.FramePCM(audio.PCM) {
		pkt := pktz.Packetize(c.Encode(frame))
		sent := time.Duration(i) * codec.FrameDuration

		act := imp.Next()
		if act.Drop {
			continue
		}
		arrivals = append(arrivals, arrival{pkt: pkt, at: sent + act.Delay})
		if act.Duplicate {
			arrivals = append(arrivals, arrival{pkt: pkt, at: sent + act.Delay})
		}
	}
	sort.SliceStable(arrivals, func(i, j int) bool { return arrivals[i].at < arrivals[j].at })

	// A buffer deep enough to absorb the lossy-wan profile's 80 ms of jitter
	// plus its 30 ms reorder hold.
	jb, err := jbuf.New(jbuf.Config{
		Codec:       c,
		TargetDepth: 8,
		MaxDepth:    40,
		Conceal:     jbuf.ConcealRepeat,
	})
	if err != nil {
		t.Fatalf("jbuf.New: %v", err)
	}

	var frames []jbuf.Frame
	for _, a := range arrivals {
		jb.Push(a.pkt)
		if f, ok := jb.Pop(); ok {
			frames = append(frames, f)
		}
	}
	frames = append(frames, jb.Drain()...)

	return frames, imp.Stats(), jb.Stats()
}

// egress captures the audio the pipeline sends back, over real UDP.
type egress struct {
	conn   *net.UDPConn
	recv   *rtp.Receiver
	sender *rtp.Sender
	cancel context.CancelFunc
	done   chan struct{}

	// startSeq is pinned so reassembly can order by signed delta.
	startSeq uint16

	mu       sync.Mutex
	payloads map[uint16][]byte
}

func newEgress(t *testing.T, c codec.Codec) *egress {
	t.Helper()

	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("binding the egress receiver: %v", err)
	}
	sendConn, err := net.DialUDP("udp", nil, recvConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		recvConn.Close()
		t.Fatalf("dialing the egress receiver: %v", err)
	}

	e := &egress{
		conn:     sendConn,
		recv:     rtp.NewReceiver(recvConn, codec.SampleRate8k),
		payloads: make(map[uint16][]byte),
		done:     make(chan struct{}),
	}

	// Pin the starting sequence so reassembly can order frames by signed
	// delta, which is wrap-safe. Sorting raw uint16 values would rotate the
	// audio whenever the sequence crossed 65535.
	const startSeq = 4000
	e.startSeq = startSeq
	e.sender = rtp.NewSenderAt(sendConn, c, 0xE6E55001, nil, startSeq, 0)

	e.recv.OnPacket(func(_ *rtp.Session, pkt *pionrtp.Packet, _ net.Addr, _ time.Time) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.payloads[pkt.SequenceNumber] = pkt.Payload
	})

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go func() {
		defer close(e.done)
		if err := e.recv.Serve(ctx); err != nil {
			t.Errorf("egress Serve: %v", err)
		}
	}()

	t.Cleanup(func() {
		e.sender.Drain()
		cancel()
		<-e.done
		sendConn.Close()
		recvConn.Close()
	})
	return e
}

// send puts one synthesized frame on the wire.
func (e *egress) send(c codec.Codec, pcm []int16) {
	if err := e.sender.Send(c.Encode(pcm)); err != nil {
		panic("egress send: " + err.Error())
	}
}

// settle waits for the receiver to stop seeing new packets.
func (e *egress) settle() {
	last := ^uint64(0)
	for i := 0; i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		if got := e.recv.Packets(); got == last {
			return
		}
		last = e.recv.Packets()
	}
}

// pcm reassembles everything received, in send order.
func (e *egress) pcm(c codec.Codec) []int16 {
	e.mu.Lock()
	defer e.mu.Unlock()

	offsets := make([]int, 0, len(e.payloads))
	byOffset := make(map[int][]byte, len(e.payloads))
	for seq, payload := range e.payloads {
		off := int(int16(seq - e.startSeq))
		offsets = append(offsets, off)
		byOffset[off] = payload
	}
	sort.Ints(offsets)

	var out []int16
	for _, off := range offsets {
		out = append(out, c.Decode(byOffset[off])...)
	}
	return out
}

// TestFullPipelineEndToEnd is the phase-4 acceptance test. Real audio goes in
// as RTP over an impaired network, through the jitter buffer, the recognizer,
// the agent with a real tool call, the synthesizer, and back out as RTP over a
// real socket. Everything is deterministic and nothing touches the cloud.
func TestFullPipelineEndToEnd(t *testing.T) {
	const (
		seed     = 20260108
		callID   = "c-capstone"
		theCodec = codec.PCMU
	)

	// One utterance that triggers the account lookup, so the trace includes a
	// tool call rather than a bare completion.
	const phrase = "hello I'm calling about my account balance"

	transcriber, err := stt.NewMock(stt.MockConfig{
		Phrases:            []string{phrase},
		FramesPerPartial:   25,
		FramesPerUtterance: 100,
		DegradeOnConcealed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := llm.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := llm.NewMock(llm.MockConfig{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	synth, err := tts.NewMock(tts.MockConfig{})
	if err != nil {
		t.Fatal(err)
	}

	// Inbound leg: 2 seconds of audio over the worst profile the demo ships.
	audio := codec.Tone(440, codec.SampleRate8k, 2*time.Second, 0.5)
	frames, sent, jbStats := inboundFrames(t, audio, theCodec, chaos.Profiles["lossy-wan"].Network, seed)
	if len(frames) == 0 {
		t.Fatal("the inbound leg produced no frames")
	}
	if sent.Dropped == 0 {
		t.Fatal("the lossy-wan profile dropped nothing; the test would prove little")
	}

	// Outbound leg over a real socket.
	out := newEgress(t, theCodec)

	s, err := New(Config{
		CallID: callID,
		STT:    transcriber,
		LLM:    agent,
		TTS:    synth,
		Tools:  reg,
		OnAudio: func(pcm []int16) {
			out.send(theCodec, pcm)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Feed the jitter buffer's output into the pipeline.
	audioIn := make(chan stt.Audio)
	go func() {
		defer close(audioIn)
		for _, f := range frames {
			audioIn <- stt.Audio{PCM: f.PCM, Concealed: f.Concealed}
		}
	}()

	res, err := s.Run(context.Background(), audioIn)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// --- the transcript reached the agent ---

	if len(res.Turns) == 0 {
		t.Fatal("no turns were produced")
	}
	turn := res.Turns[0]
	if turn.Err != nil {
		t.Fatalf("turn 0 failed: %v", turn.Err)
	}
	if turn.Transcript != phrase {
		t.Errorf("transcript = %q, want %q", turn.Transcript, phrase)
	}

	// --- the agent called the tool and used its result ---

	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0] != llm.AccountLookupToolName {
		t.Errorf("tools used = %v, want [%s]", turn.ToolCalls, llm.AccountLookupToolName)
	}
	if !strings.Contains(turn.Reply, "Dana Okafor") {
		t.Errorf("reply %q does not carry data only the tool could supply", turn.Reply)
	}

	// --- packet loss is visible in the AI layer, not just the network layer ---

	if res.ConcealedFramesIn == 0 {
		t.Error("no concealed frames reached the recognizer despite network loss")
	}
	if turn.Confidence >= 0.95 {
		t.Errorf("confidence = %v; concealed audio should have degraded it below 0.95",
			turn.Confidence)
	}

	// --- the reply came back as audio on the wire ---

	out.sender.Drain()
	out.settle()

	if res.FramesOut == 0 {
		t.Fatal("no audio was synthesized")
	}
	gotPCM := out.pcm(theCodec)
	if len(gotPCM) == 0 {
		t.Fatal("nothing arrived on the egress socket")
	}
	if wantFrames := res.FramesOut; len(gotPCM) != wantFrames*codec.SamplesPerFrame {
		t.Errorf("received %d samples, want %d (%d frames)",
			len(gotPCM), wantFrames*codec.SamplesPerFrame, wantFrames)
	}

	// The strongest assertion available: the tone's pitch identifies the exact
	// text that was synthesized, so this proves the agent's own reply reached
	// the caller rather than some other utterance or stale audio.
	wantFreq := tts.TextFrequency(turn.Reply)
	gotFreq := measureFrequency(gotPCM, codec.SampleRate8k)
	if math.Abs(gotFreq-wantFreq) > wantFreq*0.1 {
		t.Errorf("egress audio measured %.1f Hz, want %.1f Hz for the reply %q",
			gotFreq, wantFreq, turn.Reply)
	}

	t.Logf("inbound:  %d packets sent, %d dropped by the network, %d concealed by the buffer (%.1f%%)",
		sent.Offered, sent.Dropped, jbStats.Concealed, jbStats.ConcealRate()*100)
	t.Logf("recognized: %q (confidence %.3f, %.1f%% of audio was filler)",
		turn.Transcript, turn.Confidence, res.ConcealedFraction()*100)
	t.Logf("agent:    tool %s -> %q (%d in / %d out tokens, %d rounds)",
		turn.ToolCalls[0], turn.Reply, turn.InputTokens, turn.OutputTokens, turn.ToolRounds)
	t.Logf("outbound: %d frames synthesized, %d samples received at %.1f Hz (expected %.1f Hz)",
		res.FramesOut, len(gotPCM), gotFreq, wantFreq)
}

// TestFullPipelineIsDeterministic is what makes this test suitable as the CI
// regression gate for every later phase: the same inputs must produce the same
// transcript, reply, tool calls and token counts, every time.
func TestFullPipelineIsDeterministic(t *testing.T) {
	run := func() Result {
		transcriber, err := stt.NewMock(stt.MockConfig{
			Phrases:            []string{"hello I'm calling about my account balance"},
			FramesPerPartial:   25,
			FramesPerUtterance: 100,
			DegradeOnConcealed: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		reg, err := llm.DefaultRegistry()
		if err != nil {
			t.Fatal(err)
		}
		agent, err := llm.NewMock(llm.MockConfig{Registry: reg})
		if err != nil {
			t.Fatal(err)
		}
		synth, err := tts.NewMock(tts.MockConfig{})
		if err != nil {
			t.Fatal(err)
		}

		audio := codec.Tone(440, codec.SampleRate8k, 2*time.Second, 0.5)
		frames, _, _ := inboundFrames(t, audio, codec.PCMU,
			chaos.Profiles["lossy-wan"].Network, 777)

		s, err := New(Config{
			CallID: "c-det", STT: transcriber, LLM: agent, TTS: synth, Tools: reg,
		})
		if err != nil {
			t.Fatal(err)
		}

		ch := make(chan stt.Audio)
		go func() {
			defer close(ch)
			for _, f := range frames {
				ch <- stt.Audio{PCM: f.PCM, Concealed: f.Concealed}
			}
		}()
		res, err := s.Run(context.Background(), ch)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	a, b := run(), run()

	if len(a.Turns) != len(b.Turns) {
		t.Fatalf("turn counts differ: %d vs %d", len(a.Turns), len(b.Turns))
	}
	for i := range a.Turns {
		x, y := a.Turns[i], b.Turns[i]
		if x.Transcript != y.Transcript {
			t.Errorf("turn %d transcript differs: %q vs %q", i, x.Transcript, y.Transcript)
		}
		if x.Reply != y.Reply {
			t.Errorf("turn %d reply differs: %q vs %q", i, x.Reply, y.Reply)
		}
		if x.Confidence != y.Confidence {
			t.Errorf("turn %d confidence differs: %v vs %v", i, x.Confidence, y.Confidence)
		}
		if x.InputTokens != y.InputTokens || x.OutputTokens != y.OutputTokens {
			t.Errorf("turn %d tokens differ: %d/%d vs %d/%d",
				i, x.InputTokens, x.OutputTokens, y.InputTokens, y.OutputTokens)
		}
		if strings.Join(x.ToolCalls, ",") != strings.Join(y.ToolCalls, ",") {
			t.Errorf("turn %d tool calls differ: %v vs %v", i, x.ToolCalls, y.ToolCalls)
		}
	}
	// Audio accounting must match exactly; only wall-clock timings may differ.
	if a.FramesIn != b.FramesIn || a.ConcealedFramesIn != b.ConcealedFramesIn {
		t.Errorf("inbound accounting differs: %d/%d vs %d/%d",
			a.FramesIn, a.ConcealedFramesIn, b.FramesIn, b.ConcealedFramesIn)
	}
	if a.FramesOut != b.FramesOut {
		t.Errorf("FramesOut differs: %d vs %d", a.FramesOut, b.FramesOut)
	}
}

// TestPipelineOnCleanNetworkKeepsFullConfidence is the control for the capstone:
// with no impairment nothing is concealed, so the recognizer reports full
// confidence. If this and the impaired case agreed, the degradation signal would
// be meaningless.
func TestPipelineOnCleanNetworkKeepsFullConfidence(t *testing.T) {
	transcriber, err := stt.NewMock(stt.MockConfig{
		Phrases:            []string{"hello I'm calling about my account balance"},
		FramesPerPartial:   25,
		FramesPerUtterance: 100,
		Confidence:         0.95,
		DegradeOnConcealed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := llm.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := llm.NewMock(llm.MockConfig{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	synth, err := tts.NewMock(tts.MockConfig{})
	if err != nil {
		t.Fatal(err)
	}

	audio := codec.Tone(440, codec.SampleRate8k, 2*time.Second, 0.5)
	frames, sent, jbStats := inboundFrames(t, audio, codec.PCMU, chaos.Network{}, 1)

	if sent.Dropped != 0 {
		t.Fatalf("the clean profile dropped %d packets", sent.Dropped)
	}
	if jbStats.Concealed != 0 {
		t.Fatalf("the buffer concealed %d frames on a clean network", jbStats.Concealed)
	}

	s, err := New(Config{
		CallID: "c-clean", STT: transcriber, LLM: agent, TTS: synth, Tools: reg,
	})
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan stt.Audio)
	go func() {
		defer close(ch)
		for _, f := range frames {
			ch <- stt.Audio{PCM: f.PCM, Concealed: f.Concealed}
		}
	}()
	res, err := s.Run(context.Background(), ch)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Turns) == 0 {
		t.Fatal("no turns")
	}
	if res.ConcealedFramesIn != 0 {
		t.Errorf("ConcealedFramesIn = %d on a clean network, want 0", res.ConcealedFramesIn)
	}
	if got := res.Turns[0].Confidence; got != 0.95 {
		t.Errorf("confidence = %v on a clean network, want the full 0.95", got)
	}
	t.Logf("clean network: confidence %.3f, 0 frames concealed", res.Turns[0].Confidence)
}

// measureFrequency estimates a tone's frequency from its zero crossings. A sine
// wave crosses zero twice per cycle.
func measureFrequency(pcm []int16, sampleRate int) float64 {
	if len(pcm) < 2 {
		return 0
	}
	var crossings int
	for i := 1; i < len(pcm); i++ {
		if (pcm[i-1] < 0) != (pcm[i] < 0) {
			crossings++
		}
	}
	seconds := float64(len(pcm)) / float64(sampleRate)
	return float64(crossings) / 2 / seconds
}
