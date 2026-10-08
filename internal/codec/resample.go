package codec

import (
	"fmt"
	"math"
	"time"
)

// lp2x is a 63-tap Hamming-windowed sinc lowpass with its cutoff at a quarter of
// the sample rate: the anti-imaging filter for 2x upsampling and the anti-aliasing
// filter for 2x decimation. 63 taps is more than this demo needs, but resampling
// runs at 8-16 kHz so the cost is irrelevant and the stopband is clean.
var lp2x = makeLowpass(63, 0.25)

func makeLowpass(taps int, fc float64) []float64 {
	h := make([]float64, taps)
	mid := float64(taps-1) / 2
	for n := range h {
		x := float64(n) - mid
		var s float64
		if x == 0 {
			s = 2 * fc
		} else {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		w := 0.54 - 0.46*math.Cos(2*math.Pi*float64(n)/float64(taps-1)) // Hamming
		h[n] = s * w
	}
	// Normalize to unity DC gain so the filter does not change signal level.
	var sum float64
	for _, v := range h {
		sum += v
	}
	for i := range h {
		h[i] /= sum
	}
	return h
}

// firSame convolves in with h, returning an equal-length result aligned to the
// input by compensating for the filter's group delay. Edges are treated as zero.
func firSame(in []int16, h []float64, gain float64) []int16 {
	out := make([]int16, len(in))
	mid := len(h) / 2
	for i := range in {
		var acc float64
		for k, c := range h {
			if j := i + mid - k; j >= 0 && j < len(in) {
				acc += c * float64(in[j])
			}
		}
		out[i] = clamp16(acc * gain)
	}
	return out
}

// Upsample2x doubles the sample rate by zero-stuffing and lowpass filtering. The
// 2x gain compensates for the energy lost to the inserted zeros.
func Upsample2x(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	up := make([]int16, len(in)*2)
	for i, s := range in {
		up[i*2] = s
	}
	return firSame(up, lp2x, 2.0)
}

// Downsample2x halves the sample rate, lowpass filtering first so that content
// above the new Nyquist does not alias back into the band.
func Downsample2x(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	f := firSame(in, lp2x, 1.0)
	out := make([]int16, (len(f)+1)/2)
	for i := range out {
		out[i] = f[i*2]
	}
	return out
}

// Resample converts audio between 8 kHz and 16 kHz. Those are the only rates this
// demo needs: 8 kHz on the RTP wire, 16 kHz for speech providers that prefer it.
func Resample(a Audio, target int) (Audio, error) {
	switch {
	case a.SampleRate == target:
		return a, nil
	case a.SampleRate == SampleRate8k && target == SampleRate16k:
		return Audio{PCM: Upsample2x(a.PCM), SampleRate: target}, nil
	case a.SampleRate == SampleRate16k && target == SampleRate8k:
		return Audio{PCM: Downsample2x(a.PCM), SampleRate: target}, nil
	default:
		return Audio{}, fmt.Errorf("codec: unsupported resample %d -> %d Hz", a.SampleRate, target)
	}
}

func clamp16(v float64) int16 {
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	default:
		return int16(math.Round(v))
	}
}

// Tone generates a sine wave, for synthetic call audio and tests.
func Tone(freqHz float64, sampleRate int, d time.Duration, amplitude float64) Audio {
	n := int(float64(sampleRate) * d.Seconds())
	pcm := make([]int16, n)
	amp := amplitude * math.MaxInt16
	for i := range pcm {
		pcm[i] = clamp16(amp * math.Sin(2*math.Pi*freqHz*float64(i)/float64(sampleRate)))
	}
	return Audio{PCM: pcm, SampleRate: sampleRate}
}
