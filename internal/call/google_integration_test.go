//go:build integration

package call

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/llm"
	"github.com/mharner33/voice-demo/internal/stt"
	"github.com/mharner33/voice-demo/internal/tts"
)

// These tests run the pipeline against the real Google providers. They are
// behind the integration build tag because they cost money and need
// credentials; `make test` never runs them.
//
// The golden audio is synthesized by the other provider under test rather than
// read from a committed fixture. That is deliberate: a fixture of recorded
// speech would have to come from somewhere, and generating it here means the
// two halves of phase 7 check each other — if synthesis regresses, recognition
// notices, and the audio is regenerated at the voice quality of the day rather
// than frozen at whatever it was when someone recorded it.
//
// Transcripts are compared by word error rate, never by equality. A recognizer
// that hears "four seven two nine" as "4729" has not failed, and a test that
// demanded exact text would break on every model improvement.

// liveWER is the ceiling for clean synthesized speech over a simulated
// telephone line. Google's telephony model on its own synthesized speech
// should do far better than this; the threshold is set where a genuine
// regression lives, not where today's accuracy happens to sit.
const liveWER = 0.25

// The phrase is one of the mock's, so a real run and a mock run tell the same
// story in the dashboards.
const livePhrase = "hello I'm calling about my account balance"

func skipWithoutGoogleCredentials(t *testing.T) (project string) {
	t.Helper()

	project = os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		t.Skip("GOOGLE_CLOUD_PROJECT is not set; skipping the live provider test")
	}
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		return project
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(home + "/.config/gcloud/application_default_credentials.json"); err == nil {
			return project
		}
	}
	t.Skip("no Google credentials found; skipping the live provider test")
	return ""
}

// speak synthesizes a phrase with the real voice and returns it as the frames
// a caller's audio would arrive as, having been through G.711 both ways. The
// companding matters: the demo's claim is about telephone audio, and 8-bit
// µ-law quantization is part of what a recognizer has to cope with.
func speakOverTelephony(t *testing.T, text string) []stt.Audio {
	t.Helper()

	synth, err := tts.NewGoogle(context.Background(), tts.GoogleConfig{
		Project: os.Getenv("GOOGLE_CLOUD_PROJECT"),
	})
	if err != nil {
		t.Fatalf("tts.NewGoogle: %v", err)
	}
	defer func() { _ = synth.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stream, err := synth.Synthesize(ctx, text, tts.Options{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	var pcm []int16
	for chunk := range stream {
		pcm = append(pcm, chunk.PCM...)
	}
	if len(pcm) == 0 {
		t.Fatal("the synthesizer produced no audio to recognize")
	}

	var out []stt.Audio
	for _, frame := range codec.FramePCM(pcm) {
		// Out through the codec and back, as a real call's audio is.
		wire := codec.PCMU.Encode(frame)
		out = append(out, stt.Audio{PCM: codec.PCMU.Decode(wire)})
	}
	return out
}

// runLiveCall drives the pipeline with the real recognizer and the mock agent
// and synthesizer, so the only variable is recognition.
func runLiveCall(t *testing.T, project, callID string, audio []stt.Audio) Result {
	t.Helper()

	transcriber, err := stt.NewGoogle(context.Background(), stt.GoogleConfig{Project: project})
	if err != nil {
		t.Fatalf("stt.NewGoogle: %v", err)
	}
	defer func() { _ = transcriber.Close() }()

	tools, err := llm.DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	agent, err := llm.NewMock(llm.MockConfig{Registry: tools})
	if err != nil {
		t.Fatalf("llm.NewMock: %v", err)
	}
	synth, err := tts.NewMock(tts.MockConfig{})
	if err != nil {
		t.Fatalf("tts.NewMock: %v", err)
	}

	session, err := New(Config{
		CallID: callID,
		STT:    transcriber,
		LLM:    agent,
		TTS:    synth,
		Tools:  tools,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	in := make(chan stt.Audio)
	go func() {
		defer close(in)
		for _, chunk := range audio {
			// Paced at the true packetization interval. A recognizer's
			// endpointing works in real time, so blasting the audio through as
			// fast as the channel allows would not exercise the same code path
			// the gateway does.
			time.Sleep(codec.FrameDuration)
			in <- chunk
		}
	}()

	res, err := session.Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.STTErr != nil {
		t.Fatalf("the recognizer failed mid-call: %v", res.STTErr)
	}
	return res
}

// TestLiveRoundTrip is phase 7's acceptance test: synthesized speech, over the
// codec, through the real recognizer, into a reply and back out as audio.
func TestLiveRoundTrip(t *testing.T) {
	project := skipWithoutGoogleCredentials(t)

	audio := speakOverTelephony(t, livePhrase)
	res := runLiveCall(t, project, "live-roundtrip", audio)

	if len(res.Turns) == 0 {
		t.Fatal("the recognizer produced no final transcript")
	}

	// Partials are what the STT span reports as its time to first token, so an
	// absent one means that metric is silently missing from every chart.
	if !res.HavePartial {
		t.Error("no partial hypothesis arrived; time to first token would be unreported")
	} else if res.FirstPartialAt <= 0 || res.FirstPartialAt > 30*time.Second {
		t.Errorf("first partial at %v, implausible", res.FirstPartialAt)
	}

	turn := res.Turns[0]
	wer := stt.WordErrorRate(livePhrase, turn.Transcript)
	t.Logf("transcript %q (WER %.2f, confidence %.3f, unknown=%v), first partial at %v",
		turn.Transcript, wer, turn.Confidence, turn.ConfidenceUnknown, res.FirstPartialAt)

	if wer > liveWER {
		t.Errorf("word error rate %.2f exceeds %.2f for %q",
			wer, liveWER, turn.Transcript)
	}

	if turn.Reply == "" {
		t.Error("the agent produced no reply to speak")
	}
	if res.FramesOut == 0 {
		t.Error("no audio was synthesized back to the caller")
	}

	// Whichever answer the service gave about confidence, the two fields have
	// to agree: a reported figure must not be flagged unknown, and an
	// unreported one must not arrive as a confident zero.
	if turn.ConfidenceUnknown && turn.Confidence != 0 {
		t.Errorf("confidence %g is flagged unknown", turn.Confidence)
	}
	if !turn.ConfidenceUnknown && turn.Confidence <= 0 {
		t.Errorf("confidence %g is reported as known", turn.Confidence)
	}
}

// TestLiveConcealedAudioDegradesRecognition is the demo's central claim,
// checked against a real recognizer rather than a mock that was written to
// degrade. Replace a third of the frames with jitter-buffer filler and the
// transcript must get worse.
//
// The offline TestConcealedAudioDegradesTheEvaluation proves the plumbing
// reports degradation; only this proves the degradation is real.
func TestLiveConcealedAudioDegradesRecognition(t *testing.T) {
	project := skipWithoutGoogleCredentials(t)

	clean := speakOverTelephony(t, livePhrase)
	if len(clean) < 30 {
		t.Fatalf("only %d frames of audio; too short to lose a third of", len(clean))
	}

	// Every third frame becomes silence marked as concealed, which is what the
	// jitter buffer emits for a packet that never arrived.
	lossy := make([]stt.Audio, len(clean))
	for i, c := range clean {
		if i%3 == 1 {
			lossy[i] = stt.Audio{
				PCM:       make([]int16, codec.SamplesPerFrame),
				Concealed: true,
			}
			continue
		}
		lossy[i] = c
	}

	cleanRes := runLiveCall(t, project, "live-clean", clean)
	lossyRes := runLiveCall(t, project, "live-lossy", lossy)

	cleanText := firstTranscript(cleanRes)
	lossyText := firstTranscript(lossyRes)

	cleanWER := stt.WordErrorRate(livePhrase, cleanText)
	lossyWER := stt.WordErrorRate(livePhrase, lossyText)
	t.Logf("clean  %q (WER %.2f)", cleanText, cleanWER)
	t.Logf("lossy  %q (WER %.2f)", lossyText, lossyWER)

	if lossyWER <= cleanWER {
		t.Errorf("losing a third of the frames did not degrade the transcript: "+
			"clean WER %.2f, lossy WER %.2f", cleanWER, lossyWER)
	}
	if got := lossyRes.ConcealedFraction(); got < 0.3 {
		t.Errorf("concealed fraction = %.2f, want about a third", got)
	}
}

func firstTranscript(r Result) string {
	if len(r.Turns) == 0 {
		return ""
	}
	return r.Turns[0].Transcript
}
