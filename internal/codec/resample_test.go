package codec

import (
	"math"
	"testing"
	"time"
)

func TestResampleRatesAndLengths(t *testing.T) {
	in := Tone(300, SampleRate8k, 100*time.Millisecond, 0.5)

	up, err := Resample(in, SampleRate16k)
	if err != nil {
		t.Fatalf("8k->16k: %v", err)
	}
	if up.SampleRate != SampleRate16k {
		t.Errorf("sample rate = %d, want 16000", up.SampleRate)
	}
	if len(up.PCM) != len(in.PCM)*2 {
		t.Errorf("upsampled to %d samples, want %d", len(up.PCM), len(in.PCM)*2)
	}

	down, err := Resample(up, SampleRate8k)
	if err != nil {
		t.Fatalf("16k->8k: %v", err)
	}
	if len(down.PCM) != len(in.PCM) {
		t.Errorf("downsampled to %d samples, want %d", len(down.PCM), len(in.PCM))
	}

	same, err := Resample(in, SampleRate8k)
	if err != nil {
		t.Fatalf("8k->8k: %v", err)
	}
	if !equalPCM(same.PCM, in.PCM) {
		t.Error("same-rate resample modified the audio")
	}

	if _, err := Resample(in, 44100); err == nil {
		t.Error("Resample accepted an unsupported target rate")
	}
}

// TestResampleRoundTripPreservesTone is the test that matters: a voiceband tone
// must survive 8k -> 16k -> 8k intact. The filter edges are excluded because a
// zero-padded FIR necessarily attenuates the first and last few samples.
func TestResampleRoundTripPreservesTone(t *testing.T) {
	in := Tone(300, SampleRate8k, 500*time.Millisecond, 0.5)

	up, err := Resample(in, SampleRate16k)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Resample(up, SampleRate8k)
	if err != nil {
		t.Fatal(err)
	}

	const edge = 128 // two filter lengths of settling at each end
	snr := snrDB(in.PCM[edge:len(in.PCM)-edge], out.PCM[edge:len(out.PCM)-edge])
	if snr < 30 {
		t.Errorf("round-trip SNR %.1f dB, want >= 30", snr)
	}
	t.Logf("8k->16k->8k round-trip SNR: %.1f dB", snr)
}

// TestDownsampleAttenuatesAliasing checks the anti-alias filter actually works. A
// 3.6 kHz tone at 16 kHz is above the 4 kHz Nyquist of the 8 kHz target only
// marginally, so instead we use 6 kHz, which must be strongly suppressed rather
// than folded down to 2 kHz where it would be audible garbage.
func TestDownsampleAttenuatesAliasing(t *testing.T) {
	in := Tone(6000, SampleRate16k, 200*time.Millisecond, 0.8)
	out := Downsample2x(in.PCM)

	const edge = 128
	got := rms(out[edge : len(out)-edge])
	want := rms(in.PCM[edge : len(in.PCM)-edge])

	// Expect heavy attenuation; without the filter the alias would come through
	// at roughly full amplitude.
	if got > want*0.1 {
		t.Errorf("6 kHz content survived decimation: rms %.0f vs input %.0f (want < %.0f)",
			got, want, want*0.1)
	}
	t.Logf("6 kHz alias attenuation: %.1f dB", 20*math.Log10(want/math.Max(got, 1e-9)))
}

// TestUpsamplePreservesLevel guards against the classic zero-stuffing bug: if the
// 2x gain compensation is missing, the signal comes out 6 dB quiet.
func TestUpsamplePreservesLevel(t *testing.T) {
	in := Tone(400, SampleRate8k, 200*time.Millisecond, 0.5)
	up := Upsample2x(in.PCM)

	const edge = 128
	want := rms(in.PCM[edge : len(in.PCM)-edge])
	got := rms(up[edge*2 : len(up)-edge*2])

	if ratio := got / want; ratio < 0.9 || ratio > 1.1 {
		t.Errorf("upsampled RMS ratio %.3f, want ~1.0 (level change of %.1f dB)",
			ratio, 20*math.Log10(ratio))
	}
}

func TestResampleEmptyInput(t *testing.T) {
	if got := Upsample2x(nil); got != nil {
		t.Errorf("Upsample2x(nil) = %v, want nil", got)
	}
	if got := Downsample2x(nil); got != nil {
		t.Errorf("Downsample2x(nil) = %v, want nil", got)
	}
}

func TestClamp16Saturates(t *testing.T) {
	tests := []struct {
		in   float64
		want int16
	}{
		{0, 0},
		{100.4, 100},
		{100.6, 101},
		{-100.6, -101},
		{40000, math.MaxInt16},
		{-40000, math.MinInt16},
	}
	for _, tc := range tests {
		if got := clamp16(tc.in); got != tc.want {
			t.Errorf("clamp16(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestToneShape(t *testing.T) {
	a := Tone(1000, SampleRate8k, 100*time.Millisecond, 1.0)
	if len(a.PCM) != 800 {
		t.Errorf("got %d samples, want 800", len(a.PCM))
	}
	if a.SampleRate != SampleRate8k {
		t.Errorf("sample rate = %d, want 8000", a.SampleRate)
	}
	// Full-amplitude tone must approach but not exceed full scale.
	var peak int16
	for _, s := range a.PCM {
		if s > peak {
			peak = s
		}
	}
	if peak < 30000 {
		t.Errorf("peak = %d, want near full scale", peak)
	}
}

func rms(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(pcm)))
}
