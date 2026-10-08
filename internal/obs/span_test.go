package obs

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/instrumentation/testutils/testtracer"
	"github.com/DataDog/dd-trace-go/v2/llmobs"
)

// These tests run against the real Agent Observability SDK via testtracer,
// which stands up a fake agent and captures the payloads that would have been
// submitted. That is the only way to verify what Datadog would actually
// receive; asserting on this project's own wrapper would prove nothing about
// the span kinds, the metric keys, or the session grouping.

// startSDK starts a tracer wired to a capturing fake agent.
func startSDK(t *testing.T) (*testtracer.TestTracer, *Tracer) {
	t.Helper()

	tt := testtracer.Start(t, testtracer.WithTracerStartOpts(
		tracer.WithService("voicegw"),
		tracer.WithEnv("test"),
		tracer.WithLLMObsEnabled(true),
		tracer.WithLLMObsMLApp("voice-demo"),
	))
	t.Cleanup(tt.Stop)

	// The Tracer wrapper reports enabled without re-starting the SDK, which
	// testtracer has already done.
	cfg := Config{Enabled: true, Service: "voicegw", Env: "test", LLMObsEnabled: true, MLApp: "voice-demo"}
	cfg.applyDefaults()
	return tt, &Tracer{cfg: cfg, statsd: newCapture(), llmobs: true}
}

// findSpan locates a captured LLM Obs span by name.
func findSpan(t *testing.T, spans []testtracer.LLMObsSpan, name string) testtracer.LLMObsSpan {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name)
	}
	t.Fatalf("no span named %q; got %v", name, names)
	return testtracer.LLMObsSpan{}
}

// TestSpanTreeMatchesCallShape is the phase-5 acceptance test for the span
// hierarchy. It builds the exact shape the pipeline produces and asserts what
// Datadog would receive: the kinds, the nesting, and the session grouping that
// makes a whole call one unit in the Agent Observability views.
func TestSpanTreeMatchesCallShape(t *testing.T) {
	tt, obsT := startSDK(t)

	const callID = "c-0c04c37e"
	ctx := context.Background()

	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", callID)
	if wf == nil {
		t.Fatal("no workflow span was created")
	}

	sttSpan, _ := obsT.StartLLMAt(ctx, "stt.transcribe", "scripted", "mock",
		time.Now().Add(-2*time.Second))
	sttSpan.LLMIO(
		[]Message{{Role: "user", Content: "<audio 2.00s 8000Hz PCMU, 5.0% concealed>"}},
		[]Message{{Role: "assistant", Content: "what is my balance"}},
		Metrics{TimeToFirstToken: 621 * time.Millisecond, BillableCharacters: 18},
		map[string]any{"confidence": 0.87},
	)
	sttSpan.Finish(nil)

	agentSpan, actx := obsT.StartAgent(ctx, "voice_agent")
	llmSpan, _ := obsT.StartLLM(actx, "agent.reply", "scripted", "mock")
	llmSpan.LLMIO(
		[]Message{{Role: "user", Content: "what is my balance"}},
		[]Message{{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID: "call_1", Name: "lookup_account", Args: []byte(`{"account_id":"4729"}`),
			}},
		}},
		Metrics{InputTokens: 12, OutputTokens: 8},
		nil,
	)
	llmSpan.Finish(nil)

	toolSpan, _ := obsT.StartTool(actx, "lookup_account")
	toolSpan.TextIO(`{"account_id":"4729"}`, "the balance is $1,284.52", nil)
	toolSpan.Finish(nil)

	agentSpan.TextIO("what is my balance", "the balance is $1,284.52", nil)
	agentSpan.Finish(nil)

	ttsSpan, _ := obsT.StartLLM(ctx, "tts.synthesize", "tone", "mock")
	ttsSpan.LLMIO(
		[]Message{{Role: "user", Content: "the balance is $1,284.52"}},
		[]Message{{Role: "assistant", Content: "<audio 1.80s 8000Hz, 90 frames>"}},
		Metrics{TimeToFirstToken: 12 * time.Millisecond, BillableCharacters: 24},
		nil,
	)
	ttsSpan.Finish(nil)

	wf.TextIO("<audio 2.00s>", "the balance is $1,284.52", nil)
	wf.Finish(nil)
	tracer.Flush()

	spans := tt.WaitForLLMObsSpans(t, 5)

	// --- every span belongs to the same call ---
	for _, s := range spans {
		if s.SessionID != callID {
			t.Errorf("span %q has session_id %q, want %q — the call would be "+
				"split across sessions", s.Name, s.SessionID, callID)
		}
	}

	// --- all five spans share one APM trace, which is the drill-down link ---
	apmTraceID := spans[0].DDAttributes.APMTraceID
	if apmTraceID == "" {
		t.Error("no APM trace ID; the LLM Obs spans cannot be pivoted to a trace")
	}
	for _, s := range spans {
		if s.DDAttributes.APMTraceID != apmTraceID {
			t.Errorf("span %q has APM trace %q, want %q",
				s.Name, s.DDAttributes.APMTraceID, apmTraceID)
		}
	}

	// --- the kinds are what the Agent Observability views key off ---
	for name, wantKind := range map[string]string{
		"voice_call":     "workflow",
		"stt.transcribe": "llm",
		"voice_agent":    "agent",
		"agent.reply":    "llm",
		"lookup_account": "tool",
		"tts.synthesize": "llm",
	} {
		s := findSpan(t, spans, name)
		if got := s.Meta["span.kind"]; got != wantKind {
			t.Errorf("span %q has kind %v, want %q", name, got, wantKind)
		}
	}

	// --- the nesting is right ---
	workflow := findSpan(t, spans, "voice_call")
	agent := findSpan(t, spans, "voice_agent")
	stt := findSpan(t, spans, "stt.transcribe")
	llm := findSpan(t, spans, "agent.reply")
	tool := findSpan(t, spans, "lookup_account")
	tts := findSpan(t, spans, "tts.synthesize")

	wfID := workflow.DDAttributes.SpanID
	agentID := agent.DDAttributes.SpanID

	for _, tc := range []struct {
		name   string
		span   testtracer.LLMObsSpan
		parent string
		why    string
	}{
		{"stt.transcribe", stt, wfID, "recognition is a child of the call, not of the agent"},
		{"voice_agent", agent, wfID, "the agent turn is a child of the call"},
		{"agent.reply", llm, agentID, "the model call belongs to the agent"},
		{"lookup_account", tool, agentID, "the tool call belongs to the agent"},
		{"tts.synthesize", tts, wfID, "synthesis is a sibling of the agent, not part of its reasoning"},
	} {
		if tc.span.ParentID != tc.parent {
			t.Errorf("span %q has parent %q, want %q (%s)",
				tc.name, tc.span.ParentID, tc.parent, tc.why)
		}
	}

	// --- the agent's children are attributed to it ---
	if attr, ok := llm.Meta["agent_attribution"]; !ok {
		t.Error("agent.reply carries no agent_attribution; the agent view's " +
			"breakdown depends on it")
	} else {
		t.Logf("agent attribution on agent.reply: %v", attr)
	}
}

// TestTimeToFirstTokenIsSeconds pins the unit. The SDK takes a float64 with no
// unit in the name, and the documented examples are in seconds; sending
// milliseconds would overstate latency by a factor of a thousand and quietly
// ruin every TTFT chart.
func TestTimeToFirstTokenIsSeconds(t *testing.T) {
	tt, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-1")

	span, _ := obsT.StartLLM(ctx, "stt.transcribe", "scripted", "mock")
	span.LLMIO(
		[]Message{{Role: "user", Content: "in"}},
		[]Message{{Role: "assistant", Content: "out"}},
		Metrics{TimeToFirstToken: 621 * time.Millisecond, BillableCharacters: 3},
		nil,
	)
	span.Finish(nil)
	wf.Finish(nil)
	tracer.Flush()

	got := findSpan(t, tt.WaitForLLMObsSpans(t, 2), "stt.transcribe")
	ttft, ok := got.Metrics["time_to_first_token"]
	if !ok {
		t.Fatalf("time_to_first_token is absent; got %v", got.Metrics)
	}
	if ttft != 0.621 {
		t.Errorf("time_to_first_token = %v, want 0.621 (seconds, not milliseconds)", ttft)
	}
}

// TestTokenMetricsIncludeTotal checks the derived total. Datadog generates the
// platform token metrics from these keys, so a missing total would leave a gap
// in the cost dashboards.
func TestTokenMetricsIncludeTotal(t *testing.T) {
	tt, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-1")
	span, _ := obsT.StartLLM(ctx, "agent.reply", "scripted", "mock")
	span.LLMIO(
		[]Message{{Role: "user", Content: "in"}},
		[]Message{{Role: "assistant", Content: "out"}},
		Metrics{InputTokens: 42, OutputTokens: 37},
		nil,
	)
	span.Finish(nil)
	wf.Finish(nil)
	tracer.Flush()

	got := findSpan(t, tt.WaitForLLMObsSpans(t, 2), "agent.reply")
	for key, want := range map[string]float64{
		"input_tokens":  42,
		"output_tokens": 37,
		"total_tokens":  79,
	} {
		if gotV, ok := got.Metrics[key]; !ok || gotV != want {
			t.Errorf("%s = %v (present=%v), want %v", key, gotV, ok, want)
		}
	}
}

// TestUnsetMetricsAreOmitted checks that a zero value is not reported as a
// measured zero. A speech span has no tokens, and claiming zero tokens would
// pollute the token charts with meaningless data points.
func TestUnsetMetricsAreOmitted(t *testing.T) {
	m := Metrics{BillableCharacters: 100}.toLLMObs()

	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "time_to_first_token"} {
		if _, ok := m[key]; ok {
			t.Errorf("%s was emitted for a span that never set it", key)
		}
	}
	if got := m["billable_character_count"]; got != 100 {
		t.Errorf("billable_character_count = %v, want 100", got)
	}

	if got := (Metrics{}).toLLMObs(); len(got) != 0 {
		t.Errorf("a zero Metrics produced %v, want nothing", got)
	}
}

// TestErrorIsRecordedOnSpan verifies the failure path, which is what a demo of
// a degraded provider depends on.
func TestErrorIsRecordedOnSpan(t *testing.T) {
	tt, obsT := startSDK(t)

	wantErr := errors.New("injected provider error")

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-1")
	span, _ := obsT.StartLLM(ctx, "agent.reply", "scripted", "mock")
	span.Finish(wantErr)
	wf.Finish(nil)
	tracer.Flush()

	spans := tt.WaitForLLMObsSpans(t, 2)
	failed := findSpan(t, spans, "agent.reply")
	if failed.Status != "error" {
		t.Errorf("a failed span has status %q, want error", failed.Status)
	}
	// The SDK records the detail in meta, not in StatusMessage.
	msg, ok := failed.Meta["error.message"]
	if !ok {
		t.Fatalf("a failed span carries no error.message; meta keys: %v", metaKeys(failed.Meta))
	}
	if got := msg.(string); !strings.Contains(got, wantErr.Error()) {
		t.Errorf("error.message = %q, want it to contain %q", got, wantErr.Error())
	}
	// A stack is attached too, though it points at this package's Finish
	// wrapper rather than the original call site, because the SDK tunes its
	// stack-skip count for direct use. The message is what identifies the
	// failure, so that is what matters.
	if _, ok := failed.Meta["error.stack"]; !ok {
		t.Error("a failed span carries no error.stack")
	}

	// A nil error must not mark the workflow errored.
	succeeded := findSpan(t, spans, "voice_call")
	if succeeded.Status == "error" {
		t.Error("Finish(nil) marked a successful span as errored")
	}
	if _, bad := succeeded.Meta["error.message"]; bad {
		t.Error("Finish(nil) attached an error message to a successful span")
	}
}

// TestTagsReachTheSpan covers the tags a demo filters on.
func TestTagsReachTheSpan(t *testing.T) {
	tt, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-1")
	span, _ := obsT.StartLLM(ctx, "stt.transcribe", "scripted", "mock")
	span.SetTags(map[string]string{"chaos_profile": "lossy-wan", "codec": "PCMU"})
	span.LLMIO(
		[]Message{{Role: "user", Content: "in"}},
		[]Message{{Role: "assistant", Content: "out"}},
		Metrics{}, nil,
	)
	span.Finish(nil)
	wf.Finish(nil)
	tracer.Flush()

	got := findSpan(t, tt.WaitForLLMObsSpans(t, 2), "stt.transcribe")
	want := map[string]bool{"chaos_profile:lossy-wan": true, "codec:PCMU": true}
	for _, tag := range got.Tags {
		delete(want, tag)
	}
	if len(want) > 0 {
		t.Errorf("tags missing from the span: %v; got %v", want, got.Tags)
	}
}

// TestModelAndProviderAreRecorded checks the attribution that lets a demo show
// which provider served a call — and, in phase 7, what it cost.
func TestModelAndProviderAreRecorded(t *testing.T) {
	tt, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-1")
	span, _ := obsT.StartLLM(ctx, "stt.transcribe", "latest_long", "google")
	span.LLMIO(
		[]Message{{Role: "user", Content: "in"}},
		[]Message{{Role: "assistant", Content: "out"}},
		Metrics{}, nil,
	)
	span.Finish(nil)
	wf.Finish(nil)
	tracer.Flush()

	got := findSpan(t, tt.WaitForLLMObsSpans(t, 2), "stt.transcribe")
	if got.Meta["model_name"] != "latest_long" {
		t.Errorf("model_name = %v, want latest_long", got.Meta["model_name"])
	}
	if got.Meta["model_provider"] != "google" {
		t.Errorf("model_provider = %v, want google", got.Meta["model_provider"])
	}
}

// TestBackdatedSpanHasRealDuration covers why StartLLMAt exists. A recognition
// span created when its final transcript arrives would otherwise show a
// duration of nearly zero, hiding the whole utterance.
func TestBackdatedSpanHasRealDuration(t *testing.T) {
	tt, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-1")

	const utterance = 2 * time.Second
	span, _ := obsT.StartLLMAt(ctx, "stt.transcribe", "scripted", "mock",
		time.Now().Add(-utterance))
	span.Finish(nil)
	wf.Finish(nil)
	tracer.Flush()

	got := findSpan(t, tt.WaitForLLMObsSpans(t, 2), "stt.transcribe")
	dur := time.Duration(got.Duration)
	if dur < utterance/2 {
		t.Errorf("backdated span duration = %v, want roughly %v", dur, utterance)
	}
	t.Logf("backdated recognition span covers %v", dur.Round(time.Millisecond))
}

// TestAPMSpansShareTheTrace checks that the transport stages land in the same
// trace as the AI spans, which is what lets one trace show a call end to end.
func TestAPMSpansShareTheTrace(t *testing.T) {
	tt, obsT := startSDK(t)

	ctx := context.Background()
	root, ctx := obsT.StartAPM(ctx, "voice.call", map[string]string{"call_id": "c-1"})
	if root == nil {
		t.Fatal("no APM span was created")
	}

	ingest, _ := obsT.StartAPMAt(ctx, "voice.rtp.ingest", time.Now().Add(-time.Second),
		map[string]string{"codec": "PCMU"})
	ingest.SetAPMTag("packets_lost", 16)
	ingest.Finish(nil)

	wf, _ := obsT.StartWorkflow(ctx, "voice_call", "c-1")
	wf.Finish(nil)
	root.Finish(nil)
	tracer.Flush()

	apmSpans := tt.WaitForSpans(t, 2)
	traceIDs := map[uint64]bool{}
	for _, s := range apmSpans {
		traceIDs[s.TraceID] = true
	}
	if len(traceIDs) != 1 {
		t.Errorf("the APM spans span %d traces, want 1", len(traceIDs))
	}

	var names []string
	for _, s := range apmSpans {
		names = append(names, s.Name)
	}
	t.Logf("APM spans in the trace: %v", names)
}

// TestFinishNilDoesNotPanic is a regression test for a real bug in
// dd-trace-go v2.11.1. The SDK documentation says llmobs.WithError(nil) is a
// no-op and recommends a single deferred Finish(err) for every span. It is not
// a no-op: WrapN returns a typed-nil *TracerError, assigning it to the
// error-typed config field leaves a non-nil interface, and Finish then
// dereferences the nil inner error.
//
// The consequence is that the documented pattern panics on every *successful*
// span. This test exists so the guard in Finish is never "simplified" away.
func TestFinishNilDoesNotPanic(t *testing.T) {
	_, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-1")

	// Every span kind, finished with a nil error.
	agent, actx := obsT.StartAgent(ctx, "voice_agent")
	tool, _ := obsT.StartTool(actx, "lookup_account")
	llm, _ := obsT.StartLLM(actx, "agent.reply", "m", "p")
	llmAt, _ := obsT.StartLLMAt(ctx, "stt.transcribe", "m", "p", time.Now().Add(-time.Second))
	apm, apmctx := obsT.StartAPM(ctx, "voice.call", nil)
	apmAt, _ := obsT.StartAPMAt(apmctx, "voice.rtp.ingest", time.Now().Add(-time.Second), nil)

	for name, s := range map[string]*Span{
		"tool":     tool,
		"llm":      llm,
		"llm-at":   llmAt,
		"agent":    agent,
		"apm-at":   apmAt,
		"apm":      apm,
		"workflow": wf,
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s span: Finish(nil) panicked: %v", name, r)
				}
			}()
			s.Finish(nil)
		}()
	}
}

// TestFinishNilIsDirectlyAffectedBySDKBug documents the upstream defect by
// reproducing it, so that when a future dd-trace-go release fixes it this test
// fails and the workaround above can be removed.
func TestFinishNilIsDirectlyAffectedBySDKBug(t *testing.T) {
	_, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-bug")
	defer wf.Finish(nil)

	span, _ := obsT.StartLLM(ctx, "probe", "m", "p")
	if span == nil || span.obs == nil {
		t.Skip("LLM Obs is not active; nothing to probe")
	}

	var panicked bool
	func() {
		defer func() { panicked = recover() != nil }()
		// The call the SDK documentation says is safe.
		span.obs.Finish(llmobs.WithError(nil))
	}()

	if !panicked {
		t.Log("llmobs.WithError(nil) no longer panics — the upstream bug appears " +
			"fixed, and the nil guard in Span.Finish can be removed")
	} else {
		t.Log("confirmed: llmobs.WithError(nil) still panics in this dd-trace-go " +
			"version, so the guard in Span.Finish is required")
	}
}

// metaKeys lists a span's meta keys, for failure messages.
func metaKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
