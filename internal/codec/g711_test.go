package codec

import (
	"math"
	"testing"
	"time"
)

// uLawNegativeZero is the one u-law code with no unique linear preimage: both
// 0x7F and 0xFF decode to 0, and the encoder canonicalizes 0 to 0xFF. This is a
// property of G.711 itself, not of this implementation.
const uLawNegativeZero = 0x7F

// TestG711ReferenceAnchors checks this implementation against published ITU-T
// G.711 values. These are the anchors every conformant codec must agree on, so
// a regression here means the wire format is wrong.
func TestG711ReferenceAnchors(t *testing.T) {
	uLaw := []struct {
		code uint8
		want int16
	}{
		{0x00, -32124}, // most negative
		{0x7F, 0},      // negative zero
		{0x80, 32124},  // most positive
		{0xFF, 0},      // positive zero (canonical silence)
	}
	for _, tc := range uLaw {
		if got := ULawToLinear(tc.code); got != tc.want {
			t.Errorf("ULawToLinear(%#02x) = %d, want %d", tc.code, got, tc.want)
		}
	}

	aLaw := []struct {
		code uint8
		want int16
	}{
		{0x00, -5504},
		{0x55, -8}, // smallest negative; A-law has no exact zero
		{0x80, 5504},
		{0xD5, 8}, // smallest positive (canonical silence)
	}
	for _, tc := range aLaw {
		if got := ALawToLinear(tc.code); got != tc.want {
			t.Errorf("ALawToLinear(%#02x) = %d, want %d", tc.code, got, tc.want)
		}
	}

	// Canonical silence encodings, the values a real gateway puts on the wire
	// during a silent interval.
	if got := LinearToULaw(0); got != 0xFF {
		t.Errorf("LinearToULaw(0) = %#02x, want 0xff", got)
	}
	if got := LinearToALaw(0); got != 0xD5 {
		t.Errorf("LinearToALaw(0) = %#02x, want 0xd5", got)
	}
}

// TestG711CodeIdempotency verifies encode(decode(code)) == code for every code.
// This is the strongest structural property of G.711: each code is the canonical
// representative of its own quantization interval.
func TestG711CodeIdempotency(t *testing.T) {
	for i := 0; i < 256; i++ {
		code := uint8(i)

		if got := LinearToALaw(ALawToLinear(code)); got != code {
			t.Errorf("A-law not idempotent: enc(dec(%#02x)) = %#02x", code, got)
		}

		got := LinearToULaw(ULawToLinear(code))
		if code == uLawNegativeZero {
			if got != 0xFF {
				t.Errorf("u-law negative zero: enc(dec(%#02x)) = %#02x, want 0xff", code, got)
			}
			continue
		}
		if got != code {
			t.Errorf("u-law not idempotent: enc(dec(%#02x)) = %#02x", code, got)
		}
	}
}

// TestG711DecodeStability verifies that a decoded sample re-encodes and re-decodes
// to the identical value. Unlike code idempotency this holds with no exceptions,
// so repeated transcoding of a call cannot drift.
func TestG711DecodeStability(t *testing.T) {
	for i := 0; i < 256; i++ {
		code := uint8(i)

		u := ULawToLinear(code)
		if got := ULawToLinear(LinearToULaw(u)); got != u {
			t.Errorf("u-law decode unstable for %#02x: %d -> %d", code, u, got)
		}
		a := ALawToLinear(code)
		if got := ALawToLinear(LinearToALaw(a)); got != a {
			t.Errorf("A-law decode unstable for %#02x: %d -> %d", code, a, got)
		}
	}
}

// TestG711DecodeMonotonic verifies the decode tables are monotonic in magnitude
// within each segment ordering, which is what makes the companding curve valid.
func TestG711DecodeMonotonic(t *testing.T) {
	// For u-law, codes 0x00..0x7F are negative and increase toward zero;
	// codes 0x80..0xFF are positive and decrease toward zero.
	for i := 1; i <= 0x7F; i++ {
		if ULawToLinear(uint8(i)) < ULawToLinear(uint8(i-1)) {
			t.Fatalf("u-law negative half not monotonic at %#02x", i)
		}
	}
	for i := 0x81; i <= 0xFF; i++ {
		if ULawToLinear(uint8(i)) > ULawToLinear(uint8(i-1)) {
			t.Fatalf("u-law positive half not monotonic at %#02x", i)
		}
	}
}

// TestG711SignPreserved verifies that any sample above the first quantization
// step keeps its sign through a round trip. This is the property that actually
// matters — a sign flip would invert the waveform — and it catches the classic
// int16 negation-overflow bug.
//
// Samples with |v| <= 3 are below the smallest u-law step and legitimately
// collapse toward zero, so they are excluded.
func TestG711SignPreserved(t *testing.T) {
	const minMagnitude = 4
	for v := minMagnitude; v <= 32767; v++ {
		if got := ULawToLinear(LinearToULaw(int16(v))); got <= 0 {
			t.Fatalf("u-law lost the sign of +%d: got %d", v, got)
		}
		if got := ULawToLinear(LinearToULaw(int16(-v))); got >= 0 {
			t.Fatalf("u-law lost the sign of -%d: got %d", v, got)
		}
		if got := ALawToLinear(LinearToALaw(int16(v))); got <= 0 {
			t.Fatalf("A-law lost the sign of +%d: got %d", v, got)
		}
		if got := ALawToLinear(LinearToALaw(int16(-v))); got >= 0 {
			t.Fatalf("A-law lost the sign of -%d: got %d", v, got)
		}
	}
}

// TestG711AsymmetryBounded documents that G.711 is NOT exactly symmetric about
// zero. The reference encoder shifts before negating (pcm>>2, then negate), so
// +v and -v can round to different quantization intervals: -953>>2 is -239 while
// 953>>2 is 238. The asymmetry is bounded by one step of the top segment (1024 in
// 16-bit terms); anything larger means the segment search is broken.
func TestG711AsymmetryBounded(t *testing.T) {
	const maxAsymmetry = 1024

	var worstU, worstA, atU, atA int
	for v := 1; v <= 32767; v++ {
		pos := int(ULawToLinear(LinearToULaw(int16(v))))
		neg := int(ULawToLinear(LinearToULaw(int16(-v))))
		if d := abs(pos + neg); d > worstU {
			worstU, atU = d, v
		}
		pos = int(ALawToLinear(LinearToALaw(int16(v))))
		neg = int(ALawToLinear(LinearToALaw(int16(-v))))
		if d := abs(pos + neg); d > worstA {
			worstA, atA = d, v
		}
	}
	if worstU > maxAsymmetry {
		t.Errorf("u-law asymmetry %d at v=%d, want <= %d", worstU, atU, maxAsymmetry)
	}
	if worstA > maxAsymmetry {
		t.Errorf("A-law asymmetry %d at v=%d, want <= %d", worstA, atA, maxAsymmetry)
	}
	t.Logf("worst asymmetry: u-law %d at v=%d, A-law %d at v=%d", worstU, atU, worstA, atA)
}

// TestG711RelativeErrorBounded is the companding guarantee: because the segment
// exponent scales the step with the signal, relative error stays roughly constant
// instead of growing with amplitude. Below 256 the fixed step floor dominates, so
// the bound only applies above it.
func TestG711RelativeErrorBounded(t *testing.T) {
	const (
		minMagnitude = 256
		maxRelError  = 0.05 // measured worst: 4.2% u-law, 3.1% A-law
	)
	var worstU, worstA float64
	for v := minMagnitude; v <= 32767; v++ {
		u := float64(abs(int(ULawToLinear(LinearToULaw(int16(v))))-v)) / float64(v)
		a := float64(abs(int(ALawToLinear(LinearToALaw(int16(v))))-v)) / float64(v)
		if u > worstU {
			worstU = u
		}
		if a > worstA {
			worstA = a
		}
	}
	if worstU > maxRelError {
		t.Errorf("u-law relative error %.4f, want <= %.2f", worstU, maxRelError)
	}
	if worstA > maxRelError {
		t.Errorf("A-law relative error %.4f, want <= %.2f", worstA, maxRelError)
	}
	t.Logf("worst relative error: u-law %.4f, A-law %.4f", worstU, worstA)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// TestG711RoundTripSNR is the test that matters perceptually: G.711 is a
// near-constant-SNR codec, so a round trip must preserve a tone well. The ~38 dB
// figure is the textbook expectation for 8-bit companding; 30 dB is a safe floor.
func TestG711RoundTripSNR(t *testing.T) {
	for _, c := range []Codec{PCMU, PCMA} {
		in := Tone(440, SampleRate8k, 200*time.Millisecond, 0.5)
		out := c.Decode(c.Encode(in.PCM))

		if len(out) != len(in.PCM) {
			t.Fatalf("%s: length changed %d -> %d", c, len(in.PCM), len(out))
		}
		snr := snrDB(in.PCM, out)
		if snr < 30 {
			t.Errorf("%s: round-trip SNR %.1f dB, want >= 30", c, snr)
		}
		t.Logf("%s round-trip SNR: %.1f dB", c, snr)
	}
}

// TestG711ClippingSaturates checks that out-of-range input saturates to the
// extreme code rather than wrapping around, which would turn a loud passage into
// audible garbage.
func TestG711ClippingSaturates(t *testing.T) {
	if got := LinearToULaw(math.MaxInt16); got != 0x80 {
		t.Errorf("LinearToULaw(MaxInt16) = %#02x, want 0x80", got)
	}
	if got := LinearToULaw(math.MinInt16); got != 0x00 {
		t.Errorf("LinearToULaw(MinInt16) = %#02x, want 0x00", got)
	}
	// MinInt16 is the overflow trap: negating it in int16 would stay negative.
	if ULawToLinear(LinearToULaw(math.MinInt16)) > 0 {
		t.Error("LinearToULaw(MinInt16) produced a positive sample — sign overflow")
	}
	if ALawToLinear(LinearToALaw(math.MinInt16)) > 0 {
		t.Error("LinearToALaw(MinInt16) produced a positive sample — sign overflow")
	}
}

func TestCodecBlockHelpers(t *testing.T) {
	pcm := Tone(300, SampleRate8k, 40*time.Millisecond, 0.3).PCM

	for _, c := range []Codec{PCMU, PCMA} {
		enc := c.Encode(pcm)
		if len(enc) != len(pcm) {
			t.Fatalf("%s: encoded %d bytes from %d samples, want 1:1", c, len(enc), len(pcm))
		}
		if got := len(c.Decode(enc)); got != len(pcm) {
			t.Fatalf("%s: decoded %d samples, want %d", c, got, len(pcm))
		}
	}

	if got, ok := CodecFromPayloadType(PayloadTypePCMU); !ok || got != PCMU {
		t.Errorf("payload type 0 -> %q, %v; want PCMU, true", got, ok)
	}
	if got, ok := CodecFromPayloadType(PayloadTypePCMA); !ok || got != PCMA {
		t.Errorf("payload type 8 -> %q, %v; want PCMA, true", got, ok)
	}
	if _, ok := CodecFromPayloadType(96); ok {
		t.Error("payload type 96 unexpectedly resolved to a codec")
	}
	if got := PCMU.PayloadType(); got != PayloadTypePCMU {
		t.Errorf("PCMU.PayloadType() = %d, want 0", got)
	}
	if got := PCMA.PayloadType(); got != PayloadTypePCMA {
		t.Errorf("PCMA.PayloadType() = %d, want 8", got)
	}
}

// FuzzULawRoundTrip asserts that decoding arbitrary bytes never panics and that
// re-encoding the result reproduces the same decoded audio. Arbitrary bytes are
// exactly what a lossy network delivers.
func FuzzULawRoundTrip(f *testing.F) {
	f.Add([]byte{0xFF, 0x00, 0x80, 0x7F})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		pcm := DecodeULaw(data)
		if len(pcm) != len(data) {
			t.Fatalf("decoded %d samples from %d bytes", len(pcm), len(data))
		}
		if got := DecodeULaw(EncodeULaw(pcm)); !equalPCM(got, pcm) {
			t.Error("u-law decode is not stable across a re-encode")
		}

		apcm := DecodeALaw(data)
		if got := DecodeALaw(EncodeALaw(apcm)); !equalPCM(got, apcm) {
			t.Error("A-law decode is not stable across a re-encode")
		}
	})
}

func equalPCM(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// snrDB reports the signal-to-noise ratio of got relative to want, in decibels.
func snrDB(want, got []int16) float64 {
	var sig, noise float64
	for i := range want {
		s := float64(want[i])
		e := float64(got[i]) - s
		sig += s * s
		noise += e * e
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/noise)
}
