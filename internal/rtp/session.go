package rtp

import (
	"sync"
	"time"

	pionrtp "github.com/pion/rtp"
)

// seenWindow is how many recent packets the duplicate detector remembers. At a
// 20 ms packetization interval, 8192 packets is about 164 seconds of history —
// far beyond any reordering a real network produces.
const seenWindow = 8192

// jitterGain is the RFC 3550 noise-reduction divisor for the jitter estimator's
// exponential moving average.
const jitterGain = 16.0

// Stats is an independent measurement of what the network did to a stream,
// derived only from what arrived.
type Stats struct {
	// SSRC identifies the stream.
	SSRC uint32
	// Received is the count of unique packets that arrived.
	Received uint64
	// Expected is how many packets should have arrived, from the sequence range.
	Expected uint64
	// Lost is Expected minus Received. It can only be known once the stream's
	// highest sequence number is known, so it trails by any in-flight packets.
	Lost uint64
	// Duplicated counts packets that arrived more than once.
	Duplicated uint64
	// Reordered counts packets that arrived after a higher sequence number had
	// already been seen.
	Reordered uint64
	// LossPct is Lost as a percentage of Expected.
	LossPct float64
	// JitterMs is the current RFC 3550 interarrival jitter estimate.
	JitterMs float64
	// MaxJitterMs is the high-water mark of the estimate.
	MaxJitterMs float64
	// Duration spans the first to the last packet arrival.
	Duration time.Duration
	// PayloadBytes totals the audio payload received.
	PayloadBytes uint64
}

// Session tracks one RTP stream, identified by SSRC. Safe for concurrent use:
// the receive loop writes while the metrics reporter reads.
type Session struct {
	mu        sync.Mutex
	ssrc      uint32
	clockRate float64

	started bool

	// Sequence accounting. The extended sequence space starts at 2^31 so a
	// stream beginning near a wrap boundary cannot underflow uint32.
	baseExt    uint32
	maxExt     uint32
	maxExtSeq  uint16 // the 16-bit sequence number corresponding to maxExt
	received   uint64
	duplicates uint64
	reordered  uint64
	payload    uint64

	// Jitter state. Units are RTP timestamp ticks.
	jitter     float64
	maxJitter  float64
	lastTS     uint32
	lastTSTime time.Time
	haveLastTS bool

	firstArrival time.Time
	lastArrival  time.Time

	seen seenSet
}

// NewSession creates a stats tracker for one stream. clockRate is the codec's
// sample rate — 8000 for G.711 — and converts arrival times into the same units
// as RTP timestamps.
func NewSession(ssrc uint32, clockRate int) *Session {
	return &Session{ssrc: ssrc, clockRate: float64(clockRate)}
}

// Observe records one received packet with its arrival time. Passing the true
// arrival time is what makes the jitter estimate meaningful, so the receive loop
// stamps the clock before doing any processing.
func (s *Session) Observe(pkt *pionrtp.Packet, arrival time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seq := pkt.SequenceNumber

	if !s.started {
		s.started = true
		s.baseExt = 1 << 31
		s.maxExt = s.baseExt
		s.maxExtSeq = seq
		s.firstArrival = arrival
		s.lastArrival = arrival
		s.received = 1
		s.payload += uint64(len(pkt.Payload))
		s.seen.add(s.baseExt)
		s.noteTimestampLocked(pkt.Timestamp, arrival)
		return
	}

	// A signed 16-bit difference gives the shortest distance to the current high
	// water mark, which handles sequence wrap in both directions.
	delta := int32(int16(seq - s.maxExtSeq))
	ext := uint32(int64(s.maxExt) + int64(delta))

	if s.seen.duplicate(ext) {
		s.duplicates++
		// A duplicate carries no new audio and must not count toward Received,
		// or loss would be understated.
		s.lastArrival = arrival
		return
	}

	if delta > 0 {
		s.maxExt = ext
		s.maxExtSeq = seq
	} else {
		s.reordered++
	}

	s.seen.add(ext)
	s.received++
	s.payload += uint64(len(pkt.Payload))
	s.lastArrival = arrival
	s.noteTimestampLocked(pkt.Timestamp, arrival)
}

// noteTimestampLocked applies the RFC 3550 interarrival jitter estimator:
//
//	D(i-1,i) = (R(i) - R(i-1)) - (S(i) - S(i-1))
//	J += (|D(i-1,i)| - J) / 16
//
// R is the arrival time in timestamp ticks and S is the packet's RTP timestamp.
// Because it works on differences, a stream delayed by a large but *constant*
// latency measures zero jitter — which is correct, and the reason jitter and
// latency are reported separately.
func (s *Session) noteTimestampLocked(rtpTS uint32, arrival time.Time) {
	if !s.haveLastTS {
		s.haveLastTS = true
		s.lastTS = rtpTS
		s.lastTSTime = arrival
		return
	}

	// int32 conversion makes the timestamp delta wrap-safe across 2^32.
	tsDelta := float64(int32(rtpTS - s.lastTS))
	arrivalDelta := arrival.Sub(s.lastTSTime).Seconds() * s.clockRate

	d := arrivalDelta - tsDelta
	if d < 0 {
		d = -d
	}
	s.jitter += (d - s.jitter) / jitterGain
	if s.jitter > s.maxJitter {
		s.maxJitter = s.jitter
	}

	s.lastTS = rtpTS
	s.lastTSTime = arrival
}

// Stats snapshots the current measurements.
func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := Stats{
		SSRC:         s.ssrc,
		Received:     s.received,
		Duplicated:   s.duplicates,
		Reordered:    s.reordered,
		JitterMs:     s.jitter / s.clockRate * 1000,
		MaxJitterMs:  s.maxJitter / s.clockRate * 1000,
		PayloadBytes: s.payload,
	}
	if !s.started {
		return st
	}
	st.Expected = uint64(s.maxExt-s.baseExt) + 1
	if st.Expected > st.Received {
		st.Lost = st.Expected - st.Received
	}
	if st.Expected > 0 {
		st.LossPct = float64(st.Lost) / float64(st.Expected) * 100
	}
	st.Duration = s.lastArrival.Sub(s.firstArrival)
	return st
}

// SSRC returns the stream identifier.
func (s *Session) SSRC() uint32 { return s.ssrc }

// seenSet is a sliding bitmap of extended sequence numbers, giving exact
// duplicate detection in bounded memory.
type seenSet struct {
	bits   [seenWindow / 64]uint64
	high   uint32
	inited bool
}

func (s *seenSet) add(ext uint32) {
	if !s.inited {
		s.inited = true
		s.high = ext
		s.set(ext)
		return
	}
	if ext > s.high {
		// Clear the bits being slid across, so stale entries from a previous lap
		// around the ring are not mistaken for duplicates.
		if ext-s.high >= seenWindow {
			s.bits = [seenWindow / 64]uint64{}
		} else {
			for e := s.high + 1; e != ext+1; e++ {
				s.clear(e)
			}
		}
		s.high = ext
	}
	s.set(ext)
}

// duplicate reports whether ext has already been recorded. Packets older than
// the window report false: we genuinely cannot tell, and guessing "duplicate"
// would understate loss, which is the worse failure for this demo.
func (s *seenSet) duplicate(ext uint32) bool {
	switch {
	case !s.inited:
		return false
	case ext > s.high:
		return false
	case s.high-ext >= seenWindow:
		return false
	default:
		return s.get(ext)
	}
}

func (s *seenSet) idx(ext uint32) (word uint32, bit uint64) {
	i := ext % seenWindow
	return i / 64, 1 << (i % 64)
}

func (s *seenSet) set(ext uint32)   { w, b := s.idx(ext); s.bits[w] |= b }
func (s *seenSet) clear(ext uint32) { w, b := s.idx(ext); s.bits[w] &^= b }

func (s *seenSet) get(ext uint32) bool {
	w, b := s.idx(ext)
	return s.bits[w]&b != 0
}
