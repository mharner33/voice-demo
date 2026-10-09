package loadgen

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
)

// The built-in fixtures have to differ in duration, because duration is what
// varies the conversation: the mock recognizer finalizes an utterance every
// fixed number of frames, so a longer call has more turns. Fixtures that were
// all the same length would produce a load run of identical calls.
func TestSyntheticFixturesVaryInDuration(t *testing.T) {
	fixtures := SyntheticFixtures()
	if len(fixtures) < 3 {
		t.Fatalf("only %d built-in fixtures", len(fixtures))
	}

	seen := map[time.Duration]bool{}
	names := map[string]bool{}
	for _, f := range fixtures {
		if f.Audio.SampleRate != codec.SampleRate8k {
			t.Errorf("fixture %q is %d Hz, not the wire rate", f.Name, f.Audio.SampleRate)
		}
		d := f.Duration().Round(100 * time.Millisecond)
		if seen[d] {
			t.Errorf("fixture %q duplicates the duration %v", f.Name, d)
		}
		seen[d] = true

		if names[f.Name] {
			t.Errorf("fixture name %q is used twice", f.Name)
		}
		names[f.Name] = true
	}

	// The mock finalizes an utterance every two seconds of audio, so the set
	// has to span enough length to produce both single-turn and multi-turn
	// calls or every call in a run would have the same shape.
	var shortest, longest time.Duration
	for i, f := range fixtures {
		d := f.Duration()
		if i == 0 || d < shortest {
			shortest = d
		}
		if d > longest {
			longest = d
		}
	}
	if shortest > 3*time.Second {
		t.Errorf("the shortest fixture is %v; nothing produces a one-turn call", shortest)
	}
	if longest < 8*time.Second {
		t.Errorf("the longest fixture is %v; nothing produces a four-turn call", longest)
	}
}

// The audio has to be speech-shaped rather than a continuous tone: bursts with
// pauses, so a saved call sounds like turns when a person plays it back.
func TestSyntheticFixturesHaveSpeechShape(t *testing.T) {
	f := SyntheticFixtures()[2] // a long enough one to contain a pause

	var loud, silent int
	for _, s := range f.Audio.PCM {
		if s > 1000 || s < -1000 {
			loud++
		}
		if s == 0 {
			silent++
		}
	}
	if loud == 0 {
		t.Fatalf("fixture %q is silent", f.Name)
	}
	if silent == 0 {
		t.Errorf("fixture %q has no pauses; it is a continuous tone", f.Name)
	}
	// Mostly speech, with pauses — not the other way round.
	if silent > len(f.Audio.PCM)/2 {
		t.Errorf("fixture %q is %d%% silence", f.Name, 100*silent/len(f.Audio.PCM))
	}
}

func TestFixturePayloadsAreWireFrames(t *testing.T) {
	f := Fixture{Name: "t", Audio: codec.Tone(300, codec.SampleRate8k, time.Second, 0.5)}

	payloads := f.Payloads(codec.PCMU)
	if len(payloads) != 50 {
		t.Errorf("one second of audio produced %d frames, want 50", len(payloads))
	}
	for i, p := range payloads {
		if len(p) != codec.G711FrameBytes {
			t.Fatalf("frame %d is %d bytes, want %d", i, len(p), codec.G711FrameBytes)
		}
	}
}

// Writing and reloading a fixture set has to round-trip, since that is how the
// synthetic audio becomes something a person can listen to or pass back in
// with -file.
func TestFixtureWriteAndLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixtures")

	want := SyntheticFixtures()
	if err := WriteFixtures(dir, want); err != nil {
		t.Fatalf("WriteFixtures: %v", err)
	}

	got, err := LoadFixtures(dir)
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("loaded %d fixtures, wrote %d", len(got), len(want))
	}

	byName := map[string]Fixture{}
	for _, f := range got {
		byName[f.Name] = f
	}
	for _, w := range want {
		g, ok := byName[w.Name]
		if !ok {
			t.Errorf("fixture %q did not survive the round trip", w.Name)
			continue
		}
		if len(g.Audio.PCM) != len(w.Audio.PCM) {
			t.Errorf("fixture %q came back with %d samples, wrote %d",
				w.Name, len(g.Audio.PCM), len(w.Audio.PCM))
		}
		if g.Audio.SampleRate != w.Audio.SampleRate {
			t.Errorf("fixture %q came back at %d Hz", w.Name, g.Audio.SampleRate)
		}
	}
}

// Loading is sorted by name, because the fixture a given call gets must not
// depend on the order the filesystem happens to return.
func TestLoadFixturesIsOrdered(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zulu", "alpha", "mike"} {
		path := filepath.Join(dir, name+".wav")
		audio := codec.Tone(300, codec.SampleRate8k, 200*time.Millisecond, 0.5)
		if err := codec.WriteWAVFile(path, audio); err != nil {
			t.Fatal(err)
		}
	}
	// A non-WAV file must be ignored rather than failing the load.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFixtures(dir)
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("loaded %d fixtures, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("fixture %d is %q, want %q", i, got[i].Name, w)
		}
	}
}

func TestLoadFixturesRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := LoadFixtures(t.TempDir()); err == nil {
		t.Error("an empty fixture directory was accepted; the run would have no audio")
	}
	if _, err := LoadFixtures(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing fixture directory was accepted")
	}
}
