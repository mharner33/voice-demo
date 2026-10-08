package jbuf

import (
	"math"
	"sync"
	"testing"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
)

// baseTS is an arbitrary starting RTP timestamp, deliberately not zero so that
// the tests exercise the extended-timestamp arithmetic rather than accidentally
// relying on an origin of zero.
const baseTS = uint32(1_000_000)

// markerByte maps a frame index to a payload byte that uniquely identifies it
// after G.711 decoding. 0x7F and 0xFF both decode to zero in u-law, so the range
// is kept to 1..126 where every value decodes distinctly.
func markerByte(i int) byte { return byte(1 + i%126) }

// frameFor builds the packet for frame index i of a contiguous stream.
func frameFor(i int) *pionrtp.Packet {
	return packetAt(uint16(i), baseTS+uint32(i)*codec.SamplesPerFrame, markerByte(i))
}

func packetAt(seq uint16, ts uint32, marker byte) *pionrtp.Packet {
	payload := make([]byte, codec.G711FrameBytes)
	for i := range payload {
		payload[i] = marker
	}
	return &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    uint8(codec.PayloadTypePCMU),
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xFEEDFACE,
		},
		Payload: payload,
	}
}

// wantSample is the PCM value a frame's payload decodes to.
func wantSample(i int) int16 { return codec.ULawToLinear(markerByte(i)) }

// frameIndexOf recovers which frame a popped slot holds, from its timestamp.
func frameIndexOf(t *testing.T, f Frame) int {
	t.Helper()
	delta := f.Timestamp - baseTS
	if delta%codec.SamplesPerFrame != 0 {
		t.Fatalf("frame timestamp %d is off the frame grid", f.Timestamp)
	}
	return int(delta / codec.SamplesPerFrame)
}

func newTestBuffer(t *testing.T, cfg Config) *Buffer {
	t.Helper()
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return b
}

// popAll drains n playout slots, skipping slots the buffer is not ready to emit.
func popAll(b *Buffer, n int) []Frame {
	out := make([]Frame, 0, n)
	for i := 0; i < n; i++ {
		f, ok := b.Pop()
		if !ok {
			continue
		}
		out = append(out, f)
	}
	return out
}

// runStream simulates how a call actually runs: the consumer drains at the same
// rate the producer fills, one Pop per Push once playout has begun, with a final
// Drain at end of call. Depth therefore hovers around the target instead of
// growing without bound, which is the regime the buffer is designed for.
func runStream(b *Buffer, pushOrder []int) []Frame {
	var out []Frame
	for _, i := range pushOrder {
		b.Push(frameFor(i))
		if f, ok := b.Pop(); ok {
			out = append(out, f)
		}
	}
	return append(out, b.Drain()...)
}

// TestSteadyStateLockstepStream covers the operating regime a real call lives
// in. The push-everything-then-drain tests elsewhere in this file verify
// ordering logic; this one verifies the buffer holds its depth near the target
// over a long stream and never evicts, which is what keeps latency stable.
func TestSteadyStateLockstepStream(t *testing.T) {
	const (
		frames = 1000
		target = 3
	)
	b := newTestBuffer(t, Config{TargetDepth: target, MaxDepth: 25})

	order := make([]int, frames)
	for i := range order {
		order[i] = i
	}

	got := runStream(b, order)
	if len(got) != frames {
		t.Fatalf("emitted %d slots from %d frames, want %d", len(got), frames, frames)
	}
	for i, f := range got {
		if f.Concealed {
			t.Errorf("slot %d concealed in a perfect stream", i)
		}
		if idx := frameIndexOf(t, f); idx != i {
			t.Errorf("slot %d holds frame %d, want %d", i, idx, i)
		}
	}

	st := b.Stats()
	if st.Evicted != 0 {
		t.Errorf("Evicted = %d in lockstep, want 0 — depth should track the target",
			st.Evicted)
	}
	if st.Concealed != 0 {
		t.Errorf("Concealed = %d, want 0", st.Concealed)
	}
	// Depth never needs to exceed the prebuffer in lockstep, so latency stays
	// at the target rather than drifting upward over a long call.
	if st.MaxObservedDepth > target+1 {
		t.Errorf("MaxObservedDepth = %d, want <= %d — buffer latency grew during the call",
			st.MaxObservedDepth, target+1)
	}
	t.Logf("1000-frame lockstep call: max depth %d frames (%.0f ms), 0 evicted, 0 concealed",
		st.MaxObservedDepth, float64(st.MaxObservedDepth)*20)
}

// TestLockstepWithReorderingAndLoss is the scripted-sequence test from the plan,
// run in the realistic lockstep regime: reordering within the buffer's depth is
// repaired, genuine gaps are concealed in place, and playout never drifts.
func TestLockstepWithReorderingAndLoss(t *testing.T) {
	// Frames 0..39 with 11 and 12 never sent, and several adjacent swaps.
	order := []int{
		0, 1, 3, 2, 4, 6, 5, 7, 8, 10, 9, // 11, 12 missing
		14, 13, 15, 16, 18, 17, 19, 20, 22, 21, 23,
		24, 25, 27, 26, 28, 30, 29, 31, 32, 34, 33, 35, 36, 38, 37, 39,
	}
	missing := map[int]bool{11: true, 12: true}

	b := newTestBuffer(t, Config{TargetDepth: 4, MaxDepth: 25})
	got := runStream(b, order)

	// 40 slots span the stream, including the two holes.
	const slots = 40
	if len(got) != slots {
		t.Fatalf("emitted %d slots, want %d", len(got), slots)
	}
	for i, f := range got {
		if idx := frameIndexOf(t, f); idx != i {
			t.Fatalf("slot %d holds frame %d: playout drifted", i, idx)
		}
		if missing[i] {
			if !f.Concealed {
				t.Errorf("slot %d should be concealed", i)
			}
			continue
		}
		if f.Concealed {
			t.Errorf("slot %d concealed, but its packet arrived within the buffer depth", i)
		}
		if f.PCM[0] != wantSample(i) {
			t.Errorf("slot %d audio = %d, want %d", i, f.PCM[0], wantSample(i))
		}
	}

	st := b.Stats()
	if st.Concealed != uint64(len(missing)) {
		t.Errorf("Concealed = %d, want %d", st.Concealed, len(missing))
	}
	if st.Late != 0 {
		t.Errorf("Late = %d, want 0 — every reorder was within the buffer depth", st.Late)
	}
	if st.Evicted != 0 {
		t.Errorf("Evicted = %d, want 0", st.Evicted)
	}
}

// TestInOrderDeliveryPlaysBackUnchanged is the control case: a perfect stream
// must come out in order with no concealment and no loss.
func TestInOrderDeliveryPlaysBackUnchanged(t *testing.T) {
	const frames = 50
	// MaxDepth exceeds the frame count because this test pushes the whole
	// stream before popping; a smaller cap would correctly evict the early
	// frames and the test would be measuring eviction instead of ordering.
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: frames + 10})

	for i := 0; i < frames; i++ {
		if got := b.Push(frameFor(i)); got != PushAccepted {
			t.Fatalf("frame %d: Push = %v, want accepted", i, got)
		}
	}

	got := popAll(b, frames)
	if len(got) != frames {
		t.Fatalf("popped %d frames, want %d", len(got), frames)
	}
	for i, f := range got {
		if f.Concealed {
			t.Errorf("slot %d was concealed in a perfect stream", i)
		}
		if idx := frameIndexOf(t, f); idx != i {
			t.Errorf("slot %d holds frame %d, want %d", i, idx, i)
		}
		if len(f.PCM) != codec.SamplesPerFrame {
			t.Fatalf("slot %d has %d samples, want %d", i, len(f.PCM), codec.SamplesPerFrame)
		}
		if f.PCM[0] != wantSample(i) {
			t.Errorf("slot %d audio = %d, want %d", i, f.PCM[0], wantSample(i))
		}
	}

	st := b.Stats()
	if st.Concealed != 0 {
		t.Errorf("Concealed = %d, want 0", st.Concealed)
	}
	if st.Late != 0 || st.Duplicate != 0 || st.Evicted != 0 {
		t.Errorf("Late=%d Duplicate=%d Evicted=%d, want all 0",
			st.Late, st.Duplicate, st.Evicted)
	}
	if st.ConcealRate() != 0 {
		t.Errorf("ConcealRate = %v, want 0", st.ConcealRate())
	}
}

// TestReorderedDeliveryIsRepaired is the buffer's core purpose: packets that
// arrive out of order must play back in order, with no concealment, because
// they all arrived within the buffer's depth.
func TestReorderedDeliveryIsRepaired(t *testing.T) {
	// Adjacent swaps plus one three-deep rotation.
	pushOrder := []int{1, 0, 3, 2, 4, 7, 5, 6, 9, 8, 10, 13, 12, 11, 14}

	b := newTestBuffer(t, Config{TargetDepth: 5, MaxDepth: 30})
	for _, i := range pushOrder {
		if got := b.Push(frameFor(i)); got != PushAccepted {
			t.Fatalf("frame %d: Push = %v, want accepted", i, got)
		}
	}

	got := popAll(b, len(pushOrder))
	if len(got) != len(pushOrder) {
		t.Fatalf("popped %d frames, want %d", len(got), len(pushOrder))
	}
	for i, f := range got {
		if f.Concealed {
			t.Errorf("slot %d concealed, but every packet arrived in time", i)
		}
		if idx := frameIndexOf(t, f); idx != i {
			t.Errorf("slot %d holds frame %d, want %d — reordering was not repaired",
				i, idx, i)
		}
		if f.PCM[0] != wantSample(i) {
			t.Errorf("slot %d audio = %d, want %d", i, f.PCM[0], wantSample(i))
		}
	}
	if st := b.Stats(); st.Concealed != 0 {
		t.Errorf("Concealed = %d, want 0", st.Concealed)
	}
}

// TestGapsAreConcealedInPlace checks that a missing packet produces exactly one
// concealed slot at the right position, and that the frames after it stay
// correctly aligned. Misaligning playout after a gap would shift all subsequent
// audio, which is far worse than the single lost frame.
func TestGapsAreConcealedInPlace(t *testing.T) {
	const frames = 30
	missing := map[int]bool{5: true, 6: true, 17: true}

	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: frames + 10})
	for i := 0; i < frames; i++ {
		if missing[i] {
			continue
		}
		b.Push(frameFor(i))
	}

	got := popAll(b, frames)
	if len(got) != frames {
		t.Fatalf("popped %d slots, want %d", len(got), frames)
	}
	for i, f := range got {
		idx := frameIndexOf(t, f)
		if idx != i {
			t.Fatalf("slot %d holds frame %d: playout drifted after a gap", i, idx)
		}
		if missing[i] {
			if !f.Concealed {
				t.Errorf("slot %d should be concealed", i)
			}
			continue
		}
		if f.Concealed {
			t.Errorf("slot %d concealed, but its packet arrived", i)
		}
		if f.PCM[0] != wantSample(i) {
			t.Errorf("slot %d audio = %d, want %d", i, f.PCM[0], wantSample(i))
		}
	}

	st := b.Stats()
	if st.Concealed != uint64(len(missing)) {
		t.Errorf("Concealed = %d, want %d", st.Concealed, len(missing))
	}
	wantRate := float64(len(missing)) / float64(frames)
	if math.Abs(st.ConcealRate()-wantRate) > 1e-9 {
		t.Errorf("ConcealRate = %v, want %v", st.ConcealRate(), wantRate)
	}
}

// TestLatePacketsAreDropped covers the distinguishing failure of a shallow
// buffer: the packet arrived, but playout had already moved past its slot, so it
// is useless. It must be counted as late rather than silently corrupting the
// timeline by being played in the wrong place.
func TestLatePacketsAreDropped(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 2, MaxDepth: 20})

	for i := 0; i < 6; i++ {
		b.Push(frameFor(i))
	}
	// Advance playout past frames 0-3.
	played := popAll(b, 4)
	if len(played) != 4 {
		t.Fatalf("popped %d frames, want 4", len(played))
	}

	// Frame 1's slot is long gone.
	if got := b.Push(frameFor(1)); got != PushLate {
		t.Errorf("Push of an already-played frame = %v, want late", got)
	}
	if st := b.Stats(); st.Late != 1 {
		t.Errorf("Late = %d, want 1", st.Late)
	}

	// The remaining frames must still play correctly, in order.
	rest := popAll(b, 2)
	for i, f := range rest {
		want := 4 + i
		if idx := frameIndexOf(t, f); idx != want {
			t.Errorf("slot %d holds frame %d, want %d", want, idx, want)
		}
		if f.Concealed {
			t.Errorf("frame %d concealed, but it arrived on time", want)
		}
	}
}

func TestDuplicatePacketsAreRejected(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: 20})

	for i := 0; i < 5; i++ {
		b.Push(frameFor(i))
	}
	for i := 0; i < 5; i++ {
		if got := b.Push(frameFor(i)); got != PushDuplicate {
			t.Errorf("second Push of frame %d = %v, want duplicate", i, got)
		}
	}

	st := b.Stats()
	if st.Duplicate != 5 {
		t.Errorf("Duplicate = %d, want 5", st.Duplicate)
	}
	if st.CurrentDepth != 5 {
		t.Errorf("CurrentDepth = %d, want 5 — duplicates must not consume depth",
			st.CurrentDepth)
	}

	// Each frame must play exactly once.
	got := popAll(b, 6)
	if len(got) != 6 {
		t.Fatalf("popped %d slots, want 6", len(got))
	}
	for i := 0; i < 5; i++ {
		if got[i].Concealed {
			t.Errorf("slot %d concealed", i)
		}
		if idx := frameIndexOf(t, got[i]); idx != i {
			t.Errorf("slot %d holds frame %d", i, idx)
		}
	}
	if !got[5].Concealed {
		t.Error("slot 5 should be concealed; nothing was pushed for it")
	}
}

// TestPrebufferingWaitsForTargetDepth checks the buffer refuses to start playout
// until it holds enough depth to absorb jitter. Starting immediately would
// guarantee an underrun on the very next irregular arrival.
func TestPrebufferingWaitsForTargetDepth(t *testing.T) {
	const target = 4
	b := newTestBuffer(t, Config{TargetDepth: target, MaxDepth: 20})

	for i := 0; i < target-1; i++ {
		b.Push(frameFor(i))
		if _, ok := b.Pop(); ok {
			t.Fatalf("Pop succeeded at depth %d, below the target of %d", i+1, target)
		}
		if st := b.Stats(); !st.Prebuffering {
			t.Errorf("Prebuffering = false at depth %d", i+1)
		}
	}

	b.Push(frameFor(target - 1))
	f, ok := b.Pop()
	if !ok {
		t.Fatalf("Pop failed at the target depth of %d", target)
	}
	if idx := frameIndexOf(t, f); idx != 0 {
		t.Errorf("playout started at frame %d, want 0", idx)
	}
	if st := b.Stats(); st.Prebuffering {
		t.Error("Prebuffering = true after playout started")
	}
}

// TestPrebufferingStartsAtEarliestFrame covers prebuffering combined with
// reordering: if the first packets arrive out of order, playout must still begin
// at the oldest one, not at whichever happened to arrive first.
func TestPrebufferingStartsAtEarliestFrame(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: 20})

	for _, i := range []int{2, 0, 1} {
		b.Push(frameFor(i))
	}

	f, ok := b.Pop()
	if !ok {
		t.Fatal("Pop failed at the target depth")
	}
	if idx := frameIndexOf(t, f); idx != 0 {
		t.Errorf("playout started at frame %d, want 0", idx)
	}
	if f.Concealed {
		t.Error("the first slot was concealed despite its packet having arrived")
	}
}

// TestOverflowEvictsOldestAndBoundsLatency covers the producer outrunning the
// consumer. The buffer must cap its depth — an unbounded buffer would trade a
// quality problem for an ever-growing latency problem.
func TestOverflowEvictsOldestAndBoundsLatency(t *testing.T) {
	const maxDepth = 5
	b := newTestBuffer(t, Config{TargetDepth: 2, MaxDepth: maxDepth})

	const frames = 20
	for i := 0; i < frames; i++ {
		if got := b.Push(frameFor(i)); got != PushAccepted {
			t.Fatalf("frame %d: Push = %v, want accepted", i, got)
		}
	}

	st := b.Stats()
	if st.CurrentDepth > maxDepth {
		t.Errorf("CurrentDepth = %d, exceeds MaxDepth %d", st.CurrentDepth, maxDepth)
	}
	if st.Evicted != frames-maxDepth {
		t.Errorf("Evicted = %d, want %d", st.Evicted, frames-maxDepth)
	}
	if st.MaxObservedDepth > maxDepth {
		t.Errorf("MaxObservedDepth = %d, exceeds MaxDepth %d", st.MaxObservedDepth, maxDepth)
	}

	// Playout resumes at the oldest surviving frame, and the surviving frames
	// play in order with no concealment — the evicted ones are simply gone.
	got := popAll(b, maxDepth)
	if len(got) != maxDepth {
		t.Fatalf("popped %d frames, want %d", len(got), maxDepth)
	}
	wantFirst := frames - maxDepth
	for i, f := range got {
		if f.Concealed {
			t.Errorf("slot %d concealed; evicted frames must not also be concealed", i)
		}
		if idx := frameIndexOf(t, f); idx != wantFirst+i {
			t.Errorf("slot %d holds frame %d, want %d", i, idx, wantFirst+i)
		}
	}
}

// TestOverflowWhilePlayingAdvancesPlayout checks that eviction during playout
// moves the playout position forward, so an evicted frame is counted once as an
// eviction and not a second time as a concealment.
func TestOverflowWhilePlayingAdvancesPlayout(t *testing.T) {
	const maxDepth = 4
	b := newTestBuffer(t, Config{TargetDepth: 2, MaxDepth: maxDepth})

	for i := 0; i < 2; i++ {
		b.Push(frameFor(i))
	}
	if _, ok := b.Pop(); !ok {
		t.Fatal("Pop failed at the target depth")
	}

	// Flood the buffer well past its cap.
	for i := 2; i < 30; i++ {
		b.Push(frameFor(i))
	}

	st := b.Stats()
	if st.CurrentDepth > maxDepth {
		t.Errorf("CurrentDepth = %d, exceeds MaxDepth %d", st.CurrentDepth, maxDepth)
	}
	if st.Evicted == 0 {
		t.Fatal("Evicted = 0 after flooding well past MaxDepth")
	}

	// Everything still buffered must play without concealment.
	got := popAll(b, maxDepth)
	for i, f := range got {
		if f.Concealed {
			t.Errorf("slot %d concealed after eviction advanced playout", i)
		}
	}
	if len(got) != maxDepth {
		t.Errorf("popped %d frames, want %d", len(got), maxDepth)
	}
}

func TestStarvationIsCountedSeparately(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 2, MaxDepth: 20})

	// Frames 0, 1 and 5: a hole at 2-4 with a frame waiting behind it, then
	// true starvation past frame 5.
	for _, i := range []int{0, 1, 5} {
		b.Push(frameFor(i))
	}

	got := popAll(b, 9)
	if len(got) != 9 {
		t.Fatalf("popped %d slots, want 9", len(got))
	}

	st := b.Stats()
	// Slots 2,3,4 are holes with frame 5 still buffered; slots 6,7,8 are
	// starvation with nothing left at all.
	if st.Concealed != 6 {
		t.Errorf("Concealed = %d, want 6", st.Concealed)
	}
	if st.Starved != 3 {
		t.Errorf("Starved = %d, want 3 — only the slots with an empty buffer count",
			st.Starved)
	}
}

// TestTimestampWrap covers the 32-bit RTP timestamp rollover. A call long enough
// to wrap must keep playing in order rather than treating the wrap as a jump
// backward and discarding everything as late.
func TestTimestampWrap(t *testing.T) {
	const frames = 40
	// Start close enough to 2^32 that the timestamp wraps mid-stream.
	start := uint32(math.MaxUint32 - 10*codec.SamplesPerFrame)

	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: frames + 10})
	for i := 0; i < frames; i++ {
		pkt := packetAt(uint16(i), start+uint32(i)*codec.SamplesPerFrame, markerByte(i))
		if got := b.Push(pkt); got != PushAccepted {
			t.Fatalf("frame %d across the wrap: Push = %v, want accepted", i, got)
		}
	}

	got := popAll(b, frames)
	if len(got) != frames {
		t.Fatalf("popped %d frames, want %d", len(got), frames)
	}
	for i, f := range got {
		if f.Concealed {
			t.Errorf("slot %d concealed across a timestamp wrap", i)
		}
		if want := start + uint32(i)*codec.SamplesPerFrame; f.Timestamp != want {
			t.Errorf("slot %d timestamp = %d, want %d", i, f.Timestamp, want)
		}
		if f.PCM[0] != wantSample(i) {
			t.Errorf("slot %d audio = %d, want %d", i, f.PCM[0], wantSample(i))
		}
	}
	if st := b.Stats(); st.Concealed != 0 {
		t.Errorf("Concealed = %d across a timestamp wrap, want 0", st.Concealed)
	}
}

func TestOffGridAndBadSizeRejected(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: 20})
	b.Push(frameFor(0))

	// Half a frame off the grid: it could never line up with a playout slot, so
	// buffering it would waste depth on audio that can never be played.
	off := packetAt(100, baseTS+codec.SamplesPerFrame/2, 0x40)
	if got := b.Push(off); got != PushOffGrid {
		t.Errorf("off-grid Push = %v, want off-grid", got)
	}

	short := &pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, Timestamp: baseTS + codec.SamplesPerFrame},
		Payload: make([]byte, 80),
	}
	if got := b.Push(short); got != PushBadSize {
		t.Errorf("short-payload Push = %v, want bad-size", got)
	}

	empty := &pionrtp.Packet{
		Header:  pionrtp.Header{Version: 2, Timestamp: baseTS + codec.SamplesPerFrame},
		Payload: nil,
	}
	if got := b.Push(empty); got != PushBadSize {
		t.Errorf("empty-payload Push = %v, want bad-size", got)
	}

	st := b.Stats()
	if st.OffGrid != 1 {
		t.Errorf("OffGrid = %d, want 1", st.OffGrid)
	}
	if st.BadSize != 2 {
		t.Errorf("BadSize = %d, want 2", st.BadSize)
	}
	if st.Accepted != 1 {
		t.Errorf("Accepted = %d, want 1", st.Accepted)
	}
	if st.Pushed != 4 {
		t.Errorf("Pushed = %d, want 4", st.Pushed)
	}
}

// TestAdaptiveGrowsOnLateAndShrinksWhenClean exercises the adaptive controller.
// Growth is immediate because late packets are audio being discarded right now;
// shrinking is slow because reclaiming latency is never urgent and oscillating
// between depths would be worse than a little extra delay.
func TestAdaptiveGrowsOnLateAndShrinksWhenClean(t *testing.T) {
	const (
		initial   = 2
		maxTarget = 5
	)
	b := newTestBuffer(t, Config{
		TargetDepth:       initial,
		MaxDepth:          40,
		Adaptive:          true,
		AdaptiveMaxTarget: maxTarget,
	})

	for i := 0; i < 10; i++ {
		b.Push(frameFor(i))
	}
	popAll(b, 5) // playout now past frame 4

	if got := b.Stats().TargetDepth; got != initial {
		t.Fatalf("TargetDepth = %d before any late packet, want %d", got, initial)
	}

	// Each late packet deepens the buffer by one frame, up to the cap.
	for i := 0; i < 10; i++ {
		b.Push(frameFor(0)) // always late
	}
	st := b.Stats()
	if st.TargetDepth != maxTarget {
		t.Errorf("TargetDepth = %d after repeated late packets, want the cap %d",
			st.TargetDepth, maxTarget)
	}
	if st.Late != 10 {
		t.Errorf("Late = %d, want 10", st.Late)
	}

	// A long clean stretch gives depth back, one frame at a time.
	next := 10
	for round := 0; round < 2; round++ {
		for i := 0; i < adaptiveDecayPops; i++ {
			b.Push(frameFor(next))
			b.Pop()
			next++
		}
	}
	if got := b.Stats().TargetDepth; got >= maxTarget {
		t.Errorf("TargetDepth = %d after %d clean pops, want it to have shrunk below %d",
			got, 2*adaptiveDecayPops, maxTarget)
	}
	t.Logf("target depth: %d initial -> %d after late packets -> %d after recovery",
		initial, maxTarget, b.Stats().TargetDepth)
}

func TestAdaptiveDisabledByDefault(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 2, MaxDepth: 20})

	for i := 0; i < 5; i++ {
		b.Push(frameFor(i))
	}
	popAll(b, 4)
	for i := 0; i < 10; i++ {
		b.Push(frameFor(0))
	}

	if got := b.Stats().TargetDepth; got != 2 {
		t.Errorf("TargetDepth = %d with adaptation off, want it pinned at 2", got)
	}
}

func TestDrainEmptiesBufferAndFillsHoles(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: 20})
	for _, i := range []int{0, 1, 3, 4} {
		b.Push(frameFor(i))
	}

	got := b.Drain()
	if len(got) != 5 {
		t.Fatalf("Drain returned %d frames, want 5 (four pushed plus one hole)", len(got))
	}
	for i, f := range got {
		if idx := frameIndexOf(t, f); idx != i {
			t.Errorf("slot %d holds frame %d", i, idx)
		}
		if wantConcealed := i == 2; f.Concealed != wantConcealed {
			t.Errorf("slot %d Concealed = %v, want %v", i, f.Concealed, wantConcealed)
		}
	}
	if st := b.Stats(); st.CurrentDepth != 0 {
		t.Errorf("CurrentDepth = %d after Drain, want 0", st.CurrentDepth)
	}
	if got := b.Drain(); len(got) != 0 {
		t.Errorf("second Drain returned %d frames, want 0", len(got))
	}
}

// TestDrainPlaysCallShorterThanPrebuffer guards a real edge case: a call that
// ends before the buffer ever reached its target depth must still be played, not
// discarded.
func TestDrainPlaysCallShorterThanPrebuffer(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 10, MaxDepth: 20})
	for i := 0; i < 3; i++ {
		b.Push(frameFor(i))
	}

	if _, ok := b.Pop(); ok {
		t.Fatal("Pop succeeded below the target depth")
	}
	got := b.Drain()
	if len(got) != 3 {
		t.Fatalf("Drain returned %d frames, want 3", len(got))
	}
	for i, f := range got {
		if f.Concealed {
			t.Errorf("slot %d concealed", i)
		}
		if f.PCM[0] != wantSample(i) {
			t.Errorf("slot %d audio = %d, want %d", i, f.PCM[0], wantSample(i))
		}
	}
}

func TestEmptyBufferPopReturnsNotReady(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: 20})
	for i := 0; i < 5; i++ {
		if _, ok := b.Pop(); ok {
			t.Fatal("Pop succeeded on an empty buffer")
		}
	}
	st := b.Stats()
	if st.Popped != 0 {
		t.Errorf("Popped = %d, want 0", st.Popped)
	}
	if !st.Prebuffering {
		t.Error("Prebuffering = false on an empty buffer")
	}
}

func TestResetReturnsToInitialState(t *testing.T) {
	b := newTestBuffer(t, Config{
		TargetDepth: 2, MaxDepth: 20, Adaptive: true, AdaptiveMaxTarget: 6,
	})

	for i := 0; i < 10; i++ {
		b.Push(frameFor(i))
	}
	popAll(b, 5)
	b.Push(frameFor(0)) // late, grows the target

	b.Reset()

	st := b.Stats()
	if st != (Stats{TargetDepth: 2, Prebuffering: true}) {
		t.Errorf("Stats after Reset = %+v, want a zero value with target 2 and prebuffering", st)
	}

	// The buffer must be usable again, including a fresh timestamp origin.
	for i := 0; i < 3; i++ {
		if got := b.Push(frameFor(i)); got != PushAccepted {
			t.Fatalf("frame %d after Reset: Push = %v", i, got)
		}
	}
	f, ok := b.Pop()
	if !ok {
		t.Fatal("Pop failed after Reset")
	}
	if idx := frameIndexOf(t, f); idx != 0 {
		t.Errorf("playout restarted at frame %d, want 0", idx)
	}
}

func TestStatsDepthConversions(t *testing.T) {
	st := Stats{CurrentDepth: 3, TargetDepth: 5}
	if got := st.DepthMs(); math.Abs(got-60) > 1e-9 {
		t.Errorf("DepthMs() = %v, want 60", got)
	}
	if got := st.TargetDepthMs(); math.Abs(got-100) > 1e-9 {
		t.Errorf("TargetDepthMs() = %v, want 100", got)
	}
	if got := (Stats{}).ConcealRate(); got != 0 {
		t.Errorf("ConcealRate() on an empty Stats = %v, want 0", got)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"zero value takes defaults", Config{}, false},
		{"explicit valid", Config{Codec: codec.PCMA, TargetDepth: 2, MaxDepth: 10}, false},
		{"bad codec", Config{Codec: "OPUS", TargetDepth: 2, MaxDepth: 10}, true},
		{"zero target takes default", Config{Codec: codec.PCMU, MaxDepth: 10}, false},
		{"negative target", Config{Codec: codec.PCMU, TargetDepth: -1, MaxDepth: 10}, true},
		{"max below target", Config{Codec: codec.PCMU, TargetDepth: 10, MaxDepth: 5}, true},
		{"noise out of range", Config{Codec: codec.PCMU, TargetDepth: 2, MaxDepth: 10, NoiseLevel: 2}, true},
		{"adaptive cap above max", Config{
			Codec: codec.PCMU, TargetDepth: 2, MaxDepth: 10, AdaptiveMaxTarget: 20,
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultsAreTelephonySane(t *testing.T) {
	b := newTestBuffer(t, Config{})
	st := b.Stats()

	if st.TargetDepth != DefaultTargetDepth {
		t.Errorf("default TargetDepth = %d, want %d", st.TargetDepth, DefaultTargetDepth)
	}
	// 60 ms of prebuffer and a 500 ms ceiling.
	if got := st.TargetDepthMs(); math.Abs(got-60) > 1e-9 {
		t.Errorf("default target depth = %v ms, want 60", got)
	}
	if got := float64(DefaultMaxDepth) * 20; got != 500 {
		t.Errorf("default max depth = %v ms, want 500", got)
	}
}

func TestPushResultStrings(t *testing.T) {
	for r, want := range map[PushResult]string{
		PushAccepted:  "accepted",
		PushDuplicate: "duplicate",
		PushLate:      "late",
		PushOffGrid:   "off-grid",
		PushBadSize:   "bad-size",
	} {
		if got := r.String(); got != want {
			t.Errorf("PushResult(%d).String() = %q, want %q", int(r), got, want)
		}
	}
	if got := PushResult(99).String(); got == "" {
		t.Error("an unknown PushResult stringified to empty")
	}
}

// TestConcurrentPushPop exercises the mutex: the receive loop pushes while the
// playout loop pops. Meaningful under -race.
func TestConcurrentPushPop(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: 50})

	const frames = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < frames; i++ {
			b.Push(frameFor(i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < frames; i++ {
			b.Pop()
			b.Stats()
		}
	}()
	wg.Wait()

	// Ordering is the invariant that must hold regardless of interleaving:
	// playout timestamps advance by exactly one frame each time.
	st := b.Stats()
	if st.Popped == 0 {
		t.Error("nothing was popped")
	}
	if st.CurrentDepth > 50 {
		t.Errorf("CurrentDepth = %d, exceeds MaxDepth", st.CurrentDepth)
	}
}

// TestPlayoutTimestampsAreStrictlySequential pins the buffer's fundamental
// output contract: whatever arrives, the emitted slots form an unbroken ladder
// at one frame per step. Any deviation would desynchronize downstream audio.
func TestPlayoutTimestampsAreStrictlySequential(t *testing.T) {
	b := newTestBuffer(t, Config{TargetDepth: 3, MaxDepth: 20})

	// A deliberately nasty mix: reordering, gaps, duplicates and a late packet.
	for _, i := range []int{2, 0, 1, 5, 4, 4, 9, 7, 8} {
		b.Push(frameFor(i))
	}

	got := popAll(b, 12)
	if len(got) < 2 {
		t.Fatal("not enough frames popped to check sequencing")
	}
	for i := 1; i < len(got); i++ {
		delta := got[i].Timestamp - got[i-1].Timestamp
		if delta != codec.SamplesPerFrame {
			t.Errorf("slot %d advanced the timestamp by %d, want exactly %d",
				i, delta, codec.SamplesPerFrame)
		}
	}
	for i, f := range got {
		if len(f.PCM) != codec.SamplesPerFrame {
			t.Errorf("slot %d has %d samples, want %d", i, len(f.PCM), codec.SamplesPerFrame)
		}
	}
}
