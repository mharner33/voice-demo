package codec

import (
	"testing"
	"time"
)

func TestFramePCMExactMultiple(t *testing.T) {
	pcm := make([]int16, SamplesPerFrame*3)
	for i := range pcm {
		pcm[i] = int16(i)
	}

	frames := FramePCM(pcm)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, f := range frames {
		if len(f) != SamplesPerFrame {
			t.Fatalf("frame %d has %d samples, want %d", i, len(f), SamplesPerFrame)
		}
	}
	if frames[0][0] != 0 || frames[1][0] != SamplesPerFrame || frames[2][0] != 2*SamplesPerFrame {
		t.Error("frames are not in source order")
	}
}

// TestFramePCMZeroPadsTail verifies every emitted frame is full length. A real
// gateway puts constant-size payloads on the wire, and downstream loss accounting
// assumes it.
func TestFramePCMZeroPadsTail(t *testing.T) {
	pcm := make([]int16, SamplesPerFrame+5)
	for i := range pcm {
		pcm[i] = 1234
	}

	frames := FramePCM(pcm)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	tail := frames[1]
	if len(tail) != SamplesPerFrame {
		t.Fatalf("tail frame has %d samples, want %d", len(tail), SamplesPerFrame)
	}
	for i := 0; i < 5; i++ {
		if tail[i] != 1234 {
			t.Errorf("tail[%d] = %d, want 1234", i, tail[i])
		}
	}
	for i := 5; i < SamplesPerFrame; i++ {
		if tail[i] != 0 {
			t.Errorf("tail[%d] = %d, want 0 padding", i, tail[i])
		}
	}
}

// TestFramePCMDoesNotAliasSource guards the copy in FramePCM: frames are encoded
// concurrently downstream, so they must not share memory with the source buffer.
func TestFramePCMDoesNotAliasSource(t *testing.T) {
	pcm := make([]int16, SamplesPerFrame*2)
	frames := FramePCM(pcm)

	for i := range pcm {
		pcm[i] = 999
	}
	for i, f := range frames {
		for j, s := range f {
			if s != 0 {
				t.Fatalf("frame %d sample %d = %d: frame aliases the source buffer", i, j, s)
			}
		}
	}
}

func TestFramePCMEmpty(t *testing.T) {
	if got := FramePCM(nil); got != nil {
		t.Errorf("FramePCM(nil) = %v, want nil", got)
	}
	if got := FramePCM([]int16{}); got != nil {
		t.Errorf("FramePCM(empty) = %v, want nil", got)
	}
}

func TestFrameCount(t *testing.T) {
	tests := []struct {
		samples int
		want    int
	}{
		{0, 0},
		{-1, 0},
		{1, 1},
		{SamplesPerFrame, 1},
		{SamplesPerFrame + 1, 2},
		{SamplesPerFrame * 5, 5},
	}
	for _, tc := range tests {
		if got := FrameCount(tc.samples); got != tc.want {
			t.Errorf("FrameCount(%d) = %d, want %d", tc.samples, got, tc.want)
		}
	}
}

// TestFramingConstantsMatchTelephony pins the wire format: 20 ms at 8 kHz is
// 160 samples and 160 G.711 bytes. Changing these silently breaks interop.
func TestFramingConstantsMatchTelephony(t *testing.T) {
	if SamplesPerFrame != 160 {
		t.Errorf("SamplesPerFrame = %d, want 160", SamplesPerFrame)
	}
	if G711FrameBytes != 160 {
		t.Errorf("G711FrameBytes = %d, want 160", G711FrameBytes)
	}
	if FrameDuration != 20*time.Millisecond {
		t.Errorf("FrameDuration = %v, want 20ms", FrameDuration)
	}
	if got := Duration8k(SamplesPerFrame); got != FrameDuration {
		t.Errorf("Duration8k(%d) = %v, want %v", SamplesPerFrame, got, FrameDuration)
	}
	if got := Duration8k(SampleRate8k); got != time.Second {
		t.Errorf("Duration8k(8000) = %v, want 1s", got)
	}
}

// TestFrameEncodesToExactWireSize ties framing to the codec: one frame must
// become exactly one 160-byte RTP payload.
func TestFrameEncodesToExactWireSize(t *testing.T) {
	pcm := Tone(440, SampleRate8k, 100*time.Millisecond, 0.5).PCM
	frames := FramePCM(pcm)
	if len(frames) != 5 {
		t.Fatalf("100ms produced %d frames, want 5", len(frames))
	}
	for _, c := range []Codec{PCMU, PCMA} {
		for i, f := range frames {
			if got := len(c.Encode(f)); got != G711FrameBytes {
				t.Fatalf("%s frame %d encoded to %d bytes, want %d", c, i, got, G711FrameBytes)
			}
		}
	}
}
