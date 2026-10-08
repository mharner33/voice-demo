// Package call orchestrates one voice call: audio in, transcript, agent reply,
// synthesized audio out.
//
// It is the layer where the timings worth instrumenting are measured, because
// it is the only place that sees the whole turn. Those measurements are
// collected into a Result rather than reported directly, so that phase 5 can
// turn them into spans and metrics without restructuring anything here.
package call

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/llm"
	"github.com/mharner33/voice-demo/internal/obs"
	"github.com/mharner33/voice-demo/internal/stt"
	"github.com/mharner33/voice-demo/internal/tts"
)

// DefaultMaxToolRounds bounds the agent loop. A model that keeps asking for
// tools must be cut off, or one bad turn would hang a call indefinitely.
const DefaultMaxToolRounds = 3

// Config describes one call's pipeline.
type Config struct {
	// CallID identifies the call in logs, traces, and LLM Obs sessions.
	CallID string

	// STT, LLM and TTS are the providers. All three are required.
	STT stt.Transcriber
	LLM llm.Agent
	TTS tts.Synthesizer

	// Tools are offered to the agent. May be nil for a tool-free agent.
	Tools *llm.Registry

	// MaxToolRounds bounds the agent loop per turn.
	MaxToolRounds int

	// Voice is passed through to the synthesizer.
	Voice string

	// OnAudio receives each chunk of synthesized audio destined for the
	// caller. It runs on the pipeline goroutine, so it must not block: hand
	// the audio to a sender and return.
	OnAudio func(pcm []int16)

	// Obs records spans and metrics. A nil Tracer is valid and does nothing,
	// which is what lets the pipeline be instrumented unconditionally and
	// still run with no Datadog agent present.
	Obs *obs.Tracer

	// Tags identify this call on the metrics it produces.
	Tags obs.CallTags
}

func (c *Config) applyDefaults() {
	if c.MaxToolRounds == 0 {
		c.MaxToolRounds = DefaultMaxToolRounds
	}
}

// Validate rejects a pipeline that cannot run.
func (c Config) Validate() error {
	if c.CallID == "" {
		return fmt.Errorf("call: CallID is required")
	}
	if c.STT == nil {
		return fmt.Errorf("call: STT is required")
	}
	if c.LLM == nil {
		return fmt.Errorf("call: LLM is required")
	}
	if c.TTS == nil {
		return fmt.Errorf("call: TTS is required")
	}
	if c.MaxToolRounds < 1 {
		return fmt.Errorf("call: MaxToolRounds = %d, want >= 1", c.MaxToolRounds)
	}
	return nil
}

// Turn is one utterance and the agent's response to it. Every duration is
// measured from the start of the call unless named a latency, in which case it
// is the elapsed time of that step.
type Turn struct {
	Index      int
	Transcript string
	Confidence float64
	Reply      string

	// ToolCalls names the tools invoked, in order.
	ToolCalls []string
	// ToolRounds is how many times the agent was called for this turn. One
	// means it replied without tools.
	ToolRounds int

	// FinalAt is when the transcript was finalized.
	FinalAt time.Duration
	// LLMLatency covers every round trip to the agent for this turn.
	LLMLatency time.Duration
	// TTSFirstByte is how long after the agent replied that the first audio
	// chunk arrived — the silence the caller actually perceives.
	TTSFirstByte time.Duration
	// TTSAudio is the duration of synthesized speech produced.
	TTSAudio time.Duration

	InputTokens  int
	OutputTokens int

	// Err records a provider failure for this turn. The call continues, since
	// one failed turn should not drop the line.
	Err error

	// framesOut is this turn's synthesized frame count. It is unexported
	// because Result.FramesOut is the figure callers want; keeping it here
	// avoids threading a second return value out of handleTurn.
	framesOut int
}

// Result summarizes a finished call. This is both the test's assertion surface
// and, in phase 5, the body of the call log.
type Result struct {
	CallID string
	Turns  []Turn

	// FirstPartialAt is when the recognizer first produced any hypothesis. It
	// is the headline responsiveness number for the STT stage.
	FirstPartialAt time.Duration
	// HavePartial distinguishes "no partial was produced" from "produced at
	// time zero".
	HavePartial bool

	// Audio accounting.
	FramesIn          int
	ConcealedFramesIn int
	FramesOut         int

	// Token totals across every turn.
	InputTokens  int
	OutputTokens int

	// ToolCalls is the total across turns.
	ToolCalls int
	// Errors counts turns that hit a provider failure.
	Errors int

	// Duration is wall-clock time for the call.
	Duration time.Duration

	// Provider identification, for span annotation.
	STTInfo stt.Info
	LLMInfo llm.Info
	TTSInfo tts.Info

	// TraceID is the APM trace this call belongs to, for log correlation.
	TraceID string
}

// AudioInDuration is how much inbound audio was processed.
func (r Result) AudioInDuration() time.Duration {
	return codec.Duration8k(r.FramesIn * codec.SamplesPerFrame)
}

// ConcealedFraction is the share of inbound audio that the jitter buffer had to
// synthesize. It is the bridge between the network metrics and the AI metrics:
// this is how much of what the recognizer heard was filler.
func (r Result) ConcealedFraction() float64 {
	if r.FramesIn == 0 {
		return 0
	}
	return float64(r.ConcealedFramesIn) / float64(r.FramesIn)
}

// MeanConfidence is the recognizer's average confidence across the call's
// turns. It is the headline AI-quality figure for the demo because it is the
// one that degrades when the *network* does.
func (r Result) MeanConfidence() float64 {
	if len(r.Turns) == 0 {
		return 0
	}
	var sum float64
	for _, t := range r.Turns {
		sum += t.Confidence
	}
	return sum / float64(len(r.Turns))
}

// RepliedEveryTurn reports whether every turn produced something to say. A call
// with no turns at all did not fail to reply, so it counts as true.
func (r Result) RepliedEveryTurn() bool {
	for _, t := range r.Turns {
		if t.Reply == "" {
			return false
		}
	}
	return true
}

// CostUSD is the model spend for the call, from the provider's own price list.
func (r Result) CostUSD() float64 {
	in, out := r.LLMInfo.Pricing.Cost(r.InputTokens, r.OutputTokens)
	return in + out
}

// Transcripts returns the finalized transcript of each turn.
func (r Result) Transcripts() []string {
	out := make([]string, 0, len(r.Turns))
	for _, t := range r.Turns {
		out = append(out, t.Transcript)
	}
	return out
}

// Replies returns the agent's reply for each turn.
func (r Result) Replies() []string {
	out := make([]string, 0, len(r.Turns))
	for _, t := range r.Turns {
		out = append(out, t.Reply)
	}
	return out
}

// Session runs one call's pipeline.
type Session struct {
	cfg Config
}

// sttInputDescriptor renders what was handed to the recognizer. Raw audio
// cannot go on a span, so the span records the shape of the audio instead:
// duration, rate, codec, and how much of it the jitter buffer had to invent.
// That last figure is what connects a degraded transcript to a lossy network.
func sttInputDescriptor(frames, concealed int, rate int, codecName string) string {
	d := codec.Duration8k(frames * codec.SamplesPerFrame)
	pct := 0.0
	if frames > 0 {
		pct = float64(concealed) / float64(frames) * 100
	}
	return fmt.Sprintf("<audio %.2fs %dHz %s, %.1f%% concealed>",
		d.Seconds(), rate, codecName, pct)
}

// New creates a session.
func New(cfg Config) (*Session, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Session{cfg: cfg}, nil
}

// audioTeeBuffer decouples the frame counter from the recognizer.
const audioTeeBuffer = 8

// Run drives the call until the audio channel closes or the context is
// cancelled, then returns what happened.
//
// Turns are handled serially: the agent and synthesizer for one utterance
// complete before the next final transcript is processed. A production agent
// would want barge-in handling here, but serial execution keeps the trace shape
// legible, which is the point of the demo.
func (s *Session) Run(ctx context.Context, audio <-chan stt.Audio) (Result, error) {
	start := time.Now()
	res := Result{
		CallID:  s.cfg.CallID,
		STTInfo: s.cfg.STT.Info(),
		LLMInfo: s.cfg.LLM.Info(),
		TTSInfo: s.cfg.TTS.Info(),
	}

	// The workflow span is the call's root in the Agent Observability views.
	// Its session ID is the call ID, which is what groups every child span of
	// the call into one session; the SDK propagates it down the context, so it
	// is set here and nowhere else.
	//
	// This must come before any goroutine is launched. Starting a span
	// reassigns ctx, so a goroutine that captured the variable earlier would
	// both race on it and miss the span it is supposed to be parented to. The
	// SDK documents the same rule: pass a goroutine the context returned by
	// the parent's constructor.
	wf, ctx := s.cfg.Obs.StartWorkflow(ctx, "voice_call", s.cfg.CallID)
	res.TraceID = wf.APMTraceID()

	// Tee the inbound audio so the pipeline can account for it without the
	// recognizer having to report anything.
	counted := make(chan stt.Audio, audioTeeBuffer)
	var (
		mu        sync.Mutex
		framesIn  int
		concealed int
	)
	go func() {
		defer close(counted)
		for chunk := range audio {
			mu.Lock()
			framesIn++
			if chunk.Concealed {
				concealed++
			}
			mu.Unlock()

			select {
			case <-ctx.Done():
				// Keep draining the source so the producer is not blocked on a
				// channel nobody is reading.
				for range audio {
				}
				return
			case counted <- chunk:
			}
		}
	}()

	snapshotAudio := func() {
		mu.Lock()
		res.FramesIn, res.ConcealedFramesIn = framesIn, concealed
		mu.Unlock()
	}

	results, err := s.cfg.STT.Stream(ctx, counted)
	if err != nil {
		res.Duration = time.Since(start)
		snapshotAudio()
		err = fmt.Errorf("call %s: opening the transcriber: %w", s.cfg.CallID, err)
		wf.Finish(err)
		return res, err
	}

	// Utterance boundaries, so each recognition span covers exactly its own
	// utterance rather than the whole stream.
	var (
		turnIdx          int
		utteranceStart   = start
		utteranceFrames  int
		utteranceConceal int
		partialAt        time.Duration
		havePartial      bool
	)

	for r := range results {
		if !r.IsFinal {
			if !havePartial {
				havePartial = true
				partialAt = time.Since(utteranceStart)
			}
			if !res.HavePartial {
				res.HavePartial = true
				res.FirstPartialAt = time.Since(start)
			}
			continue
		}

		// Frame counts for just this utterance, for the span's audio descriptor.
		mu.Lock()
		frames, conceal := framesIn, concealed
		mu.Unlock()
		uFrames := frames - utteranceFrames
		uConceal := conceal - utteranceConceal

		s.recordSTT(ctx, r, utteranceStart, uFrames, uConceal, partialAt, havePartial)

		turn := s.handleTurn(ctx, turnIdx, r, start)
		res.Turns = append(res.Turns, turn)
		res.InputTokens += turn.InputTokens
		res.OutputTokens += turn.OutputTokens
		res.ToolCalls += len(turn.ToolCalls)
		res.FramesOut += turn.framesOut
		if turn.Err != nil {
			res.Errors++
		}

		s.cfg.Obs.RecordTurn(obs.TurnStats{
			FinalAt:      turn.FinalAt,
			LLMLatency:   turn.LLMLatency,
			TTSFirstByte: turn.TTSFirstByte,
		}, s.cfg.Tags)

		turnIdx++
		utteranceStart = time.Now()
		utteranceFrames, utteranceConceal = frames, conceal
		partialAt, havePartial = 0, false

		if ctx.Err() != nil {
			break
		}
	}

	snapshotAudio()
	res.Duration = time.Since(start)

	// The workflow span's own input and output summarize the call, so the
	// Agent Observability session view is legible without opening children.
	wf.TextIO(
		sttInputDescriptor(res.FramesIn, res.ConcealedFramesIn,
			s.cfg.STT.Info().SampleRate, string(s.cfg.Tags.Codec)),
		strings.Join(res.Replies(), " "),
		map[string]any{
			"turns":            len(res.Turns),
			"tool_calls":       res.ToolCalls,
			"errors":           res.Errors,
			"concealed_pct":    res.ConcealedFraction() * 100,
			"audio_in_seconds": res.AudioInDuration().Seconds(),
			"cost_usd":         res.CostUSD(),
		},
	)

	s.recordEvaluations(wf, res)

	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		wf.Finish(err)
		return res, err
	}
	wf.Finish(nil)
	return res, nil
}

// recordEvaluations submits the call's quality judgements against the workflow
// span, which is what populates the Agent Observability Evaluations view.
//
// All three are derived from data the pipeline already has rather than from a
// second model grading the first. That is deliberate: an LLM judge would be a
// more impressive demo of evaluations and a much worse demo of *this* system,
// because the figure that makes the argument here is one that moves with packet
// loss. A judge would mostly measure the judge.
func (s *Session) recordEvaluations(wf *obs.Span, res Result) {
	tags := map[string]string{
		"codec":    string(s.cfg.Tags.Codec),
		"provider": res.LLMInfo.Provider,
		"model":    res.LLMInfo.Model,
	}

	// Submitted even when there were no turns: a call that transcribed nothing
	// is exactly the outcome worth seeing in the Evaluations view, and omitting
	// it would quietly bias the series towards calls that went well.
	s.cfg.Obs.EvalScore(wf, obs.EvalTranscriptConfidence, res.MeanConfidence(), tags)
	s.cfg.Obs.EvalCategorical(wf, obs.EvalAudioQuality,
		obs.AudioQualityBucket(res.ConcealedFraction()), tags)
	s.cfg.Obs.EvalBool(wf, obs.EvalReplied, res.RepliedEveryTurn(), tags)
}

// recordSTT emits one Agent Observability span per recognized utterance.
//
// The span is created when the final transcript arrives and backdated to when
// the utterance began, because that is the only moment at which both ends of it
// are known. Cost is reported as billable characters rather than tokens, since
// that is how speech services actually bill.
func (s *Session) recordSTT(ctx context.Context, r stt.Result, utteranceStart time.Time,
	frames, concealed int, partialAt time.Duration, havePartial bool) {

	info := s.cfg.STT.Info()
	span, _ := s.cfg.Obs.StartLLMAt(ctx, "stt.transcribe", info.Model, info.Provider, utteranceStart)
	if span == nil {
		return
	}

	m := obs.Metrics{BillableCharacters: len(r.Text)}
	if havePartial {
		// For a recognizer, the first partial hypothesis is the analogue of a
		// first token: the moment the caller could have seen something.
		m.TimeToFirstToken = partialAt
	}

	span.LLMIO(
		[]obs.Message{{
			Role:    "user",
			Content: sttInputDescriptor(frames, concealed, info.SampleRate, string(s.cfg.Tags.Codec)),
		}},
		[]obs.Message{{Role: "assistant", Content: r.Text}},
		m,
		map[string]any{
			"confidence":     r.Confidence,
			"audio_frames":   frames,
			"concealed":      concealed,
			"sample_rate":    info.SampleRate,
			"audio_duration": codec.Duration8k(frames * codec.SamplesPerFrame).Seconds(),
		},
	)
	span.Finish(nil)
}

// handleTurn runs the agent and synthesizer for one finalized utterance.
func (s *Session) handleTurn(ctx context.Context, idx int, final stt.Result, start time.Time) Turn {
	turn := Turn{
		Index:      idx,
		Transcript: final.Text,
		Confidence: final.Confidence,
		FinalAt:    time.Since(start),
	}

	// The agent span is the parent of this turn's model calls and tool calls,
	// which is what produces the agent view's tool breakdown.
	agentSpan, actx := s.cfg.Obs.StartAgent(ctx, "voice_agent")

	reply, rounds, calls, llmLatency, err := s.runAgent(actx, final.Text)
	turn.LLMLatency = llmLatency
	turn.ToolRounds = rounds
	turn.ToolCalls = calls
	turn.InputTokens = reply.InputTokens
	turn.OutputTokens = reply.OutputTokens
	if err != nil {
		turn.Err = err
		agentSpan.TextIO(final.Text, "", s.turnMetadata(turn))
		agentSpan.Finish(err)
		return turn
	}
	turn.Reply = reply.Text

	agentSpan.TextIO(final.Text, reply.Text, s.turnMetadata(turn))
	agentSpan.Finish(nil)

	if turn.Reply == "" {
		// Nothing to say is not a failure, but it is worth recording: a silent
		// agent turn is invisible to the caller and easy to miss otherwise.
		turn.Err = fmt.Errorf("call %s: turn %d produced no reply text", s.cfg.CallID, idx)
		return turn
	}

	// speak uses the turn's context rather than the agent's, so synthesis
	// becomes a sibling of the agent span: it is a separate inference call,
	// not part of the agent's reasoning.
	framesOut, firstByte, audioDur, err := s.speak(ctx, turn.Reply)
	turn.TTSFirstByte = firstByte
	turn.TTSAudio = audioDur
	turn.framesOut = framesOut
	if err != nil {
		turn.Err = err
	}
	return turn
}

// runAgent executes the tool-calling loop for one turn, accumulating tokens
// across every round trip so the turn's reported cost is the whole turn.
func (s *Session) runAgent(ctx context.Context, transcript string) (
	final llm.Reply, rounds int, toolsUsed []string, latency time.Duration, err error,
) {
	begin := time.Now()
	req := llm.Request{
		Transcript: transcript,
		Tools:      s.cfg.Tools.Tools(),
		CallID:     s.cfg.CallID,
	}

	info := s.cfg.LLM.Info()

	for rounds = 1; rounds <= s.cfg.MaxToolRounds; rounds++ {
		roundStart := time.Now()
		reply, rerr := s.cfg.LLM.Reply(ctx, req)

		// Tokens accumulate even on the round that fails, since the provider
		// charged for it.
		final.InputTokens += reply.InputTokens
		final.OutputTokens += reply.OutputTokens

		// One span per round trip: a turn that needs a tool really does call
		// the model twice, and collapsing them would hide half the latency.
		s.recordLLMRound(ctx, info, req, reply, roundStart, rerr)

		if rerr != nil {
			return final, rounds, toolsUsed, time.Since(begin),
				fmt.Errorf("call %s: agent: %w", s.cfg.CallID, rerr)
		}
		final.Text = reply.Text

		if !reply.NeedsTools() {
			return final, rounds, toolsUsed, time.Since(begin), nil
		}
		if s.cfg.Tools == nil {
			return final, rounds, toolsUsed, time.Since(begin),
				fmt.Errorf("call %s: agent asked for tools but none are registered", s.cfg.CallID)
		}

		results := make([]llm.ToolResult, 0, len(reply.ToolCalls))
		for _, tc := range reply.ToolCalls {
			toolsUsed = append(toolsUsed, tc.Name)
			results = append(results, s.runTool(ctx, tc))
		}
		req = llm.Request{
			Transcript:  transcript,
			Tools:       s.cfg.Tools.Tools(),
			ToolResults: results,
			CallID:      s.cfg.CallID,
		}
	}

	// The loop was cut off. Whatever text the agent last produced stands, but
	// the turn is marked so the truncation is visible rather than silent.
	rounds = s.cfg.MaxToolRounds
	return final, rounds, toolsUsed, time.Since(begin),
		fmt.Errorf("call %s: agent still requesting tools after %d rounds",
			s.cfg.CallID, s.cfg.MaxToolRounds)
}

// speak synthesizes a reply and hands the audio to the configured sink.
func (s *Session) speak(ctx context.Context, text string) (
	frames int, firstByte time.Duration, audioDur time.Duration, err error,
) {
	begin := time.Now()
	info := s.cfg.TTS.Info()

	span, _ := s.cfg.Obs.StartLLM(ctx, "tts.synthesize", info.Model, info.Provider)
	// A single deferred Finish handles both paths. WithError(nil) is a no-op,
	// so a successful span is not marked errored.
	defer func() { span.Finish(err) }()

	stream, err := s.cfg.TTS.Synthesize(ctx, text, tts.Options{Voice: s.cfg.Voice})
	if err != nil {
		err = fmt.Errorf("call %s: synthesizing: %w", s.cfg.CallID, err)
		span.LLMIO(
			[]obs.Message{{Role: "user", Content: text}}, nil,
			obs.Metrics{BillableCharacters: len(text)},
			map[string]any{"voice": s.cfg.Voice},
		)
		return 0, 0, 0, err
	}

	rate := info.SampleRate
	if rate <= 0 {
		rate = codec.SampleRate8k
	}

	var samples int
	for chunk := range stream {
		if frames == 0 {
			firstByte = time.Since(begin)
		}
		frames++
		samples += len(chunk.PCM)
		if s.cfg.OnAudio != nil {
			s.cfg.OnAudio(chunk.PCM)
		}
	}

	audioDur = time.Duration(samples) * time.Second / time.Duration(rate)

	span.LLMIO(
		[]obs.Message{{Role: "user", Content: text}},
		[]obs.Message{{Role: "assistant", Content: fmt.Sprintf(
			"<audio %.2fs %dHz, %d frames>", audioDur.Seconds(), rate, frames)}},
		obs.Metrics{
			// For a synthesizer, first byte is the analogue of a first token:
			// the moment the caller stops hearing silence.
			TimeToFirstToken:   firstByte,
			BillableCharacters: len(text),
		},
		map[string]any{
			"voice":          s.cfg.Voice,
			"sample_rate":    rate,
			"frames":         frames,
			"audio_duration": audioDur.Seconds(),
		},
	)

	err = ctx.Err()
	return frames, firstByte, audioDur, err
}

// turnMetadata is the contextual data recorded on a turn's agent span.
func (s *Session) turnMetadata(t Turn) map[string]any {
	return map[string]any{
		"turn":        t.Index,
		"confidence":  t.Confidence,
		"tool_rounds": t.ToolRounds,
		"tools":       t.ToolCalls,
	}
}

// recordLLMRound emits one Agent Observability span for a single model round
// trip. The requested tool calls are recorded on the span's own output message
// as well as on their separate tool spans, so the request is visible from
// either direction.
func (s *Session) recordLLMRound(ctx context.Context, info llm.Info,
	req llm.Request, reply llm.Reply, start time.Time, callErr error) {

	span, _ := s.cfg.Obs.StartLLMAt(ctx, "agent.reply", info.Model, info.Provider, start)
	if span == nil {
		return
	}

	// The tools offered are recorded on the span, not merely the ones called.
	// Without them a turn where the model decided it did not need a tool is
	// indistinguishable from a turn where no tool was available.
	span.SetToolDefinitions(toolDefinitions(req.Tools))

	input := []obs.Message{{Role: "user", Content: req.Transcript}}
	for _, tr := range req.ToolResults {
		content := tr.Content
		role := "tool"
		if tr.Err != nil {
			role = "tool_error"
		}
		input = append(input, obs.Message{Role: role, Content: content})
	}

	out := obs.Message{Role: "assistant", Content: reply.Text}
	for _, tc := range reply.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, obs.ToolCall{
			ID: tc.ID, Name: tc.Name, Args: tc.Args,
		})
	}

	inCost, outCost := info.Pricing.Cost(reply.InputTokens, reply.OutputTokens)

	span.LLMIO(input, []obs.Message{out},
		obs.Metrics{
			InputTokens:   reply.InputTokens,
			OutputTokens:  reply.OutputTokens,
			InputCostUSD:  inCost,
			OutputCostUSD: outCost,
		},
		map[string]any{
			"tools_offered":     len(req.Tools),
			"tools_requested":   len(reply.ToolCalls),
			"continuing_a_turn": len(req.ToolResults) > 0,
		},
	)
	span.Finish(callErr)
}

// toolDefinitions converts the agent's tool list to the observability layer's
// form. The two types are separate so that internal/llm does not have to know
// about spans, which is the same reason obs.Message exists.
func toolDefinitions(tools []llm.Tool) []obs.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]obs.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		out = append(out, obs.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		})
	}
	return out
}

// runTool executes one tool call inside its own Agent Observability span.
func (s *Session) runTool(ctx context.Context, tc llm.ToolCall) llm.ToolResult {
	span, tctx := s.cfg.Obs.StartTool(ctx, tc.Name)

	res := s.cfg.Tools.Run(tctx, tc)

	span.TextIO(string(tc.Args), res.Content, map[string]any{"tool_id": tc.ID})
	span.Finish(res.Err)
	return res
}
