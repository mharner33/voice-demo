package rtp

// MOS estimation via the ITU-T G.107 E-model, reduced to the terms that matter
// for a G.711 packet-voice path: delay and packet loss.
//
// This is an estimate, not a measured opinion score. It is here because a single
// 1-to-5 number is what a voice operations team actually watches, and because it
// makes the effect of a chaos profile legible on a dashboard at a glance.

const (
	// g711MaxR is the E-model transmission rating factor for an otherwise
	// perfect G.711 path, which corresponds to a MOS of about 4.4.
	g711MaxR = 93.2

	// g711Bpl is the packet-loss robustness factor for G.711 without packet
	// loss concealment (ITU-T G.113 Appendix I).
	g711Bpl = 4.3

	// delayKnee is where delay starts to hurt disproportionately, in ms.
	delayKnee = 177.3
)

// MOSEstimate converts one-way delay and packet loss into an estimated mean
// opinion score in the range 1.0 to 4.5.
//
// burstRatio describes how clustered the loss is: 1 means independent random
// loss, and higher values mean burstier. Per the E-model, bursty loss is *worse*
// than uniform loss at the same rate — loss concealment can hide an isolated
// missing frame, but not a run of them, so clustered loss destroys whole
// syllables instead of degrading evenly. This is also why the chaos package
// offers a burst loss model: it is the more damaging and more realistic case.
func MOSEstimate(delayMs, lossPct, burstRatio float64) float64 {
	if burstRatio < 1 {
		burstRatio = 1
	}
	if delayMs < 0 {
		delayMs = 0
	}
	switch {
	case lossPct < 0:
		lossPct = 0
	case lossPct > 100:
		lossPct = 100
	}

	// Delay impairment: linear, with an extra penalty past the knee.
	id := 0.024 * delayMs
	if delayMs > delayKnee {
		id += 0.11 * (delayMs - delayKnee)
	}

	// Equipment impairment from loss. G.711's own codec impairment is zero, so
	// the whole term comes from the loss.
	ieEff := 95 * lossPct / (lossPct/burstRatio + g711Bpl)

	return rToMOS(g711MaxR - id - ieEff)
}

// MOS estimates a score from a Stats snapshot. Jitter stands in for delay: a
// receiver cannot observe one-way delay without synchronized clocks, but jitter
// is what forces a jitter buffer to add delay, so it is the honest proxy here.
func (s Stats) MOS() float64 {
	return MOSEstimate(s.JitterMs, s.LossPct, 1)
}

// rToMOS maps an E-model R factor onto the MOS scale (ITU-T G.107 §B.4).
func rToMOS(r float64) float64 {
	switch {
	case r <= 0:
		return 1
	case r >= 100:
		return 4.5
	default:
		mos := 1 + 0.035*r + 7e-6*r*(r-60)*(100-r)
		if mos < 1 {
			return 1
		}
		if mos > 4.5 {
			return 4.5
		}
		return mos
	}
}
