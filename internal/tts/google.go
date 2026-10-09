package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	"cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
	"google.golang.org/api/option"

	"github.com/mharner33/voice-demo/internal/codec"
)

// Google TTS defaults.
const (
	// DefaultGoogleVoice is a standard en-US neural voice. The plan named
	// "neural2-C"; the API wants the full voice name, and the language is a
	// separate field that must agree with it.
	DefaultGoogleVoice = "en-US-Neural2-C"

	// DefaultGoogleLanguage must match the voice's own language, or the
	// service substitutes a voice rather than failing.
	DefaultGoogleLanguage = "en-US"
)

// GoogleConfig parameterizes the real synthesizer.
type GoogleConfig struct {
	// Project is the Google Cloud project to bill, passed as the request's
	// quota project.
	//
	// It is optional only in the sense that a service account carries its own
	// project. With user credentials — a gcloud login, which is how a laptop
	// demo authenticates — the API refuses the request outright without one,
	// and the recognizer's project cannot stand in: that one travels in the
	// recognizer resource path, which this API has no equivalent of.
	Project string

	// Voice is the Google voice name; defaults to DefaultGoogleVoice. A per
	// call Options.Voice overrides it.
	Voice string

	// Language is a BCP-47 tag; defaults to DefaultGoogleLanguage.
	Language string

	// SampleRate of the audio to request. Defaults to the 8 kHz telephony
	// rate, which is what the return RTP stream carries, so nothing has to be
	// resampled on the way back out.
	SampleRate int

	// SpeakingRate scales the voice's natural speed, 0.25 to 2.0. Zero means
	// the voice's own rate.
	SpeakingRate float64

	// ClientOptions are passed to the API client, for tests that point it at a
	// local server and for a deployment that supplies credentials explicitly.
	ClientOptions []option.ClientOption
}

func (c *GoogleConfig) applyDefaults() {
	if c.Voice == "" {
		c.Voice = DefaultGoogleVoice
	}
	if c.Language == "" {
		c.Language = DefaultGoogleLanguage
	}
	if c.SampleRate == 0 {
		c.SampleRate = codec.SampleRate8k
	}
}

// Validate rejects a configuration that cannot work.
func (c GoogleConfig) Validate() error {
	if strings.TrimSpace(c.Language) == "" {
		return fmt.Errorf("tts: google: Language is empty")
	}
	if c.SampleRate != codec.SampleRate8k && c.SampleRate != codec.SampleRate16k {
		return fmt.Errorf("tts: google: SampleRate = %d, want 8000 or 16000", c.SampleRate)
	}
	if c.SpeakingRate != 0 && (c.SpeakingRate < 0.25 || c.SpeakingRate > 2.0) {
		return fmt.Errorf("tts: google: SpeakingRate = %v, want 0 or 0.25-2.0", c.SpeakingRate)
	}
	return nil
}

// Google is a Synthesizer backed by Cloud Text-to-Speech.
//
// It uses the unary SynthesizeSpeech call and chunks the result into
// packetization-sized frames, rather than the StreamingSynthesize call that
// this interface's streaming shape might suggest. Two reasons, both about what
// the demo needs to be true:
//
// Streaming synthesis is restricted to a subset of voices, so adopting it
// would silently narrow which voices work and fail at request time for the
// rest. And its responses are documented as 24 kHz LINEAR16 regardless of the
// requested rate, which would need a 3:1 resampler this project does not have
// — the only rates here are the 8 kHz on the wire and the 16 kHz speech
// services prefer.
//
// What is lost is a genuinely lower time-to-first-byte. What is kept is the
// metric's meaning: first byte is still measured and still reported, it is
// simply the whole request's latency, which is exactly the silence the caller
// hears before the agent speaks.
type Google struct {
	cfg    GoogleConfig
	client *texttospeech.Client
}

// NewGoogle creates a synthesizer backed by the real API. Credentials come
// from Application Default Credentials unless ClientOptions override them.
func NewGoogle(ctx context.Context, cfg GoogleConfig) (*Google, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts := make([]option.ClientOption, 0, len(cfg.ClientOptions)+1)
	if cfg.Project != "" {
		opts = append(opts, option.WithQuotaProject(cfg.Project))
	}
	opts = append(opts, cfg.ClientOptions...)

	client, err := texttospeech.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("tts: google: creating the client: %w", err)
	}
	return &Google{cfg: cfg, client: client}, nil
}

// Info identifies the provider. The voice doubles as the model name, since
// that is what a Google voice actually selects.
func (g *Google) Info() Info {
	return Info{Provider: "google", Model: g.cfg.Voice, SampleRate: g.cfg.SampleRate}
}

// Close releases the client's connections.
func (g *Google) Close() error {
	return g.client.Close()
}

// googleAudioBuffer bounds how far ahead synthesized audio is queued. The
// whole utterance is already in hand by this point, so the buffer only
// decouples the chunker from the consumer.
const googleAudioBuffer = 16

// Synthesize speaks the text and streams it out in 20 ms frames.
func (g *Google) Synthesize(ctx context.Context, text string, opts Options) (<-chan Audio, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ErrEmptyText
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	voice := g.cfg.Voice
	if opts.Voice != "" {
		voice = opts.Voice
	}

	resp, err := g.client.SynthesizeSpeech(ctx, &texttospeechpb.SynthesizeSpeechRequest{
		Input: &texttospeechpb.SynthesisInput{
			InputSource: &texttospeechpb.SynthesisInput_Text{Text: text},
		},
		Voice: &texttospeechpb.VoiceSelectionParams{
			LanguageCode: g.cfg.Language,
			Name:         voice,
		},
		AudioConfig: &texttospeechpb.AudioConfig{
			// PCM, not LINEAR16. They are the same samples, but LINEAR16
			// responses carry a WAV header, and a header fed straight into the
			// G.711 encoder would be played to the caller as a short burst of
			// noise at the start of every reply.
			AudioEncoding:   texttospeechpb.AudioEncoding_PCM,
			SampleRateHertz: int32(g.cfg.SampleRate),
			SpeakingRate:    g.cfg.SpeakingRate,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("tts: google: synthesizing: %w", err)
	}

	pcm, err := decodeGooglePCM(resp.GetAudioContent())
	if err != nil {
		return nil, err
	}

	out := make(chan Audio, googleAudioBuffer)
	go func() {
		defer close(out)

		// Emit in packetization-sized chunks so the audio drops straight into
		// the RTP sender, as the mock does.
		chunk := g.cfg.SampleRate / 50
		for off := 0; off < len(pcm); off += chunk {
			end := off + chunk
			if end > len(pcm) {
				end = len(pcm)
			}
			select {
			case <-ctx.Done():
				return
			case out <- Audio{PCM: pcm[off:end]}:
			}
		}
	}()

	return out, nil
}

// wavMagic is the first four bytes of a RIFF/WAVE file.
var wavMagic = []byte("RIFF")

// decodeGooglePCM converts the response's bytes to linear PCM.
//
// A WAV header here would mean the request asked for the wrong encoding, so it
// is rejected rather than stripped: silently accepting it would leave the
// configuration wrong everywhere else it matters, including the sample rate the
// span reports.
func decodeGooglePCM(b []byte) ([]int16, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("tts: google: the response carried no audio")
	}
	if bytes.HasPrefix(b, wavMagic) {
		return nil, fmt.Errorf("tts: google: the response is WAV-wrapped, " +
			"so the request did not ask for headerless PCM")
	}
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("tts: google: %d bytes of 16-bit PCM is not a whole number of samples", len(b))
	}

	pcm := make([]int16, len(b)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return pcm, nil
}
