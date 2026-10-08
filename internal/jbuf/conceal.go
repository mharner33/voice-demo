package jbuf

import (
	"fmt"
	"math/rand"

	"github.com/mharner33/voice-demo/internal/codec"
)

// ConcealMode selects what the buffer plays when a frame is missing at its
// playout slot. Something must be played — the audio timeline cannot pause —
// and the choice is audible.
type ConcealMode int

const (
	// ConcealRepeat replays the last good frame with progressive attenuation.
	// This is the crude form of what G.711 Appendix I packet loss concealment
	// does, and it sounds markedly better than silence for isolated losses
	// because it preserves the pitch and energy of the surrounding speech.
	ConcealRepeat ConcealMode = iota

	// ConcealSilence plays digital zero. Honest and simple, but a run of
	// silence frames is heard as a click followed by a dropout.
	ConcealSilence

	// ConcealNoise plays low-level noise, the "comfort noise" convention from
	// telephony: subscribers read absolute silence as a dropped call, so a
	// quiet hiss is reassuring even though it carries nothing.
	ConcealNoise
)

func (m ConcealMode) String() string {
	switch m {
	case ConcealRepeat:
		return "repeat"
	case ConcealSilence:
		return "silence"
	case ConcealNoise:
		return "noise"
	default:
		return fmt.Sprintf("ConcealMode(%d)", int(m))
	}
}

// concealFadeStep is how much each successive concealed frame is attenuated in
// repeat mode. Repeating a frame unchanged produces an obvious buzz, so the
// energy is faded out across a run of losses.
const concealFadeStep = 0.5

// concealer synthesizes replacement frames for missing audio.
type concealer struct {
	mode       ConcealMode
	noiseLevel float64
	rng        *rand.Rand

	lastGood  []int16
	runLength int // consecutive concealed frames, for progressive fade
}

func newConcealer(mode ConcealMode, noiseLevel float64, seed int64) *concealer {
	return &concealer{
		mode:       mode,
		noiseLevel: noiseLevel,
		rng:        rand.New(rand.NewSource(seed)),
	}
}

// observe records a successfully played frame, which repeat mode draws on.
func (c *concealer) observe(pcm []int16) {
	c.runLength = 0
	if c.mode != ConcealRepeat {
		return
	}
	if cap(c.lastGood) < len(pcm) {
		c.lastGood = make([]int16, len(pcm))
	}
	c.lastGood = c.lastGood[:len(pcm)]
	copy(c.lastGood, pcm)
}

// conceal returns a replacement frame of exactly codec.SamplesPerFrame samples.
func (c *concealer) conceal() []int16 {
	c.runLength++
	out := make([]int16, codec.SamplesPerFrame)

	switch c.mode {
	case ConcealSilence:
		return out

	case ConcealNoise:
		amp := c.noiseLevel * 32767
		for i := range out {
			out[i] = int16((c.rng.Float64()*2 - 1) * amp)
		}
		return out

	default: // ConcealRepeat
		if len(c.lastGood) == 0 {
			// Nothing has played yet, so there is nothing to repeat. This
			// happens when a call's very first packet is lost.
			return out
		}
		// Attenuate by the fade step once per consecutive concealed frame, so a
		// long gap decays to silence instead of buzzing.
		gain := 1.0
		for i := 0; i < c.runLength; i++ {
			gain *= concealFadeStep
		}
		n := len(c.lastGood)
		if n > len(out) {
			n = len(out)
		}
		for i := 0; i < n; i++ {
			out[i] = int16(float64(c.lastGood[i]) * gain)
		}
		return out
	}
}

// reset clears concealment history, for buffer reuse across calls.
func (c *concealer) reset() {
	c.lastGood = c.lastGood[:0]
	c.runLength = 0
}
