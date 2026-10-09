package stt

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mharner33/voice-demo/internal/codec"
)

// Google STT v2 defaults.
const (
	// DefaultGoogleModel is "telephony", not the "latest_long" this project's
	// plan originally named. That name belongs to the v1 API; v2's catalogue is
	// long, short, telephony, and the chirp family. For 8 kHz G.711 audio
	// arriving over RTP, telephony is the model actually trained on it.
	DefaultGoogleModel = "telephony"

	// DefaultGoogleLanguage is the BCP-47 tag for the recognizer.
	DefaultGoogleLanguage = "en-US"

	// DefaultGoogleLocation is the API location. Any other value needs a
	// regional endpoint, which NewGoogle derives.
	DefaultGoogleLocation = "global"

	// DefaultGoogleFramesPerRequest batches five 20 ms frames into one
	// request. Sending each frame separately would work, but it means fifty
	// RPCs a second per call carrying 320 bytes each; 100 ms is the cadence
	// real telephony integrations use and it costs nothing in responsiveness,
	// since partial hypotheses arrive hundreds of milliseconds apart anyway.
	DefaultGoogleFramesPerRequest = 5

	// DefaultGoogleMaxStreamDuration stops sending audio before the API's
	// own five-minute limit on a streaming recognition. Hitting that limit
	// returns an error mid-call; stopping short of it ends recognition
	// cleanly, which is the better failure for a demo. Calls here are seconds
	// long, so this only matters if someone leaves one running.
	DefaultGoogleMaxStreamDuration = 4*time.Minute + 30*time.Second
)

// googleImplicitRecognizer is the recognizer resource for an ad-hoc request:
// the "_" segment means "no stored Recognizer, take the config from this
// request", which is what this demo wants — the configuration lives in code
// rather than in a cloud resource someone has to create first.
const googleImplicitRecognizer = "projects/%s/locations/%s/recognizers/_"

// GoogleConfig parameterizes the real recognizer.
type GoogleConfig struct {
	// Project is the Google Cloud project that owns the request, and the one
	// that gets billed. Required unless Recognizer is set.
	Project string

	// Location defaults to DefaultGoogleLocation.
	Location string

	// Recognizer overrides the full resource name derived from Project and
	// Location, for a deployment that really does keep a stored Recognizer.
	Recognizer string

	// Model defaults to DefaultGoogleModel.
	Model string

	// Language is a BCP-47 tag; defaults to DefaultGoogleLanguage.
	Language string

	// FramesPerRequest batches 20 ms frames into one streaming request.
	FramesPerRequest int

	// MaxStreamDuration bounds how long audio is sent on one stream.
	MaxStreamDuration time.Duration

	// ClientOptions are passed to the API client, for tests that point it at a
	// local server and for a deployment that supplies credentials explicitly
	// rather than through the ambient environment.
	ClientOptions []option.ClientOption
}

func (c *GoogleConfig) applyDefaults() {
	if c.Location == "" {
		c.Location = DefaultGoogleLocation
	}
	if c.Model == "" {
		c.Model = DefaultGoogleModel
	}
	if c.Language == "" {
		c.Language = DefaultGoogleLanguage
	}
	if c.FramesPerRequest == 0 {
		c.FramesPerRequest = DefaultGoogleFramesPerRequest
	}
	if c.MaxStreamDuration == 0 {
		c.MaxStreamDuration = DefaultGoogleMaxStreamDuration
	}
	if c.Recognizer == "" && c.Project != "" {
		c.Recognizer = fmt.Sprintf(googleImplicitRecognizer, c.Project, c.Location)
	}
}

// Validate rejects a configuration that cannot work.
func (c GoogleConfig) Validate() error {
	if c.Recognizer == "" {
		return fmt.Errorf("stt: google: a project is required " +
			"(set -google-project or GOOGLE_CLOUD_PROJECT)")
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("stt: google: Model is empty")
	}
	if strings.TrimSpace(c.Language) == "" {
		return fmt.Errorf("stt: google: Language is empty")
	}
	if c.FramesPerRequest < 1 {
		return fmt.Errorf("stt: google: FramesPerRequest = %d, want >= 1", c.FramesPerRequest)
	}
	if c.MaxStreamDuration <= 0 {
		return fmt.Errorf("stt: google: MaxStreamDuration = %v, want > 0", c.MaxStreamDuration)
	}
	return nil
}

// Google is a Transcriber backed by Cloud Speech-to-Text v2.
//
// Audio goes out at the 8 kHz the wire already carries it at, with no
// resampling. The plan called for upsampling to 16 kHz because Google's docs
// recommend that rate, but the same docs are explicit that the recommendation
// is about the *source*: "if that's not possible, use the native sample rate
// of the audio source (instead of resampling)". Interpolating 8 kHz telephony
// audio to 16 kHz adds no information for the recognizer to use, doubles the
// bytes on the wire, and would make the span's reported sample rate a fiction.
type Google struct {
	cfg    GoogleConfig
	client *speech.Client
}

// NewGoogle creates a recognizer backed by the real API. Credentials come from
// Application Default Credentials unless ClientOptions override them, so a key
// never passes through this program's flags or logs.
func NewGoogle(ctx context.Context, cfg GoogleConfig) (*Google, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts := make([]option.ClientOption, 0, len(cfg.ClientOptions)+2)
	// User credentials — a gcloud login, which is how a laptop demo
	// authenticates — carry no project of their own, and the API rejects a
	// request that does not name one to charge. A service account has its own
	// project, so this is additive rather than required.
	if cfg.Project != "" {
		opts = append(opts, option.WithQuotaProject(cfg.Project))
	}
	// A regional location is served by a regional endpoint; the default
	// endpoint only answers for "global", and a mismatch fails at request
	// time with an error that does not mention endpoints.
	if cfg.Location != DefaultGoogleLocation {
		opts = append(opts, option.WithEndpoint(
			fmt.Sprintf("%s-speech.googleapis.com:443", cfg.Location)))
	}
	opts = append(opts, cfg.ClientOptions...)

	client, err := speech.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("stt: google: creating the client: %w", err)
	}
	return &Google{cfg: cfg, client: client}, nil
}

// Info identifies the provider. The sample rate is the wire rate, because that
// is what is actually sent.
func (g *Google) Info() Info {
	return Info{Provider: "google", Model: g.cfg.Model, SampleRate: codec.SampleRate8k}
}

// Close releases the client's connections.
func (g *Google) Close() error {
	return g.client.Close()
}

// googleResultBuffer matches the mock's, so switching providers does not change
// how much slack the pipeline has.
const googleResultBuffer = 32

// Stream opens a StreamingRecognize call and translates both directions.
//
// Sending and receiving run on separate goroutines because the API is
// full-duplex: hypotheses for audio already sent arrive while more audio is
// still going out, and that overlap is the whole point of a streaming
// recognizer.
func (g *Google) Stream(ctx context.Context, audio <-chan Audio) (<-chan Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stream, err := g.client.StreamingRecognize(ctx)
	if err != nil {
		return nil, fmt.Errorf("stt: google: opening the stream: %w", err)
	}

	// The configuration goes first, before any audio, and is sent here rather
	// than on the sending goroutine so that a rejected configuration surfaces
	// as an error from Stream instead of as a stream that quietly produces
	// nothing.
	if err := stream.Send(g.configRequest()); err != nil {
		return nil, fmt.Errorf("stt: google: sending the configuration: %w", err)
	}

	out := make(chan Result, googleResultBuffer)

	sendErr := make(chan error, 1)
	go func() { sendErr <- g.send(ctx, stream, audio) }()
	go g.receive(ctx, stream, out, sendErr)

	return out, nil
}

// configRequest builds the opening request of the stream.
func (g *Google) configRequest() *speechpb.StreamingRecognizeRequest {
	return &speechpb.StreamingRecognizeRequest{
		Recognizer: g.cfg.Recognizer,
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					// Explicit decoding, not auto: the audio is headerless PCM
					// off the wire, so there is nothing for the service to
					// detect and guessing would be its only option.
					DecodingConfig: &speechpb.RecognitionConfig_ExplicitDecodingConfig{
						ExplicitDecodingConfig: &speechpb.ExplicitDecodingConfig{
							Encoding:          speechpb.ExplicitDecodingConfig_LINEAR16,
							SampleRateHertz:   codec.SampleRate8k,
							AudioChannelCount: 1,
						},
					},
					Model:         g.cfg.Model,
					LanguageCodes: []string{g.cfg.Language},
				},
				StreamingFeatures: &speechpb.StreamingRecognitionFeatures{
					// Interim results are not optional for this demo: the
					// first partial is what the STT span reports as its time
					// to first token, and without them that metric would just
					// be the final transcript's latency under another name.
					InterimResults: true,
				},
			},
		},
	}
}

// send batches frames and writes them to the stream, closing the send side
// when the audio ends. It returns the error that ended sending, if any.
func (g *Google) send(ctx context.Context, stream speechpb.Speech_StreamingRecognizeClient,
	audio <-chan Audio) error {

	deadline := time.NewTimer(g.cfg.MaxStreamDuration)
	defer deadline.Stop()

	// Batch buffer, sized for the configured batch so a steady call does one
	// allocation per request rather than one per frame.
	batchBytes := g.cfg.FramesPerRequest * codec.SamplesPerFrame * 2
	buf := make([]byte, 0, batchBytes)
	frames := 0

	flush := func() error {
		if frames == 0 {
			return nil
		}
		err := stream.Send(&speechpb.StreamingRecognizeRequest{
			Recognizer:       g.cfg.Recognizer,
			StreamingRequest: &speechpb.StreamingRecognizeRequest_Audio{Audio: buf},
		})
		// A fresh buffer rather than a reslice: the sent message is handed to
		// the transport, and nothing in the gRPC contract promises the bytes
		// have been copied by the time Send returns.
		buf, frames = make([]byte, 0, batchBytes), 0
		return err
	}

	for {
		select {
		case <-ctx.Done():
			// No flush: the call is over and the bytes have nowhere useful to
			// go. CloseSend still runs so the receiver sees a clean end.
			return closeSend(stream, ctx.Err())

		case <-deadline.C:
			// The API caps a streaming recognition at five minutes. Stop
			// sending and let the results drain rather than letting the
			// service terminate the stream with an error mid-call.
			if err := flush(); err != nil {
				return closeSend(stream, err)
			}
			return closeSend(stream, nil)

		case chunk, ok := <-audio:
			if !ok {
				if err := flush(); err != nil {
					return closeSend(stream, err)
				}
				return closeSend(stream, nil)
			}

			// Concealed audio is sent like any other. It is what the caller
			// would have heard, so it is what the recognizer should hear: a
			// transcript degraded by packet loss is the demo's central claim,
			// and filtering the filler out here would hide it.
			buf = appendPCM16LE(buf, chunk.PCM)
			frames++
			if frames >= g.cfg.FramesPerRequest {
				if err := flush(); err != nil {
					return closeSend(stream, err)
				}
			}
		}
	}
}

// closeSend half-closes the stream, preferring an earlier error to the close's
// own: a send that already failed is the more useful explanation.
func closeSend(stream speechpb.Speech_StreamingRecognizeClient, err error) error {
	closeErr := stream.CloseSend()
	if err != nil {
		return err
	}
	return closeErr
}

// receive translates responses into results and closes the output channel.
//
// It owns the output channel, including reporting the sending goroutine's
// error, so that a consumer sees exactly one error and exactly one close no
// matter which half of the stream failed.
func (g *Google) receive(ctx context.Context, stream speechpb.Speech_StreamingRecognizeClient,
	out chan<- Result, sendErr <-chan error) {

	defer close(out)

	emit := func(r Result) bool {
		select {
		case <-ctx.Done():
			return false
		case out <- r:
			return true
		}
	}

	var recvErr error
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			recvErr = err
			break
		}
		for _, r := range resp.GetResults() {
			res, ok := convertGoogleResult(r)
			if !ok {
				continue
			}
			if !emit(res) {
				return
			}
		}
	}

	// Which half of the stream to believe. When the service aborts, the
	// sending goroutine learns only that the stream is gone — gRPC reports
	// that as io.EOF from Send and documents the real status as being
	// available from the receiving side. So an uninformative send error gives
	// way to the receive error, which is the one carrying the service's own
	// message.
	err := <-sendErr
	if err == nil || errors.Is(err, io.EOF) || isCancellation(err) {
		err = recvErr
	}
	if err != nil && !isCancellation(err) {
		emit(Result{Err: fmt.Errorf("stt: google: %w", err)})
	}
}

// isCancellation reports whether an error is just the call ending. Teardown
// cancels the context on every call, so this is the normal path and must not
// be reported as a recognizer failure. It arrives either as the context error
// or as the gRPC status the server turned it into.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled
}

// convertGoogleResult maps one streaming result onto this package's Result. The
// second return is false for a result carrying no hypothesis, which the API
// does send: an endpointing event arrives as a result with no alternatives.
func convertGoogleResult(r *speechpb.StreamingRecognitionResult) (Result, bool) {
	alts := r.GetAlternatives()
	if len(alts) == 0 {
		return Result{}, false
	}
	top := alts[0]

	res := Result{
		Text:    strings.TrimSpace(top.GetTranscript()),
		IsFinal: r.GetIsFinal(),
		// The service reports how far into the audio this result reaches,
		// which is a better answer than counting frames here: it is the
		// provider's own account of what it has consumed.
		AudioDuration: r.GetResultEndOffset().AsDuration(),
	}

	// Confidence is documented as set only on finals, and as "not guaranteed
	// to be accurate" with 0.0 as a sentinel for unset. Passing that zero on
	// would read as total uncertainty about a transcript the recognizer may be
	// perfectly sure of, and it is the call's headline quality figure, so an
	// absent value is reported as absent.
	if r.GetIsFinal() {
		if c := top.GetConfidence(); c > 0 {
			res.Confidence = float64(c)
		} else {
			res.ConfidenceUnknown = true
		}
	}

	return res, true
}

// appendPCM16LE appends linear PCM as the little-endian bytes the API expects.
func appendPCM16LE(dst []byte, pcm []int16) []byte {
	for _, s := range pcm {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(s))
	}
	return dst
}
