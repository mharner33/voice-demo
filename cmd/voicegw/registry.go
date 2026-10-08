package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/mharner33/voice-demo/internal/control"
	"github.com/mharner33/voice-demo/internal/jbuf"
	"github.com/mharner33/voice-demo/internal/rtp"
)

// gateway owns the live calls and implements the control plane.
type gateway struct {
	jbCfg     jbuf.Config
	providers providers
	tel       telemetry
	mediaPort int

	// requireSignaling rejects media from an SSRC that never signaled. It is
	// off by default so the media path can still be exercised on its own, which
	// is how every test before the control plane existed drives it.
	requireSignaling bool

	mu      sync.Mutex
	bySSRC  map[uint32]*activeCall
	byID    map[string]*activeCall
	nextSeq uint64
	// warned remembers which unsignaled SSRCs have been logged, so a
	// misconfigured client does not flood the log at fifty packets a second.
	warned map[uint32]bool
}

func newGateway(jbCfg jbuf.Config, p providers, tel telemetry, mediaPort int, requireSignaling bool) *gateway {
	return &gateway{
		jbCfg:            jbCfg,
		providers:        p,
		tel:              tel,
		mediaPort:        mediaPort,
		requireSignaling: requireSignaling,
		bySSRC:           make(map[uint32]*activeCall),
		byID:             make(map[string]*activeCall),
	}
}

// StartCall establishes a call before any media arrives, the INVITE analogue.
func (g *gateway) StartCall(ctx context.Context, req control.StartRequest) (control.StartResult, error) {
	g.mu.Lock()

	if existing, ok := g.bySSRC[req.SSRC]; ok {
		g.mu.Unlock()
		return control.StartResult{}, fmt.Errorf("%w: ssrc %#08x is call %s",
			control.ErrCallExists, req.SSRC, existing.id)
	}

	g.nextSeq++
	params := control.CallParams{
		CallID:         fmt.Sprintf("c-%08x-%d", req.SSRC, g.nextSeq),
		From:           req.From,
		To:             req.To,
		Codec:          req.Codec,
		SSRC:           req.SSRC,
		ReturnPort:     req.ReturnPort,
		NetworkProfile: req.NetworkProfile,
		StartedAt:      time.Now(),
	}
	g.mu.Unlock()

	c, err := newActiveCall(params, g.jbCfg, g.providers, g.tel, true)
	if err != nil {
		return control.StartResult{}, err
	}

	g.mu.Lock()
	// Re-check under the lock: a concurrent StartCall for the same SSRC could
	// have won the race while this one was building its pipeline.
	if existing, ok := g.bySSRC[req.SSRC]; ok {
		g.mu.Unlock()
		c.finish("superseded")
		return control.StartResult{}, fmt.Errorf("%w: ssrc %#08x is call %s",
			control.ErrCallExists, req.SSRC, existing.id)
	}
	g.bySSRC[req.SSRC] = c
	g.byID[params.CallID] = c
	g.mu.Unlock()

	log.Printf("call %s: established (ssrc %#08x, codec %s, network %q, return port %d)",
		params.CallID, req.SSRC, req.Codec,
		orNone(req.NetworkProfile), req.ReturnPort)

	return control.StartResult{
		CallID:    params.CallID,
		MediaPort: g.mediaPort,
		Codec:     params.Codec,
		TraceID:   c.root.APMTraceID(),
	}, nil
}

// EndCall tears a call down, the BYE analogue.
//
// The sender's final sequence number is applied to the statistics before they
// are read, which is the whole reason this message exists: loss at the tail of
// a stream is invisible to a receiver, because it never saw the sequence
// numbers that would reveal the gap.
func (g *gateway) EndCall(ctx context.Context, callID string, report control.SenderReport) (control.CallSummary, error) {
	g.mu.Lock()
	c, ok := g.byID[callID]
	if ok {
		delete(g.byID, callID)
		delete(g.bySSRC, c.params.SSRC)
	}
	g.mu.Unlock()

	if !ok {
		return control.CallSummary{}, fmt.Errorf("%w: %s", control.ErrCallNotFound, callID)
	}

	if report.HaveRange {
		if sess := c.sess.Load(); sess != nil {
			sess.NoteSequenceRange(report.FirstSequence, report.FinalSequence)
		}
	}
	c.senderReport.Store(&report)

	reason := report.Reason
	if reason == "" {
		reason = "bye"
	}
	c.finish(reason)

	ns := c.stats()
	jb := c.jb.Stats()
	res, _, haveRes := c.snapshot()

	sum := control.CallSummary{
		PacketsReceived: ns.Received,
		PacketsExpected: ns.Expected,
		PacketsLost:     ns.Lost,
		LossPct:         ns.LossPct,
		JitterMs:        ns.JitterMs,
		MOS:             ns.MOS(),
		FramesConcealed: jb.Concealed,
		ConcealPct:      jb.ConcealRate() * 100,
		TraceID:         c.root.APMTraceID(),
	}
	if haveRes {
		sum.Turns = len(res.Turns)
		sum.ToolCalls = res.ToolCalls
		sum.Transcripts = res.Transcripts()
		sum.Replies = res.Replies()
	}
	return sum, nil
}

// onPacket routes one inbound packet to its call, creating an unsignaled call
// if the SSRC never signaled and that is permitted.
func (g *gateway) onPacket(sess *rtp.Session, ssrc uint32, src net.Addr, arrival time.Time) *activeCall {
	g.mu.Lock()
	c, known := g.bySSRC[ssrc]
	if !known {
		if g.requireSignaling {
			g.mu.Unlock()
			g.warnUnsignaled(ssrc)
			return nil
		}

		g.nextSeq++
		params := control.CallParams{
			CallID:    fmt.Sprintf("c-%08x-%d", ssrc, g.nextSeq),
			Codec:     g.jbCfg.Codec,
			SSRC:      ssrc,
			StartedAt: arrival,
		}
		g.mu.Unlock()

		var err error
		c, err = newActiveCall(params, g.jbCfg, g.providers, g.tel, false)
		if err != nil {
			log.Printf("stream %#08x: cannot start: %v", ssrc, err)
			return nil
		}

		g.mu.Lock()
		if existing, ok := g.bySSRC[ssrc]; ok {
			g.mu.Unlock()
			c.finish("superseded")
			return existing
		}
		g.bySSRC[ssrc] = c
		g.byID[params.CallID] = c
		g.mu.Unlock()

		log.Printf("call %s: started unsignaled (ssrc %#08x) — no trailing-loss "+
			"accounting or return audio without StartCall", params.CallID, ssrc)
	} else {
		g.mu.Unlock()
	}

	c.bind(sess, src)
	c.touch(arrival)
	return c
}

// warnUnsignaled logs at most one line per SSRC so a misconfigured client does
// not flood the log at fifty packets a second.
func (g *gateway) warnUnsignaled(ssrc uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.warned == nil {
		g.warned = make(map[uint32]bool)
	}
	if g.warned[ssrc] {
		return
	}
	g.warned[ssrc] = true
	log.Printf("stream %#08x: rejected, no StartCall and -require-signaling is set", ssrc)
}

// reapIdle finishes calls that stopped sending without a teardown message. It
// is the fallback for a client that crashed, and the only path for unsignaled
// calls.
func (g *gateway) reapIdle(now time.Time, timeout time.Duration) {
	g.mu.Lock()
	var stale []*activeCall
	for ssrc, c := range g.bySSRC {
		if c.idle(now, timeout) {
			stale = append(stale, c)
			delete(g.bySSRC, ssrc)
			delete(g.byID, c.id)
		}
	}
	g.mu.Unlock()

	for _, c := range stale {
		reason := "idle-timeout"
		if c.signaled {
			// A signaled call that timed out never sent BYE, which is worth
			// distinguishing: its loss figures are a lower bound.
			reason = "idle-timeout-no-bye"
		}
		c.finish(reason)
		report("ended", c)
	}
}

// live returns the calls still in progress, for the periodic report.
func (g *gateway) live() []*activeCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*activeCall, 0, len(g.bySSRC))
	for _, c := range g.bySSRC {
		out = append(out, c)
	}
	return out
}

// drain finishes every remaining call, for shutdown.
func (g *gateway) drain(reason string) []*activeCall {
	g.mu.Lock()
	out := make([]*activeCall, 0, len(g.bySSRC))
	for ssrc, c := range g.bySSRC {
		out = append(out, c)
		delete(g.bySSRC, ssrc)
		delete(g.byID, c.id)
	}
	g.mu.Unlock()

	for _, c := range out {
		c.finish(reason)
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
