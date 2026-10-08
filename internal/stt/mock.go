package stt

import (
	"context"
	"fmt"
	"strings"

	"github.com/mharner33/voice-demo/internal/codec"
)

// DefaultPhrases are the utterances the mock recognizes when none are
// configured. They are phrased like real contact-centre audio so that demo
// transcripts, and the LLM replies derived from them, read plausibly.
var DefaultPhrases = []string{
	"hello I'm calling about my account balance",
	"my account number is four seven two nine",
	"I'd like to dispute a charge from last month",
	"can you transfer me to billing please",
	"thank you that's all I needed today",
}

// Mock defaults, in 20 ms frames: a partial every 500 ms and an utterance every
// two seconds, which is roughly the cadence a streaming recognizer produces on
// conversational speech.
const (
	DefaultFramesPerPartial   = 25
	DefaultFramesPerUtterance = 100
	DefaultConfidence         = 0.95
)

// MockConfig parameterizes the mock transcriber.
type MockConfig struct {
	// Phrases are the utterances to emit, in order, cycling if the call runs
	// long enough to exhaust them. Empty means DefaultPhrases.
	Phrases []string

	// FramesPerPartial is how much audio passes between partial hypotheses.
	FramesPerPartial int

	// FramesPerUtterance is how much audio is consumed before an utterance is
	// finalized and the next one begins.
	FramesPerUtterance int

	// Confidence is reported on final results.
	Confidence float64

	// DegradeOnConcealed lowers reported confidence in proportion to how much
	// of the utterance's audio the jitter buffer had to synthesize. This is
	// what makes packet loss visible in the AI layer rather than only in the
	// network layer, which is the connection the demo exists to draw.
	DegradeOnConcealed bool
}

func (c *MockConfig) applyDefaults() {
	if len(c.Phrases) == 0 {
		c.Phrases = DefaultPhrases
	}
	if c.FramesPerPartial == 0 {
		c.FramesPerPartial = DefaultFramesPerPartial
	}
	if c.FramesPerUtterance == 0 {
		c.FramesPerUtterance = DefaultFramesPerUtterance
	}
	if c.Confidence == 0 {
		c.Confidence = DefaultConfidence
	}
}

// Validate rejects configurations that would produce nonsense output.
func (c MockConfig) Validate() error {
	if c.FramesPerPartial < 1 {
		return fmt.Errorf("stt: FramesPerPartial = %d, want >= 1", c.FramesPerPartial)
	}
	if c.FramesPerUtterance < 1 {
		return fmt.Errorf("stt: FramesPerUtterance = %d, want >= 1", c.FramesPerUtterance)
	}
	if c.FramesPerUtterance < c.FramesPerPartial {
		return fmt.Errorf("stt: FramesPerUtterance = %d < FramesPerPartial = %d",
			c.FramesPerUtterance, c.FramesPerPartial)
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return fmt.Errorf("stt: Confidence = %v, want 0-1", c.Confidence)
	}
	for i, p := range c.Phrases {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("stt: Phrases[%d] is empty", i)
		}
	}
	return nil
}

// Mock is a deterministic Transcriber.
//
// Its output is a function of how much audio it has consumed, never of
// wall-clock time. That makes a pipeline test exactly reproducible and keeps it
// instant, which is what allows the mock to be the default provider for every
// later phase's tests.
type Mock struct {
	cfg MockConfig
}

// NewMock creates a mock transcriber. The zero MockConfig is valid.
func NewMock(cfg MockConfig) (*Mock, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Mock{cfg: cfg}, nil
}

// Info identifies the mock.
func (m *Mock) Info() Info {
	return Info{Provider: "mock", Model: "scripted", SampleRate: codec.SampleRate8k}
}

// mockResultBuffer is how many results the mock will queue before blocking. A
// few hundred milliseconds of hypotheses is plenty of slack for a caller that
// is draining, and bounding it means a caller that stops draining blocks rather
// than growing without limit.
const mockResultBuffer = 32

// Stream consumes audio and emits scripted hypotheses.
func (m *Mock) Stream(ctx context.Context, audio <-chan Audio) (<-chan Result, error) {
	out := make(chan Result, mockResultBuffer)

	go func() {
		defer close(out)

		var (
			framesTotal     int
			utteranceStart  int
			concealedFrames int
			phraseIdx       int
		)

		emit := func(r Result) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- r:
				return true
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-audio:
				if !ok {
					// End of stream. A real recognizer finalizes whatever it
					// has rather than discarding a partial utterance, so an
					// utterance in progress is emitted as a final.
					if framesTotal > utteranceStart {
						emit(m.finalResult(phraseIdx, framesTotal, framesTotal-utteranceStart, concealedFrames))
					}
					return
				}

				framesTotal++
				if chunk.Concealed {
					concealedFrames++
				}
				into := framesTotal - utteranceStart

				switch {
				case into >= m.cfg.FramesPerUtterance:
					if !emit(m.finalResult(phraseIdx, framesTotal, into, concealedFrames)) {
						return
					}
					phraseIdx++
					utteranceStart = framesTotal
					concealedFrames = 0

				case into%m.cfg.FramesPerPartial == 0:
					if !emit(m.partialResult(phraseIdx, framesTotal, into)) {
						return
					}
				}
			}
		}
	}()

	return out, nil
}

// phrase returns the utterance for an index, cycling through the configured set.
func (m *Mock) phrase(idx int) string {
	return m.cfg.Phrases[idx%len(m.cfg.Phrases)]
}

// partialResult builds a growing prefix of the current utterance, which is how
// a streaming recognizer reveals a hypothesis before committing to it.
func (m *Mock) partialResult(phraseIdx, framesTotal, into int) Result {
	words := strings.Fields(m.phrase(phraseIdx))

	// Reveal words in proportion to the audio consumed so far.
	n := len(words) * into / m.cfg.FramesPerUtterance
	if n < 1 {
		n = 1
	}
	if n > len(words) {
		n = len(words)
	}

	return Result{
		Text:          strings.Join(words[:n], " "),
		IsFinal:       false,
		AudioDuration: codec.Duration8k(framesTotal * codec.SamplesPerFrame),
	}
}

func (m *Mock) finalResult(phraseIdx, framesTotal, into, concealed int) Result {
	conf := m.cfg.Confidence
	if m.cfg.DegradeOnConcealed && into > 0 {
		conf *= 1 - float64(concealed)/float64(into)
		if conf < 0 {
			conf = 0
		}
	}
	return Result{
		Text:          m.phrase(phraseIdx),
		IsFinal:       true,
		Confidence:    conf,
		AudioDuration: codec.Duration8k(framesTotal * codec.SamplesPerFrame),
	}
}
