package obs

import (
	"strings"
	"testing"
	"time"
)

func enabledTracer() (*Tracer, *captureStatsd) {
	return newTestTracer(Config{Enabled: true, Service: "voicegw", Env: "demo"})
}

var testTags = CallTags{
	CallID:          "c-deadbeef",
	Codec:           "PCMU",
	ProviderProfile: "provider-degraded",
	STTProvider:     "mock",
	LLMProvider:     "mock",
	TTSProvider:     "mock",
}

// TestCallTagsOmitCallID guards against the classic metrics mistake. A call ID
// is unique per call, so tagging metrics with it would create unbounded
// cardinality — an expensive and slow-querying mess. The call ID belongs on
// spans and in the call log, where looking up one call is the point.
func TestCallTagsOmitCallID(t *testing.T) {
	for _, tag := range testTags.Slice() {
		if strings.HasPrefix(tag, "call_id:") {
			t.Fatalf("call_id leaked into the metric tags: %q", tag)
		}
		if strings.Contains(tag, "c-deadbeef") {
			t.Fatalf("the call ID leaked into the metric tags: %q", tag)
		}
	}
}

func TestCallTagsContent(t *testing.T) {
	got := map[string]bool{}
	for _, tag := range testTags.Slice() {
		got[tag] = true
	}
	for _, want := range []string{
		"codec:PCMU", "provider_profile:provider-degraded",
		"stt_provider:mock", "llm_provider:mock", "tts_provider:mock",
	} {
		if !got[want] {
			t.Errorf("missing tag %q; got %v", want, testTags.Slice())
		}
	}

	// Empty fields are omitted rather than emitted as "key:".
	sparse := CallTags{Codec: "PCMA"}.Slice()
	if len(sparse) != 1 || sparse[0] != "codec:PCMA" {
		t.Errorf("sparse tags = %v, want just codec:PCMA", sparse)
	}
}

func TestRecordNetwork(t *testing.T) {
	tr, cap := enabledTracer()

	tr.RecordNetwork(NetworkStats{
		Received:   284,
		Lost:       16,
		Reordered:  95,
		Duplicated: 3,
		LossPct:    5.33,
		JitterMs:   29.8,
		MOS:        2.06,
	}, testTags)

	for name, want := range map[string]int64{
		mRTPReceived:   284,
		mRTPLost:       16,
		mRTPReordered:  95,
		mRTPDuplicated: 3,
	} {
		got, ok := cap.count(name)
		if !ok {
			t.Errorf("%s was not emitted", name)
			continue
		}
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}

	// Jitter, loss percentage and MOS are distributions so the agent computes
	// percentiles across calls; the receiver only knows its own current value.
	for name, want := range map[string]float64{
		mRTPJitter:  29.8,
		mRTPLossPct: 5.33,
		mMOS:        2.06,
	} {
		got, ok := cap.dist(name)
		if !ok {
			t.Fatalf("%s was not emitted as a distribution", name)
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want [%v]", name, got, want)
		}
	}

	if got := cap.tagsFor(mRTPJitter); len(got) == 0 {
		t.Error("no tags were attached to the jitter metric")
	}
}

func TestRecordBuffer(t *testing.T) {
	tr, cap := enabledTracer()

	tr.RecordBuffer(BufferStats{
		Popped:        310,
		Concealed:     26,
		Starved:       10,
		Late:          4,
		Evicted:       2,
		DepthMs:       120,
		TargetDepthMs: 160,
		ConcealPct:    8.39,
	}, testTags)

	for name, want := range map[string]int64{
		mJbufConcealed: 26,
		mJbufLate:      4,
		mJbufEvicted:   2,
		mJbufStarved:   10,
	} {
		if got, ok := cap.count(name); !ok || got != want {
			t.Errorf("%s = %d (present=%v), want %d", name, got, ok, want)
		}
	}
	for name, want := range map[string]float64{
		mJbufDepth:      120,
		mJbufTarget:     160,
		mJbufConcealPct: 8.39,
	} {
		got, ok := cap.dist(name)
		if !ok || len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v (present=%v), want [%v]", name, got, ok, want)
		}
	}
}

func TestRecordCall(t *testing.T) {
	tr, cap := enabledTracer()

	tr.RecordCall(PipelineStats{
		Turns:          3,
		ToolCalls:      2,
		Errors:         1,
		AudioIn:        6 * time.Second,
		ConcealedPct:   5.0,
		DroppedFrames:  7,
		FirstPartialAt: 621 * time.Millisecond,
		HavePartial:    true,
	}, 5936*time.Millisecond, testTags)

	for name, want := range map[string]int64{
		mCallTurns:     3,
		mCallToolCalls: 2,
		mCallErrors:    1,
		mCallDropped:   7,
	} {
		if got, ok := cap.count(name); !ok || got != want {
			t.Errorf("%s = %d (present=%v), want %d", name, got, ok, want)
		}
	}
	for name, want := range map[string]float64{
		mCallDuration:    5936,
		mCallAudioIn:     6000,
		mCallConcealed:   5.0,
		mSTTFirstPartial: 621,
	} {
		got, ok := cap.dist(name)
		if !ok || len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v (present=%v), want [%v]", name, got, ok, want)
		}
	}
}

// TestRecordCallOmitsAbsentPartial checks that a call which produced no partial
// hypothesis reports nothing rather than a misleading zero.
func TestRecordCallOmitsAbsentPartial(t *testing.T) {
	tr, cap := enabledTracer()
	tr.RecordCall(PipelineStats{HavePartial: false}, time.Second, testTags)

	if _, ok := cap.dist(mSTTFirstPartial); ok {
		t.Error("first_partial_ms was emitted for a call that produced no partial")
	}
}

// TestRecordTurnComputesEndToEnd checks the number that matters most to a
// caller: the silence between finishing speaking and hearing a reply.
func TestRecordTurnComputesEndToEnd(t *testing.T) {
	tr, cap := enabledTracer()

	tr.RecordTurn(TurnStats{
		FinalAt:      2 * time.Second,
		LLMLatency:   6005 * time.Millisecond,
		TTSFirstByte: 120 * time.Millisecond,
	}, testTags)

	got, ok := cap.dist(mTurnE2E)
	if !ok {
		t.Fatal("the end-to-end latency was not emitted")
	}
	// 6005 + 120 = 6125 ms of dead air.
	if len(got) != 1 || got[0] != 6125 {
		t.Errorf("e2e latency = %v, want [6125]", got)
	}

	if d, _ := cap.dist(mTurnLLMLatency); len(d) != 1 || d[0] != 6005 {
		t.Errorf("llm latency = %v, want [6005]", d)
	}
	if d, _ := cap.dist(mTurnTTSFirst); len(d) != 1 || d[0] != 120 {
		t.Errorf("tts first byte = %v, want [120]", d)
	}
}

// TestNoTokenMetricsOverStatsD pins a deliberate choice. Datadog derives
// platform metrics from the token attributes on Agent Observability spans, so
// emitting token counts here as well would double count them.
func TestNoTokenMetricsOverStatsD(t *testing.T) {
	tr, cap := enabledTracer()

	tr.RecordNetwork(NetworkStats{Received: 1}, testTags)
	tr.RecordBuffer(BufferStats{Popped: 1}, testTags)
	tr.RecordCall(PipelineStats{Turns: 1}, time.Second, testTags)
	tr.RecordTurn(TurnStats{LLMLatency: time.Second}, testTags)

	cap.mu.Lock()
	defer cap.mu.Unlock()
	for name := range cap.counts {
		if strings.Contains(name, "token") {
			t.Errorf("token metric %q emitted over DogStatsD; it belongs on the span", name)
		}
	}
	for name := range cap.dists {
		if strings.Contains(name, "token") {
			t.Errorf("token metric %q emitted over DogStatsD; it belongs on the span", name)
		}
	}
}

// TestMetricNamesAreNamespacedAndUnique keeps the emitted surface reviewable
// and keeps the dashboard honest.
func TestMetricNamesAreNamespacedAndUnique(t *testing.T) {
	names := MetricNames()
	if len(names) == 0 {
		t.Fatal("MetricNames() is empty")
	}

	seen := map[string]bool{}
	for _, n := range names {
		if !strings.HasPrefix(n, metricNamespace+".") {
			t.Errorf("%q is not namespaced under %q", n, metricNamespace)
		}
		if seen[n] {
			t.Errorf("%q is listed twice", n)
		}
		seen[n] = true
	}
}

func TestIncr(t *testing.T) {
	tr, cap := enabledTracer()
	tr.Incr("custom.event", []string{"k:v"})

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.incrs["custom.event"] != 1 {
		t.Errorf("custom.event count = %d, want 1", cap.incrs["custom.event"])
	}
}
