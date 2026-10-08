package tts

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
)

// DefaultMsPerChar sets how long synthesized speech runs. Conversational
// English is roughly 150 words per minute, about 13 characters per second, so
// 75 ms per character puts a mock utterance at a believable length — which
// matters because the duration feeds the call's audio timeline and its metrics.
const DefaultMsPerChar = 75

// Mock tone frequencies, in Hz. The pitch is derived from the text so that
// different replies are audibly and verifiably different, while the same reply
// always produces identical audio.
const (
	mockMinFreq = 180
	mockMaxFreq = 520
	mockAmp     = 0.4
)

// MockConfig parameterizes the mock synthesizer.
type MockConfig struct {
	// MsPerChar controls utterance length.
	MsPerChar int
	// SampleRate of the generated audio. Defaults to the 8 kHz telephony rate,
	// so the output needs no resampling before it goes back out over RTP.
	SampleRate int
	// Voice is reported in Info and otherwise unused.
	Voice string
}

func (c *MockConfig) applyDefaults() {
	if c.MsPerChar == 0 {
		c.MsPerChar = DefaultMsPerChar
	}
	if c.SampleRate == 0 {
		c.SampleRate = codec.SampleRate8k
	}
	if c.Voice == "" {
		c.Voice = "tone"
	}
}

// Validate rejects configurations that would produce unusable audio.
func (c MockConfig) Validate() error {
	if c.MsPerChar < 1 {
		return fmt.Errorf("tts: MsPerChar = %d, want >= 1", c.MsPerChar)
	}
	if c.SampleRate != codec.SampleRate8k && c.SampleRate != codec.SampleRate16k {
		return fmt.Errorf("tts: SampleRate = %d, want 8000 or 16000", c.SampleRate)
	}
	return nil
}

// Mock is a deterministic Synthesizer. It emits a tone whose pitch and length
// are functions of the text, so a test can assert both without a golden audio
// file and a demo still produces something audible.
type Mock struct {
	cfg MockConfig
}

// NewMock creates a mock synthesizer. The zero MockConfig is valid.
func NewMock(cfg MockConfig) (*Mock, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Mock{cfg: cfg}, nil
}

// Info identifies the mock.
func (m *Mock) Info() Info {
	return Info{Provider: "mock", Model: "tone", SampleRate: m.cfg.SampleRate}
}

// mockAudioBuffer bounds how far ahead the mock will generate. Synthesis is
// cheap, so the buffer exists to decouple the generator from the consumer
// rather than to hide latency.
const mockAudioBuffer = 16

// Duration reports how long the mock will speak the given text, which lets a
// caller reason about the audio timeline without synthesizing.
func (m *Mock) Duration(text string) time.Duration {
	return time.Duration(len(text)) * time.Duration(m.cfg.MsPerChar) * time.Millisecond
}

// Synthesize streams a tone representing the text.
func (m *Mock) Synthesize(ctx context.Context, text string, opts Options) (<-chan Audio, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmptyText
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	freq := TextFrequency(text)
	totalSamples := int(m.Duration(text).Seconds() * float64(m.cfg.SampleRate))
	// Emit in packetization-sized chunks so the output drops straight into the
	// RTP sender without regrouping.
	chunk := m.cfg.SampleRate / 50

	out := make(chan Audio, mockAudioBuffer)
	go func() {
		defer close(out)

		for off := 0; off < totalSamples; off += chunk {
			n := chunk
			if off+n > totalSamples {
				n = totalSamples - off
			}
			pcm := make([]int16, chunk) // zero-padded tail, as on the wire
			for i := 0; i < n; i++ {
				t := float64(off+i) / float64(m.cfg.SampleRate)
				pcm[i] = int16(mockAmp * math.MaxInt16 * math.Sin(2*math.Pi*freq*t))
			}
			select {
			case <-ctx.Done():
				return
			case out <- Audio{PCM: pcm}:
			}
		}
	}()

	return out, nil
}

// TextFrequency maps text to a tone frequency, deterministically. Exported so
// tests can assert that the audio corresponds to the text that produced it,
// which is how the pipeline test verifies the agent's reply actually reached
// the caller rather than some other utterance.
func TextFrequency(text string) float64 {
	const fnvOffset, fnvPrime = 2166136261, 16777619
	h := uint32(fnvOffset)
	for i := 0; i < len(text); i++ {
		h ^= uint32(text[i])
		h *= fnvPrime
	}
	return mockMinFreq + float64(h%(mockMaxFreq-mockMinFreq))
}
