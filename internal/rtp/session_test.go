package rtp

import (
	"math"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
)

// epoch is an arbitrary fixed base time. Using a fixed clock rather than
// time.Now() keeps every assertion in this file exact and flake-free.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// streamSpec describes a synthetic RTP stream for the stats tests.
type streamSpec struct {
	packets   int
	startSeq  uint16
	startTS   uint32
	frameTick time.Duration // nominal spacing, matching the RTP timestamp rate

	// dropFn reports whether packet i never arrives.
	dropFn func(i int) bool
	// arrivalOffset perturbs packet i's arrival time away from nominal.
	arrivalOffset func(i int) time.Duration
}

// feed synthesizes the stream and feeds it to a session in arrival order,
// returning how many packets were actually delivered.
func feed(s *Session, spec streamSpec) (delivered int) {
	type arrival struct {
		pkt *pionrtp.Packet
		at  time.Time
	}
	var arrivals []arrival

	for i := 0; i < spec.packets; i++ {
		if spec.dropFn != nil && spec.dropFn(i) {
			continue
		}
		at := epoch.Add(time.Duration(i) * spec.frameTick)
		if spec.arrivalOffset != nil {
			at = at.Add(spec.arrivalOffset(i))
		}
		arrivals = append(arrivals, arrival{
			pkt: &pionrtp.Packet{
				Header: pionrtp.Header{
					Version:        2,
					PayloadType:    uint8(codec.PayloadTypePCMU),
					SequenceNumber: spec.startSeq + uint16(i),
					Timestamp:      spec.startTS + uint32(i)*codec.SamplesPerFrame,
					SSRC:           0xDEADBEEF,
				},
				Payload: make([]byte, codec.G711FrameBytes),
			},
			at: at,
		})
	}

	// A receiver sees packets in arrival-time order, not sequence order. Sorting
	// here is what turns an arrival-time perturbation into real reordering.
	for i := 1; i < len(arrivals); i++ {
		for j := i; j > 0 && arrivals[j].at.Before(arrivals[j-1].at); j-- {
			arrivals[j], arrivals[j-1] = arrivals[j-1], arrivals[j]
		}
	}
	for _, a := range arrivals {
		s.Observe(a.pkt, a.at)
	}
	return len(arrivals)
}

// TestPerfectStreamMeasuresZeroLossAndJitter is the control case. If this does
// not read exactly zero, every impaired measurement is suspect.
func TestPerfectStreamMeasuresZeroLossAndJitter(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)
	feed(s, streamSpec{packets: 500, startSeq: 1000, startTS: 160000, frameTick: codec.FrameDuration})

	st := s.Stats()
	if st.Received != 500 {
		t.Errorf("Received = %d, want 500", st.Received)
	}
	if st.Expected != 500 {
		t.Errorf("Expected = %d, want 500", st.Expected)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0", st.Lost)
	}
	if st.LossPct != 0 {
		t.Errorf("LossPct = %v, want 0", st.LossPct)
	}
	if st.Reordered != 0 || st.Duplicated != 0 {
		t.Errorf("Reordered = %d, Duplicated = %d, want 0, 0", st.Reordered, st.Duplicated)
	}
	if st.JitterMs > 0.001 {
		t.Errorf("JitterMs = %v, want ~0", st.JitterMs)
	}
	if want := 499 * codec.FrameDuration; st.Duration != want {
		t.Errorf("Duration = %v, want %v", st.Duration, want)
	}
	if want := uint64(500 * codec.G711FrameBytes); st.PayloadBytes != want {
		t.Errorf("PayloadBytes = %d, want %d", st.PayloadBytes, want)
	}
}

// TestScriptedLossIsMeasuredExactly is the phase-2 acceptance test: 500 frames
// with a scripted 5% loss must be reported as exactly 5%, reconstructed from
// sequence gaps alone.
func TestScriptedLossIsMeasuredExactly(t *testing.T) {
	const (
		packets  = 500
		dropEach = 20 // one in twenty => exactly 5%
	)
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	// Drop at offset 10 within each group of 20. That puts all 25 drops strictly
	// between the first and last packet, which matters: the sequence range is
	// only observable between the lowest and highest sequence number actually
	// seen, so loss at the very edges of a stream is genuinely undetectable
	// without RTCP. Dropping every 20th packet starting at 0 would lose one drop
	// to that blind spot and measure 4.8%.
	dropped := func(i int) bool { return i%dropEach == dropEach/2 }

	delivered := feed(s, streamSpec{
		packets:   packets,
		startSeq:  1000,
		startTS:   160000,
		frameTick: codec.FrameDuration,
		dropFn:    dropped,
	})

	wantLost := 0
	for i := 0; i < packets; i++ {
		if dropped(i) {
			wantLost++
		}
	}
	if wantLost != packets/dropEach {
		t.Fatalf("test setup: %d drops, want %d", wantLost, packets/dropEach)
	}

	st := s.Stats()
	if st.Received != uint64(delivered) {
		t.Errorf("Received = %d, want %d", st.Received, delivered)
	}
	if st.Expected != packets {
		t.Errorf("Expected = %d, want %d", st.Expected, packets)
	}
	if st.Lost != uint64(wantLost) {
		t.Errorf("Lost = %d, want %d", st.Lost, wantLost)
	}

	wantPct := float64(wantLost) / packets * 100
	if math.Abs(st.LossPct-wantPct) > 1e-9 {
		t.Errorf("LossPct = %v, want %v", st.LossPct, wantPct)
	}
	t.Logf("scripted %d/%d lost -> measured %.2f%%", wantLost, packets, st.LossPct)
}

// TestConstantLatencyMeasuresZeroJitter pins the most commonly misunderstood
// property of RFC 3550 jitter: it measures variation in delay, not delay. A call
// over a satellite link with rock-steady 300 ms latency has zero jitter.
func TestConstantLatencyMeasuresZeroJitter(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)
	feed(s, streamSpec{
		packets:       300,
		startSeq:      1,
		startTS:       0,
		frameTick:     codec.FrameDuration,
		arrivalOffset: func(int) time.Duration { return 300 * time.Millisecond },
	})

	if st := s.Stats(); st.JitterMs > 0.001 {
		t.Errorf("JitterMs = %v under constant 300ms latency, want ~0", st.JitterMs)
	}
}

// TestFixedJitterConvergesToKnownValue verifies the estimator against a case
// with a closed-form answer. With arrivals alternating between nominal and
// nominal+j, every interarrival delta differs from its timestamp delta by
// exactly j, so the EWMA J += (|D| - J)/16 converges to j.
//
// This only holds while j stays below the packetization interval. Once the
// offset exceeds 20 ms the packets physically change places on the wire and the
// alternating pattern no longer describes what the receiver sees — that regime
// is covered by TestLargeJitterCausesReordering.
func TestFixedJitterConvergesToKnownValue(t *testing.T) {
	for _, jitterMs := range []float64{1, 5, 15} {
		j := time.Duration(jitterMs * float64(time.Millisecond))

		s := NewSession(0xDEADBEEF, codec.SampleRate8k)
		feed(s, streamSpec{
			packets:   500, // ~70 packets suffice for 99% convergence
			startSeq:  1,
			startTS:   0,
			frameTick: codec.FrameDuration,
			arrivalOffset: func(i int) time.Duration {
				if i%2 == 1 {
					return j
				}
				return 0
			},
		})

		st := s.Stats()
		if math.Abs(st.JitterMs-jitterMs) > 0.1 {
			t.Errorf("alternating %.0fms offset: JitterMs = %.3f, want %.1f",
				jitterMs, st.JitterMs, jitterMs)
		}
		if st.Lost != 0 {
			t.Errorf("alternating %.0fms offset: Lost = %d, want 0", jitterMs, st.Lost)
		}
		if st.Reordered != 0 {
			t.Errorf("alternating %.0fms offset: Reordered = %d, want 0 — jitter "+
				"below the frame interval must not reorder", jitterMs, st.Reordered)
		}
		t.Logf("alternating %.0fms offset -> jitter %.3f ms (max %.3f)",
			jitterMs, st.JitterMs, st.MaxJitterMs)
	}
}

// TestLargeJitterCausesReordering covers the regime above the packetization
// interval. Jitter of 80 ms at a 20 ms frame interval genuinely swaps packets on
// the wire, so the receiver must report reordering — and must still report zero
// loss, because every packet did arrive.
func TestLargeJitterCausesReordering(t *testing.T) {
	const jitterMs = 80
	j := time.Duration(jitterMs * float64(time.Millisecond))

	s := NewSession(0xDEADBEEF, codec.SampleRate8k)
	feed(s, streamSpec{
		packets:   500,
		startSeq:  1,
		startTS:   0,
		frameTick: codec.FrameDuration,
		arrivalOffset: func(i int) time.Duration {
			if i%2 == 1 {
				return j
			}
			return 0
		},
	})

	st := s.Stats()
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0 — reordered packets still arrived", st.Lost)
	}
	if st.Reordered == 0 {
		t.Error("Reordered = 0, want > 0: 80ms of jitter at a 20ms frame " +
			"interval must swap packets")
	}
	// No closed form here, but the estimate must still land in the right order
	// of magnitude rather than collapsing to zero or exploding.
	if st.JitterMs < jitterMs/2 || st.JitterMs > jitterMs*2 {
		t.Errorf("JitterMs = %.1f, want roughly %d", st.JitterMs, jitterMs)
	}
	t.Logf("80ms offset -> jitter %.1f ms, %d reordered, %d lost",
		st.JitterMs, st.Reordered, st.Lost)
}

func TestDuplicatesDoNotUnderstateLoss(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	const packets = 100
	for i := 0; i < packets; i++ {
		pkt := testPacket(uint16(100+i), uint32(i)*codec.SamplesPerFrame)
		at := epoch.Add(time.Duration(i) * codec.FrameDuration)
		s.Observe(pkt, at)
		if i%10 == 0 {
			// Same packet arrives a second time, as a retransmitting middlebox
			// or a looped network path would produce.
			s.Observe(pkt, at.Add(time.Millisecond))
		}
	}

	st := s.Stats()
	if st.Duplicated != 10 {
		t.Errorf("Duplicated = %d, want 10", st.Duplicated)
	}
	if st.Received != packets {
		t.Errorf("Received = %d, want %d — duplicates must not count as new audio",
			st.Received, packets)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0", st.Lost)
	}
	if st.Reordered != 0 {
		t.Errorf("Reordered = %d, want 0 — a duplicate is not a reorder", st.Reordered)
	}
}

func TestReorderingIsNotCountedAsLoss(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	// Deliver in order 0,2,1,3,5,4,6... — adjacent swaps, nothing lost.
	order := []int{0, 2, 1, 3, 5, 4, 6, 8, 7, 9}
	for n, i := range order {
		s.Observe(
			testPacket(uint16(500+i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(n)*codec.FrameDuration),
		)
	}

	st := s.Stats()
	if st.Received != 10 {
		t.Errorf("Received = %d, want 10", st.Received)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0 — reordered packets did arrive", st.Lost)
	}
	if st.Reordered != 3 {
		t.Errorf("Reordered = %d, want 3", st.Reordered)
	}
}

// TestSequenceWrap covers the 16-bit sequence rollover. A 20-minute call wraps
// the sequence space, so getting this wrong would show a loss spike every
// 65536 packets.
func TestSequenceWrap(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	const packets = 100
	start := uint16(65500) // wraps after 36 packets
	for i := 0; i < packets; i++ {
		s.Observe(
			testPacket(start+uint16(i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration),
		)
	}

	st := s.Stats()
	if st.Expected != packets {
		t.Errorf("Expected = %d across a sequence wrap, want %d", st.Expected, packets)
	}
	if st.Received != packets {
		t.Errorf("Received = %d, want %d", st.Received, packets)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d across a sequence wrap, want 0", st.Lost)
	}
}

// TestSequenceWrapWithLoss combines both edge cases, since a loss that straddles
// the wrap point is where naive extended-sequence arithmetic breaks.
func TestSequenceWrapWithLoss(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	const packets = 100
	start := uint16(65520) // wraps after 16 packets
	dropped := 0
	for i := 0; i < packets; i++ {
		// Drop three packets straddling the wrap.
		if i == 15 || i == 16 || i == 17 {
			dropped++
			continue
		}
		s.Observe(
			testPacket(start+uint16(i), uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration),
		)
	}

	st := s.Stats()
	if st.Expected != packets {
		t.Errorf("Expected = %d, want %d", st.Expected, packets)
	}
	if st.Lost != uint64(dropped) {
		t.Errorf("Lost = %d, want %d", st.Lost, dropped)
	}
}

// TestTimestampWrap covers the 32-bit RTP timestamp rollover. Without wrap-safe
// arithmetic the jitter estimate would spike to an absurd value at the boundary.
func TestTimestampWrap(t *testing.T) {
	s := NewSession(0xDEADBEEF, codec.SampleRate8k)

	// Start close enough to 2^32 that the timestamp wraps mid-stream.
	startTS := uint32(math.MaxUint32 - 10*codec.SamplesPerFrame)
	for i := 0; i < 100; i++ {
		s.Observe(
			testPacket(uint16(1+i), startTS+uint32(i)*codec.SamplesPerFrame),
			epoch.Add(time.Duration(i)*codec.FrameDuration),
		)
	}

	st := s.Stats()
	if st.JitterMs > 0.001 {
		t.Errorf("JitterMs = %v across a timestamp wrap, want ~0", st.JitterMs)
	}
	if st.Lost != 0 {
		t.Errorf("Lost = %d, want 0", st.Lost)
	}
}

func TestEmptySessionReportsZeroes(t *testing.T) {
	s := NewSession(0x1234, codec.SampleRate8k)

	st := s.Stats()
	if st.SSRC != 0x1234 {
		t.Errorf("SSRC = %#x, want 0x1234", st.SSRC)
	}
	if st.Received != 0 || st.Expected != 0 || st.Lost != 0 || st.LossPct != 0 {
		t.Errorf("unstarted session reported activity: %+v", st)
	}
	if s.SSRC() != 0x1234 {
		t.Errorf("SSRC() = %#x, want 0x1234", s.SSRC())
	}
}

func TestSinglePacketSession(t *testing.T) {
	s := NewSession(1, codec.SampleRate8k)
	s.Observe(testPacket(42, 0), epoch)

	st := s.Stats()
	if st.Received != 1 || st.Expected != 1 || st.Lost != 0 {
		t.Errorf("single packet: Received=%d Expected=%d Lost=%d, want 1 1 0",
			st.Received, st.Expected, st.Lost)
	}
	if st.JitterMs != 0 {
		t.Errorf("JitterMs = %v with one packet, want 0", st.JitterMs)
	}
}

// --- seenSet unit tests ---

func TestSeenSetDetectsDuplicatesInWindow(t *testing.T) {
	var s seenSet
	s.add(1000)

	if !s.duplicate(1000) {
		t.Error("duplicate(1000) = false immediately after add")
	}
	if s.duplicate(1001) {
		t.Error("duplicate(1001) = true for a future sequence number")
	}
	if s.duplicate(999) {
		t.Error("duplicate(999) = true for a never-seen earlier number")
	}
}

// TestSeenSetClearsSlidWindow guards the ring-buffer aliasing bug: after sliding
// a full lap, a stale bit must not read as a duplicate.
func TestSeenSetClearsSlidWindow(t *testing.T) {
	var s seenSet
	s.add(100)

	// Slide exactly one full lap so 100 and 100+seenWindow share a bit index.
	s.add(100 + seenWindow)
	if s.duplicate(100+seenWindow) != true {
		t.Error("the just-added entry is not reported as seen")
	}

	// Walk forward far enough that the old entry falls out of the window, then
	// confirm its slot has been cleared rather than aliasing.
	var s2 seenSet
	s2.add(0)
	for e := uint32(1); e <= seenWindow; e++ {
		if e != seenWindow && s2.duplicate(e) {
			t.Fatalf("duplicate(%d) = true before it was ever added", e)
		}
		s2.add(e)
	}
}

func TestSeenSetOldPacketsReportNotDuplicate(t *testing.T) {
	var s seenSet
	s.add(seenWindow * 3)

	// Far older than the window: we cannot know, and must not guess "duplicate"
	// because that would understate loss.
	if s.duplicate(1) {
		t.Error("duplicate() = true for a packet older than the window")
	}
}

func testPacket(seq uint16, ts uint32) *pionrtp.Packet {
	return &pionrtp.Packet{
		Header: pionrtp.Header{
			Version:        2,
			PayloadType:    uint8(codec.PayloadTypePCMU),
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xDEADBEEF,
		},
		Payload: make([]byte, codec.G711FrameBytes),
	}
}
