// Package jbuf implements the adaptive jitter buffer that sits between the
// irregular arrival of RTP packets and the steady cadence that a speech decoder
// requires.
//
// The buffer is pull-driven: the consumer calls Pop once per packetization
// interval, exactly as an audio device's callback would. That makes the pull
// cadence itself the clock, so every decision the buffer makes — is this packet
// too late, is this slot a hole, is the buffer over its latency budget — is a
// function of logical playout position rather than wall-clock time. The
// behavior is therefore fully deterministic and testable without a fake clock.
//
// The central trade-off is latency against loss. A deeper buffer absorbs more
// jitter but adds mouth-to-ear delay; a shallower one is more responsive but
// discards late packets that a deeper buffer would have played. Both failure
// modes are counted separately so a dashboard can show which one is hurting.
package jbuf

import (
	"fmt"
	"sync"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
)

// Defaults chosen for a 20 ms G.711 stream. 60 ms of prebuffer absorbs ordinary
// jitter without a conversationally noticeable delay; 500 ms is the hard
// latency ceiling, past which a call feels broken regardless of audio quality.
const (
	DefaultTargetDepth = 3  // 60 ms
	DefaultMaxDepth    = 25 // 500 ms
	DefaultNoiseLevel  = 0.002
)

// tsBase offsets the extended timestamp space so that packets arriving before
// the first one seen can still be represented without underflowing.
const tsBase = 1 << 40

// Config parameterizes a Buffer.
type Config struct {
	// Codec is the G.711 variant carried in the payloads.
	Codec codec.Codec

	// TargetDepth is how many frames accumulate before playout starts, and the
	// depth the adaptive controller aims to hold.
	TargetDepth int

	// MaxDepth is the hard cap. Exceeding it means the producer is outrunning
	// the consumer, so the oldest frame is evicted and playout jumps forward to
	// keep latency bounded.
	MaxDepth int

	// Conceal selects what plays in a hole.
	Conceal ConcealMode

	// NoiseLevel is the amplitude for ConcealNoise, as a fraction of full scale.
	NoiseLevel float64

	// Adaptive lets the buffer grow its target depth in response to late
	// packets and shrink again when the network settles.
	Adaptive bool

	// AdaptiveMaxTarget caps adaptive growth. Defaults to MaxDepth/2 so that
	// adaptation cannot drive the buffer into permanent overflow.
	AdaptiveMaxTarget int

	// Seed makes ConcealNoise reproducible.
	Seed int64
}

func (c *Config) applyDefaults() {
	if c.Codec == "" {
		c.Codec = codec.PCMU
	}
	if c.TargetDepth == 0 {
		c.TargetDepth = DefaultTargetDepth
	}
	if c.MaxDepth == 0 {
		c.MaxDepth = DefaultMaxDepth
	}
	if c.NoiseLevel == 0 {
		c.NoiseLevel = DefaultNoiseLevel
	}
	if c.AdaptiveMaxTarget == 0 {
		c.AdaptiveMaxTarget = c.MaxDepth / 2
		if c.AdaptiveMaxTarget < c.TargetDepth {
			c.AdaptiveMaxTarget = c.TargetDepth
		}
	}
}

// Validate rejects configurations that cannot work, so a bad flag fails at
// startup rather than producing quietly wrong audio.
func (c Config) Validate() error {
	if c.Codec != codec.PCMU && c.Codec != codec.PCMA {
		return fmt.Errorf("jbuf: codec must be PCMU or PCMA, got %q", c.Codec)
	}
	if c.TargetDepth < 1 {
		return fmt.Errorf("jbuf: TargetDepth = %d, want >= 1", c.TargetDepth)
	}
	if c.MaxDepth < c.TargetDepth {
		return fmt.Errorf("jbuf: MaxDepth = %d < TargetDepth = %d", c.MaxDepth, c.TargetDepth)
	}
	if c.NoiseLevel < 0 || c.NoiseLevel > 1 {
		return fmt.Errorf("jbuf: NoiseLevel = %v, want 0-1", c.NoiseLevel)
	}
	if c.AdaptiveMaxTarget > c.MaxDepth {
		return fmt.Errorf("jbuf: AdaptiveMaxTarget = %d > MaxDepth = %d",
			c.AdaptiveMaxTarget, c.MaxDepth)
	}
	return nil
}

// PushResult reports what the buffer did with a packet.
type PushResult int

const (
	// PushAccepted means the frame was buffered for playout.
	PushAccepted PushResult = iota
	// PushDuplicate means a frame already occupied that timestamp.
	PushDuplicate
	// PushLate means playout has already passed that timestamp. The packet
	// arrived, but too late to be of any use — the distinguishing failure of a
	// buffer that is too shallow for the network it is on.
	PushLate
	// PushOffGrid means the timestamp does not fall on the frame grid, so the
	// packet could never be played. Holding it would waste depth.
	PushOffGrid
	// PushBadSize means the payload is not one full G.711 frame.
	PushBadSize
)

func (r PushResult) String() string {
	switch r {
	case PushAccepted:
		return "accepted"
	case PushDuplicate:
		return "duplicate"
	case PushLate:
		return "late"
	case PushOffGrid:
		return "off-grid"
	case PushBadSize:
		return "bad-size"
	default:
		return fmt.Sprintf("PushResult(%d)", int(r))
	}
}

// Frame is one packetization interval of playable audio.
type Frame struct {
	// PCM is always exactly codec.SamplesPerFrame samples.
	PCM []int16
	// Timestamp is the RTP timestamp of the slot this frame fills.
	Timestamp uint32
	// Concealed is true when the packet was missing and the audio synthesized.
	Concealed bool
}

// Stats describes buffer behavior. Late and Concealed are the two numbers worth
// watching: they are the opposite sides of the latency/loss trade-off, and
// moving the target depth trades one for the other.
type Stats struct {
	Pushed    uint64
	Accepted  uint64
	Late      uint64
	Duplicate uint64
	OffGrid   uint64
	BadSize   uint64

	// Evicted counts frames discarded because the buffer hit MaxDepth.
	Evicted uint64

	// Popped counts playout slots emitted, concealed or not.
	Popped uint64
	// Concealed counts slots filled with synthesized audio.
	Concealed uint64
	// Starved counts concealed slots where the buffer held nothing at all, as
	// opposed to a hole with later frames already waiting behind it.
	Starved uint64

	// CurrentDepth and MaxObservedDepth are in frames.
	CurrentDepth     int
	MaxObservedDepth int
	// TargetDepth is the current target, which adaptation may have moved.
	TargetDepth int
	// Prebuffering is true while the buffer is still filling to target.
	Prebuffering bool
}

// DepthMs converts the current depth to milliseconds of buffered audio.
func (s Stats) DepthMs() float64 {
	return float64(s.CurrentDepth) * float64(codec.FrameDuration) / float64(1e6)
}

// TargetDepthMs converts the current target depth to milliseconds.
func (s Stats) TargetDepthMs() float64 {
	return float64(s.TargetDepth) * float64(codec.FrameDuration) / float64(1e6)
}

// ConcealRate is the fraction of emitted slots that were synthesized. This is
// the number that tracks perceived audio quality, since it counts every slot the
// listener heard filler in — whether the packet was lost in the network or
// merely arrived too late.
func (s Stats) ConcealRate() float64 {
	if s.Popped == 0 {
		return 0
	}
	return float64(s.Concealed) / float64(s.Popped)
}

// adaptiveDecayPops is how many clean playout slots must pass before the
// adaptive controller gives a frame of depth back. Growth is immediate because
// late packets are actively being lost; shrinking is slow because reclaiming
// latency is never urgent and oscillation is worse than a little extra delay.
const adaptiveDecayPops = 100

// Buffer reorders RTP frames into a steady playout sequence. Safe for
// concurrent use: the receive loop pushes while the playout loop pops.
type Buffer struct {
	mu  sync.Mutex
	cfg Config

	// frames holds buffered payloads keyed by extended timestamp.
	frames map[uint64][]byte

	// Extended timestamp tracking, to survive the 32-bit wrap.
	tsInit bool
	refLow uint32
	refExt uint64
	// refOrigin is the extended timestamp of the first packet seen. It anchors
	// the frame grid for the whole call and must never move: the grid is defined
	// relative to a fixed point, and re-anchoring it on the advancing playout
	// position would make the modulo test wrap for any packet behind playout.
	refOrigin uint64
	// originLow is that first packet's 32-bit wire timestamp, needed to map
	// extended timestamps back to the values a receiver would see.
	originLow uint32
	playout   uint64 // extended timestamp of the next slot to emit

	playing bool

	conceal *concealer

	baseTarget  int
	target      int
	cleanPops   int
	maxObserved int

	st Stats
}

// New creates a buffer. The zero Config is valid and yields the defaults.
func New(cfg Config) (*Buffer, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Buffer{
		cfg:        cfg,
		frames:     make(map[uint64][]byte, cfg.MaxDepth*2),
		conceal:    newConcealer(cfg.Conceal, cfg.NoiseLevel, cfg.Seed),
		baseTarget: cfg.TargetDepth,
		target:     cfg.TargetDepth,
	}, nil
}

// Push offers a packet to the buffer.
func (b *Buffer) Push(pkt *pionrtp.Packet) PushResult {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.st.Pushed++

	if len(pkt.Payload) != codec.G711FrameBytes {
		b.st.BadSize++
		return PushBadSize
	}

	ext := b.extend(pkt.Timestamp)

	// Only timestamps on the frame grid can ever line up with a playout slot.
	// The difference is taken as signed because a packet can legitimately be
	// older than the first one seen, and an unsigned wrap here would misreport
	// such a packet as off-grid.
	if (int64(ext)-int64(b.refOrigin))%codec.SamplesPerFrame != 0 {
		b.st.OffGrid++
		return PushOffGrid
	}

	if b.playing && ext < b.playout {
		b.st.Late++
		b.adaptGrowLocked()
		return PushLate
	}
	if _, dup := b.frames[ext]; dup {
		b.st.Duplicate++
		return PushDuplicate
	}

	// Copy the payload: the receive loop reuses its read buffer.
	payload := make([]byte, len(pkt.Payload))
	copy(payload, pkt.Payload)
	b.frames[ext] = payload
	b.st.Accepted++

	b.evictOverflowLocked()

	if d := len(b.frames); d > b.maxObserved {
		b.maxObserved = d
	}
	return PushAccepted
}

// wireTS maps an extended timestamp back to the 32-bit value that appeared on
// the wire, so emitted frames carry timestamps a receiver would recognize. Both
// conversions truncate modulo 2^32, which is exactly the wrap RTP uses.
func (b *Buffer) wireTS(ext uint64) uint32 {
	return b.originLow + uint32(int64(ext)-int64(b.refOrigin))
}

// evictOverflowLocked enforces MaxDepth. When the producer outruns the consumer,
// dropping the oldest frame and advancing playout past it is what keeps latency
// bounded — the alternative, rejecting new arrivals, would hold the buffer
// permanently full and never recover the delay.
func (b *Buffer) evictOverflowLocked() {
	for len(b.frames) > b.cfg.MaxDepth {
		oldest := uint64(0)
		first := true
		for ts := range b.frames {
			if first || ts < oldest {
				oldest, first = ts, false
			}
		}
		delete(b.frames, oldest)
		b.st.Evicted++
		if b.playing && oldest >= b.playout {
			// Skip playout past the evicted slot so the hole is not then also
			// counted as a concealment: the frame is gone because of latency
			// control, which is a different failure from losing it.
			b.playout = oldest + codec.SamplesPerFrame
		}
	}
}

// Pop emits the next playout slot. The second return is false while the buffer
// is still prebuffering, which is the consumer's signal to wait rather than to
// play filler.
func (b *Buffer) Pop() (Frame, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.popLocked(false)
}

// popLocked implements Pop. With force set, playout starts even if the buffer
// never reached its target depth, which is what Drain needs at end of call.
func (b *Buffer) popLocked(force bool) (Frame, bool) {
	if !b.playing {
		if len(b.frames) == 0 {
			b.st.CurrentDepth = 0
			return Frame{}, false
		}
		if !force && len(b.frames) < b.target {
			b.st.CurrentDepth = len(b.frames)
			return Frame{}, false
		}
		// Start at the earliest buffered frame: during prebuffering packets may
		// have arrived out of order, and playout must begin at the oldest.
		b.playout = b.earliestLocked()
		b.playing = true
	}

	slot := b.playout
	b.playout += codec.SamplesPerFrame
	b.st.Popped++

	payload, ok := b.frames[slot]
	if !ok {
		b.st.Concealed++
		if len(b.frames) == 0 {
			b.st.Starved++
		}
		b.cleanPops = 0
		return Frame{
			PCM:       b.conceal.conceal(),
			Timestamp: b.wireTS(slot),
			Concealed: true,
		}, true
	}

	delete(b.frames, slot)
	pcm := b.cfg.Codec.Decode(payload)
	b.conceal.observe(pcm)

	b.cleanPops++
	b.adaptShrinkLocked()

	return Frame{PCM: pcm, Timestamp: b.wireTS(slot)}, true
}

// Drain empties the buffer, filling any remaining holes. Playout is forced to
// start even if the buffer never reached its target depth, so a call shorter
// than the prebuffer is not silently discarded.
func (b *Buffer) Drain() []Frame {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []Frame
	for len(b.frames) > 0 {
		f, ok := b.popLocked(true)
		if !ok {
			break
		}
		out = append(out, f)
	}
	return out
}

func (b *Buffer) earliestLocked() uint64 {
	earliest := uint64(0)
	first := true
	for ts := range b.frames {
		if first || ts < earliest {
			earliest, first = ts, false
		}
	}
	return earliest
}

// adaptGrowLocked deepens the buffer in response to a late packet. Growth is
// immediate: a late packet is audio already being thrown away.
func (b *Buffer) adaptGrowLocked() {
	if !b.cfg.Adaptive || b.target >= b.cfg.AdaptiveMaxTarget {
		return
	}
	b.target++
	b.cleanPops = 0
}

// adaptShrinkLocked reclaims depth after a sustained clean stretch.
func (b *Buffer) adaptShrinkLocked() {
	if !b.cfg.Adaptive || b.target <= b.baseTarget {
		return
	}
	if b.cleanPops >= adaptiveDecayPops {
		b.target--
		b.cleanPops = 0
	}
}

// Stats snapshots buffer state.
func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.st
	st.CurrentDepth = len(b.frames)
	st.MaxObservedDepth = b.maxObserved
	st.TargetDepth = b.target
	st.Prebuffering = !b.playing
	return st
}

// Reset returns the buffer to its initial state for reuse on another call.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.frames = make(map[uint64][]byte, b.cfg.MaxDepth*2)
	b.tsInit = false
	b.originLow = 0
	b.playing = false
	b.playout = 0
	b.target = b.baseTarget
	b.cleanPops = 0
	b.maxObserved = 0
	b.st = Stats{}
	b.conceal.reset()
}

// extend maps a wrapping 32-bit RTP timestamp into a monotonic 64-bit space.
// A signed 32-bit delta from the most recent reference gives the shortest
// distance, which is correct in both directions and survives the wrap.
func (b *Buffer) extend(ts uint32) uint64 {
	if !b.tsInit {
		b.tsInit = true
		b.refLow = ts
		b.refExt = tsBase
		b.refOrigin = tsBase
		b.originLow = ts
		return tsBase
	}
	delta := int32(ts - b.refLow)
	ext := uint64(int64(b.refExt) + int64(delta))
	if ext > b.refExt {
		b.refLow, b.refExt = ts, ext
	}
	return ext
}
