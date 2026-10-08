package tts

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/faults"
)

func newTestMock(t *testing.T, cfg MockConfig) *Mock {
	t.Helper()
	m, err := NewMock(cfg)
	if err != nil {
		t.Fatalf("NewMock(%+v): %v", cfg, err)
	}
	return m
}

// collect drains a synthesis stream into one PCM buffer.
func collect(t *testing.T, s Synthesizer, text string) ([]int16, int) {
	t.Helper()
	stream, err := s.Synthesize(context.Background(), text, Options{})
	if err != nil {
		t.Fatalf("Synthesize(%q): %v", text, err)
	}
	var (
		pcm    []int16
		chunks int
	)
	for a := range stream {
		chunks++
		pcm = append(pcm, a.PCM...)
	}
	return pcm, chunks
}

// TestMockDurationScalesWithText checks the property the pipeline relies on:
// synthesized audio has to be proportional to the reply, because that duration
// feeds the call's audio timeline.
func TestMockDurationScalesWithText(t *testing.T) {
	m := newTestMock(t, MockConfig{MsPerChar: 50})

	short, _ := collect(t, m, "hi")
	long, _ := collect(t, m, "this is a considerably longer reply")

	if len(long) <= len(short) {
		t.Errorf("longer text produced %d samples, not more than %d", len(long), len(short))
	}

	// 10 characters at 50 ms is 500 ms, which is 4000 samples at 8 kHz. The
	// result is rounded up to whole 20 ms frames.
	pcm, chunks := collect(t, m, "0123456789")
	wantSamples := 10 * 50 * codec.SampleRate8k / 1000
	if len(pcm) < wantSamples {
		t.Errorf("got %d samples, want at least %d", len(pcm), wantSamples)
	}
	if want := codec.FrameCount(wantSamples); chunks != want {
		t.Errorf("got %d chunks, want %d", chunks, want)
	}
	if got := m.Duration("0123456789"); got != 500*time.Millisecond {
		t.Errorf("Duration() = %v, want 500ms", got)
	}
}

// TestMockEmitsFrameSizedChunks matters because the output goes straight back
// out over RTP: a chunk that is not one packetization interval would have to be
// regrouped before it could be sent.
func TestMockEmitsFrameSizedChunks(t *testing.T) {
	for _, rate := range []int{codec.SampleRate8k, codec.SampleRate16k} {
		m := newTestMock(t, MockConfig{SampleRate: rate})

		stream, err := m.Synthesize(context.Background(), "a reply of some length", Options{})
		if err != nil {
			t.Fatal(err)
		}
		want := rate / 50 // 20 ms
		for a := range stream {
			if len(a.PCM) != want {
				t.Fatalf("rate %d: chunk has %d samples, want %d", rate, len(a.PCM), want)
			}
		}
	}
}

// TestMockAudioIdentifiesItsText is how the pipeline test verifies the agent's
// actual reply reached the caller, rather than some other utterance: the tone's
// pitch is a function of the text.
func TestMockAudioIdentifiesItsText(t *testing.T) {
	a := TextFrequency("the first reply")
	b := TextFrequency("a different reply")

	if a == b {
		t.Error("two different replies map to the same frequency")
	}
	if a != TextFrequency("the first reply") {
		t.Error("TextFrequency is not stable for the same text")
	}
	for _, f := range []float64{a, b} {
		if f < mockMinFreq || f >= mockMaxFreq {
			t.Errorf("frequency %v is outside [%d, %d)", f, mockMinFreq, mockMaxFreq)
		}
	}
}

// TestMockAudioMatchesExpectedTone confirms the generated audio really is the
// tone the text maps to, by measuring zero crossings rather than trusting the
// generator.
func TestMockAudioMatchesExpectedTone(t *testing.T) {
	const text = "a reply long enough to measure accurately"
	m := newTestMock(t, MockConfig{MsPerChar: 100}) // ~4 seconds

	pcm, _ := collect(t, m, text)
	if len(pcm) == 0 {
		t.Fatal("no audio")
	}

	// A sine wave crosses zero twice per cycle.
	var crossings int
	for i := 1; i < len(pcm); i++ {
		if (pcm[i-1] < 0) != (pcm[i] < 0) {
			crossings++
		}
	}
	seconds := float64(len(pcm)) / codec.SampleRate8k
	gotFreq := float64(crossings) / 2 / seconds
	wantFreq := TextFrequency(text)

	if math.Abs(gotFreq-wantFreq) > wantFreq*0.05 {
		t.Errorf("measured %.1f Hz, want %.1f Hz from TextFrequency", gotFreq, wantFreq)
	}
	t.Logf("%q -> %.1f Hz (measured %.1f Hz)", text, wantFreq, gotFreq)
}

func TestMockIsDeterministic(t *testing.T) {
	const text = "the same reply twice"
	a, _ := collect(t, newTestMock(t, MockConfig{}), text)
	b, _ := collect(t, newTestMock(t, MockConfig{}), text)

	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sample %d differs between runs: %d vs %d", i, a[i], b[i])
		}
	}
}

// TestMockRejectsEmptyText pins a deliberate choice: empty text is an error
// rather than an empty stream, so a pipeline bug that drops the agent's reply
// surfaces instead of producing a silent call.
func TestMockRejectsEmptyText(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	for _, text := range []string{"", "   ", "\t\n"} {
		if _, err := m.Synthesize(context.Background(), text, Options{}); !errors.Is(err, ErrEmptyText) {
			t.Errorf("Synthesize(%q) = %v, want ErrEmptyText", text, err)
		}
	}
}

func TestMockHonorsContextCancellation(t *testing.T) {
	m := newTestMock(t, MockConfig{MsPerChar: 1000}) // a long utterance

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.Synthesize(ctx, "long reply here", Options{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range stream {
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the audio channel did not close after cancellation")
	}

	// A context already cancelled is rejected up front.
	if _, err := m.Synthesize(ctx, "text", Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Synthesize with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestMockAmplitudeIsReasonable(t *testing.T) {
	pcm, _ := collect(t, newTestMock(t, MockConfig{}), "a reply")

	var peak int16
	for _, s := range pcm {
		if s > peak {
			peak = s
		}
	}
	// Audible but not clipping.
	if peak < 5000 {
		t.Errorf("peak amplitude %d is too quiet to hear", peak)
	}
	if peak >= math.MaxInt16 {
		t.Errorf("peak amplitude %d is clipping", peak)
	}
}

func TestMockInfo(t *testing.T) {
	info := newTestMock(t, MockConfig{}).Info()
	if info.Provider != "mock" {
		t.Errorf("Provider = %q, want mock", info.Provider)
	}
	if info.Model == "" {
		t.Error("Model is empty")
	}
	// The default rate must match the wire, so output needs no resampling.
	if info.SampleRate != codec.SampleRate8k {
		t.Errorf("SampleRate = %d, want 8000", info.SampleRate)
	}
}

func TestMockConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MockConfig
		wantErr bool
	}{
		{"zero value takes defaults", MockConfig{}, false},
		{"explicit 16k", MockConfig{SampleRate: codec.SampleRate16k}, false},
		{"negative ms per char", MockConfig{MsPerChar: -1}, true},
		{"unsupported rate", MockConfig{SampleRate: 44100}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMock(tc.cfg); (err != nil) != tc.wantErr {
				t.Errorf("NewMock() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// --- fault decorator ---

func TestWithFaultsCleanReturnsOriginal(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	got, err := WithFaults(m, faults.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got != Synthesizer(m) {
		t.Error("a clean config did not return the original synthesizer unwrapped")
	}
}

func TestWithFaultsInjectsError(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	faulty, err := WithFaults(m, faults.Config{ErrorRate: 1.0})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := faulty.Synthesize(context.Background(), "text", Options{}); !errors.Is(err, faults.ErrInjected) {
		t.Errorf("Synthesize = %v, want ErrInjected", err)
	}
	if faulty.Info() != m.Info() {
		t.Error("the wrapper changed Info()")
	}
}

// TestWithFaultsDelaysFirstByte checks the fault lands where the caller would
// perceive it: as silence before the agent starts speaking.
func TestWithFaultsDelaysFirstByte(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	faulty, err := WithFaults(m, faults.Config{ExtraLatency: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	stream, err := faulty.Synthesize(context.Background(), "a reply", Options{})
	if err != nil {
		t.Fatal(err)
	}
	<-stream
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Errorf("first chunk arrived after %v, want at least ~30ms", elapsed)
	}
	for range stream {
	}
}
