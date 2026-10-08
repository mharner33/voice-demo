package control

import (
	"context"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mharner33/voice-demo/internal/codec"
	controlpb "github.com/mharner33/voice-demo/proto/controlpb"
)

// ErrCallNotFound is returned by a Handler for an unknown call ID. The server
// maps it to gRPC NotFound so a client can distinguish "that call does not
// exist" from a transport failure.
var ErrCallNotFound = errors.New("control: no such call")

// ErrCallExists is returned when a setup request names an SSRC that is already
// in a call.
var ErrCallExists = errors.New("control: a call with that SSRC is already active")

// StartRequest is a validated setup request.
type StartRequest struct {
	From           string
	To             string
	Codec          codec.Codec
	SSRC           uint32
	ReturnPort     int
	NetworkProfile string
}

// StartResult is what the gateway answers with.
type StartResult struct {
	CallID    string
	MediaPort int
	Codec     codec.Codec
	TraceID   string
}

// Handler is the gateway side of the control plane. The gateway implements it;
// this package owns only the protocol.
type Handler interface {
	StartCall(ctx context.Context, req StartRequest) (StartResult, error)
	EndCall(ctx context.Context, callID string, report SenderReport) (CallSummary, error)
}

// Server adapts a Handler to the gRPC service.
type Server struct {
	controlpb.UnimplementedControlPlaneServer
	h Handler
}

// NewServer wraps a Handler.
func NewServer(h Handler) *Server { return &Server{h: h} }

// StartCall negotiates a call.
func (s *Server) StartCall(ctx context.Context, req *controlpb.StartCallRequest) (*controlpb.StartCallResponse, error) {
	if err := validateStart(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	c, err := codecFromProto(req.Codec)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	res, err := s.h.StartCall(ctx, StartRequest{
		From:           req.From,
		To:             req.To,
		Codec:          c,
		SSRC:           req.Ssrc,
		ReturnPort:     int(req.ReturnPort),
		NetworkProfile: req.NetworkProfile,
	})
	if err != nil {
		if errors.Is(err, ErrCallExists) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "starting the call: %v", err)
	}

	return &controlpb.StartCallResponse{
		CallId:    res.CallID,
		MediaPort: uint32(res.MediaPort),
		Codec:     codecToProto(res.Codec),
		TraceId:   res.TraceID,
	}, nil
}

// EndCall tears a call down and returns the gateway's accounting.
func (s *Server) EndCall(ctx context.Context, req *controlpb.EndCallRequest) (*controlpb.EndCallResponse, error) {
	if req == nil || req.CallId == "" {
		return nil, status.Error(codes.InvalidArgument, "control: call_id is required")
	}

	report := SenderReport{
		FramesOffered:  req.FramesOffered,
		PacketsSent:    req.PacketsSent,
		PacketsDropped: req.PacketsDropped,
		Reason:         req.Reason,
	}

	// Both ends are required together: half a range cannot fix a symmetric
	// blind spot, and accepting one would silently leave the other edge
	// uncounted while reporting the loss figure as exact.
	if req.HaveFirstSequence != req.HaveFinalSequence {
		return nil, status.Error(codes.InvalidArgument,
			"control: first_sequence and final_sequence must be supplied together")
	}
	if req.HaveFinalSequence {
		if req.FinalSequence > 65535 || req.FirstSequence > 65535 {
			return nil, status.Error(codes.InvalidArgument,
				"control: a sequence number exceeds the 16-bit RTP range")
		}
		report.HaveRange = true
		report.FirstSequence = uint16(req.FirstSequence)
		report.FinalSequence = uint16(req.FinalSequence)
	}

	sum, err := s.h.EndCall(ctx, req.CallId, report)
	if err != nil {
		if errors.Is(err, ErrCallNotFound) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "ending the call: %v", err)
	}

	return &controlpb.EndCallResponse{
		PacketsReceived: sum.PacketsReceived,
		PacketsExpected: sum.PacketsExpected,
		PacketsLost:     sum.PacketsLost,
		LossPct:         sum.LossPct,
		JitterMs:        sum.JitterMs,
		Mos:             sum.MOS,
		FramesConcealed: sum.FramesConcealed,
		ConcealPct:      sum.ConcealPct,
		Turns:           uint32(sum.Turns),
		ToolCalls:       uint32(sum.ToolCalls),
		Transcripts:     sum.Transcripts,
		Replies:         sum.Replies,
		TraceId:         sum.TraceID,
	}, nil
}

// Serve runs the control plane on lis until ctx is cancelled.
func Serve(ctx context.Context, lis net.Listener, h Handler) error {
	srv := grpc.NewServer()
	controlpb.RegisterControlPlaneServer(srv, NewServer(h))

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// GracefulStop waits for in-flight calls, which matters because an
			// EndCall in progress is the only chance to record the call.
			srv.GracefulStop()
		case <-done:
		}
	}()

	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("control: serving: %w", err)
	}
	return nil
}
