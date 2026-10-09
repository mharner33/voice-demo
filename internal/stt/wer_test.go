package stt

import "testing"

func TestWordErrorRate(t *testing.T) {
	const ref = "hello I'm calling about my account balance"

	tests := []struct {
		name string
		hyp  string
		want float64
	}{
		{"identical", ref, 0},
		{"case and punctuation are not errors", "Hello, I'm calling about my account balance.", 0},
		{"one substitution of seven words", "hello I'm calling about my account balanced", 1.0 / 7},
		{"one deletion", "hello I'm calling about my balance", 1.0 / 7},
		{"one insertion", "hello I'm calling about my current account balance", 1.0 / 7},
		{"nothing recognized", "", 1},
		{"completely wrong", "the quick brown fox jumped over things", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WordErrorRate(ref, tt.hyp)
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("WordErrorRate(%q, %q) = %g, want %g", ref, tt.hyp, got, tt.want)
			}
		})
	}
}

// A hypothesis longer than the reference can exceed 1, which is a property of
// the definition rather than a bug: three invented words against a one-word
// reference really is three errors.
func TestWordErrorRateCanExceedOne(t *testing.T) {
	if got := WordErrorRate("balance", "my account balance is overdrawn"); got <= 1 {
		t.Errorf("WordErrorRate = %g, want > 1", got)
	}
}

func TestWordErrorRateEmptyReference(t *testing.T) {
	if got := WordErrorRate("", ""); got != 0 {
		t.Errorf("WordErrorRate(\"\", \"\") = %g, want 0", got)
	}
	if got := WordErrorRate("", "something"); got != 1 {
		t.Errorf("WordErrorRate(\"\", %q) = %g, want 1", "something", got)
	}
}
