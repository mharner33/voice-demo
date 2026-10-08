// Package tts defines the text-to-speech seam that turns an agent's reply back
// into audio for the caller, and provides a deterministic mock implementation.
//
// Synthesis streams rather than returning one buffer, because a real provider
// starts emitting audio before it has finished the utterance. That first-byte
// latency is what the caller actually perceives as the agent's responsiveness,
// so the interface has to expose it.
package tts

import (
	"context"
	"fmt"
)

// Audio is one chunk of synthesized linear PCM.
type Audio struct {
	PCM []int16
}

// Options tunes one synthesis request.
type Options struct {
	// Voice names the provider's voice. Empty means the implementation's default.
	Voice string
}

// Info identifies the implementation, for span annotation.
type Info struct {
	Provider   string // "mock", "google"
	Model      string // "tone", "neural2-C"
	SampleRate int    // the rate of the audio produced
}

// Synthesizer turns text into streamed audio.
//
// Synthesize closes the returned channel when the utterance is complete or the
// context is cancelled. Callers must drain it.
type Synthesizer interface {
	Synthesize(ctx context.Context, text string, opts Options) (<-chan Audio, error)
	Info() Info
}

// ErrEmptyText is returned when there is nothing to synthesize. It is an error
// rather than an empty stream so that a pipeline bug which drops the agent's
// reply is noticed instead of producing a silent call.
var ErrEmptyText = fmt.Errorf("tts: no text to synthesize")
