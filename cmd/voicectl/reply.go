package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/jbuf"
	"github.com/mharner33/voice-demo/internal/rtp"
)

// replyListener receives the agent's synthesized audio back over RTP.
//
// It runs the same jitter buffer the gateway uses, which is the honest thing to
// do: the return path is a real RTP stream and is subject to the same
// reordering and loss as the forward one. Reusing the buffer also means the
// saved audio is what a caller would actually have heard.
type replyListener struct {
	conn *net.UDPConn
	recv *rtp.Receiver
	jb   *jbuf.Buffer

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	frames int
	bound  bool
}

// newReplyListener binds a UDP socket for return audio. Port zero lets the
// kernel choose, and the chosen port is what gets declared at call setup.
func newReplyListener(port int, c codec.Codec) (*replyListener, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, fmt.Errorf("binding the reply listener: %w", err)
	}

	jb, err := jbuf.New(jbuf.Config{
		Codec: c,
		// A shallow buffer is right here: the return path is local and the
		// goal is to capture the audio, not to smooth it for a human ear.
		TargetDepth: 2,
		MaxDepth:    200,
		Conceal:     jbuf.ConcealSilence,
	})
	if err != nil {
		conn.Close()
		return nil, err
	}

	l := &replyListener{
		conn: conn,
		recv: rtp.NewReceiver(conn, codec.SampleRate8k),
		jb:   jb,
		done: make(chan struct{}),
	}

	l.recv.OnPacket(func(_ *rtp.Session, pkt *pionrtp.Packet, _ net.Addr, _ time.Time) {
		l.mu.Lock()
		if !l.bound {
			l.bound = true
			log.Printf("receiving the agent's reply on %s", conn.LocalAddr())
		}
		l.frames++
		l.mu.Unlock()

		l.jb.Push(pkt)
	})

	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	go func() {
		defer close(l.done)
		if err := l.recv.Serve(ctx); err != nil {
			log.Printf("reply listener: %v", err)
		}
	}()

	return l, nil
}

// Port is the port to declare at call setup.
func (l *replyListener) Port() int {
	return l.conn.LocalAddr().(*net.UDPAddr).Port
}

// Frames reports how many packets of reply audio arrived.
func (l *replyListener) Frames() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.frames
}

// Audio drains the buffer into one PCM buffer.
func (l *replyListener) Audio() codec.Audio {
	var pcm []int16
	for _, f := range l.jb.Drain() {
		pcm = append(pcm, f.PCM...)
	}
	return codec.Audio{PCM: pcm, SampleRate: codec.SampleRate8k}
}

// Close stops the listener.
func (l *replyListener) Close() {
	l.cancel()
	<-l.done
	l.conn.Close()
}

// Stats exposes what the return stream looked like, so a demo can show that the
// reply path is a real RTP stream with its own accounting.
func (l *replyListener) Stats() (rtp.Stats, bool) {
	sessions := l.recv.Sessions()
	if len(sessions) == 0 {
		return rtp.Stats{}, false
	}
	return sessions[0].Stats(), true
}
