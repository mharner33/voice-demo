package rtp

import (
	"math"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
)

// These tests cover the loss blind spot at the edges of a stream, and the
// control plane's fix for it.
//
// Loss is reconstructed from gaps between sequence numbers that arrived, so
// loss at either edge leaves no evidence. The trailing case is the familiar
// one. The leading case is easy to miss and just as wrong: a receiver whose
// first packet was dropped anchors its sequence base on the second one and
// silently starts counting a packet in. Both ends therefore have to be declared.

// TestTrailingLossIsInvisibleWithoutTheRange establishes the problem for the
// tail of a stream.
func TestTrailingLossIsInvisibleWithoutTheRange(t *testing.T) {
	const (
		packets  = 100
		tailDrop = 5
	)
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	// Everything arrives except the last five.
	for i := 0; i < packets-tailDrop; i++ {
		s.Observe(testPacket(uint16(1000+i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration))
	}

	st := s.Stats()
	if st.Lost != 0 {
		t.Errorf("Lost = %d before the range is known, want 0: trailing loss "+
			"cannot be seen from the stream alone", st.Lost)
	}
	if st.SequenceRangeKnown {
		t.Error("SequenceRangeKnown = true without the sender having declared it")
	}

	// The sender declares what it actually emitted.
	s.NoteSequenceRange(1000, uint16(1000+packets-1))

	st = s.Stats()
	if st.Lost != tailDrop {
		t.Errorf("Lost = %d after the range is known, want %d", st.Lost, tailDrop)
	}
	if st.Expected != packets {
		t.Errorf("Expected = %d, want %d", st.Expected, packets)
	}
	if !st.SequenceRangeKnown {
		t.Error("SequenceRangeKnown = false after NoteSequenceRange")
	}
}

// TestLeadingLossIsInvisibleWithoutTheRange is the case the design originally
// missed. It was found by running the real client against the real gateway and
// noticing the gateway under-counting by exactly one packet on some seeds.
func TestLeadingLossIsInvisibleWithoutTheRange(t *testing.T) {
	const (
		packets  = 100
		headDrop = 3
	)
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	// The first three never arrive, so the receiver anchors on packet 3.
	for i := headDrop; i < packets; i++ {
		s.Observe(testPacket(uint16(1000+i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration))
	}

	st := s.Stats()
	if st.Lost != 0 {
		t.Errorf("Lost = %d before the range is known, want 0", st.Lost)
	}
	if st.Expected != packets-headDrop {
		t.Errorf("Expected = %d, want %d: the receiver can only account for the "+
			"range it saw", st.Expected, packets-headDrop)
	}

	s.NoteSequenceRange(1000, uint16(1000+packets-1))

	st = s.Stats()
	if st.Lost != headDrop {
		t.Errorf("Lost = %d after the range is known, want %d: a dropped first "+
			"packet is as invisible as a dropped last one", st.Lost, headDrop)
	}
	if st.Expected != packets {
		t.Errorf("Expected = %d, want %d", st.Expected, packets)
	}
}

// TestLossAtBothEdges covers the combination, which is what a percentage loss
// profile produces in practice.
func TestLossAtBothEdges(t *testing.T) {
	const (
		packets  = 200
		headDrop = 4
		tailDrop = 7
		midDrop  = 10
	)
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	dropped := 0
	for i := 0; i < packets; i++ {
		switch {
		case i < headDrop, i >= packets-tailDrop:
			dropped++
			continue
		case i%20 == 0:
			dropped++
			continue
		}
		s.Observe(testPacket(uint16(500+i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration))
	}

	// Before the range is known, only the middle gaps are countable.
	before := s.Stats().Lost
	if before == 0 {
		t.Fatal("no mid-stream loss was detected at all")
	}
	if before >= uint64(dropped) {
		t.Errorf("Lost = %d before the range is known, want fewer than the %d "+
			"actually dropped", before, dropped)
	}

	s.NoteSequenceRange(500, uint16(500+packets-1))

	st := s.Stats()
	if st.Lost != uint64(dropped) {
		t.Errorf("Lost = %d, want %d", st.Lost, dropped)
	}
	if st.Expected != packets {
		t.Errorf("Expected = %d, want %d", st.Expected, packets)
	}
	t.Logf("%d dropped across both edges and the middle: %d countable from the "+
		"stream alone, %d once the range was declared", dropped, before, st.Lost)
	_ = midDrop
}

// TestOutOfOrderOpeningDoesNotUndercount is the latent bug found alongside the
// leading-loss gap. If the first packet to *arrive* is not the lowest sequence
// number, anchoring the range on it makes Expected too small — and could make
// it smaller than Received, which is nonsense.
func TestOutOfOrderOpeningDoesNotUndercount(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	// Packet 5 arrives first, then 0 through 4, then the rest in order.
	order := []int{5, 0, 1, 2, 3, 4, 6, 7, 8, 9}
	for n, i := range order {
		s.Observe(testPacket(uint16(2000+i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(n)*codec.FrameDuration))
	}

	st := s.Stats()
	if st.Received != uint64(len(order)) {
		t.Errorf("Received = %d, want %d", st.Received, len(order))
	}
	if st.Expected != uint64(len(order)) {
		t.Errorf("Expected = %d, want %d: the range must cover the earliest "+
			"sequence seen, not merely the first one to arrive", st.Expected, len(order))
	}
	if st.Expected < st.Received {
		t.Errorf("Expected (%d) is below Received (%d), which is impossible",
			st.Expected, st.Received)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0: every packet arrived", st.Lost)
	}
}

// TestNoteSequenceRangeIgnoresNarrowerRange guards against a sender whose
// report would shrink the observed range, which must never discard real data.
func TestNoteSequenceRangeIgnoresNarrowerRange(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)
	for i := 0; i < 50; i++ {
		s.Observe(testPacket(uint16(100+i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration))
	}
	before := s.Stats()

	// A range strictly inside what arrived.
	s.NoteSequenceRange(110, 130)

	after := s.Stats()
	if after.Expected != before.Expected {
		t.Errorf("Expected changed from %d to %d on a narrower declared range",
			before.Expected, after.Expected)
	}
	if after.Received != before.Received {
		t.Errorf("Received changed from %d to %d", before.Received, after.Received)
	}
	if after.Lost != 0 {
		t.Errorf("Lost = %d, want 0", after.Lost)
	}
}

// TestNoteSequenceRangeAcrossWrap covers a call long enough to cross the 16-bit
// sequence rollover, where naive arithmetic on either end would produce an
// absurd range.
func TestNoteSequenceRangeAcrossWrap(t *testing.T) {
	const packets = 100
	start := uint16(65520) // wraps after 16 packets

	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	// Drop the first two and the last three, straddling the wrap.
	dropped := 0
	for i := 0; i < packets; i++ {
		if i < 2 || i >= packets-3 {
			dropped++
			continue
		}
		s.Observe(testPacket(start+uint16(i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration))
	}

	s.NoteSequenceRange(start, start+uint16(packets-1))

	st := s.Stats()
	if st.Expected != packets {
		t.Errorf("Expected = %d across a sequence wrap, want %d", st.Expected, packets)
	}
	if st.Lost != uint64(dropped) {
		t.Errorf("Lost = %d, want %d", st.Lost, dropped)
	}
}

// TestNoteSequenceRangeOnEmptySessionIsSafe covers a call where nothing arrived
// at all. There is no anchor, so the receiver legitimately reports nothing; the
// sender's own count is the only account of such a call.
func TestNoteSequenceRangeOnEmptySessionIsSafe(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)
	s.NoteSequenceRange(0, math.MaxUint16)

	st := s.Stats()
	if st.Received != 0 || st.Expected != 0 || st.Lost != 0 {
		t.Errorf("stats on a session that saw nothing: %+v", st)
	}
	if st.SequenceRangeKnown {
		t.Error("SequenceRangeKnown = true on a session with no anchor to apply it to")
	}
}

// TestSequenceRangeMakesLossExactNotApproximate pins the reported guarantee: it
// is the flag that tells a dashboard whether the loss figure can be trusted as
// a measurement or only as a lower bound.
func TestSequenceRangeMakesLossExactNotApproximate(t *testing.T) {
	s := NewSession(1, codec.SampleRate8k)
	for i := 0; i < 10; i++ {
		s.Observe(testPacket(uint16(i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration))
	}

	if s.Stats().SequenceRangeKnown {
		t.Error("the range is reported known before the sender declared it")
	}
	s.NoteSequenceRange(0, 9)
	if !s.Stats().SequenceRangeKnown {
		t.Error("the range is not reported known after the sender declared it")
	}
}
