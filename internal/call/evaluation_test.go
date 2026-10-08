package call

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DataDog/dd-trace-go/v2/instrumentation/testutils/testtracer"

	"github.com/mharner33/voice-demo/internal/llm"
	"github.com/mharner33/voice-demo/internal/obs"
)

// This is the phase-6 acceptance test. Phase 5 proved the span tree reaches
// Datadog; this proves the three things phase 6 added reach it too — the tools
// offered, the cost of the turn, and the call's evaluations.

// TestPipelineAnnotatesToolDefinitions asserts that every model round records
// what the agent was allowed to do, not merely what it did.
func TestPipelineAnnotatesToolDefinitions(t *testing.T) {
	spans, _, _ := instrumentedRunWithTracer(t, 100, 0)

	rounds := spansNamed(spans, "agent.reply")
	if len(rounds) != 2 {
		t.Fatalf("got %d model rounds, want 2 (the request and the continuation)", len(rounds))
	}

	// Both rounds matter. The second one is the interesting case: the tool has
	// already run, and the trace should still show it was on offer.
	for i, s := range rounds {
		raw, ok := s.Meta["tool_definitions"]
		if !ok {
			t.Errorf("model round %d carries no tool_definitions", i)
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("re-encoding tool_definitions: %v", err)
		}
		var defs []struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		}
		if err := json.Unmarshal(encoded, &defs); err != nil {
			t.Fatalf("decoding tool_definitions %s: %v", encoded, err)
		}
		if len(defs) != 1 || defs[0].Name != llm.AccountLookupToolName {
			t.Errorf("model round %d tool definitions = %s, want one %q",
				i, encoded, llm.AccountLookupToolName)
		}
		if len(defs) > 0 && len(defs[0].Schema) == 0 {
			t.Errorf("model round %d recorded the tool with no schema", i)
		}
	}
}

// TestMockCallReportsNoCost covers the default demo configuration. The mock
// agent is free, so the cost metrics must be absent rather than zero — a zero
// would pull down any average taken across real and mock calls alike.
func TestMockCallReportsNoCost(t *testing.T) {
	spans, res, _ := instrumentedRunWithTracer(t, 100, 0)

	if !res.LLMInfo.Pricing.Free() {
		t.Fatalf("the mock agent reported a price list: %+v", res.LLMInfo.Pricing)
	}
	if got := res.CostUSD(); got != 0 {
		t.Errorf("Result.CostUSD() = %g on the mock agent, want 0", got)
	}

	for i, s := range spansNamed(spans, "agent.reply") {
		for _, key := range []string{"input_cost", "output_cost"} {
			if v, ok := s.Metrics[key]; ok {
				t.Errorf("model round %d reported %s = %g for a free provider", i, key, v)
			}
		}
	}
}

// TestPricedProviderReportsCost is the other half: with a price list in place,
// the cost reaches the span and matches the tokens the turn actually used.
func TestPricedProviderReportsCost(t *testing.T) {
	// The arithmetic is checked directly rather than by standing up a priced
	// provider, since Pricing is the single place the conversion happens.
	p := llm.Pricing{InputPerMTok: 5, OutputPerMTok: 25}

	in, out := p.Cost(1_000_000, 1_000_000)
	if in != 5 || out != 25 {
		t.Errorf("Cost(1M,1M) = %g/%g, want 5/25", in, out)
	}

	in, out = p.Cost(2_000, 500)
	if want := 0.01; in != want {
		t.Errorf("input cost for 2000 tokens = %g, want %g", in, want)
	}
	if want := 0.0125; out != want {
		t.Errorf("output cost for 500 tokens = %g, want %g", out, want)
	}

	if !(llm.Pricing{}).Free() {
		t.Error("an empty price list does not report itself as free")
	}
	if p.Free() {
		t.Error("a real price list reported itself as free")
	}
}

// TestPipelineSubmitsCallEvaluations is the headline phase-6 assertion: one
// call populates the Evaluations view with all three judgements, joined to the
// call's own workflow span.
func TestPipelineSubmitsCallEvaluations(t *testing.T) {
	spans, res, tt := instrumentedRunWithTracer(t, 100, 0)

	wf := findSpan(t, spans, "voice_call")

	metrics := tt.WaitForLLMObsMetrics(t, 3)
	byLabel := make(map[string]testtracer.LLMObsMetric, len(metrics))
	for _, m := range metrics {
		byLabel[m.Label] = m
	}

	for _, label := range []string{
		obs.EvalTranscriptConfidence, obs.EvalAudioQuality, obs.EvalReplied,
	} {
		m, ok := byLabel[label]
		if !ok {
			var got []string
			for _, x := range metrics {
				got = append(got, x.Label)
			}
			t.Errorf("no %q evaluation was submitted; got %v", label, got)
			continue
		}
		// Each evaluation must hang off this call's workflow span, or it would
		// not show up against the call in the view.
		if m.JoinOn.Span == nil {
			t.Errorf("%q was submitted with no span to join on", label)
			continue
		}
		if m.JoinOn.Span.SpanID != wf.SpanID {
			t.Errorf("%q joined to span %q, want the call's workflow span %q",
				label, m.JoinOn.Span.SpanID, wf.SpanID)
		}
	}

	// The confidence evaluation must carry the pipeline's own figure, not a
	// placeholder, since that is the number the demo claims moves with loss.
	if score := byLabel[obs.EvalTranscriptConfidence].ScoreValue; score == nil {
		t.Error("the confidence evaluation has no score")
	} else if *score != res.MeanConfidence() {
		t.Errorf("submitted confidence %g, but the call measured %g",
			*score, res.MeanConfidence())
	}

	if replied := byLabel[obs.EvalReplied].BooleanValue; replied == nil || !*replied {
		t.Errorf("replied_every_turn = %v on a call where every turn replied", replied)
	}

	// A clean run must be labeled clean, or the categorical bucket says nothing.
	if q := byLabel[obs.EvalAudioQuality].CategoricalValue; q == nil {
		t.Error("the audio quality evaluation has no value")
	} else if *q != obs.AudioQualityClean {
		t.Errorf("audio_quality = %q on a lossless run, want %q", *q, obs.AudioQualityClean)
	}
}

// TestConcealedAudioDegradesTheEvaluation is the test that makes the demo's
// central claim falsifiable: packet loss has to move an AI-quality number. If
// this ever passes trivially — identical confidence on clean and lossy audio —
// the thing the dashboards are built to show is not happening.
func TestConcealedAudioDegradesTheEvaluation(t *testing.T) {
	_, clean, _ := instrumentedRunWithTracer(t, 100, 0)
	_, lossy, tt := instrumentedRunWithTracer(t, 100, 4) // every 4th frame concealed

	if lossy.ConcealedFramesIn == 0 {
		t.Fatal("the lossy run concealed no frames, so there is nothing to measure")
	}

	if lossy.MeanConfidence() >= clean.MeanConfidence() {
		t.Errorf("confidence did not degrade: clean %.3f, lossy %.3f",
			clean.MeanConfidence(), lossy.MeanConfidence())
	}

	// And the degradation has to be the figure that was actually submitted.
	metrics := tt.WaitForLLMObsMetrics(t, 3)
	var submitted *float64
	var bucket *string
	for _, m := range metrics {
		switch m.Label {
		case obs.EvalTranscriptConfidence:
			submitted = m.ScoreValue
		case obs.EvalAudioQuality:
			bucket = m.CategoricalValue
		}
	}
	if submitted == nil {
		t.Fatal("the lossy run submitted no confidence evaluation")
	}
	if *submitted != lossy.MeanConfidence() {
		t.Errorf("submitted confidence %g, but the call measured %g",
			*submitted, lossy.MeanConfidence())
	}
	if bucket == nil {
		t.Fatal("the lossy run submitted no audio quality evaluation")
	}
	if *bucket == obs.AudioQualityClean {
		t.Errorf("audio_quality = %q with %.1f%% of frames concealed",
			*bucket, lossy.ConcealedFraction()*100)
	}

	t.Logf("clean confidence %.3f, lossy confidence %.3f at %.1f%% concealed (%s)",
		clean.MeanConfidence(), lossy.MeanConfidence(),
		lossy.ConcealedFraction()*100, *bucket)
}

// TestEvaluationsSurviveADisabledTracer makes sure the submission path is safe
// when telemetry is off, which is the default for `make test` and for a demo
// run with no agent present. An evaluation is addressed by span ID, so with no
// spans there is nothing to join to and the submission has to be dropped.
func TestEvaluationsSurviveADisabledTracer(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	for _, tc := range []struct {
		name string
		obs  *obs.Tracer
	}{
		{"nil tracer", nil},
		{"disabled tracer", mustStartForTest(t, obs.Config{Enabled: false})},
		{"telemetry on, llm obs off", mustStartForTest(t, obs.Config{
			Enabled: true, Service: "voicegw", Env: "test", LLMObsEnabled: false,
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(t, Config{
				CallID: "c-" + tc.name, STT: transcriber, LLM: agent, TTS: synth,
				Tools: reg, Obs: tc.obs,
			})
			res, err := s.Run(context.Background(), audioChan(100, 0))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(res.Turns) == 0 {
				t.Error("the call produced no turns")
			}
		})
	}
}

func mustStartForTest(t *testing.T, cfg obs.Config) *obs.Tracer {
	t.Helper()
	tr, err := obs.StartForTest(cfg)
	if err != nil {
		t.Fatalf("obs.StartForTest: %v", err)
	}
	return tr
}

// TestResultEvaluationInputs covers the derived figures directly, including the
// empty-call case that the pipeline test cannot easily produce.
func TestResultEvaluationInputs(t *testing.T) {
	var empty Result
	if got := empty.MeanConfidence(); got != 0 {
		t.Errorf("MeanConfidence on a call with no turns = %g, want 0", got)
	}
	if !empty.RepliedEveryTurn() {
		t.Error("a call with no turns should not count as having failed to reply")
	}

	r := Result{Turns: []Turn{
		{Confidence: 0.9, Reply: "one"},
		{Confidence: 0.7, Reply: "two"},
	}}
	if got, want := r.MeanConfidence(), 0.8; got != want {
		t.Errorf("MeanConfidence = %g, want %g", got, want)
	}
	if !r.RepliedEveryTurn() {
		t.Error("RepliedEveryTurn = false when both turns replied")
	}

	silent := Result{Turns: []Turn{{Confidence: 0.9, Reply: "one"}, {Confidence: 0.9}}}
	if silent.RepliedEveryTurn() {
		t.Error("RepliedEveryTurn = true despite a turn with no reply")
	}
}
