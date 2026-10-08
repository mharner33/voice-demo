// Command voicegw is the media gateway: it receives RTP/G.711 and measures what
// the network did to each stream.
//
// Phase 2 scope. The gRPC control plane, jitter buffer, STT/LLM/TTS pipeline, and
// Datadog instrumentation arrive in phases 3 through 8; see docs/plan.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/rtp"
)

func main() {
	var (
		addr     = flag.String("addr", ":5004", "UDP address to receive RTP on")
		interval = flag.Duration("report-interval", 5*time.Second, "how often to report live stream stats")
		idle     = flag.Duration("idle-timeout", 3*time.Second, "silence after which a stream is treated as ended")
	)
	flag.Parse()

	if err := run(*addr, *interval, *idle); err != nil {
		log.Fatalf("voicegw: %v", err)
	}
}

func run(addr string, reportInterval, idleTimeout time.Duration) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	defer conn.Close()

	log.Printf("listening for RTP on %s", conn.LocalAddr())

	recv := rtp.NewReceiver(conn, codec.SampleRate8k)

	// Track last-seen times so a finished call can be reported once and retired,
	// standing in for the BYE that the phase-5 control plane will deliver.
	var (
		mu       sync.Mutex
		lastSeen = make(map[uint32]time.Time)
		reported = make(map[uint32]bool)
	)
	recv.OnPacket(func(sess *rtp.Session, _ *pionrtp.Packet, arrival time.Time) {
		mu.Lock()
		defer mu.Unlock()
		if _, known := lastSeen[sess.SSRC()]; !known {
			log.Printf("stream %#08x: started", sess.SSRC())
		}
		lastSeen[sess.SSRC()] = arrival
		reported[sess.SSRC()] = false
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
			case <-ticker.C:
				now := time.Now()
				for _, sess := range recv.Sessions() {
					mu.Lock()
					seen, ok := lastSeen[sess.SSRC()]
					done := ok && now.Sub(seen) > idleTimeout
					already := reported[sess.SSRC()]
					if done {
						reported[sess.SSRC()] = true
					}
					mu.Unlock()

					switch {
					case done && !already:
						logStats("ended", sess.Stats())
					case !done:
						logStats("live", sess.Stats())
					}
				}
			}
		}
	}()

	serveErr := recv.Serve(ctx)
	stop()
	wg.Wait()

	// Final report, including any stream that was still live at shutdown.
	for _, sess := range recv.Sessions() {
		logStats("final", sess.Stats())
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

func logStats(phase string, st rtp.Stats) {
	if st.Received == 0 {
		return
	}
	fmt.Fprintf(os.Stdout,
		"[%s] ssrc=%#08x recv=%d expected=%d lost=%d (%.2f%%) dup=%d reorder=%d "+
			"jitter=%.2fms max_jitter=%.2fms mos=%.2f dur=%s\n",
		phase, st.SSRC, st.Received, st.Expected, st.Lost, st.LossPct,
		st.Duplicated, st.Reordered, st.JitterMs, st.MaxJitterMs, st.MOS(),
		st.Duration.Round(time.Millisecond))
}
