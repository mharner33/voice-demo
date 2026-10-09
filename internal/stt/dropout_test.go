package stt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/faults"
)

func newFaultingMock(t *testing.T, after time.Duration) *StreamFault {
	t.Helper()

	m, err := NewMock(MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	sf := WithStreamFault(m)
	sf.SetFailAfter(after)
	return sf
}

// run feeds n frames through a transcriber and collects everything it emits.
func run(t *testing.T, tr Transcriber, n int) []Result {
	t.Helper()

	audio := make(chan Audio)
	results, err := tr.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	go func() {
		defer close(audio)
		for i := 0; i < n; i++ {
			audio <- Audio{PCM: make([]int16, codec.SamplesPerFrame)}
		}
	}()

	var out []Result
	for r := range results {
		out = append(out, r)
	}
	return out
}

// The headline behaviour: a stream that was producing transcripts stops, and
// says why. Before phase 7 this was inexpressible — a dying recognizer closed
// its channel exactly as a caller who stopped talking did.
func TestStreamFailsMidCall(t *testing.T) {
	// Three seconds of audio in, failing after two.
	sf := newFaultingMock(t, 2*time.Second)
	got := run(t, sf, 150)

	if len(got) == 0 {
		t.Fatal("no results at all; the fault swallowed the whole stream")
	}

	last := got[len(got)-1]
	if last.Err == nil {
		t.Fatalf("the last result carries no error: %+v", last)
	}
	if !errors.Is(last.Err, faults.ErrInjected) {
		t.Errorf("error = %v, want it to identify itself as injected", last.Err)
	}
	// The message names where in the call it happened, which is what makes a
	// trace legible without cross-referencing anything.
	if want := "2s"; !contains(last.Err.Error(), want) {
		t.Errorf("error = %q, want it to name roughly %s of audio", last.Err, want)
	}

	// Everything before the error is real output: the hypotheses the
	// recognizer had already produced survive, since a stream dying does not
	// retract what it already said.
	var finals int
	for _, r := range got[:len(got)-1] {
		if r.Err != nil {
			t.Errorf("an error arrived before the end of the stream: %v", r.Err)
		}
		if r.IsFinal {
			finals++
		}
	}
	if finals == 0 {
		t.Error("no final transcript survived the fault; the first utterance should have")
	}
}

// Exactly one error, and it is last. A consumer has to be able to rely on
// that: the pipeline records the first one it sees and keeps going.
func TestStreamFaultEmitsOneErrorLast(t *testing.T) {
	sf := newFaultingMock(t, time.Second)
	got := run(t, sf, 200)

	var errs int
	for i, r := range got {
		if r.Err == nil {
			continue
		}
		errs++
		if i != len(got)-1 {
			t.Errorf("error at result %d of %d, want it last", i, len(got))
		}
	}
	if errs != 1 {
		t.Errorf("%d errors, want exactly 1", errs)
	}
}

// An inert wrapper has to be invisible. The gateway wraps every run, including
// the ones where nothing is meant to fail.
func TestStreamFaultOffIsInvisible(t *testing.T) {
	m, err := NewMock(MockConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sf := WithStreamFault(m)

	plain := run(t, m, 150)
	wrapped := run(t, sf, 150)

	if len(plain) != len(wrapped) {
		t.Fatalf("wrapped produced %d results, unwrapped %d", len(wrapped), len(plain))
	}
	for i := range plain {
		if plain[i] != wrapped[i] {
			t.Errorf("result %d differs: %+v vs %+v", i, wrapped[i], plain[i])
		}
	}
}

// A call shorter than the fault point is not failed. Otherwise arming the
// fault would kill every call rather than the long ones, and a demo could not
// show a call surviving alongside one that did not.
func TestStreamFaultDoesNotFireOnAShortCall(t *testing.T) {
	sf := newFaultingMock(t, 5*time.Second)
	got := run(t, sf, 50) // one second of audio

	for _, r := range got {
		if r.Err != nil {
			t.Errorf("a one-second call was failed by a five-second fault point: %v", r.Err)
		}
	}
	if len(got) == 0 {
		t.Error("no results from a call that should have been untouched")
	}
}

// The caller's audio keeps being consumed after the recognizer is gone. A
// wrapper that stopped reading would stall the gateway's playout loop, and a
// caller's voice cannot be paused — so the frames are drained and discarded,
// which is what happens to speech when the far end has stopped listening.
func TestStreamFaultKeepsDrainingAudio(t *testing.T) {
	sf := newFaultingMock(t, 200*time.Millisecond)

	audio := make(chan Audio)
	results, err := sf.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Drain results so the fault can fire and the stream can finish.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range results {
		}
	}()

	// Far more audio than the fault point, unbuffered. Every send has to be
	// accepted, or this blocks and the test times out — which is precisely
	// the stall that would freeze the gateway.
	sent := 0
	for i := 0; i < 500; i++ {
		select {
		case audio <- Audio{PCM: make([]int16, codec.SamplesPerFrame)}:
			sent++
		case <-time.After(5 * time.Second):
			t.Fatalf("the wrapper stopped reading audio after %d frames; "+
				"playout would stall", sent)
		}
	}
	close(audio)
	<-done

	if sent != 500 {
		t.Errorf("only %d of 500 frames were accepted", sent)
	}
}

// The fault point is read when the stream opens, so retuning it does not
// change a call already in flight — a trace whose failure point moved
// mid-call would be unexplainable.
func TestStreamFaultIsFixedForTheCall(t *testing.T) {
	sf := newFaultingMock(t, 0)

	audio := make(chan Audio)
	results, err := sf.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Arm it after the stream is already open.
	sf.SetFailAfter(100 * time.Millisecond)

	go func() {
		defer close(audio)
		for i := 0; i < 150; i++ {
			audio <- Audio{PCM: make([]int16, codec.SamplesPerFrame)}
		}
	}()

	for r := range results {
		if r.Err != nil {
			t.Errorf("a call that opened unarmed was failed: %v", r.Err)
		}
	}

	// The next call picks it up.
	for _, r := range run(t, sf, 150) {
		if r.Err != nil {
			return
		}
	}
	t.Error("the next call was not failed, so the setting never took effect")
}

// Measured in audio consumed rather than wall-clock, so the same call fails at
// the same point every time. Same argument as the mock recognizer's.
func TestStreamFaultIsReproducible(t *testing.T) {
	first := run(t, newFaultingMock(t, 2*time.Second), 150)
	second := run(t, newFaultingMock(t, 2*time.Second), 150)

	if len(first) != len(second) {
		t.Fatalf("two identical runs produced %d and %d results", len(first), len(second))
	}
	for i := range first {
		if (first[i].Err == nil) != (second[i].Err == nil) {
			t.Fatalf("result %d differs in whether it carries an error", i)
		}
		if first[i].Err == nil && first[i] != second[i] {
			t.Errorf("result %d differs: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestStreamFaultInfoIsUnchanged(t *testing.T) {
	m, err := NewMock(MockConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sf := WithStreamFault(m)
	sf.SetFailAfter(time.Second)

	if sf.Info() != m.Info() {
		t.Errorf("Info() = %+v, want the wrapped provider's %+v", sf.Info(), m.Info())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
