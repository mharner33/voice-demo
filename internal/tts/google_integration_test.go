//go:build integration

package tts

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
)

// These tests call the real Text-to-Speech API. They are behind the
// integration build tag because they cost money and need credentials; `make
// test` never runs them. Run with:
//
//	make test-integration
//
// The offline tests in google_test.go cover the request this program builds
// and the response handling. What can only be checked here is that a real
// voice, asked for headerless 8 kHz PCM, returns audio this pipeline can put
// on the wire — and how long it takes to come back.

// skipWithoutGoogleCredentials skips unless the environment can authenticate,
// and returns the project to bill.
//
// Application Default Credentials have several sources, so an unset
// GOOGLE_APPLICATION_CREDENTIALS does not prove there are none; a gcloud login
// also works. This checks the signals available without making a request. The
// project is required either way: with user credentials the API rejects a
// request that does not name one.
func skipWithoutGoogleCredentials(t *testing.T) (project string) {
	t.Helper()

	project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		t.Skip("GOOGLE_CLOUD_PROJECT is not set; skipping the live synthesis test")
	}
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		return project
	}
	if home, err := os.UserHomeDir(); err == nil {
		adc := home + "/.config/gcloud/application_default_credentials.json"
		if _, err := os.Stat(adc); err == nil {
			return project
		}
	}
	t.Skip("no Google credentials found; skipping the live synthesis test")
	return ""
}

func liveSynthesizer(t *testing.T, cfg GoogleConfig) *Google {
	t.Helper()
	cfg.Project = skipWithoutGoogleCredentials(t)

	g, err := NewGoogle(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewGoogle: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestLiveSynthesisIsPlayableTelephonyAudio asserts the properties the return
// RTP path depends on: 8 kHz, frame-sized chunks, and real signal rather than
// silence. A WAV header or a wrong sample rate would satisfy a naive length
// check and still be unlistenable.
func TestLiveSynthesisIsPlayableTelephonyAudio(t *testing.T) {
	g := liveSynthesizer(t, GoogleConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const text = "Your current balance is four hundred and twelve dollars."

	start := time.Now()
	stream, err := g.Synthesize(ctx, text, Options{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	var (
		pcm       []int16
		frames    int
		firstByte time.Duration
	)
	for chunk := range stream {
		if frames == 0 {
			firstByte = time.Since(start)
		}
		frames++
		pcm = append(pcm, chunk.PCM...)
		if n := len(chunk.PCM); n > codec.SamplesPerFrame {
			t.Fatalf("chunk %d carried %d samples, more than one %d-sample frame",
				frames-1, n, codec.SamplesPerFrame)
		}
	}
	if frames == 0 {
		t.Fatal("the service returned no audio")
	}

	// Time to first byte is the silence the caller hears before the agent
	// speaks, so it is the metric the span reports. A plausible figure for a
	// unary request is tens to hundreds of milliseconds; the assertion is only
	// that it was measured at all and is not absurd.
	if firstByte <= 0 || firstByte > 30*time.Second {
		t.Errorf("time to first byte = %v, implausible", firstByte)
	}
	t.Logf("first byte after %v, %d frames, %d samples", firstByte, frames, len(pcm))

	// Spoken at roughly 150 words per minute, this sentence runs two to five
	// seconds. A WAV-header mistake or a 24 kHz response would land far
	// outside that band when measured against the 8 kHz rate claimed.
	spoken := time.Duration(len(pcm)) * time.Second / time.Duration(g.Info().SampleRate)
	if spoken < 1500*time.Millisecond || spoken > 8*time.Second {
		t.Errorf("the audio is %v long, which is not this sentence at 8 kHz", spoken)
	}

	// Real speech, not silence. A quarter of full scale is conservative; even
	// a quiet voice clears it comfortably.
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	rms := math.Sqrt(sum / float64(len(pcm)))
	if rms < 200 {
		t.Errorf("RMS amplitude = %.0f, which is effectively silence", rms)
	}
	t.Logf("%v of audio, RMS %.0f", spoken, rms)
}

// TestLiveSynthesisRejectsAnUnknownVoice confirms the failure is loud. A
// mistyped voice name must not quietly fall back to some other voice, because
// the voice is what Info reports as the model on every span.
func TestLiveSynthesisRejectsAnUnknownVoice(t *testing.T) {
	g := liveSynthesizer(t, GoogleConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := g.Synthesize(ctx, "hello", Options{Voice: "en-US-NotAVoice-Z"})
	if err == nil {
		t.Fatal("an unknown voice was accepted")
	}
	t.Logf("unknown voice rejected: %v", err)
}
