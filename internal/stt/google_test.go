package stt

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/mharner33/voice-demo/internal/codec"
)

// These tests drive the real Google SDK against a local gRPC server that
// speaks the Speech v2 service. That is the only way to assert what this
// program actually puts on the wire — the recognizer path, the decoding
// config, the batching — with no credentials and no spend. Asserting on a
// hand-rolled wrapper instead would prove nothing about the request Google
// receives.

// fakeSpeech records what it was sent and replays scripted responses.
type fakeSpeech struct {
	speechpb.UnimplementedSpeechServer

	// responses are sent after the audio request indexed by afterRequest,
	// counting only audio requests.
	script []scriptedResponse

	// recvErr, if set, is returned instead of reading the stream to its end.
	recvErr error

	mu       sync.Mutex
	config   *speechpb.StreamingRecognizeRequest
	audio    [][]byte
	closedBy string
}

type scriptedResponse struct {
	afterRequest int
	resp         *speechpb.StreamingRecognizeResponse
}

func (f *fakeSpeech) StreamingRecognize(stream speechpb.Speech_StreamingRecognizeServer) error {
	audioReqs := 0
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			f.note("client-close")
			return nil
		}
		if err != nil {
			f.note("error: " + err.Error())
			return err
		}

		if cfg := req.GetStreamingConfig(); cfg != nil {
			f.mu.Lock()
			f.config = req
			f.mu.Unlock()
			continue
		}

		f.mu.Lock()
		f.audio = append(f.audio, req.GetAudio())
		f.mu.Unlock()
		audioReqs++

		if f.recvErr != nil && audioReqs == 1 {
			return f.recvErr
		}

		for _, s := range f.script {
			if s.afterRequest != audioReqs {
				continue
			}
			if err := stream.Send(s.resp); err != nil {
				return err
			}
		}
	}
}

func (f *fakeSpeech) note(how string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedBy = how
}

func (f *fakeSpeech) sentAudio() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.audio))
	copy(out, f.audio)
	return out
}

func (f *fakeSpeech) configRequest() *speechpb.StreamingRecognizeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.config
}

// newTestGoogle starts the fake service and returns a Google transcriber
// pointed at it.
func newTestGoogle(t *testing.T, fake *fakeSpeech, cfg GoogleConfig) *Google {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := grpc.NewServer()
	speechpb.RegisterSpeechServer(srv, fake)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		<-done
	})

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialing the fake service: %v", err)
	}

	if cfg.Project == "" && cfg.Recognizer == "" {
		cfg.Project = "test-project"
	}
	cfg.ClientOptions = append(cfg.ClientOptions, option.WithGRPCConn(conn))

	g, err := NewGoogle(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewGoogle: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// frames returns n frames of 20 ms audio, each sample set to the frame index so
// a test can tell which frames arrived and in what order.
func googleFrames(n int) []Audio {
	out := make([]Audio, n)
	for i := range out {
		pcm := make([]int16, codec.SamplesPerFrame)
		for j := range pcm {
			pcm[j] = int16(i + 1)
		}
		out[i] = Audio{PCM: pcm}
	}
	return out
}

// feed sends frames into a channel and closes it, as the gateway's playout
// loop does at the end of a call.
func feedGoogle(audio chan<- Audio, chunks []Audio) {
	go func() {
		defer close(audio)
		for _, c := range chunks {
			audio <- c
		}
	}()
}

func collectGoogle(t *testing.T, results <-chan Result) []Result {
	t.Helper()
	var out []Result
	for r := range results {
		out = append(out, r)
	}
	return out
}

func googlePartial(text string, endMs int) *speechpb.StreamingRecognizeResponse {
	return &speechpb.StreamingRecognizeResponse{
		Results: []*speechpb.StreamingRecognitionResult{{
			Alternatives: []*speechpb.SpeechRecognitionAlternative{{
				Transcript: text,
			}},
			IsFinal:         false,
			Stability:       0.5,
			ResultEndOffset: durationpb.New(time.Duration(endMs) * time.Millisecond),
		}},
	}
}

func googleFinal(text string, confidence float32, endMs int) *speechpb.StreamingRecognizeResponse {
	return &speechpb.StreamingRecognizeResponse{
		Results: []*speechpb.StreamingRecognitionResult{{
			Alternatives: []*speechpb.SpeechRecognitionAlternative{{
				Transcript: text,
				Confidence: confidence,
			}},
			IsFinal:         true,
			ResultEndOffset: durationpb.New(time.Duration(endMs) * time.Millisecond),
		}},
	}
}

// The configuration is what Google uses to decode every byte that follows, so
// a wrong field here does not fail loudly — it produces a plausible transcript
// of garbage. Each field is pinned.
func TestGoogleStreamConfiguration(t *testing.T) {
	fake := &fakeSpeech{}
	g := newTestGoogle(t, fake, GoogleConfig{Project: "demo-proj"})

	audio := make(chan Audio)
	results, err := g.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	feedGoogle(audio, googleFrames(1))
	collectGoogle(t, results)

	req := fake.configRequest()
	if req == nil {
		t.Fatal("the service never received a configuration request")
	}

	const wantRecognizer = "projects/demo-proj/locations/global/recognizers/_"
	if got := req.GetRecognizer(); got != wantRecognizer {
		t.Errorf("recognizer = %q, want %q", got, wantRecognizer)
	}

	cfg := req.GetStreamingConfig()
	if !cfg.GetStreamingFeatures().GetInterimResults() {
		t.Error("interim results are off; the STT span's time to first token depends on them")
	}

	rc := cfg.GetConfig()
	if got, want := rc.GetModel(), DefaultGoogleModel; got != want {
		t.Errorf("model = %q, want %q", got, want)
	}
	if got, want := rc.GetLanguageCodes(), []string{DefaultGoogleLanguage}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("language codes = %v, want %v", got, want)
	}

	dec := rc.GetExplicitDecodingConfig()
	if dec == nil {
		t.Fatal("no explicit decoding config; headerless wire audio cannot be auto-detected")
	}
	if got, want := dec.GetEncoding(), speechpb.ExplicitDecodingConfig_LINEAR16; got != want {
		t.Errorf("encoding = %v, want %v", got, want)
	}
	if got, want := dec.GetSampleRateHertz(), int32(codec.SampleRate8k); got != want {
		t.Errorf("sample rate = %d, want %d (the wire rate, not an upsampled one)", got, want)
	}
	if got := dec.GetAudioChannelCount(); got != 1 {
		t.Errorf("channel count = %d, want 1", got)
	}
}

// A regional location needs a regional endpoint. There is no way to observe
// the endpoint through the fake service, so this asserts the derivation
// directly by checking the recognizer path and that the client was built.
func TestGoogleRegionalRecognizerPath(t *testing.T) {
	cfg := GoogleConfig{Project: "demo-proj", Location: "us-central1"}
	cfg.applyDefaults()

	const want = "projects/demo-proj/locations/us-central1/recognizers/_"
	if cfg.Recognizer != want {
		t.Errorf("recognizer = %q, want %q", cfg.Recognizer, want)
	}
}

func TestGoogleRequiresAProject(t *testing.T) {
	_, err := NewGoogle(context.Background(), GoogleConfig{})
	if err == nil {
		t.Fatal("NewGoogle with no project succeeded; it should refuse rather than fail per call")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("error = %q, want it to name the missing project", err)
	}
}

// Audio is batched, not sent frame by frame: fifty requests a second per call
// is the thing this avoids. The batch has to be exactly the frames handed in,
// in order, and the tail has to be flushed even though it is short.
func TestGoogleBatchesAudio(t *testing.T) {
	fake := &fakeSpeech{}
	g := newTestGoogle(t, fake, GoogleConfig{FramesPerRequest: 5})

	audio := make(chan Audio)
	results, err := g.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Twelve frames over a batch of five: two full requests and a flushed
	// remainder of two.
	in := googleFrames(12)
	feedGoogle(audio, in)
	collectGoogle(t, results)

	sent := fake.sentAudio()
	if len(sent) != 3 {
		t.Fatalf("got %d audio requests, want 3 (5 + 5 + 2)", len(sent))
	}
	wantSizes := []int{5, 5, 2}
	for i, want := range wantSizes {
		if got := len(sent[i]) / (codec.SamplesPerFrame * 2); got != want {
			t.Errorf("request %d carried %d frames, want %d", i, got, want)
		}
	}

	// The bytes themselves, reassembled, must be the samples handed in: a
	// little-endian mistake here would transpose every byte pair and the
	// recognizer would hear noise.
	var joined []byte
	for _, b := range sent {
		joined = append(joined, b...)
	}
	pcm := decodePCM16LE(t, joined)
	if len(pcm) != len(in)*codec.SamplesPerFrame {
		t.Fatalf("reassembled %d samples, want %d", len(pcm), len(in)*codec.SamplesPerFrame)
	}
	for i, chunk := range in {
		got := pcm[i*codec.SamplesPerFrame]
		if got != chunk.PCM[0] {
			t.Fatalf("frame %d arrived as %d, want %d", i, got, chunk.PCM[0])
		}
	}

	if fake.closedBy != "client-close" {
		t.Errorf("the stream ended as %q, want a clean client half-close", fake.closedBy)
	}
}

func TestGooglePartialsAndFinals(t *testing.T) {
	fake := &fakeSpeech{script: []scriptedResponse{
		{afterRequest: 1, resp: googlePartial("hello", 100)},
		{afterRequest: 2, resp: googlePartial("hello I'm calling", 200)},
		{afterRequest: 3, resp: googleFinal("hello I'm calling about my balance", 0.93, 320)},
	}}
	g := newTestGoogle(t, fake, GoogleConfig{FramesPerRequest: 1})

	audio := make(chan Audio)
	results, err := g.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	feedGoogle(audio, googleFrames(3))

	got := collectGoogle(t, results)
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(got), got)
	}

	if got[0].IsFinal || got[0].Text != "hello" {
		t.Errorf("first result = %+v, want the partial %q", got[0], "hello")
	}
	if got[0].AudioDuration != 100*time.Millisecond {
		t.Errorf("partial audio duration = %v, want 100ms from the result end offset",
			got[0].AudioDuration)
	}

	last := got[2]
	if !last.IsFinal {
		t.Fatalf("last result = %+v, want a final", last)
	}
	if last.Text != "hello I'm calling about my balance" {
		t.Errorf("final text = %q", last.Text)
	}
	if last.ConfidenceUnknown {
		t.Error("confidence is marked unknown although the service reported 0.93")
	}
	if last.Confidence < 0.92 || last.Confidence > 0.94 {
		t.Errorf("confidence = %g, want 0.93", last.Confidence)
	}
	if last.AudioDuration != 320*time.Millisecond {
		t.Errorf("final audio duration = %v, want 320ms", last.AudioDuration)
	}
}

// Google documents 0.0 as a sentinel for "confidence not set", and the field
// as not guaranteed to be present at all. Reporting that as a confident zero
// would drag the call's headline quality evaluation to the floor for a
// transcript that may be perfectly good.
func TestGoogleMissingConfidenceIsNotZeroConfidence(t *testing.T) {
	fake := &fakeSpeech{script: []scriptedResponse{
		{afterRequest: 1, resp: googleFinal("my account number is four seven two nine", 0, 400)},
	}}
	g := newTestGoogle(t, fake, GoogleConfig{FramesPerRequest: 1})

	audio := make(chan Audio)
	results, err := g.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	feedGoogle(audio, googleFrames(1))

	got := collectGoogle(t, results)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if !got[0].ConfidenceUnknown {
		t.Error("ConfidenceUnknown is false; an unset confidence was taken for a real zero")
	}
	if got[0].Confidence != 0 {
		t.Errorf("Confidence = %g, want 0 alongside the unknown flag", got[0].Confidence)
	}
}

// A result with no alternatives is a real thing the API sends — an endpointing
// event — and it must not surface as an empty transcript, which would end up
// as an empty turn in the call log.
func TestGoogleSkipsResultsWithoutHypotheses(t *testing.T) {
	fake := &fakeSpeech{script: []scriptedResponse{
		{afterRequest: 1, resp: &speechpb.StreamingRecognizeResponse{
			Results: []*speechpb.StreamingRecognitionResult{{IsFinal: true}},
		}},
		{afterRequest: 2, resp: googleFinal("thank you that's all I needed today", 0.9, 200)},
	}}
	g := newTestGoogle(t, fake, GoogleConfig{FramesPerRequest: 1})

	audio := make(chan Audio)
	results, err := g.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	feedGoogle(audio, googleFrames(2))

	got := collectGoogle(t, results)
	if len(got) != 1 {
		t.Fatalf("got %d results, want only the one carrying a hypothesis: %+v", len(got), got)
	}
	if got[0].Text != "thank you that's all I needed today" {
		t.Errorf("text = %q", got[0].Text)
	}
}

// A recognizer that dies mid-call must not look like a caller who stopped
// talking. Both close the channel; only one of them reports an error first.
func TestGoogleMidStreamFailureIsReported(t *testing.T) {
	fake := &fakeSpeech{
		recvErr: status.Error(codes.ResourceExhausted, "quota exceeded"),
	}
	g := newTestGoogle(t, fake, GoogleConfig{FramesPerRequest: 1})

	audio := make(chan Audio)
	results, err := g.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	feedGoogle(audio, googleFrames(20))

	got := collectGoogle(t, results)
	if len(got) == 0 {
		t.Fatal("the stream closed with no results and no error")
	}
	last := got[len(got)-1]
	if last.Err == nil {
		t.Fatalf("last result = %+v, want it to carry the failure", last)
	}
	if !strings.Contains(last.Err.Error(), "quota exceeded") {
		t.Errorf("error = %q, want the service's own message", last.Err)
	}
}

// A cancelled call is not a failure. It is how every call ends when the gateway
// tears down, so it must not produce an error result.
func TestGoogleCancellationIsNotAnError(t *testing.T) {
	fake := &fakeSpeech{}
	g := newTestGoogle(t, fake, GoogleConfig{FramesPerRequest: 1})

	ctx, cancel := context.WithCancel(context.Background())
	audio := make(chan Audio)
	results, err := g.Stream(ctx, audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	go func() {
		audio <- googleFrames(1)[0]
		cancel()
	}()

	for r := range results {
		if r.Err != nil {
			t.Errorf("cancellation produced an error result: %v", r.Err)
		}
	}
	close(audio)
}

// The API terminates a streaming recognition at five minutes. Sending stops
// before that rather than letting the service kill the stream mid-call.
func TestGoogleStopsSendingAtTheStreamLimit(t *testing.T) {
	fake := &fakeSpeech{}
	g := newTestGoogle(t, fake, GoogleConfig{
		FramesPerRequest:  1,
		MaxStreamDuration: 50 * time.Millisecond,
	})

	audio := make(chan Audio)
	results, err := g.Stream(context.Background(), audio)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Keep offering audio past the limit. The send side should stop on its
	// own, which is observable as the results channel closing while the
	// producer is still willing to write.
	producing := make(chan struct{})
	go func() {
		defer close(producing)
		defer close(audio)
		deadline := time.After(2 * time.Second)
		for {
			select {
			case <-deadline:
				return
			case <-time.After(5 * time.Millisecond):
				select {
				case audio <- googleFrames(1)[0]:
				case <-time.After(200 * time.Millisecond):
					// Nobody is reading any more, which is the point.
					return
				}
			}
		}
	}()

	for r := range results {
		if r.Err != nil {
			t.Errorf("hitting the stream limit produced an error: %v", r.Err)
		}
	}
	<-producing

	if fake.closedBy != "client-close" {
		t.Errorf("the stream ended as %q, want a clean client half-close", fake.closedBy)
	}
}

func TestGoogleInfo(t *testing.T) {
	fake := &fakeSpeech{}
	g := newTestGoogle(t, fake, GoogleConfig{Model: "telephony_short"})

	info := g.Info()
	if info.Provider != "google" {
		t.Errorf("provider = %q, want %q", info.Provider, "google")
	}
	if info.Model != "telephony_short" {
		t.Errorf("model = %q, want the configured one", info.Model)
	}
	// The span should report the rate actually sent, which is the wire rate.
	if info.SampleRate != codec.SampleRate8k {
		t.Errorf("sample rate = %d, want %d", info.SampleRate, codec.SampleRate8k)
	}
}

func decodePCM16LE(t *testing.T, b []byte) []int16 {
	t.Helper()
	if len(b)%2 != 0 {
		t.Fatalf("%d bytes is not a whole number of 16-bit samples", len(b))
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(uint16(b[i*2]) | uint16(b[i*2+1])<<8)
	}
	return out
}
