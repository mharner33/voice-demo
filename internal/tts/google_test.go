package tts

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/mharner33/voice-demo/internal/codec"
)

// As with the recognizer, these tests drive the real SDK against a local gRPC
// server speaking the Text-to-Speech service, so the assertions are about the
// request Google would actually receive.

// fakeTTS records the request and returns scripted audio.
type fakeTTS struct {
	texttospeechpb.UnimplementedTextToSpeechServer

	audio []byte
	err   error

	mu  sync.Mutex
	req *texttospeechpb.SynthesizeSpeechRequest
}

func (f *fakeTTS) SynthesizeSpeech(ctx context.Context, req *texttospeechpb.SynthesizeSpeechRequest) (
	*texttospeechpb.SynthesizeSpeechResponse, error) {

	f.mu.Lock()
	f.req = req
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	return &texttospeechpb.SynthesizeSpeechResponse{AudioContent: f.audio}, nil
}

func (f *fakeTTS) request() *texttospeechpb.SynthesizeSpeechRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.req
}

func newTestGoogle(t *testing.T, fake *fakeTTS, cfg GoogleConfig) *Google {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := grpc.NewServer()
	texttospeechpb.RegisterTextToSpeechServer(srv, fake)

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
	cfg.ClientOptions = append(cfg.ClientOptions, option.WithGRPCConn(conn))

	g, err := NewGoogle(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewGoogle: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// pcmBytes renders samples as the little-endian bytes the API returns.
func pcmBytes(pcm []int16) []byte {
	out := make([]byte, 0, len(pcm)*2)
	for _, s := range pcm {
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}

// ramp is audio whose every sample is distinguishable, so a test can prove the
// samples arrived in order and none were lost at a chunk boundary.
func ramp(n int) []int16 {
	pcm := make([]int16, n)
	for i := range pcm {
		pcm[i] = int16(i % math.MaxInt16)
	}
	return pcm
}

func drain(t *testing.T, stream <-chan Audio) ([]int16, []int) {
	t.Helper()
	var (
		pcm   []int16
		sizes []int
	)
	for chunk := range stream {
		pcm = append(pcm, chunk.PCM...)
		sizes = append(sizes, len(chunk.PCM))
	}
	return pcm, sizes
}

// The request decides what the caller hears. A wrong encoding or rate here
// would not fail; it would play the reply back at the wrong speed or as noise.
func TestGoogleSynthesizeRequest(t *testing.T) {
	fake := &fakeTTS{audio: pcmBytes(ramp(800))}
	g := newTestGoogle(t, fake, GoogleConfig{})

	stream, err := g.Synthesize(context.Background(), "your balance is four hundred dollars", Options{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	drain(t, stream)

	req := fake.request()
	if req == nil {
		t.Fatal("the service never received a request")
	}
	if got, want := req.GetInput().GetText(), "your balance is four hundred dollars"; got != want {
		t.Errorf("text = %q, want %q", got, want)
	}
	if got, want := req.GetVoice().GetName(), DefaultGoogleVoice; got != want {
		t.Errorf("voice = %q, want %q", got, want)
	}
	if got, want := req.GetVoice().GetLanguageCode(), DefaultGoogleLanguage; got != want {
		t.Errorf("language = %q, want %q", got, want)
	}

	ac := req.GetAudioConfig()
	// PCM rather than LINEAR16: the latter carries a WAV header, and a header
	// companded into G.711 is a burst of noise at the start of every reply.
	if got, want := ac.GetAudioEncoding(), texttospeechpb.AudioEncoding_PCM; got != want {
		t.Errorf("encoding = %v, want %v (headerless)", got, want)
	}
	if got, want := ac.GetSampleRateHertz(), int32(codec.SampleRate8k); got != want {
		t.Errorf("sample rate = %d, want %d, the rate the return RTP stream carries", got, want)
	}
}

// Options.Voice overrides the configured voice, which is how the pipeline's
// per-call Voice setting is meant to work.
func TestGoogleSynthesizePerCallVoice(t *testing.T) {
	fake := &fakeTTS{audio: pcmBytes(ramp(160))}
	g := newTestGoogle(t, fake, GoogleConfig{Voice: "en-US-Neural2-A"})

	stream, err := g.Synthesize(context.Background(), "one moment please",
		Options{Voice: "en-US-Neural2-F"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	drain(t, stream)

	if got := fake.request().GetVoice().GetName(); got != "en-US-Neural2-F" {
		t.Errorf("voice = %q, want the per-call override", got)
	}
	// The configured voice is still what Info reports, since that is the
	// provider's default rather than one call's choice.
	if got := g.Info().Model; got != "en-US-Neural2-A" {
		t.Errorf("Info().Model = %q, want the configured voice", got)
	}
}

// The audio has to come out in frame-sized chunks and in order: the pipeline
// hands each chunk straight to the RTP sender, which packetizes one chunk per
// packet.
func TestGoogleChunksAudioIntoFrames(t *testing.T) {
	// Two and a half frames, so the short tail is exercised too.
	samples := codec.SamplesPerFrame*2 + 80
	want := ramp(samples)

	fake := &fakeTTS{audio: pcmBytes(want)}
	g := newTestGoogle(t, fake, GoogleConfig{})

	stream, err := g.Synthesize(context.Background(), "checking that for you", Options{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	got, sizes := drain(t, stream)

	wantSizes := []int{codec.SamplesPerFrame, codec.SamplesPerFrame, 80}
	if len(sizes) != len(wantSizes) {
		t.Fatalf("got %d chunks of sizes %v, want %v", len(sizes), sizes, wantSizes)
	}
	for i, w := range wantSizes {
		if sizes[i] != w {
			t.Errorf("chunk %d had %d samples, want %d", i, sizes[i], w)
		}
	}

	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d (byte order or chunking is wrong)",
				i, got[i], want[i])
		}
	}
}

// A WAV-wrapped response means the request asked for the wrong encoding. It is
// rejected rather than stripped, because quietly accepting it would leave the
// sample rate — which the span reports — coming from a header nobody read.
func TestGoogleRejectsWAVWrappedAudio(t *testing.T) {
	wav := append([]byte("RIFF"), pcmBytes(ramp(160))...)
	fake := &fakeTTS{audio: wav}
	g := newTestGoogle(t, fake, GoogleConfig{})

	_, err := g.Synthesize(context.Background(), "hello", Options{})
	if err == nil {
		t.Fatal("a WAV-wrapped response was accepted")
	}
	if !strings.Contains(err.Error(), "WAV") {
		t.Errorf("error = %q, want it to name the WAV wrapper", err)
	}
}

func TestGoogleRejectsEmptyAudio(t *testing.T) {
	fake := &fakeTTS{audio: nil}
	g := newTestGoogle(t, fake, GoogleConfig{})

	_, err := g.Synthesize(context.Background(), "hello", Options{})
	if err == nil {
		t.Fatal("an empty response was accepted; the caller would hear silence")
	}
}

func TestGoogleRejectsEmptyText(t *testing.T) {
	fake := &fakeTTS{audio: pcmBytes(ramp(160))}
	g := newTestGoogle(t, fake, GoogleConfig{})

	if _, err := g.Synthesize(context.Background(), "   ", Options{}); err != ErrEmptyText {
		t.Errorf("error = %v, want ErrEmptyText", err)
	}
	if fake.request() != nil {
		t.Error("an empty utterance was still sent to the service")
	}
}

func TestGoogleReportsServiceErrors(t *testing.T) {
	fake := &fakeTTS{err: status.Error(codes.InvalidArgument, "voice not found")}
	g := newTestGoogle(t, fake, GoogleConfig{})

	_, err := g.Synthesize(context.Background(), "hello", Options{})
	if err == nil {
		t.Fatal("a failed synthesis returned no error")
	}
	if !strings.Contains(err.Error(), "voice not found") {
		t.Errorf("error = %q, want the service's own message", err)
	}
}

func TestGoogleConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  GoogleConfig
	}{
		{"unsupported sample rate", GoogleConfig{SampleRate: 24000}},
		{"speaking rate too slow", GoogleConfig{SpeakingRate: 0.1}},
		{"speaking rate too fast", GoogleConfig{SpeakingRate: 3}},
		{"blank language", GoogleConfig{Language: " "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.applyDefaults()
			if err := cfg.Validate(); err == nil {
				t.Errorf("Validate accepted %+v", tt.cfg)
			}
		})
	}
}

func TestGoogleInfo(t *testing.T) {
	fake := &fakeTTS{audio: pcmBytes(ramp(160))}
	g := newTestGoogle(t, fake, GoogleConfig{})

	info := g.Info()
	if info.Provider != "google" {
		t.Errorf("provider = %q, want %q", info.Provider, "google")
	}
	if info.SampleRate != codec.SampleRate8k {
		t.Errorf("sample rate = %d, want %d", info.SampleRate, codec.SampleRate8k)
	}
}
