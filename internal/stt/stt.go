// Package stt defines the streaming speech-to-text seam and provides a
// deterministic mock implementation.
//
// The interface is streaming rather than request/response because that is what
// makes the latency story in Datadog interesting: a real provider emits partial
// hypotheses within a few hundred milliseconds and refines them, so the useful
// metric is time-to-first-partial, not total call duration. A batch interface
// would hide exactly the number worth watching.
package stt

import (
	"context"
	"time"
)

// Audio is one chunk of linear PCM handed to a transcriber.
type Audio struct {
	// PCM is linear 16-bit audio at the rate given by the transcriber's Info.
	PCM []int16
	// Concealed is true when the jitter buffer synthesized this audio because
	// the packet never arrived. Transcribers may ignore it, but carrying it
	// this far lets the pipeline report how much of what the recognizer heard
	// was filler rather than speech.
	Concealed bool
}

// Result is one hypothesis from the recognizer.
type Result struct {
	// Text is the transcript so far for the current utterance.
	Text string
	// IsFinal marks the end of an utterance. A final result is never revised;
	// partials before it may be.
	IsFinal bool
	// Confidence is 0-1. Providers typically report it only on finals.
	Confidence float64
	// AudioDuration is how much audio had been consumed when this result was
	// produced, which is what makes a mock's output reproducible and a real
	// provider's latency attributable.
	AudioDuration time.Duration
}

// Info identifies the implementation, for annotating LLM Observability spans
// with the model and provider that actually served the call.
type Info struct {
	Provider   string // "mock", "google"
	Model      string // "scripted", "latest_long"
	SampleRate int    // the rate the transcriber expects
}

// Transcriber streams audio to a recognizer and streams hypotheses back.
//
// Stream consumes audio until the channel closes or the context is cancelled,
// then closes the result channel. Implementations must not block on a full
// result channel indefinitely; callers must drain it.
type Transcriber interface {
	Stream(ctx context.Context, audio <-chan Audio) (<-chan Result, error)
	Info() Info
}
