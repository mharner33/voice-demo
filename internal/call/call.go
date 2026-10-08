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
	"sync"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/llm"
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
		return res, fmt.Errorf("call %s: opening the transcriber: %w", s.cfg.CallID, err)
	}

	turnIdx := 0
	for r := range results {
		if !r.IsFinal {
			if !res.HavePartial {
				res.HavePartial = true
				res.FirstPartialAt = time.Since(start)
			}
			continue
		}

		turn := s.handleTurn(ctx, turnIdx, r, start)
		res.Turns = append(res.Turns, turn)
		res.InputTokens += turn.InputTokens
		res.OutputTokens += turn.OutputTokens
		res.ToolCalls += len(turn.ToolCalls)
		res.FramesOut += turn.framesOut
		if turn.Err != nil {
			res.Errors++
		}
		turnIdx++

		if ctx.Err() != nil {
			break
		}
	}

	snapshotAudio()
	res.Duration = time.Since(start)

	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return res, err
	}
	return res, nil
}

// handleTurn runs the agent and synthesizer for one finalized utterance.
func (s *Session) handleTurn(ctx context.Context, idx int, final stt.Result, start time.Time) Turn {
	turn := Turn{
		Index:      idx,
		Transcript: final.Text,
		Confidence: final.Confidence,
		FinalAt:    time.Since(start),
	}

	reply, rounds, calls, llmLatency, err := s.runAgent(ctx, final.Text)
	turn.LLMLatency = llmLatency
	turn.ToolRounds = rounds
	turn.ToolCalls = calls
	turn.InputTokens = reply.InputTokens
	turn.OutputTokens = reply.OutputTokens
	if err != nil {
		turn.Err = err
		return turn
	}
	turn.Reply = reply.Text

	if turn.Reply == "" {
		// Nothing to say is not a failure, but it is worth recording: a silent
		// agent turn is invisible to the caller and easy to miss otherwise.
		turn.Err = fmt.Errorf("call %s: turn %d produced no reply text", s.cfg.CallID, idx)
		return turn
	}

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

	for rounds = 1; rounds <= s.cfg.MaxToolRounds; rounds++ {
		reply, rerr := s.cfg.LLM.Reply(ctx, req)
		// Tokens accumulate even on the round that fails, since the provider
		// charged for it.
		final.InputTokens += reply.InputTokens
		final.OutputTokens += reply.OutputTokens
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
			results = append(results, s.cfg.Tools.Run(ctx, tc))
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
	stream, err := s.cfg.TTS.Synthesize(ctx, text, tts.Options{Voice: s.cfg.Voice})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("call %s: synthesizing: %w", s.cfg.CallID, err)
	}

	rate := s.cfg.TTS.Info().SampleRate
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
	return frames, firstByte, audioDur, ctx.Err()
}
