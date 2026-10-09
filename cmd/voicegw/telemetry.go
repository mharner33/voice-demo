package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/mharner33/voice-demo/internal/call"
	"github.com/mharner33/voice-demo/internal/control"
	"github.com/mharner33/voice-demo/internal/jbuf"
	"github.com/mharner33/voice-demo/internal/obs"
	"github.com/mharner33/voice-demo/internal/rtp"
)

// emitTelemetry writes the call's spans, metrics, and log record.
//
// All of it happens at teardown rather than during the call because the
// transport figures are cumulative: loss, jitter and buffer behavior are only
// meaningful once the stream has ended — and, for loss specifically, only once
// the sender's final sequence number has been applied.
func (c *activeCall) emitTelemetry(reason string) {
	ns := c.stats()
	jb := c.jb.Stats()
	res, runErr, haveRes := c.snapshot()
	sr := c.senderReport.Load()

	// The transport stages ran for the whole call rather than as a nested call
	// stack, so their spans are created now and backdated to cover it. Their
	// measurements go on as tags: this is the APM half of the demo, where
	// packet loss and buffer depth belong.
	if c.root != nil {
		ingest, _ := c.tel.tracer.StartAPMAt(c.rootCtx(), "voice.rtp.ingest", c.start,
			map[string]string{"codec": c.tags.Codec})
		ingest.SetAPMTag("rtp.packets_received", int64(ns.Received))
		ingest.SetAPMTag("rtp.packets_expected", int64(ns.Expected))
		ingest.SetAPMTag("rtp.packets_lost", int64(ns.Lost))
		ingest.SetAPMTag("rtp.loss_pct", ns.LossPct)
		ingest.SetAPMTag("rtp.reordered", int64(ns.Reordered))
		ingest.SetAPMTag("rtp.duplicated", int64(ns.Duplicated))
		ingest.SetAPMTag("rtp.jitter_ms", ns.JitterMs)
		ingest.SetAPMTag("rtp.max_jitter_ms", ns.MaxJitterMs)
		ingest.SetAPMTag("mos", ns.MOS())
		// Whether the loss figure is exact or a lower bound. Without the
		// sender's final sequence number, packets lost at the tail of the
		// stream are invisible.
		ingest.SetAPMTag("rtp.sequence_range_known", ns.SequenceRangeKnown)
		if sr != nil {
			ingest.SetAPMTag("rtp.sender_packets_sent", int64(sr.PacketsSent))
			ingest.SetAPMTag("rtp.sender_packets_dropped", int64(sr.PacketsDropped))
		}
		ingest.Finish(nil)

		buffer, _ := c.tel.tracer.StartAPMAt(c.rootCtx(), "voice.jitter_buffer", c.start, nil)
		buffer.SetAPMTag("jbuf.frames_played", int64(jb.Popped))
		buffer.SetAPMTag("jbuf.concealed", int64(jb.Concealed))
		buffer.SetAPMTag("jbuf.conceal_pct", jb.ConcealRate()*100)
		buffer.SetAPMTag("jbuf.late_drops", int64(jb.Late))
		buffer.SetAPMTag("jbuf.evicted", int64(jb.Evicted))
		buffer.SetAPMTag("jbuf.underruns", int64(jb.Starved))
		buffer.SetAPMTag("jbuf.target_depth_ms", jb.TargetDepthMs())
		buffer.SetAPMTag("jbuf.peak_depth_frames", int64(jb.MaxObservedDepth))
		buffer.Finish(nil)

		c.root.SetAPMTag("call.turns", len(res.Turns))
		c.root.SetAPMTag("call.tool_calls", res.ToolCalls)
		c.root.SetAPMTag("call.errors", res.Errors)
		c.root.SetAPMTag("call.pipeline_dropped", int64(c.dropped.Load()))
		c.root.SetAPMTag("call.frames_returned", int64(c.egressFwd.Load()))
		c.root.SetAPMTag("call.end_reason", reason)
		c.root.SetAPMTag("call.signaled", c.signaled)
		c.root.Finish(runErr)
	}

	// Metrics. Transport and call-level figures only; token counts live on the
	// Agent Observability spans, from which Datadog derives its own.
	c.tel.tracer.RecordNetwork(obs.NetworkStats{
		Received:   ns.Received,
		Lost:       ns.Lost,
		Reordered:  ns.Reordered,
		Duplicated: ns.Duplicated,
		LossPct:    ns.LossPct,
		JitterMs:   ns.JitterMs,
		MOS:        ns.MOS(),
		Duration:   ns.Duration,
	}, c.tags)

	c.tel.tracer.RecordBuffer(obs.BufferStats{
		Popped:        jb.Popped,
		Concealed:     jb.Concealed,
		Starved:       jb.Starved,
		Late:          jb.Late,
		Evicted:       jb.Evicted,
		DepthMs:       jb.DepthMs(),
		TargetDepthMs: jb.TargetDepthMs(),
		ConcealPct:    jb.ConcealRate() * 100,
	}, c.tags)

	if haveRes {
		c.tel.tracer.RecordCall(obs.PipelineStats{
			Turns:          len(res.Turns),
			ToolCalls:      res.ToolCalls,
			Errors:         res.Errors,
			AudioIn:        res.AudioInDuration(),
			ConcealedPct:   res.ConcealedFraction() * 100,
			DroppedFrames:  c.dropped.Load(),
			FirstPartialAt: res.FirstPartialAt,
			HavePartial:    res.HavePartial,
		}, ns.Duration, c.tags)
	}

	c.writeCallLog(reason, ns, jb, res, runErr, haveRes, sr)
}

// rootCtx returns a context carrying the call's APM root span, so the transport
// spans attach to it rather than starting traces of their own.
func (c *activeCall) rootCtx() context.Context {
	if c.rootContext != nil {
		return c.rootContext
	}
	return context.Background()
}

// writeCallLog emits the one line that ties all three layers together.
func (c *activeCall) writeCallLog(reason string, ns rtp.Stats, jb jbuf.Stats,
	res call.Result, runErr error, haveRes bool, sr *control.SenderReport) {

	rec := obs.CallRecord{
		CallID:  c.id,
		SSRC:    fmt.Sprintf("%#08x", c.params.SSRC),
		Codec:   c.tags.Codec,
		Profile: c.tel.profile.Get(),

		From:           c.params.From,
		To:             c.params.To,
		NetworkProfile: c.params.NetworkProfile,
		Signaled:       c.signaled,
		EndReason:      reason,

		PacketsRx:   ns.Received,
		PacketsLost: ns.Lost,
		LossPct:     ns.LossPct,
		Reordered:   ns.Reordered,
		Duplicated:  ns.Duplicated,
		JitterMs:    ns.JitterMs,
		MOS:         ns.MOS(),
		DurationMs:  ns.Duration.Milliseconds(),

		PacketsExpected:    ns.Expected,
		SequenceRangeKnown: ns.SequenceRangeKnown,

		Played:        jb.Popped,
		Concealed:     jb.Concealed,
		ConcealPct:    jb.ConcealRate() * 100,
		LateDrops:     jb.Late,
		Evicted:       jb.Evicted,
		Underruns:     jb.Starved,
		TargetDepthMs: jb.TargetDepthMs(),

		STTProvider: c.tags.STTProvider,
		LLMProvider: c.tags.LLMProvider,
		TTSProvider: c.tags.TTSProvider,

		DroppedFrames:  c.dropped.Load(),
		FramesReturned: c.egressFwd.Load(),
	}

	// Both sides' accounting, so the loss figure can be checked rather than
	// taken on trust.
	if sr != nil {
		rec.SenderPacketsSent = sr.PacketsSent
		rec.SenderPacketsDropped = sr.PacketsDropped
		rec.SenderFramesOffered = sr.FramesOffered
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		rec.Event = "call.failed"
		rec.Error = runErr.Error()
	}

	if haveRes {
		rec.TurnCount = len(res.Turns)
		rec.ToolCalls = res.ToolCalls
		rec.PipelineErrors = res.Errors
		rec.InputTokens = res.InputTokens
		rec.OutputTokens = res.OutputTokens
		rec.ConcealedInPct = res.ConcealedFraction() * 100
		rec.FramesOut = res.FramesOut
		rec.CostUSD = res.CostUSD()
		if res.HaveConfidence() {
			mean := res.MeanConfidence()
			rec.MeanConfidence = &mean
		}
		if res.STTErr != nil {
			rec.STTError = res.STTErr.Error()
		}
		rec.AudioQuality = obs.AudioQualityBucket(res.ConcealedFraction())
		if res.HavePartial {
			rec.FirstPartialMs = res.FirstPartialAt.Milliseconds()
		}
		for _, turn := range res.Turns {
			tr := obs.TurnRecord{
				Index:        turn.Index,
				Transcript:   turn.Transcript,
				Reply:        turn.Reply,
				ToolCalls:    turn.ToolCalls,
				ToolRounds:   turn.ToolRounds,
				InputTokens:  turn.InputTokens,
				OutputTokens: turn.OutputTokens,
				FinalMs:      turn.FinalAt.Milliseconds(),
				LLMMs:        turn.LLMLatency.Milliseconds(),
				TTSFirstMs:   turn.TTSFirstByte.Milliseconds(),
				TTSAudioMs:   turn.TTSAudio.Milliseconds(),
			}
			if !turn.ConfidenceUnknown {
				conf := turn.Confidence
				tr.Confidence = &conf
			}
			if turn.Err != nil {
				tr.Error = turn.Err.Error()
			}
			rec.Turns = append(rec.Turns, tr)
		}
	}

	rec.Correlate(c.root, c.tel.tracer)

	if err := c.tel.callLog.Write(rec); err != nil {
		log.Printf("call %s: writing the call log: %v", c.id, err)
	}
}

// report prints a human-readable line for the terminal.
func report(phase string, c *activeCall) {
	ns := c.stats()
	if ns.Received == 0 {
		return
	}
	jb := c.jb.Stats()

	exact := ""
	if !ns.SequenceRangeKnown {
		// Say so rather than letting a lower bound read as a measurement.
		exact = "+"
	}

	fmt.Fprintf(os.Stdout,
		"[%s] %s | net: recv=%d lost=%d%s (%.2f%%) reorder=%d jitter=%.1fms mos=%.2f dur=%s"+
			" | jbuf: played=%d concealed=%d (%.2f%%) late=%d"+
			" | pipe: dropped=%d returned=%d\n",
		phase, c.id,
		ns.Received, ns.Lost, exact, ns.LossPct, ns.Reordered, ns.JitterMs, ns.MOS(),
		ns.Duration.Round(time.Millisecond),
		jb.Popped, jb.Concealed, jb.ConcealRate()*100, jb.Late,
		c.dropped.Load(), c.egressFwd.Load())

	res, runErr, have := c.snapshot()
	if !have {
		return
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		fmt.Fprintf(os.Stdout, "       %s | pipeline failed: %v\n", c.id, runErr)
		return
	}

	fmt.Fprintf(os.Stdout,
		"       %s | ai: turns=%d tools=%d errors=%d tokens=%d/%d"+
			" first_partial=%s concealed_in=%.1f%% frames_out=%d\n",
		c.id, len(res.Turns), res.ToolCalls, res.Errors,
		res.InputTokens, res.OutputTokens,
		res.FirstPartialAt.Round(time.Millisecond),
		res.ConcealedFraction()*100, res.FramesOut)

	for _, turn := range res.Turns {
		if turn.Err != nil {
			fmt.Fprintf(os.Stdout, "       %s | turn %d failed: %v\n", c.id, turn.Index, turn.Err)
			continue
		}
		fmt.Fprintf(os.Stdout,
			"       %s | turn %d: %q (conf %.2f) -> %q [tools=%v llm=%s tts_fb=%s]\n",
			c.id, turn.Index, turn.Transcript, turn.Confidence, turn.Reply,
			turn.ToolCalls,
			turn.LLMLatency.Round(time.Millisecond),
			turn.TTSFirstByte.Round(time.Millisecond))
	}
}
