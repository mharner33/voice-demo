package loadgen

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
)

// Fixture is one call's worth of audio.
type Fixture struct {
	Name  string
	Audio codec.Audio
}

// Duration is how long the fixture speaks for.
func (f Fixture) Duration() time.Duration {
	return time.Duration(f.Audio.Duration() * float64(time.Second))
}

// Payloads encodes the fixture into wire frames once, so a load run companding
// the same audio for the hundredth time does not do it again.
func (f Fixture) Payloads(c codec.Codec) [][]byte {
	frames := codec.FramePCM(f.Audio.PCM)
	out := make([][]byte, len(frames))
	for i, fr := range frames {
		out[i] = c.Encode(fr)
	}
	return out
}

// Synthetic fixtures vary in *length* rather than in content, and the reason is
// worth knowing before reaching for better audio.
//
// The plan asked for several distinct WAV files so that transcripts and agent
// replies vary across a load run. With the mock recognizer they do — but not
// because of what the audio says. The mock finalizes an utterance every N
// frames consumed and cycles through its phrase list, so what determines a
// call's conversation is how much audio it carries: a four-second call has two
// turns, a nine-second call has five, and each starts from the same phrase.
// Varying the waveform would change nothing a dashboard can see.
//
// So these fixtures vary duration, which is what actually varies the
// conversation, and vary pitch only so a listener can tell them apart. With
// `-real-stt` the content does matter, and then the right fixtures are
// recordings of real speech — which is what -fixtures is for.
var syntheticShapes = []struct {
	name     string
	duration time.Duration
	pitchHz  float64
}{
	{"short-query", 2500 * time.Millisecond, 220},
	{"balance-check", 4500 * time.Millisecond, 294},
	{"dispute-charge", 6500 * time.Millisecond, 349},
	{"transfer-request", 8500 * time.Millisecond, 392},
	{"long-conversation", 11 * time.Second, 262},
}

// Speech-shaped envelope: bursts of tone separated by short gaps, so the audio
// has the on/off structure of someone talking rather than a continuous beep.
// Nothing downstream depends on this — it is so that a saved call sounds like
// turns when a person plays it back.
const (
	burstOn  = 900 * time.Millisecond
	burstOff = 250 * time.Millisecond
	burstAmp = 0.45
)

// SyntheticFixtures returns the built-in set: deterministic, needing no files,
// and varied in the one dimension the mock recognizer responds to.
func SyntheticFixtures() []Fixture {
	out := make([]Fixture, 0, len(syntheticShapes))
	for _, s := range syntheticShapes {
		out = append(out, Fixture{
			Name:  s.name,
			Audio: speechShapedTone(s.pitchHz, s.duration),
		})
	}
	return out
}

// speechShapedTone builds one fixture's audio.
func speechShapedTone(freqHz float64, d time.Duration) codec.Audio {
	rate := codec.SampleRate8k
	n := int(float64(rate) * d.Seconds())
	pcm := make([]int16, n)

	period := burstOn + burstOff
	for i := range pcm {
		t := float64(i) / float64(rate)
		phase := time.Duration(t*float64(time.Second)) % period
		if phase >= burstOn {
			continue // the pause between utterances
		}
		// A slow amplitude ramp at each burst's edges, so the audio does not
		// click — clicks are broadband and would show up as spurious energy to
		// anything measuring the signal.
		env := 1.0
		const fade = 30 * time.Millisecond
		if phase < fade {
			env = float64(phase) / float64(fade)
		} else if burstOn-phase < fade {
			env = float64(burstOn-phase) / float64(fade)
		}
		pcm[i] = int16(burstAmp * env * math.MaxInt16 * math.Sin(2*math.Pi*freqHz*t))
	}

	return codec.Audio{PCM: pcm, SampleRate: rate}
}

// LoadFixtures reads every WAV in a directory, resampling to the wire rate.
//
// This is the path for real speech: with a real recognizer the content is what
// varies the conversation, and no amount of synthesized tone substitutes for
// it.
func LoadFixtures(dir string) ([]Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("loadgen: reading the fixture directory: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".wav") {
			continue
		}
		names = append(names, e.Name())
	}
	// Sorted so a run is reproducible: the fixture a given call gets must not
	// depend on directory order.
	sort.Strings(names)

	out := make([]Fixture, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		audio, err := codec.ReadWAVFile(path)
		if err != nil {
			return nil, fmt.Errorf("loadgen: reading %s: %w", path, err)
		}
		if audio.SampleRate != codec.SampleRate8k {
			audio, err = codec.Resample(audio, codec.SampleRate8k)
			if err != nil {
				return nil, fmt.Errorf("loadgen: %s is %d Hz and cannot be resampled: %w",
					path, audio.SampleRate, err)
			}
		}
		if len(audio.PCM) == 0 {
			return nil, fmt.Errorf("loadgen: %s contains no audio", path)
		}
		out = append(out, Fixture{
			Name:  strings.TrimSuffix(name, filepath.Ext(name)),
			Audio: audio,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("loadgen: no .wav files in %s", dir)
	}
	return out, nil
}

// WriteFixtures writes a fixture set to a directory as WAV files, so the
// synthetic audio can be listened to, inspected, or passed back in with
// -file.
func WriteFixtures(dir string, fixtures []Fixture) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("loadgen: creating %s: %w", dir, err)
	}
	for _, f := range fixtures {
		path := filepath.Join(dir, f.Name+".wav")
		if err := codec.WriteWAVFile(path, f.Audio); err != nil {
			return fmt.Errorf("loadgen: writing %s: %w", path, err)
		}
	}
	return nil
}
