package obs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DataDog/dd-trace-go/v2/instrumentation/testutils/testtracer"
)

// These tests cover the phase-6 additions: tool definitions and cost on the
// LLM span, and the evaluations that populate the Evaluations view. Like the
// rest of this package's tests they run the real SDK against a capturing fake
// agent, because the thing worth verifying is the payload Datadog receives.

const accountSchema = `{"type":"object","properties":{"account_id":{"type":"string"}},"required":["account_id"]}`

// TestToolDefinitionsReachTheSpan covers the annotation that makes a
// tool-capable turn interpretable. Without it, a turn where the model decided
// no tool was needed looks exactly like a turn with no tools available.
func TestToolDefinitionsReachTheSpan(t *testing.T) {
	tt, obsT := startSDK(t)

	span, _ := obsT.StartLLM(context.Background(), "agent.reply", "claude-opus-5", "anthropic")
	span.SetToolDefinitions([]ToolDefinition{{
		Name:        "lookup_account",
		Description: "Look up a customer account by its identifier.",
		Schema:      []byte(accountSchema),
	}})
	span.LLMIO(
		[]Message{{Role: "user", Content: "what is my balance"}},
		[]Message{{Role: "assistant", Content: "Let me check."}},
		Metrics{InputTokens: 120, OutputTokens: 14},
		nil,
	)
	span.Finish(nil)

	spans := tt.WaitForLLMObsSpans(t, 1)
	got := findSpan(t, spans, "agent.reply")

	raw, ok := got.Meta["tool_definitions"]
	if !ok {
		keys := make([]string, 0, len(got.Meta))
		for k := range got.Meta {
			keys = append(keys, k)
		}
		t.Fatalf("no tool_definitions in span meta; keys were %v", keys)
	}

	// The payload round-trips through JSON on the way to the agent, so it comes
	// back as generic maps rather than the SDK's struct.
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encoding tool_definitions: %v", err)
	}
	var defs []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(encoded, &defs); err != nil {
		t.Fatalf("decoding tool_definitions %s: %v", encoded, err)
	}

	if len(defs) != 1 {
		t.Fatalf("got %d tool definitions, want 1", len(defs))
	}
	if defs[0].Name != "lookup_account" {
		t.Errorf("name = %q", defs[0].Name)
	}
	if defs[0].Description == "" {
		t.Error("the tool definition reached the span with no description")
	}
	// The schema is what tells the model what argument to supply, so an empty
	// one on the span means the trace cannot explain a malformed tool call.
	if len(defs[0].Schema) == 0 {
		t.Error("the tool definition reached the span with no schema")
	}
}

// TestToolDefinitionsAreOnlyValidOnLLMSpans pins an SDK restriction that is
// easy to trip over: tool definitions are accepted on LLM spans and silently
// dropped (with a log warning) on every other kind.
//
// This is why the pipeline annotates them on the per-round agent.reply span
// rather than on the agent span that wraps the turn, which would read as the
// more natural home for them.
func TestToolDefinitionsAreOnlyValidOnLLMSpans(t *testing.T) {
	tt, obsT := startSDK(t)

	ctx := context.Background()
	wf, ctx := obsT.StartWorkflow(ctx, "voice_call", "c-tooldefs")
	wf.SetToolDefinitions([]ToolDefinition{{Name: "lookup_account", Schema: []byte(accountSchema)}})
	wf.TextIO("in", "out", nil)

	agent, actx := obsT.StartAgent(ctx, "voice_agent")
	agent.SetToolDefinitions([]ToolDefinition{{Name: "lookup_account", Schema: []byte(accountSchema)}})
	agent.TextIO("in", "out", nil)

	llm, _ := obsT.StartLLM(actx, "agent.reply", "claude-opus-5", "anthropic")
	llm.SetToolDefinitions([]ToolDefinition{{Name: "lookup_account", Schema: []byte(accountSchema)}})
	llm.LLMIO(nil, nil, Metrics{InputTokens: 1, OutputTokens: 1}, nil)
	llm.Finish(nil)
	agent.Finish(nil)
	wf.Finish(nil)

	spans := tt.WaitForLLMObsSpans(t, 3)

	for _, name := range []string{"voice_call", "voice_agent"} {
		s := findSpan(t, spans, name)
		if _, ok := s.Meta["tool_definitions"]; ok {
			t.Errorf("%s (a non-LLM span) carries tool_definitions; the SDK is documented "+
				"to drop them, so this test should be revisited along with where the "+
				"pipeline annotates them", name)
		}
	}
	if _, ok := findSpan(t, spans, "agent.reply").Meta["tool_definitions"]; !ok {
		t.Error("the LLM span is missing tool_definitions")
	}
}

// TestCostMetricsUseTheExpectedKeys pins the two custom metric names. They have
// no SDK constants, so a typo would be invisible: the span would submit fine
// and the cost views would simply stay empty.
func TestCostMetricsUseTheExpectedKeys(t *testing.T) {
	tt, obsT := startSDK(t)

	span, _ := obsT.StartLLM(context.Background(), "agent.reply", "claude-opus-5", "anthropic")
	span.LLMIO(nil, nil, Metrics{
		InputTokens:   1000,
		OutputTokens:  200,
		InputCostUSD:  0.005,
		OutputCostUSD: 0.005,
	}, nil)
	span.Finish(nil)

	got := findSpan(t, tt.WaitForLLMObsSpans(t, 1), "agent.reply")

	for key, want := range map[string]float64{
		"input_cost":  0.005,
		"output_cost": 0.005,
	} {
		v, ok := got.Metrics[key]
		if !ok {
			t.Errorf("metric %q is missing; the cost views would be empty", key)
			continue
		}
		if v != want {
			t.Errorf("metric %q = %g, want %g", key, v, want)
		}
	}
}

// TestZeroCostIsOmitted covers the mock provider's case. A free provider must
// report no cost rather than a cost of zero, since a zero would drag down any
// average computed over a mix of real and mock calls.
func TestZeroCostIsOmitted(t *testing.T) {
	tt, obsT := startSDK(t)

	span, _ := obsT.StartLLM(context.Background(), "agent.reply", "scripted", "mock")
	span.LLMIO(nil, nil, Metrics{InputTokens: 10, OutputTokens: 4}, nil)
	span.Finish(nil)

	got := findSpan(t, tt.WaitForLLMObsSpans(t, 1), "agent.reply")
	for _, key := range []string{"input_cost", "output_cost"} {
		if v, ok := got.Metrics[key]; ok {
			t.Errorf("metric %q = %g on a free provider, want it omitted", key, v)
		}
	}
}

// TestEvaluationsAreSubmitted is the acceptance test for the Evaluations view.
// It asserts all three value kinds reach the intake joined to the right span.
func TestEvaluationsAreSubmitted(t *testing.T) {
	tt, obsT := startSDK(t)

	wf, _ := obsT.StartWorkflow(context.Background(), "voice_call", "c-eval")
	if wf == nil {
		t.Fatal("no workflow span was created")
	}
	wf.TextIO("in", "out", nil)

	tags := map[string]string{"codec": "PCMU", "provider": "anthropic"}
	obsT.EvalScore(wf, EvalTranscriptConfidence, 0.87, tags)
	obsT.EvalCategorical(wf, EvalAudioQuality, AudioQualityDegraded, tags)
	obsT.EvalBool(wf, EvalReplied, true, tags)

	// The span ID is captured before Finish so the join can be checked.
	wantSpanID := wf.obs.SpanID()
	wf.Finish(nil)

	metrics := tt.WaitForLLMObsMetrics(t, 3)
	byLabel := make(map[string]testtracer.LLMObsMetric, len(metrics))
	for _, m := range metrics {
		byLabel[m.Label] = m
	}

	score, ok := byLabel[EvalTranscriptConfidence]
	if !ok {
		t.Fatalf("no %q evaluation; got %v", EvalTranscriptConfidence, labelsOf(metrics))
	}
	if score.ScoreValue == nil {
		t.Errorf("%s was not submitted as a score: %+v", EvalTranscriptConfidence, score)
	} else if *score.ScoreValue != 0.87 {
		t.Errorf("%s = %g, want 0.87", EvalTranscriptConfidence, *score.ScoreValue)
	}

	// The evaluation is useless unless it is joined to the call's own span.
	if score.JoinOn.Span == nil {
		t.Fatalf("%s was submitted without a span to join on", EvalTranscriptConfidence)
	}
	if score.JoinOn.Span.SpanID != wantSpanID {
		t.Errorf("joined to span %q, want the workflow span %q",
			score.JoinOn.Span.SpanID, wantSpanID)
	}

	quality, ok := byLabel[EvalAudioQuality]
	if !ok {
		t.Fatalf("no %q evaluation", EvalAudioQuality)
	}
	if quality.CategoricalValue == nil {
		t.Errorf("%s was not submitted as a categorical value: %+v", EvalAudioQuality, quality)
	} else if *quality.CategoricalValue != AudioQualityDegraded {
		t.Errorf("%s = %q, want %q", EvalAudioQuality, *quality.CategoricalValue, AudioQualityDegraded)
	}

	replied, ok := byLabel[EvalReplied]
	if !ok {
		t.Fatalf("no %q evaluation", EvalReplied)
	}
	if replied.BooleanValue == nil {
		t.Errorf("%s was not submitted as a boolean: %+v", EvalReplied, replied)
	} else if !*replied.BooleanValue {
		t.Errorf("%s = false, want true", EvalReplied)
	}

	// Tags are what let the Evaluations view split confidence by codec or by
	// provider, which is the comparison the demo exists to show.
	wantTags := map[string]bool{"codec:PCMU": true, "provider:anthropic": true}
	for _, m := range metrics {
		for want := range wantTags {
			if !containsString(m.Tags, want) {
				t.Errorf("evaluation %q is missing tag %q; tags were %v", m.Label, want, m.Tags)
			}
		}
	}
}

func labelsOf(metrics []testtracer.LLMObsMetric) []string {
	out := make([]string, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, m.Label)
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestFalseEvaluationIsStillSubmitted guards against the easy mistake of
// treating a false boolean as "nothing to report". A call where the agent fell
// silent is exactly the one worth seeing.
func TestFalseEvaluationIsStillSubmitted(t *testing.T) {
	tt, obsT := startSDK(t)

	wf, _ := obsT.StartWorkflow(context.Background(), "voice_call", "c-false")
	wf.TextIO("in", "", nil)
	obsT.EvalBool(wf, EvalReplied, false, nil)
	obsT.EvalScore(wf, EvalTranscriptConfidence, 0, nil)
	wf.Finish(nil)

	metrics := tt.WaitForLLMObsMetrics(t, 2)
	if len(metrics) < 2 {
		t.Fatalf("got %d evaluations, want 2: %v", len(metrics), labelsOf(metrics))
	}
	for _, m := range metrics {
		switch m.Label {
		case EvalReplied:
			if m.BooleanValue == nil || *m.BooleanValue {
				t.Errorf("%s = %+v, want an explicit false", EvalReplied, m.BooleanValue)
			}
		case EvalTranscriptConfidence:
			if m.ScoreValue == nil || *m.ScoreValue != 0 {
				t.Errorf("%s = %+v, want an explicit zero", EvalTranscriptConfidence, m.ScoreValue)
			}
		}
	}
}

// TestEvaluationOnMissingSpanIsSafe covers the disabled-telemetry path. An
// evaluation is addressed by span ID, so with no span there is nothing to
// attach to and the submission must be dropped rather than panicking.
func TestEvaluationOnMissingSpanIsSafe(t *testing.T) {
	var disabled *Tracer
	disabled.EvalScore(nil, EvalTranscriptConfidence, 0.5, nil)
	disabled.EvalCategorical(nil, EvalAudioQuality, AudioQualityClean, nil)
	disabled.EvalBool(nil, EvalReplied, true, nil)

	// A tracer with telemetry off, and a span that was therefore never created.
	off, err := StartForTest(Config{Enabled: false})
	if err != nil {
		t.Fatalf("StartForTest: %v", err)
	}
	span, _ := off.StartWorkflow(context.Background(), "voice_call", "c-off")
	off.EvalScore(span, EvalTranscriptConfidence, 0.5, nil)
	off.EvalBool(span, EvalReplied, true, nil)
}

// TestEmptyCategoricalIsNotSubmitted avoids writing a blank bucket into the
// view, which would show up as its own category.
func TestEmptyCategoricalIsNotSubmitted(t *testing.T) {
	tt, obsT := startSDK(t)

	wf, _ := obsT.StartWorkflow(context.Background(), "voice_call", "c-empty")
	wf.TextIO("in", "out", nil)
	obsT.EvalCategorical(wf, EvalAudioQuality, "", nil)
	obsT.EvalBool(wf, EvalReplied, true, nil)
	wf.Finish(nil)

	metrics := tt.WaitForLLMObsMetrics(t, 1)
	for _, m := range metrics {
		if m.Label == EvalAudioQuality {
			t.Errorf("an empty categorical value was submitted: %+v", m)
		}
	}
}

// TestAudioQualityBucketThresholds pins the boundaries, since the bucket label
// and the confidence score have to tell the same story about a call.
func TestAudioQualityBucketThresholds(t *testing.T) {
	cases := []struct {
		concealed float64
		want      string
	}{
		{0, AudioQualityClean},
		{0.02, AudioQualityClean}, // exactly at the boundary is still clean
		{0.0201, AudioQualityDegraded},
		{0.05, AudioQualityDegraded}, // the lossy-wan profile lands here
		{0.10, AudioQualityDegraded}, // boundary
		{0.1001, AudioQualityPoor},
		{0.5, AudioQualityPoor},
	}
	for _, tc := range cases {
		if got := AudioQualityBucket(tc.concealed); got != tc.want {
			t.Errorf("AudioQualityBucket(%g) = %q, want %q", tc.concealed, got, tc.want)
		}
	}
}

// TestEvalTagsAreStable makes the rendered tag list deterministic, so two calls
// with the same tags are comparable rather than differing by map order.
func TestEvalTagsAreStable(t *testing.T) {
	in := map[string]string{"provider": "anthropic", "codec": "PCMU", "model": "claude-opus-5"}
	want := []string{"codec:PCMU", "model:claude-opus-5", "provider:anthropic"}

	for i := 0; i < 20; i++ {
		got := evalTags(in)
		if len(got) != len(want) {
			t.Fatalf("evalTags = %v, want %v", got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("evalTags = %v, want %v (sorted)", got, want)
			}
		}
	}

	if evalTags(nil) != nil {
		t.Error("evalTags(nil) should produce no tags")
	}
}
