package call

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/instrumentation/testutils/testtracer"

	"github.com/mharner33/voice-demo/internal/llm"
	"github.com/mharner33/voice-demo/internal/obs"
	"github.com/mharner33/voice-demo/internal/stt"
	"github.com/mharner33/voice-demo/internal/tts"
)

// This is the phase-5 acceptance test. It runs the real pipeline with the real
// Agent Observability SDK pointed at a capturing fake agent, then asserts the
// spans Datadog would actually receive. Asserting on the pipeline's own Result
// would prove the pipeline works, which phase 4 already covers; this proves the
// instrumentation works.

// instrumentedRun drives one call through the pipeline with telemetry on and
// returns the captured LLM Obs spans.
func instrumentedRun(t *testing.T, frames int, concealedEvery int) ([]testtracer.LLMObsSpan, Result) {
	t.Helper()

	tt := testtracer.Start(t, testtracer.WithTracerStartOpts(
		tracer.WithService("voicegw"),
		tracer.WithEnv("test"),
		tracer.WithLLMObsEnabled(true),
		tracer.WithLLMObsMLApp("voice-demo"),
	))
	t.Cleanup(tt.Stop)

	obsT, err := obs.StartForTest(obs.Config{
		Enabled: true, Service: "voicegw", Env: "test",
		LLMObsEnabled: true, MLApp: "voice-demo",
	})
	if err != nil {
		t.Fatalf("obs.StartForTest: %v", err)
	}

	// One utterance that triggers the account lookup, so the trace carries an
	// agent span with a nested tool call rather than a bare completion.
	transcriber, err := stt.NewMock(stt.MockConfig{
		Phrases:            []string{"hello I'm calling about my account balance"},
		FramesPerPartial:   25,
		FramesPerUtterance: 100,
		DegradeOnConcealed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := llm.DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := llm.NewMock(llm.MockConfig{Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	synth, err := tts.NewMock(tts.MockConfig{})
	if err != nil {
		t.Fatal(err)
	}

	s, err := New(Config{
		CallID: "c-instrumented",
		STT:    transcriber,
		LLM:    agent,
		TTS:    synth,
		Tools:  reg,
		Obs:    obsT,
		Tags: obs.CallTags{
			CallID: "c-instrumented", Codec: "PCMU", ProviderProfile: "provider-degraded",
			STTProvider: "mock", LLMProvider: "mock", TTSProvider: "mock",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Run(context.Background(), audioChan(frames, concealedEvery))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tracer.Flush()

	// workflow + stt + agent + 2 model rounds + tool + tts = 7
	return tt.WaitForLLMObsSpans(t, 7), res
}

func findSpan(t *testing.T, spans []testtracer.LLMObsSpan, name string) testtracer.LLMObsSpan {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	var names []string
	for _, s := range spans {
		names = append(names, s.Name)
	}
	t.Fatalf("no span named %q; got %v", name, names)
	return testtracer.LLMObsSpan{}
}

func spansNamed(spans []testtracer.LLMObsSpan, name string) []testtracer.LLMObsSpan {
	var out []testtracer.LLMObsSpan
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// TestPipelineEmitsExpectedSpanTree is the headline assertion: one real call
// produces the documented span shape, grouped into one session, all on one APM
// trace.
func TestPipelineEmitsExpectedSpanTree(t *testing.T) {
	spans, res := instrumentedRun(t, 100, 4)

	if len(res.Turns) != 1 {
		t.Fatalf("the call produced %d turns, want 1", len(res.Turns))
	}

	// --- one session per call ---
	for _, s := range spans {
		if s.SessionID != "c-instrumented" {
			t.Errorf("span %q has session_id %q, want the call ID", s.Name, s.SessionID)
		}
	}

	// --- one APM trace, which is the link the call log pivots on ---
	traceID := spans[0].DDAttributes.APMTraceID
	for _, s := range spans {
		if s.DDAttributes.APMTraceID != traceID {
			t.Errorf("span %q is on APM trace %q, want %q",
				s.Name, s.DDAttributes.APMTraceID, traceID)
		}
	}
	if res.TraceID != traceID {
		t.Errorf("Result.TraceID = %q but the spans are on %q; the call log "+
			"would point at the wrong trace", res.TraceID, traceID)
	}

	// --- the shape ---
	workflow := findSpan(t, spans, "voice_call")
	stt := findSpan(t, spans, "stt.transcribe")
	agent := findSpan(t, spans, "voice_agent")
	tool := findSpan(t, spans, "lookup_account")
	tts := findSpan(t, spans, "tts.synthesize")
	rounds := spansNamed(spans, "agent.reply")

	if workflow.Meta["span.kind"] != "workflow" {
		t.Errorf("voice_call kind = %v, want workflow", workflow.Meta["span.kind"])
	}
	if agent.Meta["span.kind"] != "agent" {
		t.Errorf("voice_agent kind = %v, want agent", agent.Meta["span.kind"])
	}
	if tool.Meta["span.kind"] != "tool" {
		t.Errorf("lookup_account kind = %v, want tool", tool.Meta["span.kind"])
	}

	// A tool-calling turn really does call the model twice. Collapsing the two
	// rounds into one span would hide half the latency.
	if len(rounds) != 2 {
		t.Errorf("got %d agent.reply spans, want 2 (one to request the tool, "+
			"one to reply with its result)", len(rounds))
	}

	wfID := workflow.DDAttributes.SpanID
	agentID := agent.DDAttributes.SpanID

	if stt.ParentID != wfID {
		t.Errorf("stt.transcribe parent = %q, want the workflow %q", stt.ParentID, wfID)
	}
	if agent.ParentID != wfID {
		t.Errorf("voice_agent parent = %q, want the workflow %q", agent.ParentID, wfID)
	}
	if tts.ParentID != wfID {
		t.Errorf("tts.synthesize parent = %q, want the workflow %q — synthesis is "+
			"a sibling of the agent, not part of its reasoning", tts.ParentID, wfID)
	}
	if tool.ParentID != agentID {
		t.Errorf("lookup_account parent = %q, want the agent %q", tool.ParentID, agentID)
	}
	for i, r := range rounds {
		if r.ParentID != agentID {
			t.Errorf("agent.reply[%d] parent = %q, want the agent %q", i, r.ParentID, agentID)
		}
	}

	var names []string
	for _, s := range spans {
		names = append(names, s.Name)
	}
	t.Logf("captured span tree: %v", names)
}

// TestRecognitionSpanCarriesAudioAndConfidence checks the span that connects the
// two halves of the demo. The recognition span has to say how degraded its
// input was, or there is nothing linking a poor transcript to a lossy network.
func TestRecognitionSpanCarriesAudioAndConfidence(t *testing.T) {
	spans, res := instrumentedRun(t, 100, 4) // every 4th frame concealed

	stt := findSpan(t, spans, "stt.transcribe")

	// Model attribution.
	if stt.Meta["model_provider"] != "mock" {
		t.Errorf("model_provider = %v, want mock", stt.Meta["model_provider"])
	}
	if stt.Meta["model_name"] != "scripted" {
		t.Errorf("model_name = %v, want scripted", stt.Meta["model_name"])
	}

	// The input describes the audio, since raw audio cannot go on a span.
	input, ok := stt.Meta["input"].(map[string]any)
	if !ok {
		t.Fatalf("the recognition span has no input: %v", stt.Meta)
	}
	msgs, ok := input["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("the recognition span input has no messages: %v", input)
	}
	content, _ := msgs[0].(map[string]any)["content"].(string)
	for _, want := range []string{"audio", "8000Hz", "concealed"} {
		if !strings.Contains(content, want) {
			t.Errorf("the audio descriptor %q does not mention %q", content, want)
		}
	}

	// Metadata carries the degradation and the confidence it caused.
	meta, ok := stt.Meta["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("the recognition span has no metadata: %v", stt.Meta)
	}
	conf, ok := meta["confidence"].(float64)
	if !ok {
		t.Fatalf("the recognition span records no confidence: %v", meta)
	}
	if conf >= 0.95 {
		t.Errorf("confidence = %v on audio that was 25%% concealed, want it degraded", conf)
	}
	if got, _ := meta["concealed"].(float64); got == 0 {
		t.Error("the recognition span reports no concealed frames despite degraded input")
	}

	// Cost is billable characters, not tokens: speech services bill that way.
	if _, ok := stt.Metrics["billable_character_count"]; !ok {
		t.Errorf("the recognition span has no billable_character_count: %v", stt.Metrics)
	}
	for _, tokenKey := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if _, bad := stt.Metrics[tokenKey]; bad {
			t.Errorf("the recognition span reports %s; speech is not token-billed", tokenKey)
		}
	}

	// Time to first partial is the recognizer's analogue of a first token.
	ttft, ok := stt.Metrics["time_to_first_token"]
	if !ok {
		t.Errorf("the recognition span has no time_to_first_token: %v", stt.Metrics)
	}
	// Seconds, so a sub-second value must be well under 1.
	if ttft >= 1 {
		t.Errorf("time_to_first_token = %v; a mock partial should be well under a "+
			"second, which suggests milliseconds were sent instead of seconds", ttft)
	}

	t.Logf("recognition span: confidence %.3f, ttft %.4fs, %v chars, %v concealed frames",
		conf, ttft, stt.Metrics["billable_character_count"], meta["concealed"])
	_ = res
}

// TestModelSpansCarryTokensAndToolCalls covers the cost and tool-call
// attribution that the Agent Observability views are built around.
func TestModelSpansCarryTokensAndToolCalls(t *testing.T) {
	spans, _ := instrumentedRun(t, 100, 0)

	rounds := spansNamed(spans, "agent.reply")
	if len(rounds) != 2 {
		t.Fatalf("got %d model rounds, want 2", len(rounds))
	}

	var sawToolRequest, sawTokens bool
	for _, r := range rounds {
		if _, ok := r.Metrics["total_tokens"]; ok {
			sawTokens = true
		}
		out, _ := r.Meta["output"].(map[string]any)
		msgs, _ := out["messages"].([]any)
		for _, m := range msgs {
			msg, _ := m.(map[string]any)
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				sawToolRequest = true
				first, _ := calls[0].(map[string]any)
				if first["name"] != llm.AccountLookupToolName {
					t.Errorf("the requested tool is %v, want %q",
						first["name"], llm.AccountLookupToolName)
				}
			}
		}
	}
	if !sawTokens {
		t.Error("no model round reported token counts; the cost views would be empty")
	}
	if !sawToolRequest {
		t.Error("no model round recorded the tool call it requested on its own output")
	}

	// The tool span records what ran and what came back.
	tool := findSpan(t, spans, "lookup_account")
	out, _ := tool.Meta["output"].(map[string]any)
	if got, _ := out["value"].(string); !strings.Contains(got, "Dana Okafor") {
		t.Errorf("the tool span output = %q, want the account data it returned", got)
	}
}

// TestSynthesisSpanCarriesFirstByte checks the figure a caller actually
// perceives as the agent's responsiveness.
func TestSynthesisSpanCarriesFirstByte(t *testing.T) {
	spans, _ := instrumentedRun(t, 100, 0)

	tts := findSpan(t, spans, "tts.synthesize")
	if tts.Meta["model_provider"] != "mock" {
		t.Errorf("model_provider = %v, want mock", tts.Meta["model_provider"])
	}
	if _, ok := tts.Metrics["billable_character_count"]; !ok {
		t.Errorf("the synthesis span has no billable_character_count: %v", tts.Metrics)
	}

	meta, _ := tts.Meta["metadata"].(map[string]any)
	if got, _ := meta["audio_duration"].(float64); got <= 0 {
		t.Errorf("the synthesis span reports audio_duration %v, want > 0", got)
	}
	if got, _ := meta["frames"].(float64); got <= 0 {
		t.Errorf("the synthesis span reports %v frames, want > 0", got)
	}
}

// TestTagsReachEverySpan covers the filtering a demo depends on: a viewer must
// be able to narrow the Agent Observability views to one chaos profile.
func TestTagsReachEverySpan(t *testing.T) {
	spans, _ := instrumentedRun(t, 100, 0)

	for _, s := range spans {
		var found bool
		for _, tag := range s.Tags {
			if tag == "ml_app:voice-demo" {
				found = true
			}
		}
		if !found {
			t.Errorf("span %q is not tagged with the ML app: %v", s.Name, s.Tags)
		}
	}
}

// TestDisabledTelemetryStillRunsTheCall is the other half of the contract: the
// demo has to work with no agent present, and the pipeline must produce the
// same result either way.
func TestDisabledTelemetryStillRunsTheCall(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	off, err := obs.Start(obs.Config{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	defer off.Stop()

	s := newSession(t, Config{
		STT: transcriber, LLM: agent, TTS: synth, Tools: reg, Obs: off,
	})
	res, err := s.Run(context.Background(), audioChan(2*stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatalf("Run with telemetry disabled: %v", err)
	}

	if len(res.Turns) != 2 {
		t.Errorf("got %d turns with telemetry off, want 2", len(res.Turns))
	}
	if res.TraceID != "" {
		t.Errorf("TraceID = %q with telemetry off, want empty", res.TraceID)
	}
	for i, turn := range res.Turns {
		if turn.Err != nil {
			t.Errorf("turn %d failed with telemetry off: %v", i, turn.Err)
		}
	}
}

// TestNilTracerStillRunsTheCall covers the case where no Tracer was supplied at
// all, which is what every phase-4 test does.
func TestNilTracerStillRunsTheCall(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	s := newSession(t, Config{
		STT: transcriber, LLM: agent, TTS: synth, Tools: reg, Obs: nil,
	})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatalf("Run with a nil Tracer: %v", err)
	}
	if len(res.Turns) != 1 {
		t.Errorf("got %d turns, want 1", len(res.Turns))
	}
}

// TestFailedTurnMarksSpanErrored checks that a provider failure is visible in
// the trace, which is the point of the provider-degraded profile.
func TestFailedTurnMarksSpanErrored(t *testing.T) {
	tt := testtracer.Start(t, testtracer.WithTracerStartOpts(
		tracer.WithService("voicegw"),
		tracer.WithEnv("test"),
		tracer.WithLLMObsEnabled(true),
		tracer.WithLLMObsMLApp("voice-demo"),
	))
	t.Cleanup(tt.Stop)

	obsT, err := obs.StartForTest(obs.Config{
		Enabled: true, Service: "voicegw", Env: "test",
		LLMObsEnabled: true, MLApp: "voice-demo",
	})
	if err != nil {
		t.Fatal(err)
	}

	transcriber, _, synth, reg := newProviders(t)
	failing := &stubAgent{errs: []error{errTestProvider}}

	s := newSession(t, Config{
		STT: transcriber, LLM: failing, TTS: synth, Tools: reg, Obs: obsT,
	})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 {
		t.Fatalf("Result.Errors = %d, want 1", res.Errors)
	}
	tracer.Flush()

	// workflow + stt + agent + the failed model round.
	spans := tt.WaitForLLMObsSpans(t, 4)

	round := findSpan(t, spans, "agent.reply")
	if round.Status != "error" {
		t.Errorf("the failed model round has status %q, want error", round.Status)
	}
	agentSpan := findSpan(t, spans, "voice_agent")
	if agentSpan.Status != "error" {
		t.Errorf("the agent span has status %q, want error — a failed turn should "+
			"be visible without opening its children", agentSpan.Status)
	}

	// The call itself did not fail, so the workflow must not be errored.
	wf := findSpan(t, spans, "voice_call")
	if wf.Status == "error" {
		t.Error("one failed turn marked the whole call errored")
	}

	t.Logf("failed round: status=%q message=%v", round.Status, round.Meta["error.message"])
}

var errTestProvider = errors.New("injected provider failure")
