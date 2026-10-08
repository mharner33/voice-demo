package jbuf

import (
	"testing"

	"github.com/mharner33/voice-demo/internal/codec"
)

func TestConcealSilenceIsDigitalZero(t *testing.T) {
	c := newConcealer(ConcealSilence, 0, 1)
	c.observe([]int16{1000, -1000, 2000})

	got := c.conceal()
	if len(got) != codec.SamplesPerFrame {
		t.Fatalf("got %d samples, want %d", len(got), codec.SamplesPerFrame)
	}
	for i, s := range got {
		if s != 0 {
			t.Fatalf("sample %d = %d, want 0", i, s)
		}
	}
}

// TestConcealRepeatFadesOut checks the concealment that actually sounds
// acceptable: the last good frame is replayed, attenuated once per consecutive
// concealed frame so a long gap decays to silence rather than buzzing.
func TestConcealRepeatFadesOut(t *testing.T) {
	last := make([]int16, codec.SamplesPerFrame)
	for i := range last {
		last[i] = 8000
	}

	c := newConcealer(ConcealRepeat, 0, 1)
	c.observe(last)

	want := 8000.0
	for run := 1; run <= 4; run++ {
		want *= concealFadeStep
		got := c.conceal()
		if len(got) != codec.SamplesPerFrame {
			t.Fatalf("run %d: got %d samples, want %d", run, len(got), codec.SamplesPerFrame)
		}
		if int(got[0]) != int(want) {
			t.Errorf("concealed frame %d: amplitude %d, want %d", run, got[0], int(want))
		}
	}

	// A good frame resets the fade, so an isolated loss after recovery is
	// concealed at full strength rather than at the decayed level.
	c.observe(last)
	if got := c.conceal(); int(got[0]) != int(8000*concealFadeStep) {
		t.Errorf("after recovery: amplitude %d, want %d",
			got[0], int(8000*concealFadeStep))
	}
}

// TestConcealRepeatWithNoHistoryIsSilent covers losing a call's very first
// packet: there is nothing to repeat, so the only honest output is silence.
func TestConcealRepeatWithNoHistoryIsSilent(t *testing.T) {
	c := newConcealer(ConcealRepeat, 0, 1)

	got := c.conceal()
	if len(got) != codec.SamplesPerFrame {
		t.Fatalf("got %d samples, want %d", len(got), codec.SamplesPerFrame)
	}
	for i, s := range got {
		if s != 0 {
			t.Fatalf("sample %d = %d, want 0 with no history", i, s)
		}
	}
}

// TestConcealNoiseIsBoundedAndAudible checks comfort noise: it must be nonzero,
// because subscribers read absolute silence as a dropped call, but must stay at
// the configured low level rather than becoming audible hiss.
func TestConcealNoiseIsBoundedAndAudible(t *testing.T) {
	level := 0.01 // a variable, so the int16 conversion below is not a constant expression
	c := newConcealer(ConcealNoise, level, 42)

	got := c.conceal()
	limit := int16(level * 32767)

	var nonzero int
	for i, s := range got {
		if s > limit || s < -limit {
			t.Fatalf("sample %d = %d, outside the configured level of +/-%d", i, s, limit)
		}
		if s != 0 {
			nonzero++
		}
	}
	if nonzero == 0 {
		t.Error("comfort noise produced an entirely silent frame")
	}
}

func TestConcealNoiseIsDeterministicPerSeed(t *testing.T) {
	a := newConcealer(ConcealNoise, 0.01, 7).conceal()
	b := newConcealer(ConcealNoise, 0.01, 7).conceal()

	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sample %d differs between runs with the same seed: %d vs %d",
				i, a[i], b[i])
		}
	}

	d := newConcealer(ConcealNoise, 0.01, 8).conceal()
	same := true
	for i := range a {
		if a[i] != d[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("different seeds produced identical noise")
	}
}

func TestConcealerResetClearsHistory(t *testing.T) {
	last := make([]int16, codec.SamplesPerFrame)
	for i := range last {
		last[i] = 5000
	}

	c := newConcealer(ConcealRepeat, 0, 1)
	c.observe(last)
	c.reset()

	for i, s := range c.conceal() {
		if s != 0 {
			t.Fatalf("sample %d = %d after reset, want 0", i, s)
		}
	}
}

// TestConcealModeIntegration verifies the mode configured on a Buffer is the one
// actually used for its holes.
func TestConcealModeIntegration(t *testing.T) {
	tests := []struct {
		name     string
		mode     ConcealMode
		wantZero bool
	}{
		{"silence", ConcealSilence, true},
		{"repeat", ConcealRepeat, false},
		{"noise", ConcealNoise, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBuffer(t, Config{
				TargetDepth: 2,
				MaxDepth:    20,
				Conceal:     tc.mode,
				NoiseLevel:  0.05,
				Seed:        1,
			})
			// Frames 0 and 1 arrive, frame 2 is lost, frame 3 arrives.
			for _, i := range []int{0, 1, 3} {
				b.Push(frameFor(i))
			}

			got := popAll(b, 4)
			if len(got) != 4 {
				t.Fatalf("popped %d slots, want 4", len(got))
			}
			hole := got[2]
			if !hole.Concealed {
				t.Fatal("slot 2 was not concealed")
			}

			allZero := true
			for _, s := range hole.PCM {
				if s != 0 {
					allZero = false
					break
				}
			}
			if allZero != tc.wantZero {
				t.Errorf("%s concealment produced all-zero = %v, want %v",
					tc.mode, allZero, tc.wantZero)
			}
		})
	}
}

func TestConcealModeStrings(t *testing.T) {
	for mode, want := range map[ConcealMode]string{
		ConcealRepeat:  "repeat",
		ConcealSilence: "silence",
		ConcealNoise:   "noise",
	} {
		if got := mode.String(); got != want {
			t.Errorf("ConcealMode(%d).String() = %q, want %q", int(mode), got, want)
		}
	}
	if got := ConcealMode(99).String(); got == "" {
		t.Error("an unknown ConcealMode stringified to empty")
	}
}
