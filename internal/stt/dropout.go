package stt

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/faults"
)

// StreamFault kills a recognition stream partway through a call.
//
// The open-time injector in faults.go cannot express this. It decides a
// provider's fate when the stream is opened, which models a provider that is
// unreachable or over quota — and that was a deliberate choice, because it
// keeps the injected latency attributable to one span. But it leaves the most
// interesting failure in a voice pipeline unreachable: the recognizer that was
// working and then stopped, mid-utterance, with the caller still talking.
//
// That failure is worth being able to produce on demand for two reasons. It is
// the one a demo audience asks about, and until phase 7 it was not even
// expressible — a dying recognizer closed its channel exactly as a caller who
// had stopped talking did, and the call log called it a normal hangup
// (finding 42). Having built the plumbing to tell them apart, the demo should
// be able to show it.
//
// The fault point is measured in *audio consumed*, not wall-clock time. Same
// reasoning as the mock recognizer (finding 12): a function of logical
// progress is reproducible, so a test asserts a fixed outcome and a demo
// fails at the same point in the same call every time.
type StreamFault struct {
	inner Transcriber

	mu    sync.Mutex
	after time.Duration
}

// WithStreamFault wraps a Transcriber so its stream can be failed mid-call.
// The wrapper starts inert: with no fault point set it hands Stream straight
// to the provider, so the healthy path adds nothing but one method call.
func WithStreamFault(t Transcriber) *StreamFault {
	return &StreamFault{inner: t}
}

// SetFailAfter sets how much audio a stream carries before it fails. Zero
// turns the fault off. It is settable at any time so the live /chaos endpoint
// can arm it between calls.
func (s *StreamFault) SetFailAfter(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.after = d
}

// FailAfter reports the current fault point.
func (s *StreamFault) FailAfter() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.after
}

// Info identifies the wrapped provider unchanged: a failure injected here is
// not a different recognizer.
func (s *StreamFault) Info() Info { return s.inner.Info() }

// streamFaultBuffer matches the mock's result buffer, so wrapping a provider
// does not change how much slack the pipeline has.
const streamFaultBuffer = 32

// faultGrace bounds how long the provider is given to wind down once its
// audio has been cut off, before the failure is reported regardless.
//
// The wait exists so the fault is deterministic. Without it the error and the
// provider's last hypothesis become ready at the same moment and a select
// picks between them at random, so the same call would sometimes keep its
// final transcript and sometimes lose it — which would make the rehearsal
// unrepeatable and the trace different every time. Waiting also matches what
// really happens: a provider has already sent what it has sent.
const faultGrace = 2 * time.Second

// Stream forwards audio to the provider and results back, until the fault
// point — after which it reports the failure and stops.
//
// The fault point is read once, here. A call already in progress keeps the
// setting it started with, because changing a running call's failure point
// mid-flight would make the trace it produces unexplainable.
func (s *StreamFault) Stream(ctx context.Context, audio <-chan Audio) (<-chan Result, error) {
	after := s.FailAfter()
	if after <= 0 {
		return s.inner.Stream(ctx, audio)
	}

	// The provider gets its own context and its own audio channel, so this
	// wrapper can stop feeding it and shut it down without touching the
	// caller's.
	innerCtx, cancel := context.WithCancel(ctx)
	forward := make(chan Audio)

	results, err := s.inner.Stream(innerCtx, forward)
	if err != nil {
		cancel()
		close(forward)
		return nil, err
	}

	out := make(chan Result, streamFaultBuffer)
	failed := make(chan time.Duration, 1)

	go s.pumpAudio(innerCtx, audio, forward, failed, after)
	go s.pumpResults(ctx, cancel, results, out, failed)

	return out, nil
}

// pumpAudio copies audio to the provider until the fault point, then closes
// the provider's channel and keeps draining the caller's.
//
// The draining is the part that matters. A recognizer dying does not stop the
// caller talking, and the gateway's playout loop hands every frame to this
// channel; a wrapper that stopped reading would stall playout, or in this
// pipeline silently fill the queue and drop frames. The audio goes nowhere,
// which is exactly what happens to a caller's voice when the far end has
// stopped listening.
func (s *StreamFault) pumpAudio(innerCtx context.Context, audio <-chan Audio,
	forward chan<- Audio, failed chan<- time.Duration, after time.Duration) {

	var consumed time.Duration
	forwarding := true

	// Two different reasons to stop feeding the provider, and only one of them
	// is a fault. A call that ended before the fault point must not be
	// reported as failed — otherwise arming the fault would kill every call
	// rather than the ones long enough to reach it.
	stop := func(faulted bool) {
		if !forwarding {
			return
		}
		forwarding = false
		close(forward)
		if faulted {
			select {
			case failed <- consumed:
			default:
			}
		}
	}
	defer stop(false)

	for chunk := range audio {
		if !forwarding {
			continue // drained and discarded
		}

		select {
		case forward <- chunk:
		case <-innerCtx.Done():
			stop(false)
			continue
		}

		consumed += codec.Duration8k(len(chunk.PCM))
		if consumed >= after {
			stop(true)
		}
	}
}

// injectedStreamError describes the failure. It names where in the call it
// happened, so a trace is legible without cross-referencing anything, and
// wraps faults.ErrInjected so a reader can tell a rehearsal from a real
// outage.
func injectedStreamError(consumed time.Duration) Result {
	return Result{Err: fmt.Errorf(
		"stt: the recognition stream failed %v into the call: %w",
		consumed.Round(time.Millisecond), faults.ErrInjected)}
}

// drainResults forwards whatever the provider has left before the failure is
// reported, giving up after faultGrace on a provider that will not wind down.
func drainResults(ctx context.Context, results <-chan Result, emit func(Result) bool) {
	grace := time.NewTimer(faultGrace)
	defer grace.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-grace.C:
			return
		case r, ok := <-results:
			if !ok {
				return
			}
			if !emit(r) {
				return
			}
		}
	}
}

// pumpResults forwards the provider's hypotheses until the fault fires, and
// then reports it.
//
// Results the provider had already produced stand, since a stream dying does
// not retract what it already said.
func (s *StreamFault) pumpResults(ctx context.Context, cancel context.CancelFunc,
	results <-chan Result, out chan<- Result, failed <-chan time.Duration) {

	defer close(out)
	// Cancelling here shuts the provider down once nothing is reading its
	// results. For a real provider that is what closes the gRPC stream.
	defer cancel()

	emit := func(r Result) bool {
		select {
		case <-ctx.Done():
			return false
		case out <- r:
			return true
		}
	}

	for {
		select {
		case <-ctx.Done():
			return

		case consumed := <-failed:
			// Everything the provider already produced is delivered first,
			// then the failure. See faultGrace.
			drainResults(ctx, results, emit)
			emit(injectedStreamError(consumed))
			return

		case r, ok := <-results:
			if !ok {
				// The provider's channel closed. Either the call was shorter
				// than the fault point and nothing was injected, or closing
				// its audio is what ended it — in which case the fault has
				// already been signalled and has to be reported here, since
				// there will be no further pass through this select.
				select {
				case consumed := <-failed:
					emit(injectedStreamError(consumed))
				default:
				}
				return
			}
			if !emit(r) {
				return
			}
		}
	}
}
