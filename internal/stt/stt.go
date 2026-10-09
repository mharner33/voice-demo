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
	// ConfidenceUnknown is set when the provider returned no confidence figure
	// at all, which Google's API explicitly permits. Without this flag a
	// missing figure would arrive as a confident zero and drag the call's
	// quality evaluation down for a transcript that may be perfectly good.
	// The zero value means the confidence above is real, so a provider that
	// always reports one never has to think about this.
	ConfidenceUnknown bool
	// AudioDuration is how much audio had been consumed when this result was
	// produced, which is what makes a mock's output reproducible and a real
	// provider's latency attributable.
	AudioDuration time.Duration
	// Err reports a failure that ended the stream partway through. The channel
	// closes after it, so a consumer sees at most one. It exists because a
	// recognizer that dies mid-call would otherwise be indistinguishable from
	// a caller who stopped talking: both simply close the channel.
	Err error
}

// Info identifies the implementation, for annotating LLM Observability spans
// with the model and provider that actually served the call.
type Info struct {
	Provider   string // "mock", "google"
	Model      string // "scripted", "telephony"
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
