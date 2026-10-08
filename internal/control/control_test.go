package control

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mharner33/voice-demo/internal/codec"
	controlpb "github.com/mharner33/voice-demo/proto/controlpb"
)

// fakeHandler stands in for the gateway.
type fakeHandler struct {
	mu sync.Mutex

	starts []StartRequest
	ends   []SenderReport
	endIDs []string

	startErr error
	endErr   error

	mediaPort int
	summary   CallSummary
}

func (h *fakeHandler) StartCall(_ context.Context, req StartRequest) (StartResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts = append(h.starts, req)
	if h.startErr != nil {
		return StartResult{}, h.startErr
	}
	return StartResult{
		CallID:    "c-test",
		MediaPort: h.mediaPort,
		Codec:     req.Codec,
		TraceID:   "trace-abc",
	}, nil
}

func (h *fakeHandler) EndCall(_ context.Context, callID string, r SenderReport) (CallSummary, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.endIDs = append(h.endIDs, callID)
	h.ends = append(h.ends, r)
	if h.endErr != nil {
		return CallSummary{}, h.endErr
	}
	return h.summary, nil
}

func (h *fakeHandler) lastStart(t *testing.T) StartRequest {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.starts) == 0 {
		t.Fatal("StartCall was never called")
	}
	return h.starts[len(h.starts)-1]
}

func (h *fakeHandler) lastEnd(t *testing.T) SenderReport {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ends) == 0 {
		t.Fatal("EndCall was never called")
	}
	return h.ends[len(h.ends)-1]
}

// serve starts a control plane on localhost and returns a connected client.
// Using a real listener rather than an in-memory one keeps the gRPC transport
// in the test, which is where a proto or codec mismatch would show up.
func serve(t *testing.T, h Handler) *Client {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Serve(ctx, lis, h); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	cl, err := Dial(lis.Addr().String())
	if err != nil {
		cancel()
		t.Fatalf("Dial: %v", err)
	}

	t.Cleanup(func() {
		cl.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the control plane did not shut down")
		}
	})
	return cl
}

func TestStartCallRoundTrip(t *testing.T) {
	h := &fakeHandler{mediaPort: 5004}
	cl := serve(t, h)

	res, err := cl.StartCall(context.Background(), StartRequest{
		From:           "+15551234567",
		To:             "+18005550100",
		Codec:          codec.PCMA,
		SSRC:           0xDEADBEEF,
		ReturnPort:     41234,
		NetworkProfile: "lossy-wan",
	})
	if err != nil {
		t.Fatalf("StartCall: %v", err)
	}

	if res.CallID != "c-test" {
		t.Errorf("CallID = %q", res.CallID)
	}
	if res.MediaPort != 5004 {
		t.Errorf("MediaPort = %d, want 5004", res.MediaPort)
	}
	if res.Codec != codec.PCMA {
		t.Errorf("Codec = %q, want PCMA — the negotiated codec must survive the round trip",
			res.Codec)
	}
	if res.TraceID != "trace-abc" {
		t.Errorf("TraceID = %q", res.TraceID)
	}

	got := h.lastStart(t)
	if got.From != "+15551234567" || got.To != "+18005550100" {
		t.Errorf("identifiers = %q/%q", got.From, got.To)
	}
	if got.SSRC != 0xDEADBEEF {
		t.Errorf("SSRC = %#x, want 0xdeadbeef", got.SSRC)
	}
	if got.ReturnPort != 41234 {
		t.Errorf("ReturnPort = %d, want 41234 — without it there is no return path",
			got.ReturnPort)
	}
	if got.NetworkProfile != "lossy-wan" {
		t.Errorf("NetworkProfile = %q; the gateway cannot learn this any other way",
			got.NetworkProfile)
	}
}

// TestEndCallCarriesTheSequenceRange is the field that matters most: without
// both ends of the range the gateway cannot account for loss at the edges of
// the stream.
func TestEndCallCarriesTheSequenceRange(t *testing.T) {
	h := &fakeHandler{summary: CallSummary{
		PacketsReceived: 242,
		PacketsExpected: 250,
		PacketsLost:     8,
		LossPct:         3.2,
		JitterMs:        0.3,
		MOS:             2.71,
		FramesConcealed: 8,
		ConcealPct:      3.2,
		Turns:           3,
		ToolCalls:       2,
		Transcripts:     []string{"hello", "goodbye"},
		Replies:         []string{"hi", "bye"},
		TraceID:         "trace-xyz",
	}}
	cl := serve(t, h)

	sum, err := cl.EndCall(context.Background(), "c-test", SenderReport{
		FramesOffered:  250,
		PacketsSent:    242,
		PacketsDropped: 8,
		FirstSequence:  1000,
		FinalSequence:  1249,
		HaveRange:      true,
		Reason:         "normal",
	})
	if err != nil {
		t.Fatalf("EndCall: %v", err)
	}

	got := h.lastEnd(t)
	if !got.HaveRange {
		t.Fatal("HaveRange did not survive the round trip")
	}
	if got.FirstSequence != 1000 {
		t.Errorf("FirstSequence = %d, want 1000", got.FirstSequence)
	}
	if got.FinalSequence != 1249 {
		t.Errorf("FinalSequence = %d, want 1249", got.FinalSequence)
	}
	if got.PacketsDropped != 8 || got.PacketsSent != 242 || got.FramesOffered != 250 {
		t.Errorf("sender accounting = %+v", got)
	}
	if got.Reason != "normal" {
		t.Errorf("Reason = %q", got.Reason)
	}

	// The gateway's answer must come back whole, since the client prints it as
	// a reconciliation.
	if sum.PacketsLost != 8 || sum.PacketsExpected != 250 || sum.PacketsReceived != 242 {
		t.Errorf("summary accounting = %+v", sum)
	}
	if sum.Turns != 3 || sum.ToolCalls != 2 {
		t.Errorf("summary turns/tools = %d/%d, want 3/2", sum.Turns, sum.ToolCalls)
	}
	if len(sum.Transcripts) != 2 || sum.Transcripts[0] != "hello" {
		t.Errorf("Transcripts = %v", sum.Transcripts)
	}
	if len(sum.Replies) != 2 || sum.Replies[1] != "bye" {
		t.Errorf("Replies = %v", sum.Replies)
	}
	if sum.TraceID != "trace-xyz" {
		t.Errorf("TraceID = %q", sum.TraceID)
	}
}

// TestEndCallRejectsHalfARange guards a subtle hazard: accepting one end of the
// range would leave the other edge uncounted while the loss figure was reported
// as exact. The blind spot is symmetric, so half a fix is worse than none.
func TestEndCallRejectsHalfARange(t *testing.T) {
	h := &fakeHandler{}
	cl := serve(t, h)

	for _, tc := range []struct {
		name               string
		haveFirst, haveEnd bool
	}{
		{"first only", true, false},
		{"final only", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Go through the generated client directly: the typed client cannot
			// express a half range, which is itself the point.
			_, err := cl.rpc.EndCall(context.Background(), &controlpb.EndCallRequest{
				CallId:            "c-test",
				HaveFirstSequence: tc.haveFirst,
				HaveFinalSequence: tc.haveEnd,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("EndCall with half a range = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestStartCallValidation(t *testing.T) {
	h := &fakeHandler{}
	cl := serve(t, h)

	tests := []struct {
		name string
		req  *controlpb.StartCallRequest
		want codes.Code
	}{
		{
			"zero ssrc",
			&controlpb.StartCallRequest{Ssrc: 0},
			codes.InvalidArgument,
		},
		{
			"unsupported ptime",
			&controlpb.StartCallRequest{Ssrc: 1, PtimeMs: 30},
			codes.InvalidArgument,
		},
		{
			"return port out of range",
			&controlpb.StartCallRequest{Ssrc: 1, ReturnPort: 70000},
			codes.InvalidArgument,
		},
		{
			"valid with defaults",
			&controlpb.StartCallRequest{Ssrc: 1},
			codes.OK,
		},
		{
			"valid 20ms ptime",
			&controlpb.StartCallRequest{Ssrc: 1, PtimeMs: 20},
			codes.OK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cl.rpc.StartCall(context.Background(), tc.req)
			if got := status.Code(err); got != tc.want {
				t.Errorf("code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}

// TestUnspecifiedCodecDefaultsToPCMU covers the telephony default, which is
// also what a bare RTP payload type 0 means.
func TestUnspecifiedCodecDefaultsToPCMU(t *testing.T) {
	h := &fakeHandler{}
	cl := serve(t, h)

	if _, err := cl.rpc.StartCall(context.Background(), &controlpb.StartCallRequest{
		Ssrc: 1, Codec: controlpb.Codec_CODEC_UNSPECIFIED,
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.lastStart(t).Codec; got != codec.PCMU {
		t.Errorf("Codec = %q, want PCMU", got)
	}
}

func TestErrorMapping(t *testing.T) {
	t.Run("call not found", func(t *testing.T) {
		cl := serve(t, &fakeHandler{endErr: ErrCallNotFound})
		_, err := cl.EndCall(context.Background(), "c-missing", SenderReport{})
		if status.Code(err) != codes.NotFound {
			t.Errorf("err = %v, want NotFound so a client can tell this apart "+
				"from a transport failure", err)
		}
	})

	t.Run("call already exists", func(t *testing.T) {
		cl := serve(t, &fakeHandler{startErr: ErrCallExists})
		_, err := cl.StartCall(context.Background(), StartRequest{SSRC: 1})
		if status.Code(err) != codes.AlreadyExists {
			t.Errorf("err = %v, want AlreadyExists", err)
		}
	})

	t.Run("other handler failure", func(t *testing.T) {
		cl := serve(t, &fakeHandler{startErr: errors.New("disk on fire")})
		_, err := cl.StartCall(context.Background(), StartRequest{SSRC: 1})
		if status.Code(err) != codes.Internal {
			t.Errorf("err = %v, want Internal", err)
		}
	})
}

func TestEndCallRequiresCallID(t *testing.T) {
	cl := serve(t, &fakeHandler{})
	_, err := cl.rpc.EndCall(context.Background(), &controlpb.EndCallRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestCodecConversionRoundTrip(t *testing.T) {
	for _, c := range []codec.Codec{codec.PCMU, codec.PCMA} {
		got, err := codecFromProto(codecToProto(c))
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if got != c {
			t.Errorf("%s round-tripped to %s", c, got)
		}
	}

	if _, err := codecFromProto(controlpb.Codec(99)); err == nil {
		t.Error("an unknown codec enum was accepted")
	}
}

func TestCallParamsWantsReturnAudio(t *testing.T) {
	if (CallParams{ReturnPort: 0}).WantsReturnAudio() {
		t.Error("port 0 should mean the caller is not listening")
	}
	if !(CallParams{ReturnPort: 1234}).WantsReturnAudio() {
		t.Error("a nonzero port should mean the caller is listening")
	}
}

func TestClientCloseIsSafe(t *testing.T) {
	var cl *Client
	if err := cl.Close(); err != nil {
		t.Errorf("Close on a nil Client = %v, want nil", err)
	}
}

// TestServeShutsDownGracefully matters because an EndCall in flight is the only
// chance to record a call; dropping it would lose the whole call's telemetry.
func TestServeShutsDownGracefully(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, lis, &fakeHandler{}) }()

	cl, err := Dial(lis.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if _, err := cl.StartCall(context.Background(), StartRequest{SSRC: 1}); err != nil {
		cancel()
		t.Fatalf("StartCall before shutdown: %v", err)
	}
	cl.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve = %v on shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of cancellation")
	}
}

// TestDialIsLazyAndFailsOnFirstCall documents the gRPC behavior a caller has to
// account for: grpc.NewClient does not connect, so an unreachable gateway is
// reported at the first RPC rather than at Dial. The client therefore cannot
// treat a successful Dial as evidence the gateway exists.
func TestDialIsLazyAndFailsOnFirstCall(t *testing.T) {
	// Port 1 on the loopback interface: nothing is listening.
	cl, err := Dial("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial = %v; NewClient is lazy and should not have connected", err)
	}
	defer cl.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := cl.StartCall(ctx, StartRequest{SSRC: 1}); err == nil {
		t.Error("StartCall against a dead address succeeded")
	}
}
