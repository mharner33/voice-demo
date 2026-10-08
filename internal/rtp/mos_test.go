package rtp

import (
	"math"
	"testing"
)

// TestMOSPerfectPathScoresG711Maximum pins the top of the scale. A clean G.711
// path scores about 4.4, not 5.0: the codec itself caps what is achievable, and
// a demo that showed 5.0 would be misrepresenting the model.
func TestMOSPerfectPathScoresG711Maximum(t *testing.T) {
	got := MOSEstimate(0, 0, 1)
	if math.Abs(got-4.41) > 0.02 {
		t.Errorf("MOSEstimate(0, 0, 1) = %.3f, want ~4.41", got)
	}
	t.Logf("clean G.711 path: MOS %.2f", got)
}

// TestMOSDegradesWithLoss checks the loss term against the published E-model
// curve for G.711 without packet loss concealment: roughly 4.4 clean, 3.8 at 1%,
// 2.2 at 5%. Bands are tight enough to catch a wrong Bpl or a dropped term.
func TestMOSDegradesWithLoss(t *testing.T) {
	tests := []struct {
		lossPct  float64
		wantLow  float64
		wantHigh float64
		label    string
	}{
		{0, 4.3, 4.5, "pristine"},
		{1, 3.7, 4.0, "barely noticeable"},
		{3, 2.6, 3.0, "noticeable"},
		{5, 2.0, 2.4, "annoying"},
		{10, 1.3, 1.7, "bad"},
		{20, 1.0, 1.3, "unusable"},
	}

	prev := math.Inf(1)
	for _, tc := range tests {
		got := MOSEstimate(0, tc.lossPct, 1)
		if got < tc.wantLow || got > tc.wantHigh {
			t.Errorf("%.0f%% loss (%s): MOS = %.2f, want %.1f-%.1f",
				tc.lossPct, tc.label, got, tc.wantLow, tc.wantHigh)
		}
		if got > prev {
			t.Errorf("%.0f%% loss: MOS %.2f rose above the previous %.2f — "+
				"more loss must never score better", tc.lossPct, got, prev)
		}
		prev = got
		t.Logf("%5.0f%% loss -> MOS %.2f (%s)", tc.lossPct, got, tc.label)
	}
}

// TestMOSDegradesWithDelay verifies the delay term, including the extra penalty
// past the 177.3 ms knee where conversation becomes hard to hold.
func TestMOSDegradesWithDelay(t *testing.T) {
	prev := math.Inf(1)
	for _, delay := range []float64{0, 50, 100, 150, 200, 300, 500} {
		got := MOSEstimate(delay, 0, 1)
		if got > prev {
			t.Errorf("%.0fms delay: MOS %.2f rose above the previous %.2f",
				delay, got, prev)
		}
		prev = got
		t.Logf("%5.0fms delay -> MOS %.2f", delay, got)
	}

	// The knee must bite: the drop across it is steeper than the drop before it.
	before := MOSEstimate(100, 0, 1) - MOSEstimate(177, 0, 1)
	after := MOSEstimate(177, 0, 1) - MOSEstimate(254, 0, 1)
	if after <= before {
		t.Errorf("delay penalty did not steepen past the knee: %.3f before vs %.3f after",
			before, after)
	}
}

// TestMOSBurstinessWorsensLoss checks the burst-ratio term. Per the E-model,
// clustered loss is worse than the same rate spread evenly: concealment can hide
// one missing frame but not a run of them, so bursts take out whole syllables.
// This is the justification for the chaos package's burst loss model, so the
// sign of this relationship matters to the demo's story.
func TestMOSBurstinessWorsensLoss(t *testing.T) {
	uniform := MOSEstimate(0, 5, 1)
	bursty := MOSEstimate(0, 5, 4)

	if bursty >= uniform {
		t.Errorf("bursty loss MOS %.2f >= uniform %.2f at the same 5%% rate; "+
			"the E-model requires clustered loss to score worse", bursty, uniform)
	}
	// Monotonic in burstiness.
	prev := uniform
	for _, br := range []float64{2, 3, 4, 8} {
		got := MOSEstimate(0, 5, br)
		if got > prev {
			t.Errorf("burst ratio %.0f scored %.2f, better than the less bursty %.2f",
				br, got, prev)
		}
		prev = got
	}
	t.Logf("5%% loss: uniform MOS %.2f, bursty (ratio 4) MOS %.2f", uniform, bursty)
}

// TestMOSStaysInRange makes sure no input drives the score outside the 1.0-4.5
// MOS scale, including inputs a buggy caller might supply.
func TestMOSStaysInRange(t *testing.T) {
	for _, delay := range []float64{-100, 0, 100, 1000, 100000} {
		for _, loss := range []float64{-5, 0, 50, 100, 500} {
			for _, burst := range []float64{-1, 0, 1, 10} {
				got := MOSEstimate(delay, loss, burst)
				if got < 1 || got > 4.5 {
					t.Errorf("MOSEstimate(%v, %v, %v) = %v, outside [1, 4.5]",
						delay, loss, burst, got)
				}
				if math.IsNaN(got) || math.IsInf(got, 0) {
					t.Errorf("MOSEstimate(%v, %v, %v) = %v", delay, loss, burst, got)
				}
			}
		}
	}
}

func TestMOSExtremesSaturate(t *testing.T) {
	if got := MOSEstimate(0, 100, 1); got != 1 {
		t.Errorf("total loss: MOS = %v, want 1", got)
	}
	if got := MOSEstimate(100000, 0, 1); got != 1 {
		t.Errorf("absurd delay: MOS = %v, want 1", got)
	}
}

// TestStatsMOS checks the convenience path from a measured Stats snapshot.
func TestStatsMOS(t *testing.T) {
	clean := Stats{LossPct: 0, JitterMs: 0}
	if got := clean.MOS(); math.Abs(got-4.41) > 0.02 {
		t.Errorf("clean Stats.MOS() = %.2f, want ~4.41", got)
	}

	degraded := Stats{LossPct: 5, JitterMs: 80}
	if got := degraded.MOS(); got >= clean.MOS() {
		t.Errorf("degraded Stats.MOS() = %.2f, not worse than clean %.2f",
			got, clean.MOS())
	}
	t.Logf("5%% loss + 80ms jitter -> MOS %.2f", degraded.MOS())
}

func TestRToMOSBoundaries(t *testing.T) {
	if got := rToMOS(0); got != 1 {
		t.Errorf("rToMOS(0) = %v, want 1", got)
	}
	if got := rToMOS(-50); got != 1 {
		t.Errorf("rToMOS(-50) = %v, want 1", got)
	}
	if got := rToMOS(100); got != 4.5 {
		t.Errorf("rToMOS(100) = %v, want 4.5", got)
	}
	if got := rToMOS(200); got != 4.5 {
		t.Errorf("rToMOS(200) = %v, want 4.5", got)
	}
}
