package call

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/faults"
	"github.com/mharner33/voice-demo/internal/llm"
	"github.com/mharner33/voice-demo/internal/stt"
	"github.com/mharner33/voice-demo/internal/tts"
)

// --- test doubles for failure paths the mocks cannot produce ---

// stubAgent returns scripted replies, so the pipeline's loop and error handling
// can be driven directly.
type stubAgent struct {
	mu      sync.Mutex
	replies []llm.Reply
	errs    []error
	calls   int
	// alwaysTools makes every reply request a tool, to exercise the loop guard.
	alwaysTools bool
}

func (s *stubAgent) Reply(ctx context.Context, req llm.Request) (llm.Reply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++

	if s.alwaysTools {
		return llm.Reply{
			ToolCalls:    []llm.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: llm.AccountLookupToolName, Args: json.RawMessage(`{"account_id":"4729"}`)}},
			InputTokens:  1,
			OutputTokens: 1,
		}, nil
	}
	if i < len(s.errs) && s.errs[i] != nil {
		return llm.Reply{InputTokens: 3, OutputTokens: 0}, s.errs[i]
	}
	if i < len(s.replies) {
		return s.replies[i], nil
	}
	return llm.Reply{Text: "default reply", InputTokens: 2, OutputTokens: 2}, nil
}

func (s *stubAgent) Info() llm.Info { return llm.Info{Provider: "stub", Model: "stub"} }

func (s *stubAgent) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// failingSynth always fails, to exercise the TTS error path.
type failingSynth struct{ err error }

func (f failingSynth) Synthesize(context.Context, string, tts.Options) (<-chan tts.Audio, error) {
	return nil, f.err
}
func (f failingSynth) Info() tts.Info {
	return tts.Info{Provider: "stub", Model: "stub", SampleRate: codec.SampleRate8k}
}

// --- helpers ---

func newProviders(t *testing.T) (stt.Transcriber, llm.Agent, tts.Synthesizer, *llm.Registry) {
	t.Helper()

	reg, err := llm.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	transcriber, err := stt.NewMock(stt.MockConfig{})
	if err != nil {
		t.Fatalf("stt.NewMock: %v", err)
	}
	agent, err := llm.NewMock(llm.MockConfig{Registry: reg})
	if err != nil {
		t.Fatalf("llm.NewMock: %v", err)
	}
	synth, err := tts.NewMock(tts.MockConfig{})
	if err != nil {
		t.Fatalf("tts.NewMock: %v", err)
	}
	return transcriber, agent, synth, reg
}

func newSession(t *testing.T, cfg Config) *Session {
	t.Helper()
	if cfg.CallID == "" {
		cfg.CallID = "test-call"
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// audioChan produces n frames of inbound audio, marking every concealedEvery-th
// frame as jitter-buffer filler.
func audioChan(n, concealedEvery int) <-chan stt.Audio {
	ch := make(chan stt.Audio)
	go func() {
		defer close(ch)
		for i := 0; i < n; i++ {
			ch <- stt.Audio{
				PCM:       make([]int16, codec.SamplesPerFrame),
				Concealed: concealedEvery > 0 && i%concealedEvery == 0,
			}
		}
	}()
	return ch
}

// --- tests ---

// TestPipelineProducesTurns is the core contract: inbound audio becomes
// transcripts, replies, and outbound audio.
func TestPipelineProducesTurns(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	var (
		mu        sync.Mutex
		outFrames int
	)
	s := newSession(t, Config{
		CallID: "c-1",
		STT:    transcriber,
		LLM:    agent,
		TTS:    synth,
		Tools:  reg,
		OnAudio: func(pcm []int16) {
			mu.Lock()
			defer mu.Unlock()
			outFrames++
			if len(pcm) != codec.SamplesPerFrame {
				t.Errorf("outbound frame has %d samples, want %d",
					len(pcm), codec.SamplesPerFrame)
			}
		},
	})

	// Two utterances' worth of audio.
	res, err := s.Run(context.Background(), audioChan(2*stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(res.Turns))
	}
	if res.CallID != "c-1" {
		t.Errorf("CallID = %q, want c-1", res.CallID)
	}
	if res.Errors != 0 {
		t.Errorf("Errors = %d, want 0; first turn error: %v", res.Errors, res.Turns[0].Err)
	}

	// Transcripts come from the mock's scripted phrases, in order.
	for i, turn := range res.Turns {
		if want := stt.DefaultPhrases[i]; turn.Transcript != want {
			t.Errorf("turn %d transcript = %q, want %q", i, turn.Transcript, want)
		}
		if turn.Reply == "" {
			t.Errorf("turn %d produced no reply", i)
		}
		if turn.Err != nil {
			t.Errorf("turn %d error: %v", i, turn.Err)
		}
		if turn.Index != i {
			t.Errorf("turn %d has Index %d", i, turn.Index)
		}
		if turn.Confidence <= 0 {
			t.Errorf("turn %d has confidence %v", i, turn.Confidence)
		}
	}

	if res.FramesOut == 0 {
		t.Error("FramesOut = 0; no audio was synthesized")
	}
	mu.Lock()
	if outFrames != res.FramesOut {
		t.Errorf("OnAudio saw %d frames but Result says %d", outFrames, res.FramesOut)
	}
	mu.Unlock()

	if res.FramesIn != 2*stt.DefaultFramesPerUtterance {
		t.Errorf("FramesIn = %d, want %d", res.FramesIn, 2*stt.DefaultFramesPerUtterance)
	}
	if got := res.AudioInDuration(); got != 4*time.Second {
		t.Errorf("AudioInDuration = %v, want 4s", got)
	}
	if res.InputTokens == 0 || res.OutputTokens == 0 {
		t.Errorf("tokens = %d in / %d out, want both non-zero",
			res.InputTokens, res.OutputTokens)
	}

	// Provider identification must survive into the Result for span annotation.
	if res.STTInfo.Provider != "mock" || res.LLMInfo.Provider != "mock" || res.TTSInfo.Provider != "mock" {
		t.Errorf("provider info = %q/%q/%q, want all mock",
			res.STTInfo.Provider, res.LLMInfo.Provider, res.TTSInfo.Provider)
	}
}

// TestPipelineRunsToolLoop covers the agent loop end to end: the model asks for
// a tool, the pipeline runs it, and the reply incorporates the result. This is
// the span shape phase 6 is built around.
func TestPipelineRunsToolLoop(t *testing.T) {
	transcriber, err := stt.NewMock(stt.MockConfig{
		// The first default phrase mentions a balance, which triggers the
		// account lookup rule.
		Phrases:            []string{"hello I'm calling about my account balance"},
		FramesPerUtterance: 50,
		FramesPerPartial:   25,
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

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})
	res, err := s.Run(context.Background(), audioChan(50, 0))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(res.Turns))
	}
	turn := res.Turns[0]
	if turn.Err != nil {
		t.Fatalf("turn error: %v", turn.Err)
	}

	if len(turn.ToolCalls) != 1 {
		t.Fatalf("turn used %d tools, want 1", len(turn.ToolCalls))
	}
	if turn.ToolCalls[0] != llm.AccountLookupToolName {
		t.Errorf("tool = %q, want %q", turn.ToolCalls[0], llm.AccountLookupToolName)
	}
	if turn.ToolRounds != 2 {
		t.Errorf("ToolRounds = %d, want 2 (one to request, one to reply)", turn.ToolRounds)
	}
	if res.ToolCalls != 1 {
		t.Errorf("Result.ToolCalls = %d, want 1", res.ToolCalls)
	}

	// The reply must carry data that only the tool could have supplied.
	if !strings.Contains(turn.Reply, "Dana Okafor") {
		t.Errorf("reply %q does not incorporate the tool result", turn.Reply)
	}
	// Tokens accumulate across both round trips.
	if turn.InputTokens == 0 || turn.OutputTokens == 0 {
		t.Errorf("turn tokens = %d/%d, want both non-zero",
			turn.InputTokens, turn.OutputTokens)
	}
	t.Logf("turn: %q -> tool %s -> %q (%d/%d tokens over %d rounds)",
		turn.Transcript, turn.ToolCalls[0], turn.Reply,
		turn.InputTokens, turn.OutputTokens, turn.ToolRounds)
}

// TestToolLoopIsBounded guards against a model that never stops asking for
// tools. Without the cap one bad turn would hang the call forever.
func TestToolLoopIsBounded(t *testing.T) {
	transcriber, _, synth, reg := newProviders(t)
	agent := &stubAgent{alwaysTools: true}

	s := newSession(t, Config{
		STT: transcriber, LLM: agent, TTS: synth, Tools: reg,
		MaxToolRounds: 3,
	})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(res.Turns))
	}
	turn := res.Turns[0]
	if turn.Err == nil {
		t.Fatal("a turn that never stopped requesting tools was not flagged")
	}
	if !strings.Contains(turn.Err.Error(), "after 3 rounds") {
		t.Errorf("error = %v, want it to name the round limit", turn.Err)
	}
	if turn.ToolRounds != 3 {
		t.Errorf("ToolRounds = %d, want 3", turn.ToolRounds)
	}
	if got := agent.callCount(); got != 3 {
		t.Errorf("the agent was called %d times, want exactly 3", got)
	}
	if res.Errors != 1 {
		t.Errorf("Result.Errors = %d, want 1", res.Errors)
	}
}

// TestAgentFailureDoesNotDropTheCall checks that one failed turn is recorded
// and the call continues. Dropping the line because the model hiccupped would
// be worse behavior than apologizing and carrying on.
func TestAgentFailureDoesNotDropTheCall(t *testing.T) {
	transcriber, _, synth, reg := newProviders(t)
	wantErr := errors.New("model unavailable")
	agent := &stubAgent{
		errs:    []error{wantErr, nil},
		replies: []llm.Reply{{}, {Text: "second turn worked", InputTokens: 2, OutputTokens: 2}},
	}

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})
	res, err := s.Run(context.Background(), audioChan(2*stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatalf("Run returned an error for a per-turn failure: %v", err)
	}

	if len(res.Turns) != 2 {
		t.Fatalf("got %d turns, want 2 — the call should have continued", len(res.Turns))
	}
	if res.Turns[0].Err == nil {
		t.Error("the failed turn was not flagged")
	}
	if !errors.Is(res.Turns[0].Err, wantErr) {
		t.Errorf("turn 0 error = %v, want it to wrap %v", res.Turns[0].Err, wantErr)
	}
	if res.Turns[1].Err != nil {
		t.Errorf("turn 1 failed too: %v", res.Turns[1].Err)
	}
	if res.Errors != 1 {
		t.Errorf("Errors = %d, want 1", res.Errors)
	}
	// Tokens from the failed round still count: the provider charged for it.
	if res.Turns[0].InputTokens == 0 {
		t.Error("the failed turn reported no input tokens")
	}
}

func TestSynthesisFailureIsRecordedPerTurn(t *testing.T) {
	transcriber, agent, _, reg := newProviders(t)
	wantErr := errors.New("synthesizer down")

	s := newSession(t, Config{
		STT: transcriber, LLM: agent, TTS: failingSynth{err: wantErr}, Tools: reg,
	})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(res.Turns))
	}
	turn := res.Turns[0]
	if turn.Err == nil {
		t.Fatal("the synthesis failure was not recorded")
	}
	if !errors.Is(turn.Err, wantErr) {
		t.Errorf("error = %v, want it to wrap %v", turn.Err, wantErr)
	}
	// The transcript and reply are still useful even though nothing was spoken.
	if turn.Transcript == "" {
		t.Error("the transcript was lost along with the synthesis failure")
	}
	if turn.Reply == "" {
		t.Error("the reply was lost along with the synthesis failure")
	}
	if res.FramesOut != 0 {
		t.Errorf("FramesOut = %d after a synthesis failure, want 0", res.FramesOut)
	}
}

// TestSilentAgentTurnIsFlagged covers a reply with no text: nothing is spoken,
// so the caller hears silence. That is invisible without an explicit check.
func TestSilentAgentTurnIsFlagged(t *testing.T) {
	transcriber, _, synth, reg := newProviders(t)
	agent := &stubAgent{replies: []llm.Reply{{Text: "", InputTokens: 1}}}

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(res.Turns))
	}
	if res.Turns[0].Err == nil {
		t.Error("a turn that produced no reply text was not flagged")
	}
	if !strings.Contains(res.Turns[0].Err.Error(), "no reply text") {
		t.Errorf("error = %v, want it to name the empty reply", res.Turns[0].Err)
	}
}

// TestTranscriberOpenFailureFailsTheCall distinguishes a per-turn failure from
// one that makes the call impossible: with no recognizer there is nothing to
// orchestrate, so Run must report it.
func TestTranscriberOpenFailureFailsTheCall(t *testing.T) {
	_, agent, synth, reg := newProviders(t)

	base, err := stt.NewMock(stt.MockConfig{})
	if err != nil {
		t.Fatal(err)
	}
	faultyStt, err := stt.WithFaults(base, faults.Config{ErrorRate: 1.0})
	if err != nil {
		t.Fatal(err)
	}

	s := newSession(t, Config{STT: faultyStt, LLM: agent, TTS: synth, Tools: reg})
	res, err := s.Run(context.Background(), audioChan(10, 0))

	if err == nil {
		t.Fatal("Run succeeded despite the transcriber failing to open")
	}
	if !errors.Is(err, faults.ErrInjected) {
		t.Errorf("error = %v, want it to wrap ErrInjected", err)
	}
	if len(res.Turns) != 0 {
		t.Errorf("got %d turns from a failed call", len(res.Turns))
	}
	// The partial result must still be usable for the call log.
	if res.CallID == "" {
		t.Error("the Result carries no CallID")
	}
	if res.Duration == 0 {
		t.Error("the Result carries no duration")
	}
}

// TestConcealedAudioIsAccounted is the bridge between the network and AI
// layers: the pipeline must report how much of what the recognizer heard was
// jitter-buffer filler rather than speech.
func TestConcealedAudioIsAccounted(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})

	const frames = stt.DefaultFramesPerUtterance
	res, err := s.Run(context.Background(), audioChan(frames, 4)) // every 4th concealed
	if err != nil {
		t.Fatal(err)
	}

	wantConcealed := (frames + 3) / 4
	if res.ConcealedFramesIn != wantConcealed {
		t.Errorf("ConcealedFramesIn = %d, want %d", res.ConcealedFramesIn, wantConcealed)
	}
	if got, want := res.ConcealedFraction(), float64(wantConcealed)/float64(frames); got != want {
		t.Errorf("ConcealedFraction = %v, want %v", got, want)
	}
	if got := (Result{}).ConcealedFraction(); got != 0 {
		t.Errorf("ConcealedFraction on an empty Result = %v, want 0", got)
	}
}

// TestTimingsArePopulated checks the measurements phase 5 will turn into spans
// and metrics. They are collected here because this is the only layer that sees
// a whole turn.
func TestTimingsArePopulated(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatal(err)
	}

	if !res.HavePartial {
		t.Error("HavePartial = false; no partial hypothesis was seen")
	}
	if res.FirstPartialAt <= 0 {
		t.Errorf("FirstPartialAt = %v, want > 0", res.FirstPartialAt)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", res.Duration)
	}

	if len(res.Turns) == 0 {
		t.Fatal("no turns")
	}
	turn := res.Turns[0]
	if turn.FinalAt <= 0 {
		t.Errorf("FinalAt = %v, want > 0", turn.FinalAt)
	}
	if turn.LLMLatency < 0 {
		t.Errorf("LLMLatency = %v", turn.LLMLatency)
	}
	if turn.TTSAudio <= 0 {
		t.Errorf("TTSAudio = %v, want > 0", turn.TTSAudio)
	}
	// The synthesized audio should be roughly as long as the reply warrants.
	if turn.TTSAudio < 100*time.Millisecond {
		t.Errorf("TTSAudio = %v, implausibly short for %q", turn.TTSAudio, turn.Reply)
	}
}

func TestRunWithNoAudioProducesNoTurns(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})
	res, err := s.Run(context.Background(), audioChan(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Turns) != 0 {
		t.Errorf("got %d turns from no audio", len(res.Turns))
	}
	if res.FramesIn != 0 {
		t.Errorf("FramesIn = %d, want 0", res.FramesIn)
	}
	if res.HavePartial {
		t.Error("HavePartial = true with no audio")
	}
}

func TestContextCancellationStopsTheCall(t *testing.T) {
	transcriber, agent, synth, reg := newProviders(t)

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := s.Run(ctx, audioChan(10_000, 0)); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want nil or context.Canceled", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestAgentWithoutToolsRegistry(t *testing.T) {
	transcriber, _, synth, _ := newProviders(t)
	// An agent that asks for tools when none are registered.
	agent := &stubAgent{alwaysTools: true}

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: nil})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(res.Turns))
	}
	if res.Turns[0].Err == nil {
		t.Fatal("asking for tools with no registry was not flagged")
	}
	if !strings.Contains(res.Turns[0].Err.Error(), "none are registered") {
		t.Errorf("error = %v, want it to name the missing registry", res.Turns[0].Err)
	}
}

func TestTranscriptsAndRepliesAccessors(t *testing.T) {
	res := Result{Turns: []Turn{
		{Transcript: "one", Reply: "reply one"},
		{Transcript: "two", Reply: "reply two"},
	}}
	if got := res.Transcripts(); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("Transcripts() = %v", got)
	}
	if got := res.Replies(); len(got) != 2 || got[0] != "reply one" || got[1] != "reply two" {
		t.Errorf("Replies() = %v", got)
	}
	if got := (Result{}).Transcripts(); len(got) != 0 {
		t.Errorf("Transcripts() on an empty Result = %v", got)
	}
}

func TestConfigValidation(t *testing.T) {
	transcriber, agent, synth, _ := newProviders(t)

	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"valid", Config{CallID: "c", STT: transcriber, LLM: agent, TTS: synth}, false},
		{"zero tool rounds takes default", Config{
			CallID: "c", STT: transcriber, LLM: agent, TTS: synth,
		}, false},
		{"no call id", Config{STT: transcriber, LLM: agent, TTS: synth}, true},
		{"no stt", Config{CallID: "c", LLM: agent, TTS: synth}, true},
		{"no llm", Config{CallID: "c", STT: transcriber, TTS: synth}, true},
		{"no tts", Config{CallID: "c", STT: transcriber, LLM: agent}, true},
		{"negative tool rounds", Config{
			CallID: "c", STT: transcriber, LLM: agent, TTS: synth, MaxToolRounds: -1,
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); (err != nil) != tc.wantErr {
				t.Errorf("New() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultMaxToolRoundsApplied(t *testing.T) {
	transcriber, _, synth, reg := newProviders(t)
	agent := &stubAgent{alwaysTools: true}

	s := newSession(t, Config{STT: transcriber, LLM: agent, TTS: synth, Tools: reg})
	res, err := s.Run(context.Background(), audioChan(stt.DefaultFramesPerUtterance, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Turns) == 0 {
		t.Fatal("no turns")
	}
	if got := res.Turns[0].ToolRounds; got != DefaultMaxToolRounds {
		t.Errorf("ToolRounds = %d, want the default %d", got, DefaultMaxToolRounds)
	}
}
