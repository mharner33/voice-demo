package rtp

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	pionrtp "github.com/pion/rtp"
)

// PacketHandler is called for every well-formed packet, after its stats have
// been recorded. It runs on the receive loop, so it must not block: hand audio
// off to a buffered channel and return.
//
// src is where the packet came from. The gateway needs it to send synthesized
// audio back: combining the observed source address with the return port the
// client declares at call setup is what makes the return path work from behind
// NAT, and is what SDP plus RTP does in practice.
type PacketHandler func(sess *Session, pkt *pionrtp.Packet, src net.Addr, arrival time.Time)

// Receiver reads RTP from a packet connection and demultiplexes by SSRC, keeping
// independent statistics per stream.
type Receiver struct {
	conn      net.PacketConn
	clockRate int

	mu       sync.RWMutex
	sessions map[uint32]*Session
	handler  PacketHandler

	malformed atomic.Uint64
	packets   atomic.Uint64
}

// NewReceiver creates a receiver on conn. clockRate is the codec sample rate,
// 8000 for G.711.
func NewReceiver(conn net.PacketConn, clockRate int) *Receiver {
	return &Receiver{
		conn:      conn,
		clockRate: clockRate,
		sessions:  make(map[uint32]*Session),
	}
}

// OnPacket registers the handler for received packets.
func (r *Receiver) OnPacket(h PacketHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handler = h
}

// Serve reads until ctx is cancelled or the connection is closed. It returns nil
// on a clean shutdown via either route.
func (r *Receiver) Serve(ctx context.Context) error {
	// Unblock the blocking read when the context is cancelled.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			r.conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	buf := make([]byte, MaxPacketSize)
	for {
		n, src, err := r.conn.ReadFrom(buf)
		// Stamp the arrival time before any parsing, so processing cost does not
		// leak into the jitter measurement.
		arrival := time.Now()

		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) || isTimeout(err) {
				return nil
			}
			return err
		}

		pkt, perr := Unmarshal(buf[:n])
		if perr != nil {
			// A malformed packet is counted and ignored. Over UDP this is
			// expected occasionally and must never stop the call.
			r.malformed.Add(1)
			continue
		}
		r.packets.Add(1)

		sess := r.session(pkt.SSRC)
		sess.Observe(pkt, arrival)

		r.mu.RLock()
		h := r.handler
		r.mu.RUnlock()
		if h != nil {
			// Hand over a copy of the payload: buf is reused on the next read.
			payload := make([]byte, len(pkt.Payload))
			copy(payload, pkt.Payload)
			pkt.Payload = payload
			h(sess, pkt, src, arrival)
		}
	}
}

// session returns the stats tracker for an SSRC, creating it on first sight.
func (r *Receiver) session(ssrc uint32) *Session {
	r.mu.RLock()
	s, ok := r.sessions[ssrc]
	r.mu.RUnlock()
	if ok {
		return s
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[ssrc]; ok { // lost the race
		return s
	}
	s = NewSession(ssrc, r.clockRate)
	r.sessions[ssrc] = s
	return s
}

// Session returns the tracker for an SSRC if the stream has been seen.
func (r *Receiver) Session(ssrc uint32) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[ssrc]
	return s, ok
}

// Sessions returns trackers for every stream seen so far.
func (r *Receiver) Sessions() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// Malformed counts packets that failed to parse.
func (r *Receiver) Malformed() uint64 { return r.malformed.Load() }

// Packets counts packets that parsed successfully.
func (r *Receiver) Packets() uint64 { return r.packets.Load() }

// LocalAddr reports the address the receiver is bound to, which is how a test
// discovers the port after binding to :0.
func (r *Receiver) LocalAddr() net.Addr { return r.conn.LocalAddr() }

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
