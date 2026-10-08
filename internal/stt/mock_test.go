package stt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/faults"
)

// feed streams n frames of audio into a transcriber and collects every result.
// concealedEvery marks every Nth frame concealed; zero means none.
func feed(t *testing.T, tr Transcriber, n, concealedEvery int) []Result {
	t.Helper()

	audio := make(chan Audio)
	go func() {
		defer close(audio)
		for i := 0; i < n; i++ {
			audio <- Audio{
				PCM:       make([]int16, codec.SamplesPerFrame),
				Concealed: concealedEvery > 0 && i%concealedEvery == 0,
			}
		}
	}()

	results, err := tr.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var out []Result
	for r := range results {
		out = append(out, r)
	}
	return out
}

func finals(results []Result) []Result {
	var out []Result
	for _, r := range results {
		if r.IsFinal {
			out = append(out, r)
		}
	}
	return out
}

func newTestMock(t *testing.T, cfg MockConfig) *Mock {
	t.Helper()
	m, err := NewMock(cfg)
	if err != nil {
		t.Fatalf("NewMock(%+v): %v", cfg, err)
	}
	return m
}

// TestMockEmitsScriptedUtterances checks the core contract: one final per
// FramesPerUtterance, carrying the configured phrases in order.
func TestMockEmitsScriptedUtterances(t *testing.T) {
	phrases := []string{"first utterance here", "second utterance here"}
	m := newTestMock(t, MockConfig{
		Phrases:            phrases,
		FramesPerPartial:   10,
		FramesPerUtterance: 50,
	})

	// Exactly four utterances' worth of audio.
	got := finals(feed(t, m, 200, 0))
	if len(got) != 4 {
		t.Fatalf("got %d finals from 200 frames at 50 per utterance, want 4", len(got))
	}
	// The phrase list cycles once exhausted.
	want := []string{phrases[0], phrases[1], phrases[0], phrases[1]}
	for i, r := range got {
		if r.Text != want[i] {
			t.Errorf("final %d = %q, want %q", i, r.Text, want[i])
		}
		if !r.IsFinal {
			t.Errorf("final %d is not marked final", i)
		}
	}
}

// TestMockIsDeterministic is the property that makes this mock usable as the
// default provider in every later phase's tests.
func TestMockIsDeterministic(t *testing.T) {
	cfg := MockConfig{FramesPerPartial: 7, FramesPerUtterance: 35}

	a := feed(t, newTestMock(t, cfg), 150, 0)
	b := feed(t, newTestMock(t, cfg), 150, 0)

	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("result %d differs between runs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestMockPartialsGrowMonotonically checks that partials behave like a real
// recognizer's: each one extends the last rather than jumping around, until the
// final commits the whole utterance.
func TestMockPartialsGrowMonotonically(t *testing.T) {
	m := newTestMock(t, MockConfig{
		Phrases:            []string{"one two three four five six seven eight"},
		FramesPerPartial:   10,
		FramesPerUtterance: 80,
	})

	results := feed(t, m, 80, 0)

	var partials []Result
	for _, r := range results {
		if !r.IsFinal {
			partials = append(partials, r)
		}
	}
	if len(partials) < 3 {
		t.Fatalf("got %d partials, want several before the final", len(partials))
	}

	for i, p := range partials {
		if p.Text == "" {
			t.Fatalf("partial %d is empty", i)
		}
		if i > 0 {
			prev := partials[i-1].Text
			if !strings.HasPrefix(p.Text, prev) {
				t.Errorf("partial %d (%q) does not extend partial %d (%q)",
					i, p.Text, i-1, prev)
			}
			if len(p.Text) < len(prev) {
				t.Errorf("partial %d shrank from %q to %q", i, prev, p.Text)
			}
		}
	}

	// The final must be the complete phrase, which the last partial need not be.
	f := finals(results)
	if len(f) != 1 {
		t.Fatalf("got %d finals, want 1", len(f))
	}
	if f[0].Text != "one two three four five six seven eight" {
		t.Errorf("final = %q, want the complete phrase", f[0].Text)
	}
}

// TestMockFinalizesPartialUtteranceAtEndOfStream covers a call that hangs up
// mid-sentence. A real recognizer finalizes what it has rather than discarding
// it, and dropping that audio would silently lose the last thing the caller said.
func TestMockFinalizesPartialUtteranceAtEndOfStream(t *testing.T) {
	m := newTestMock(t, MockConfig{
		Phrases:            []string{"complete phrase"},
		FramesPerPartial:   10,
		FramesPerUtterance: 100,
	})

	// Only 30 frames: well short of a full utterance.
	got := finals(feed(t, m, 30, 0))
	if len(got) != 1 {
		t.Fatalf("got %d finals from a truncated utterance, want 1", len(got))
	}
	if got[0].Text != "complete phrase" {
		t.Errorf("final = %q, want the phrase", got[0].Text)
	}
}

func TestMockNoAudioProducesNoResults(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	if got := feed(t, m, 0, 0); len(got) != 0 {
		t.Errorf("got %d results from no audio, want none", len(got))
	}
}

// TestMockAudioDurationTracksConsumption verifies the timestamp that makes mock
// output attributable: it must reflect audio consumed, not wall-clock time.
func TestMockAudioDurationTracksConsumption(t *testing.T) {
	m := newTestMock(t, MockConfig{
		FramesPerPartial:   25,
		FramesPerUtterance: 50,
	})

	results := feed(t, m, 100, 0)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	for i, r := range results {
		if r.AudioDuration <= 0 {
			t.Fatalf("result %d has AudioDuration %v", i, r.AudioDuration)
		}
		if i > 0 && r.AudioDuration < results[i-1].AudioDuration {
			t.Errorf("result %d AudioDuration went backwards: %v after %v",
				i, r.AudioDuration, results[i-1].AudioDuration)
		}
	}
	// 100 frames at 20 ms is two seconds of audio.
	if last := results[len(results)-1].AudioDuration; last != 2*time.Second {
		t.Errorf("final AudioDuration = %v, want 2s", last)
	}
}

// TestMockDegradesConfidenceWithConcealedAudio is the connection the whole demo
// is built to draw: packet loss in the network must be visible as degraded
// quality in the AI layer, not only as a loss percentage on a network chart.
func TestMockDegradesConfidenceWithConcealedAudio(t *testing.T) {
	cfg := MockConfig{
		Phrases:            []string{"degraded utterance"},
		FramesPerPartial:   25,
		FramesPerUtterance: 100,
		Confidence:         0.9,
		DegradeOnConcealed: true,
	}

	clean := finals(feed(t, newTestMock(t, cfg), 100, 0))
	if len(clean) != 1 {
		t.Fatalf("clean: got %d finals, want 1", len(clean))
	}
	if clean[0].Confidence != 0.9 {
		t.Errorf("clean confidence = %v, want 0.9", clean[0].Confidence)
	}

	// Every fourth frame concealed: 25% of the audio was filler.
	lossy := finals(feed(t, newTestMock(t, cfg), 100, 4))
	if len(lossy) != 1 {
		t.Fatalf("lossy: got %d finals, want 1", len(lossy))
	}
	if lossy[0].Confidence >= clean[0].Confidence {
		t.Errorf("lossy confidence %v is not below clean %v",
			lossy[0].Confidence, clean[0].Confidence)
	}
	// 25% concealed should cost roughly 25% of the confidence.
	if want := 0.9 * 0.75; lossy[0].Confidence < want-0.05 || lossy[0].Confidence > want+0.05 {
		t.Errorf("lossy confidence = %v, want about %v", lossy[0].Confidence, want)
	}
	t.Logf("confidence: %.3f clean, %.3f with 25%% concealed audio",
		clean[0].Confidence, lossy[0].Confidence)
}

func TestMockDegradeDisabledByDefault(t *testing.T) {
	m := newTestMock(t, MockConfig{
		FramesPerPartial:   25,
		FramesPerUtterance: 100,
		Confidence:         0.9,
	})
	got := finals(feed(t, m, 100, 2)) // half the audio concealed
	if len(got) != 1 {
		t.Fatalf("got %d finals, want 1", len(got))
	}
	if got[0].Confidence != 0.9 {
		t.Errorf("confidence = %v with degradation off, want 0.9", got[0].Confidence)
	}
}

func TestMockRespectsContextCancellation(t *testing.T) {
	m := newTestMock(t, MockConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	audio := make(chan Audio)

	results, err := m.Stream(ctx, audio)
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	// The result channel must close rather than leaking the goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range results {
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the result channel did not close after cancellation")
	}
	close(audio)
}

func TestMockInfo(t *testing.T) {
	info := newTestMock(t, MockConfig{}).Info()
	if info.Provider != "mock" {
		t.Errorf("Provider = %q, want mock", info.Provider)
	}
	if info.Model == "" {
		t.Error("Model is empty")
	}
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
		{"explicit valid", MockConfig{FramesPerPartial: 5, FramesPerUtterance: 20}, false},
		{"equal partial and utterance", MockConfig{FramesPerPartial: 20, FramesPerUtterance: 20}, false},
		{"negative partial", MockConfig{FramesPerPartial: -1, FramesPerUtterance: 20}, true},
		{"utterance below partial", MockConfig{FramesPerPartial: 30, FramesPerUtterance: 10}, true},
		{"confidence above one", MockConfig{Confidence: 2}, true},
		{"empty phrase", MockConfig{Phrases: []string{"ok", "   "}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMock(tc.cfg); (err != nil) != tc.wantErr {
				t.Errorf("NewMock() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultPhrasesAreUsable(t *testing.T) {
	if len(DefaultPhrases) == 0 {
		t.Fatal("DefaultPhrases is empty")
	}
	m := newTestMock(t, MockConfig{})
	got := finals(feed(t, m, DefaultFramesPerUtterance*2, 0))
	if len(got) != 2 {
		t.Fatalf("got %d finals, want 2", len(got))
	}
	for i, r := range got {
		if r.Text != DefaultPhrases[i] {
			t.Errorf("final %d = %q, want %q", i, r.Text, DefaultPhrases[i])
		}
	}
}

// --- fault decorator ---

func TestWithFaultsCleanReturnsOriginal(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	got, err := WithFaults(m, faults.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got != Transcriber(m) {
		t.Error("a clean config did not return the original transcriber unwrapped")
	}
}

func TestWithFaultsInjectsStreamError(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	faulty, err := WithFaults(m, faults.Config{ErrorRate: 1.0})
	if err != nil {
		t.Fatal(err)
	}

	audio := make(chan Audio)
	close(audio)

	if _, err := faulty.Stream(context.Background(), audio); !errors.Is(err, faults.ErrInjected) {
		t.Errorf("Stream = %v, want ErrInjected", err)
	}
	// Info must pass through the wrapper untouched, since the span annotation
	// should still name the real provider.
	if faulty.Info() != m.Info() {
		t.Error("the wrapper changed Info()")
	}
}

func TestWithFaultsAppliesLatency(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	faulty, err := WithFaults(m, faults.Config{ExtraLatency: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	audio := make(chan Audio)
	close(audio)

	start := time.Now()
	results, err := faulty.Stream(context.Background(), audio)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	for range results {
	}
	if elapsed < 25*time.Millisecond {
		t.Errorf("Stream returned after %v, want at least ~30ms of injected latency", elapsed)
	}
}

func TestWithFaultsRejectsBadConfig(t *testing.T) {
	if _, err := WithFaults(newTestMock(t, MockConfig{}), faults.Config{ErrorRate: 5}); err == nil {
		t.Error("WithFaults accepted an invalid config")
	}
}
