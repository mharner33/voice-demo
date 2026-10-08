// Command voicegw is the media gateway: it receives RTP/G.711, runs each stream
// through a jitter buffer, and measures what the network did along the way.
//
// Phase 3 scope. The playout loop currently discards the frames it pops; phase 4
// hands them to the STT provider instead. The gRPC control plane and Datadog
// instrumentation arrive in phase 5. See docs/plan.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/jbuf"
	"github.com/mharner33/voice-demo/internal/rtp"
)

func main() {
	var (
		addr     = flag.String("addr", ":5004", "UDP address to receive RTP on")
		interval = flag.Duration("report-interval", 5*time.Second, "how often to report live stream stats")
		idle     = flag.Duration("idle-timeout", 3*time.Second, "silence after which a stream is treated as ended")

		target   = flag.Int("jbuf-target", jbuf.DefaultTargetDepth, "jitter buffer prebuffer depth in frames (20ms each)")
		maxDepth = flag.Int("jbuf-max", jbuf.DefaultMaxDepth, "jitter buffer hard cap in frames")
		adaptive = flag.Bool("jbuf-adaptive", false, "let the jitter buffer deepen itself in response to late packets")
		conceal  = flag.String("conceal", "repeat", "concealment for lost frames: repeat, silence, or noise")
	)
	flag.Parse()

	mode, err := parseConcealMode(*conceal)
	if err != nil {
		log.Fatalf("voicegw: %v", err)
	}

	cfg := jbuf.Config{
		Codec:       codec.PCMU,
		TargetDepth: *target,
		MaxDepth:    *maxDepth,
		Adaptive:    *adaptive,
		Conceal:     mode,
	}
	// Fail fast on a bad flag rather than per-call once traffic arrives.
	if _, err := jbuf.New(cfg); err != nil {
		log.Fatalf("voicegw: %v", err)
	}

	if err := run(*addr, *interval, *idle, cfg); err != nil {
		log.Fatalf("voicegw: %v", err)
	}
}

func parseConcealMode(s string) (jbuf.ConcealMode, error) {
	switch strings.ToLower(s) {
	case "repeat":
		return jbuf.ConcealRepeat, nil
	case "silence":
		return jbuf.ConcealSilence, nil
	case "noise":
		return jbuf.ConcealNoise, nil
	default:
		return 0, fmt.Errorf("unknown conceal mode %q; want repeat, silence, or noise", s)
	}
}

// call is one inbound stream: its measurements, its jitter buffer, and the
// playout loop draining that buffer at a steady cadence.
type call struct {
	ssrc uint32
	sess *rtp.Session
	jb   *jbuf.Buffer

	mu       sync.Mutex
	lastSeen time.Time
	reported bool

	stop chan struct{}
	done chan struct{}
}

func newCall(ssrc uint32, sess *rtp.Session, cfg jbuf.Config, now time.Time) (*call, error) {
	jb, err := jbuf.New(cfg)
	if err != nil {
		return nil, err
	}
	c := &call{
		ssrc:     ssrc,
		sess:     sess,
		jb:       jb,
		lastSeen: now,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go c.playout()
	return c, nil
}

// playoutHangover is how many consecutive starved slots the playout loop will
// fill before going idle to wait for more audio.
//
// Without this bound the loop would emit concealment frames forever once a call
// went quiet, and since every one of those counts as a concealed slot, the
// conceal rate would drift toward 100% and stop describing the call. The
// threshold has to exceed the longest plausible mid-call gap or real holes would
// be cut short: the lossy-wan profile bursts about 4 frames, so 10 frames
// (200 ms) leaves ample margin.
//
// This is a stopgap. Phase 5's control plane carries an explicit end-of-call
// message, which ends playout exactly instead of inferring it from silence.
const playoutHangover = 10

// playout drains the jitter buffer at the packetization interval, which is what
// an audio device's callback would do. Phase 4 forwards these frames to the STT
// provider; for now popping them is what exercises the buffer.
func (c *call) playout() {
	defer close(c.done)

	ticker := time.NewTicker(codec.FrameDuration)
	defer ticker.Stop()

	starved := 0
	for {
		select {
		case <-c.stop:
			c.jb.Drain()
			return
		case <-ticker.C:
			if starved >= playoutHangover {
				// Idle until audio arrives again, rather than emitting filler.
				if c.jb.Stats().CurrentDepth == 0 {
					continue
				}
				starved = 0
			}

			f, ok := c.jb.Pop()
			if !ok {
				continue // still prebuffering
			}
			// A concealed slot with an empty buffer behind it means the far end
			// has stopped sending, not that a packet went missing mid-call.
			if f.Concealed && c.jb.Stats().CurrentDepth == 0 {
				starved++
			} else {
				starved = 0
			}
		}
	}
}

func (c *call) touch(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = t
	c.reported = false
}

// expired reports whether the stream has gone silent long enough to be treated
// as ended, and claims the one-time end-of-call report.
func (c *call) expired(now time.Time, idle time.Duration) (ended, alreadyReported bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ended = now.Sub(c.lastSeen) > idle
	alreadyReported = c.reported
	if ended {
		c.reported = true
	}
	return ended, alreadyReported
}

func (c *call) close() {
	select {
	case <-c.stop: // already closed
	default:
		close(c.stop)
	}
	<-c.done
}

func run(addr string, reportInterval, idleTimeout time.Duration, cfg jbuf.Config) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer conn.Close()

	log.Printf("listening for RTP on %s", conn.LocalAddr())
	log.Printf("jitter buffer: target %d frames (%dms), max %d frames (%dms), conceal=%s, adaptive=%v",
		cfg.TargetDepth, cfg.TargetDepth*20, cfg.MaxDepth, cfg.MaxDepth*20,
		cfg.Conceal, cfg.Adaptive)

	recv := rtp.NewReceiver(conn, codec.SampleRate8k)

	var (
		mu    sync.Mutex
		calls = make(map[uint32]*call)
	)

	recv.OnPacket(func(sess *rtp.Session, pkt *pionrtp.Packet, arrival time.Time) {
		mu.Lock()
		c, known := calls[sess.SSRC()]
		if !known {
			var err error
			c, err = newCall(sess.SSRC(), sess, cfg, arrival)
			if err != nil {
				mu.Unlock()
				log.Printf("stream %#08x: cannot start: %v", sess.SSRC(), err)
				return
			}
			calls[sess.SSRC()] = c
			log.Printf("stream %#08x: started", sess.SSRC())
		}
		mu.Unlock()

		c.touch(arrival)

		// The jitter buffer is the one place that decides whether a packet is
		// usable. Its verdict is worth surfacing only for the unusual cases.
		switch res := c.jb.Push(pkt); res {
		case jbuf.PushAccepted, jbuf.PushDuplicate, jbuf.PushLate:
			// Counted in the buffer's own stats.
		default:
			log.Printf("stream %#08x: seq=%d rejected: %s", sess.SSRC(), pkt.SequenceNumber, res)
		}
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(reportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				mu.Lock()
				snapshot := make([]*call, 0, len(calls))
				for _, c := range calls {
					snapshot = append(snapshot, c)
				}
				mu.Unlock()

				for _, c := range snapshot {
					ended, already := c.expired(now, idleTimeout)
					switch {
					case ended && !already:
						c.close()
						report("ended", c)
					case !ended:
						report("live", c)
					}
				}
			}
		}
	}()

	serveErr := recv.Serve(ctx)
	stop()
	wg.Wait()

	mu.Lock()
	remaining := make([]*call, 0, len(calls))
	for _, c := range calls {
		remaining = append(remaining, c)
	}
	mu.Unlock()

	for _, c := range remaining {
		c.close()
		report("final", c)
	}
	if n := recv.Malformed(); n > 0 {
		log.Printf("ignored %d malformed packets", n)
	}
	if serveErr != nil {
		return serveErr
	}
	log.Print("shutdown complete")
	return nil
}

func report(phase string, c *call) {
	// Named ns rather than net, which would shadow the net package.
	ns := c.sess.Stats()
	if ns.Received == 0 {
		return
	}
	jb := c.jb.Stats()

	fmt.Fprintf(os.Stdout,
		"[%s] ssrc=%#08x | net: recv=%d expected=%d lost=%d (%.2f%%) dup=%d reorder=%d "+
			"jitter=%.1fms mos=%.2f dur=%s | jbuf: played=%d concealed=%d (%.2f%%) "+
			"late=%d evicted=%d depth=%.0f/%.0fms peak=%d\n",
		phase, ns.SSRC,
		ns.Received, ns.Expected, ns.Lost, ns.LossPct, ns.Duplicated, ns.Reordered,
		ns.JitterMs, ns.MOS(), ns.Duration.Round(time.Millisecond),
		jb.Popped, jb.Concealed, jb.ConcealRate()*100,
		jb.Late, jb.Evicted, jb.DepthMs(), jb.TargetDepthMs(), jb.MaxObservedDepth)
}
