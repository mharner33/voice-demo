package obs

import (
	"fmt"
	"time"
)

// metricNamespace prefixes every metric this project emits.
const metricNamespace = "voice"

// Metric names, gathered here so the whole emitted surface can be reviewed in
// one place and kept consistent with the dashboard.
//
// Only transport and call-level figures are emitted over DogStatsD. Token
// counts and time-to-first-token are deliberately absent: they are attached to
// the Agent Observability spans, and Datadog derives platform metrics from
// those recognized keys, so emitting them here as well would double count.
const (
	mRTPReceived   = "rtp.packets.received"
	mRTPLost       = "rtp.packets.lost"
	mRTPReordered  = "rtp.packets.reordered"
	mRTPDuplicated = "rtp.packets.duplicated"
	mRTPLossPct    = "rtp.loss_pct"
	mRTPJitter     = "rtp.jitter_ms"

	mJbufDepth      = "jbuf.depth_ms"
	mJbufTarget     = "jbuf.target_depth_ms"
	mJbufConcealed  = "jbuf.concealed"
	mJbufLate       = "jbuf.late_drops"
	mJbufEvicted    = "jbuf.evicted"
	mJbufStarved    = "jbuf.underruns"
	mJbufConcealPct = "jbuf.conceal_pct"

	mMOS = "mos.estimate"

	mCallDuration  = "call.duration_ms"
	mCallAudioIn   = "call.audio_in_ms"
	mCallTurns     = "call.turns"
	mCallToolCalls = "call.tool_calls"
	mCallErrors    = "call.errors"
	mCallConcealed = "call.concealed_pct"
	mCallDropped   = "call.pipeline_dropped"

	mSTTFirstPartial = "stt.first_partial_ms"
	mSTTFinal        = "stt.final_ms"
	mTurnLLMLatency  = "llm.latency_ms"
	mTurnTTSFirst    = "tts.first_byte_ms"
	mTurnE2E         = "call.e2e_latency_ms"
)

// NetworkStats is what the RTP layer measured. It mirrors rtp.Stats as a plain
// value type so that obs does not import the transport packages, which keeps
// the dependency arrow pointing one way.
type NetworkStats struct {
	Received   uint64
	Lost       uint64
	Reordered  uint64
	Duplicated uint64
	LossPct    float64
	JitterMs   float64
	MOS        float64
	Duration   time.Duration
}

// BufferStats is what the jitter buffer did.
type BufferStats struct {
	Popped        uint64
	Concealed     uint64
	Starved       uint64
	Late          uint64
	Evicted       uint64
	DepthMs       float64
	TargetDepthMs float64
	ConcealPct    float64
}

// PipelineStats is what the AI pipeline produced.
type PipelineStats struct {
	Turns          int
	ToolCalls      int
	Errors         int
	AudioIn        time.Duration
	ConcealedPct   float64
	DroppedFrames  uint64
	FirstPartialAt time.Duration
	HavePartial    bool
}

// TurnStats is one agent turn's latencies.
type TurnStats struct {
	FinalAt      time.Duration
	LLMLatency   time.Duration
	TTSFirstByte time.Duration
}

// CallTags identify a call on every metric it produces.
type CallTags struct {
	CallID string
	Codec  string

	// ProviderProfile is the gateway's provider-impairment profile.
	//
	// It is deliberately not called the chaos profile. The *network* profile —
	// loss, jitter, reordering — is chosen by the client, and the gateway has
	// no way to learn it: nothing in an RTP stream says how it was degraded.
	// Tagging the gateway's provider profile as though it were the network
	// profile would attribute client-side impairment to the server. The
	// network profile becomes available once the control plane carries it at
	// call setup.
	ProviderProfile string

	STTProvider string
	LLMProvider string
	TTSProvider string
}

// Slice renders the tags in DogStatsD form.
//
// call_id is deliberately omitted. It is unique per call, so including it would
// create an unbounded tag cardinality — the classic way to make a metrics bill
// explode and the queries slow. The call ID lives on the spans and in the call
// log, which is where a single call is looked up anyway.
func (t CallTags) Slice() []string {
	tags := make([]string, 0, 5)
	add := func(k, v string) {
		if v != "" {
			tags = append(tags, k+":"+v)
		}
	}
	add("codec", t.Codec)
	add("provider_profile", t.ProviderProfile)
	add("stt_provider", t.STTProvider)
	add("llm_provider", t.LLMProvider)
	add("tts_provider", t.TTSProvider)
	return tags
}

// RecordNetwork emits the transport measurements for one call.
//
// Jitter goes out as a distribution rather than a gauge so the agent computes
// percentiles across calls. The receiver tracks only a current and maximum
// value, because retaining per-packet samples to compute a p95 locally would
// be the wrong place to do that work.
func (t *Tracer) RecordNetwork(st NetworkStats, tags CallTags) {
	if t == nil || t.statsd == nil {
		return
	}
	ts := tags.Slice()

	t.statsd.Count(mRTPReceived, int64(st.Received), ts, 1)
	t.statsd.Count(mRTPLost, int64(st.Lost), ts, 1)
	t.statsd.Count(mRTPReordered, int64(st.Reordered), ts, 1)
	t.statsd.Count(mRTPDuplicated, int64(st.Duplicated), ts, 1)
	t.statsd.Distribution(mRTPLossPct, st.LossPct, ts, 1)
	t.statsd.Distribution(mRTPJitter, st.JitterMs, ts, 1)
	t.statsd.Distribution(mMOS, st.MOS, ts, 1)
}

// RecordBuffer emits the jitter buffer's behavior for one call.
func (t *Tracer) RecordBuffer(st BufferStats, tags CallTags) {
	if t == nil || t.statsd == nil {
		return
	}
	ts := tags.Slice()

	t.statsd.Count(mJbufConcealed, int64(st.Concealed), ts, 1)
	t.statsd.Count(mJbufLate, int64(st.Late), ts, 1)
	t.statsd.Count(mJbufEvicted, int64(st.Evicted), ts, 1)
	t.statsd.Count(mJbufStarved, int64(st.Starved), ts, 1)
	t.statsd.Distribution(mJbufDepth, st.DepthMs, ts, 1)
	t.statsd.Distribution(mJbufTarget, st.TargetDepthMs, ts, 1)
	t.statsd.Distribution(mJbufConcealPct, st.ConcealPct, ts, 1)
}

// RecordCall emits the call-level figures a voice operations dashboard watches.
func (t *Tracer) RecordCall(st PipelineStats, dur time.Duration, tags CallTags) {
	if t == nil || t.statsd == nil {
		return
	}
	ts := tags.Slice()

	t.statsd.Distribution(mCallDuration, float64(dur.Milliseconds()), ts, 1)
	t.statsd.Distribution(mCallAudioIn, float64(st.AudioIn.Milliseconds()), ts, 1)
	t.statsd.Count(mCallTurns, int64(st.Turns), ts, 1)
	t.statsd.Count(mCallToolCalls, int64(st.ToolCalls), ts, 1)
	t.statsd.Count(mCallErrors, int64(st.Errors), ts, 1)
	t.statsd.Count(mCallDropped, int64(st.DroppedFrames), ts, 1)
	t.statsd.Distribution(mCallConcealed, st.ConcealedPct, ts, 1)

	if st.HavePartial {
		t.statsd.Distribution(mSTTFirstPartial, float64(st.FirstPartialAt.Milliseconds()), ts, 1)
	}
}

// RecordTurn emits one turn's latencies.
//
// The end-to-end figure is what the caller actually experiences: the gap
// between finishing speaking and hearing the first audio back. It is the sum of
// the agent's thinking time and the synthesizer's first byte, and it is the
// single number most worth alerting on.
func (t *Tracer) RecordTurn(st TurnStats, tags CallTags) {
	if t == nil || t.statsd == nil {
		return
	}
	ts := tags.Slice()

	t.statsd.Distribution(mSTTFinal, float64(st.FinalAt.Milliseconds()), ts, 1)
	t.statsd.Distribution(mTurnLLMLatency, float64(st.LLMLatency.Milliseconds()), ts, 1)
	t.statsd.Distribution(mTurnTTSFirst, float64(st.TTSFirstByte.Milliseconds()), ts, 1)

	e2e := st.LLMLatency + st.TTSFirstByte
	t.statsd.Distribution(mTurnE2E, float64(e2e.Milliseconds()), ts, 1)
}

// Incr counts an arbitrary event, for the few cases that do not fit the typed
// recorders above.
func (t *Tracer) Incr(name string, tags []string) {
	if t == nil || t.statsd == nil {
		return
	}
	t.statsd.Incr(name, tags, 1)
}

// MetricNames lists every metric this package emits, fully qualified. The
// dashboard definition and the README are checked against it so they cannot
// drift from the code.
func MetricNames() []string {
	names := []string{
		mRTPReceived, mRTPLost, mRTPReordered, mRTPDuplicated, mRTPLossPct, mRTPJitter,
		mJbufDepth, mJbufTarget, mJbufConcealed, mJbufLate, mJbufEvicted, mJbufStarved,
		mJbufConcealPct, mMOS,
		mCallDuration, mCallAudioIn, mCallTurns, mCallToolCalls, mCallErrors,
		mCallConcealed, mCallDropped,
		mSTTFirstPartial, mSTTFinal, mTurnLLMLatency, mTurnTTSFirst, mTurnE2E,
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%s.%s", metricNamespace, n)
	}
	return out
}
