package rtp

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
)

// SenderStats is what the sender knows it did. It exists to be compared against
// what the receiver independently measured — agreement between the two is the
// evidence that the loss accounting is correct.
type SenderStats struct {
	FramesOffered uint64
	PacketsSent   uint64
	Dropped       uint64
	Duplicated    uint64
	Delayed       uint64
	WriteErrors   uint64
}

// Sender packetizes audio frames and writes them to a transport, applying
// network impairments on the way out.
type Sender struct {
	w    io.Writer
	pktz *Packetizer
	imp  *chaos.Impairer

	// Delayed packets are written from timer goroutines, so writes must be
	// serialized and drained before the transport closes.
	writeMu  sync.Mutex
	inFlight sync.WaitGroup
	closed   atomic.Bool

	framesOffered atomic.Uint64
	packetsSent   atomic.Uint64
	dropped       atomic.Uint64
	duplicated    atomic.Uint64
	delayed       atomic.Uint64
	writeErrors   atomic.Uint64
}

// NewSender creates a sender writing to w. A nil impairer means send everything
// immediately and untouched.
func NewSender(w io.Writer, c codec.Codec, ssrc uint32, imp *chaos.Impairer) (*Sender, error) {
	pktz, err := NewPacketizer(ssrc, c)
	if err != nil {
		return nil, err
	}
	return &Sender{w: w, pktz: pktz, imp: imp}, nil
}

// NewSenderAt is NewSender with pinned starting sequence and timestamp values,
// so a test can place the stream wherever it wants in the sequence space.
func NewSenderAt(w io.Writer, c codec.Codec, ssrc uint32, imp *chaos.Impairer, startSeq uint16, startTS uint32) *Sender {
	return &Sender{w: w, pktz: NewPacketizerAt(ssrc, c, startSeq, startTS), imp: imp}
}

// SSRC returns the stream identifier.
func (s *Sender) SSRC() uint32 { return s.pktz.SSRC() }

// Send packetizes one encoded audio frame and transmits it, subject to the
// configured impairments. A dropped packet is not an error: it consumed its
// sequence number and the receiver is meant to notice the gap.
func (s *Sender) Send(payload []byte) error {
	if s.closed.Load() {
		return fmt.Errorf("rtp: sender is closed")
	}
	s.framesOffered.Add(1)

	// The packet is built even when it will be dropped, so that the sequence
	// number and timestamp advance exactly as they would have on the wire.
	pkt := s.pktz.Packetize(payload)
	buf, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("rtp: marshaling packet seq=%d: %w", pkt.SequenceNumber, err)
	}

	action := chaos.Action{}
	if s.imp != nil {
		action = s.imp.Next()
	}
	if action.Drop {
		s.dropped.Add(1)
		return nil
	}

	copies := 1
	if action.Duplicate {
		copies = 2
		s.duplicated.Add(1)
	}

	if action.Delay <= 0 {
		for i := 0; i < copies; i++ {
			s.write(buf)
		}
		return nil
	}

	// Hold the packet back. Doing this with an independent timer per packet is
	// what makes jitter and reordering emerge naturally rather than being faked.
	s.delayed.Add(1)
	s.inFlight.Add(1)
	time.AfterFunc(action.Delay, func() {
		defer s.inFlight.Done()
		for i := 0; i < copies; i++ {
			s.write(buf)
		}
	})
	return nil
}

func (s *Sender) write(buf []byte) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return
	}
	if _, err := s.w.Write(buf); err != nil {
		s.writeErrors.Add(1)
		return
	}
	s.packetsSent.Add(1)
}

// Stream sends frames paced at interval, which is how a real endpoint emits
// audio: one 20 ms frame every 20 ms, regardless of how fast the source can be
// read. Pacing uses absolute deadlines so scheduler delays do not accumulate.
//
// A smaller interval compresses a call into less wall-clock time, which tests
// use to stay fast while keeping RTP timestamps at their true 20 ms spacing.
func (s *Sender) Stream(ctx context.Context, frames [][]byte, interval time.Duration) error {
	start := time.Now()
	for i, f := range frames {
		if deadline := start.Add(time.Duration(i) * interval); i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Until(deadline)):
			}
		}
		if err := s.Send(f); err != nil {
			return err
		}
	}
	return nil
}

// Drain waits for every delayed packet to be transmitted. Call it before closing
// the transport, or late packets are silently lost and the receiver's loss count
// will overstate what the network actually did.
func (s *Sender) Drain() { s.inFlight.Wait() }

// Close drains in-flight packets and stops accepting new ones.
func (s *Sender) Close() error {
	s.Drain()
	s.closed.Store(true)
	return nil
}

// SeqRange returns the first and last sequence numbers this sender built,
// including any the impairment layer dropped. The third return is false before
// anything has been sent.
//
// This is what the sender hands over at teardown so the receiver can account
// for loss at the edges of the stream. Both ends are needed: packets lost after
// the last one that arrived leave no evidence, and neither do packets lost
// before the first one, which instead make the receiver start counting one
// packet in.
func (s *Sender) SeqRange() (first, last uint16, ok bool) {
	if s.framesOffered.Load() == 0 {
		return 0, 0, false
	}
	// NextSeq is the number the *next* packet would carry.
	return s.pktz.StartSeq(), s.pktz.NextSeq() - 1, true
}

// Stats snapshots the sender's counters.
func (s *Sender) Stats() SenderStats {
	return SenderStats{
		FramesOffered: s.framesOffered.Load(),
		PacketsSent:   s.packetsSent.Load(),
		Dropped:       s.dropped.Load(),
		Duplicated:    s.duplicated.Load(),
		Delayed:       s.delayed.Load(),
		WriteErrors:   s.writeErrors.Load(),
	}
}
