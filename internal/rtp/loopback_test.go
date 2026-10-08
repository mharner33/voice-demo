package rtp

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
)

// testPaceInterval compresses a call into less wall-clock time than real time.
// RTP timestamps still advance at the true 20 ms rate, so the stream looks
// exactly like a real one to the receiver; only the wall clock is sped up.
const testPaceInterval = 2 * time.Millisecond

// socketBufferBytes is generous enough that the kernel does not drop datagrams
// on the loopback path. Without this the test would measure the OS receive queue
// overflowing rather than the impairments under test.
const socketBufferBytes = 4 << 20

// loopback wires a sender to a receiver over real UDP on localhost.
type loopback struct {
	t        *testing.T
	recv     *Receiver
	sendConn *net.UDPConn
	cancel   context.CancelFunc
	done     chan struct{}
}

func newLoopback(t *testing.T) *loopback {
	t.Helper()

	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("binding receiver: %v", err)
	}
	if err := recvConn.SetReadBuffer(socketBufferBytes); err != nil {
		t.Logf("could not raise the receive buffer: %v", err)
	}

	sendConn, err := net.DialUDP("udp", nil, recvConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		recvConn.Close()
		t.Fatalf("dialing sender: %v", err)
	}
	if err := sendConn.SetWriteBuffer(socketBufferBytes); err != nil {
		t.Logf("could not raise the send buffer: %v", err)
	}

	recv := NewReceiver(recvConn, codec.SampleRate8k)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		if err := recv.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	lb := &loopback{t: t, recv: recv, sendConn: sendConn, cancel: cancel, done: done}
	t.Cleanup(func() {
		lb.stop()
		sendConn.Close()
		recvConn.Close()
	})
	return lb
}

func (lb *loopback) stop() {
	lb.cancel()
	select {
	case <-lb.done:
	case <-time.After(2 * time.Second):
		lb.t.Error("receiver did not stop within 2s")
	}
}

// settle waits for the receiver's packet count to stop changing, so assertions
// are not racing packets still in the socket queue.
func (lb *loopback) settle() {
	last := ^uint64(0)
	for i := 0; i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		if got := lb.recv.Packets(); got == last {
			return
		} else {
			last = got
		}
	}
	lb.t.Log("receiver packet count never stabilized")
}

// toneFrames builds a paced stream of encoded G.711 frames.
func toneFrames(t *testing.T, c codec.Codec, n int) ([][]byte, []int16) {
	t.Helper()
	audio := codec.Tone(440, codec.SampleRate8k, time.Duration(n)*codec.FrameDuration, 0.5)
	pcmFrames := codec.FramePCM(audio.PCM)
	if len(pcmFrames) != n {
		t.Fatalf("built %d frames, want %d", len(pcmFrames), n)
	}
	out := make([][]byte, n)
	for i, f := range pcmFrames {
		out[i] = c.Encode(f)
	}
	return out, audio.PCM
}

// TestLoopbackCleanDeliversEveryPacketIntact is the control: over real UDP with
// no impairment, every packet must arrive and the audio must survive the trip
// byte for byte.
func TestLoopbackCleanDeliversEveryPacketIntact(t *testing.T) {
	const frames = 250

	lb := newLoopback(t)
	payloads, wantPCM := toneFrames(t, codec.PCMU, frames)

	// Collect payloads keyed by sequence number so the audio can be
	// reassembled regardless of arrival order.
	var mu sync.Mutex
	got := make(map[uint16][]byte)
	lb.recv.OnPacket(func(_ *Session, pkt *pionrtp.Packet, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		got[pkt.SequenceNumber] = pkt.Payload
	})

	sender, err := NewSender(lb.sendConn, codec.PCMU, 0x1111, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Stream(context.Background(), payloads, testPaceInterval); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sender.Drain()
	lb.settle()

	ss := sender.Stats()
	if ss.PacketsSent != frames {
		t.Errorf("sender sent %d packets, want %d", ss.PacketsSent, frames)
	}
	if ss.WriteErrors != 0 {
		t.Errorf("sender had %d write errors", ss.WriteErrors)
	}

	sess, ok := lb.recv.Session(sender.SSRC())
	if !ok {
		t.Fatal("receiver never saw the stream")
	}
	st := sess.Stats()
	if st.Received != frames {
		t.Errorf("receiver got %d packets, want %d (loopback UDP should not drop)",
			st.Received, frames)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0", st.Lost)
	}
	if st.Duplicated != 0 {
		t.Errorf("Duplicated = %d, want 0", st.Duplicated)
	}
	if lb.recv.Malformed() != 0 {
		t.Errorf("Malformed = %d, want 0", lb.recv.Malformed())
	}

	// Reassemble in sequence order and confirm the audio is unchanged.
	mu.Lock()
	defer mu.Unlock()
	if len(got) != frames {
		t.Fatalf("collected %d distinct payloads, want %d", len(got), frames)
	}
	var decoded []int16
	seqs := make([]uint16, 0, len(got))
	for s := range got {
		seqs = append(seqs, s)
	}
	// Sequence numbers are contiguous here, so sorting recovers send order.
	for i := 1; i < len(seqs); i++ {
		for j := i; j > 0 && seqs[j] < seqs[j-1]; j-- {
			seqs[j], seqs[j-1] = seqs[j-1], seqs[j]
		}
	}
	for _, s := range seqs {
		decoded = append(decoded, codec.PCMU.Decode(got[s])...)
	}
	if len(decoded) != len(wantPCM) {
		t.Fatalf("reassembled %d samples, want %d", len(decoded), len(wantPCM))
	}
	// The round trip through G.711 is lossy, but the *same* lossy transform was
	// applied on the way in, so the comparison is exact.
	wantRoundTripped := codec.PCMU.Decode(codec.PCMU.Encode(wantPCM))
	for i := range decoded {
		if decoded[i] != wantRoundTripped[i] {
			t.Fatalf("sample %d changed in transit: got %d, want %d",
				i, decoded[i], wantRoundTripped[i])
		}
	}
}

// TestLoopbackMeasuredLossMatchesInjectedLoss is the phase-2 acceptance test over
// a real socket: the receiver, working only from sequence gaps, must arrive at
// exactly the number of packets the sender knows it dropped.
func TestLoopbackMeasuredLossMatchesInjectedLoss(t *testing.T) {
	const (
		frames    = 400
		lossPct   = 5.0
		sentinels = 3
	)

	lb := newLoopback(t)
	payloads, _ := toneFrames(t, codec.PCMU, frames)

	imp, err := chaos.NewImpairer(chaos.Network{LossPct: lossPct}, 20260108)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSender(lb.sendConn, codec.PCMU, 0x2222, imp)
	if err != nil {
		t.Fatal(err)
	}

	if err := sender.Stream(context.Background(), payloads, testPaceInterval); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Loss of the final packets is undetectable without RTCP — the receiver
	// cannot know a sequence number it never saw. Send a few guaranteed frames
	// so the top of the sequence range is observable, which is what a real
	// endpoint's RTCP BYE accomplishes.
	if err := imp.SetNetwork(chaos.Network{}); err != nil {
		t.Fatal(err)
	}
	tail, _ := toneFrames(t, codec.PCMU, sentinels)
	for _, f := range tail {
		if err := sender.Send(f); err != nil {
			t.Fatalf("sending sentinel: %v", err)
		}
	}

	sender.Drain()
	lb.settle()

	ss := sender.Stats()
	sess, ok := lb.recv.Session(sender.SSRC())
	if !ok {
		t.Fatal("receiver never saw the stream")
	}
	st := sess.Stats()

	total := uint64(frames + sentinels)
	if st.Expected != total {
		t.Errorf("Expected = %d, want %d", st.Expected, total)
	}

	// The assertion this whole phase exists for.
	if st.Lost != ss.Dropped {
		t.Errorf("receiver measured %d lost but sender dropped %d", st.Lost, ss.Dropped)
	}
	if st.Received != total-ss.Dropped {
		t.Errorf("Received = %d, want %d", st.Received, total-ss.Dropped)
	}
	if ss.WriteErrors != 0 {
		t.Errorf("sender had %d write errors — the socket, not the model, lost packets",
			ss.WriteErrors)
	}

	// Sanity-check the injected rate landed near the target.
	gotPct := float64(ss.Dropped) / float64(total) * 100
	if gotPct < lossPct/2 || gotPct > lossPct*2 {
		t.Errorf("injected %.2f%% loss, configured %.1f%%", gotPct, lossPct)
	}
	t.Logf("injected %d/%d dropped (%.2f%%); receiver independently measured "+
		"%d lost (%.2f%%), jitter %.2f ms, MOS %.2f",
		ss.Dropped, total, gotPct, st.Lost, st.LossPct, st.JitterMs, st.MOS())
}

// TestLoopbackDuplicatesAreDetected confirms duplicate suppression works on the
// real wire, not just against synthesized input.
func TestLoopbackDuplicatesAreDetected(t *testing.T) {
	const frames = 200

	lb := newLoopback(t)
	payloads, _ := toneFrames(t, codec.PCMU, frames)

	imp, err := chaos.NewImpairer(chaos.Network{DupPct: 20}, 5)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSender(lb.sendConn, codec.PCMU, 0x3333, imp)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Stream(context.Background(), payloads, testPaceInterval); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sender.Drain()
	lb.settle()

	ss := sender.Stats()
	if ss.Duplicated == 0 {
		t.Fatal("no duplicates were injected")
	}

	sess, _ := lb.recv.Session(sender.SSRC())
	st := sess.Stats()

	if st.Duplicated != ss.Duplicated {
		t.Errorf("receiver counted %d duplicates, sender sent %d",
			st.Duplicated, ss.Duplicated)
	}
	if st.Received != frames {
		t.Errorf("Received = %d, want %d — duplicates must not inflate the count",
			st.Received, frames)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0", st.Lost)
	}
	t.Logf("%d duplicates injected and detected; Received stayed at %d",
		st.Duplicated, st.Received)
}

// TestLoopbackJitterIsObserved checks that injected delay variance shows up as
// measured jitter on a real socket. The bound is loose because the OS scheduler
// contributes its own noise — the point is that the signal is clearly present.
func TestLoopbackJitterIsObserved(t *testing.T) {
	const frames = 300

	lb := newLoopback(t)
	payloads, _ := toneFrames(t, codec.PCMU, frames)

	// Pace at the true 20 ms rate here: injected delay is only meaningful
	// relative to real packet spacing.
	imp, err := chaos.NewImpairer(chaos.Network{JitterMs: 15}, 9)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSender(lb.sendConn, codec.PCMU, 0x4444, imp)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sender.Stream(ctx, payloads[:120], codec.FrameDuration); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sender.Drain()
	lb.settle()

	sess, _ := lb.recv.Session(sender.SSRC())
	st := sess.Stats()

	if st.JitterMs < 1 {
		t.Errorf("JitterMs = %.3f with 15ms of injected jitter, want clearly > 0",
			st.JitterMs)
	}
	t.Logf("15ms injected jitter -> measured %.2f ms (max %.2f), %d reordered",
		st.JitterMs, st.MaxJitterMs, st.Reordered)
}

// TestReceiverSurvivesMalformedPackets matters because the receiver listens on a
// UDP port: anything on the network can send it garbage, and a call must not die
// because a stray packet arrived.
func TestReceiverSurvivesMalformedPackets(t *testing.T) {
	lb := newLoopback(t)

	for _, junk := range [][]byte{
		{},
		{0x00},
		{0xFF, 0xFF, 0xFF},
		[]byte("this is not an RTP packet"),
	} {
		if _, err := lb.sendConn.Write(junk); err != nil {
			t.Fatalf("writing junk: %v", err)
		}
	}

	// A valid stream must still work afterward.
	payloads, _ := toneFrames(t, codec.PCMU, 50)
	sender, err := NewSender(lb.sendConn, codec.PCMU, 0x5555, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Stream(context.Background(), payloads, testPaceInterval); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sender.Drain()
	lb.settle()

	sess, ok := lb.recv.Session(sender.SSRC())
	if !ok {
		t.Fatal("receiver stopped handling valid traffic after seeing garbage")
	}
	if st := sess.Stats(); st.Received != 50 {
		t.Errorf("Received = %d after malformed traffic, want 50", st.Received)
	}
	if lb.recv.Malformed() == 0 {
		t.Error("Malformed = 0; the junk packets were not counted")
	}
	t.Logf("%d malformed packets absorbed, %d valid packets still delivered",
		lb.recv.Malformed(), lb.recv.Packets())
}

// TestReceiverSeparatesConcurrentStreams covers the multi-call case: the gateway
// serves many calls on one port, and their statistics must not bleed together.
func TestReceiverSeparatesConcurrentStreams(t *testing.T) {
	lb := newLoopback(t)
	payloads, _ := toneFrames(t, codec.PCMU, 100)

	ssrcs := []uint32{0xAAA1, 0xAAA2, 0xAAA3}
	var wg sync.WaitGroup
	for _, ssrc := range ssrcs {
		wg.Add(1)
		go func(ssrc uint32) {
			defer wg.Done()
			sender, err := NewSender(lb.sendConn, codec.PCMU, ssrc, nil)
			if err != nil {
				t.Errorf("NewSender(%#x): %v", ssrc, err)
				return
			}
			if err := sender.Stream(context.Background(), payloads, testPaceInterval); err != nil {
				t.Errorf("Stream(%#x): %v", ssrc, err)
			}
			sender.Drain()
		}(ssrc)
	}
	wg.Wait()
	lb.settle()

	if got := len(lb.recv.Sessions()); got != len(ssrcs) {
		t.Errorf("receiver tracked %d sessions, want %d", got, len(ssrcs))
	}
	for _, ssrc := range ssrcs {
		sess, ok := lb.recv.Session(ssrc)
		if !ok {
			t.Errorf("no session for SSRC %#x", ssrc)
			continue
		}
		st := sess.Stats()
		if st.Received != 100 {
			t.Errorf("SSRC %#x: Received = %d, want 100", ssrc, st.Received)
		}
		if st.Lost != 0 {
			t.Errorf("SSRC %#x: Lost = %d, want 0", ssrc, st.Lost)
		}
	}
}

func TestReceiverStopsOnContextCancel(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	recv := NewReceiver(conn, codec.SampleRate8k)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- recv.Serve(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve returned %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s of cancellation")
	}
}

func TestReceiverStopsOnConnClose(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	recv := NewReceiver(conn, codec.SampleRate8k)
	errCh := make(chan error, 1)
	go func() { errCh <- recv.Serve(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	conn.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve returned %v on close, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s of the connection closing")
	}
}

func TestSenderRejectsSendAfterClose(t *testing.T) {
	lb := newLoopback(t)
	sender, err := NewSender(lb.sendConn, codec.PCMU, 0x6666, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(make([]byte, codec.G711FrameBytes)); err == nil {
		t.Error("Send succeeded after Close")
	}
}

func TestSenderStreamHonorsContextCancel(t *testing.T) {
	lb := newLoopback(t)
	payloads, _ := toneFrames(t, codec.PCMU, 1000)

	sender, err := NewSender(lb.sendConn, codec.PCMU, 0x7777, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = sender.Stream(ctx, payloads, 10*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Stream returned nil after its context expired")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Stream took %v to notice cancellation", elapsed)
	}
	sender.Drain()
}
