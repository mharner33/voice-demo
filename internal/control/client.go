package control

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	controlpb "github.com/mharner33/voice-demo/proto/controlpb"
)

// Client is the caller side of the control plane.
type Client struct {
	conn *grpc.ClientConn
	rpc  controlpb.ControlPlaneClient
}

// Dial connects to a control plane. The connection is plaintext: this is a
// local demo, and adding TLS would add setup friction without demonstrating
// anything about observability.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("control: dialing %s: %w", addr, err)
	}
	return &Client{conn: conn, rpc: controlpb.NewControlPlaneClient(conn)}, nil
}

// Close releases the connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// StartCall negotiates a call and returns the gateway's answer.
func (c *Client) StartCall(ctx context.Context, req StartRequest) (StartResult, error) {
	res, err := c.rpc.StartCall(ctx, &controlpb.StartCallRequest{
		From:           req.From,
		To:             req.To,
		Codec:          codecToProto(req.Codec),
		Ssrc:           req.SSRC,
		ReturnPort:     uint32(req.ReturnPort),
		NetworkProfile: req.NetworkProfile,
		PtimeMs:        SupportedPtimeMs,
	})
	if err != nil {
		return StartResult{}, fmt.Errorf("control: StartCall: %w", err)
	}

	negotiated, err := codecFromProto(res.Codec)
	if err != nil {
		return StartResult{}, fmt.Errorf("control: the gateway answered with %w", err)
	}

	return StartResult{
		CallID:    res.CallId,
		MediaPort: int(res.MediaPort),
		Codec:     negotiated,
		TraceID:   res.TraceId,
	}, nil
}

// EndCall tears the call down, handing over the sender's own accounting and
// receiving the gateway's.
func (c *Client) EndCall(ctx context.Context, callID string, r SenderReport) (CallSummary, error) {
	res, err := c.rpc.EndCall(ctx, &controlpb.EndCallRequest{
		CallId:            callID,
		FramesOffered:     r.FramesOffered,
		PacketsSent:       r.PacketsSent,
		PacketsDropped:    r.PacketsDropped,
		FirstSequence:     uint32(r.FirstSequence),
		HaveFirstSequence: r.HaveRange,
		FinalSequence:     uint32(r.FinalSequence),
		HaveFinalSequence: r.HaveRange,
		Reason:            r.Reason,
	})
	if err != nil {
		return CallSummary{}, fmt.Errorf("control: EndCall: %w", err)
	}

	return CallSummary{
		PacketsReceived: res.PacketsReceived,
		PacketsExpected: res.PacketsExpected,
		PacketsLost:     res.PacketsLost,
		LossPct:         res.LossPct,
		JitterMs:        res.JitterMs,
		MOS:             res.Mos,
		FramesConcealed: res.FramesConcealed,
		ConcealPct:      res.ConcealPct,
		Turns:           int(res.Turns),
		ToolCalls:       int(res.ToolCalls),
		Transcripts:     res.Transcripts,
		Replies:         res.Replies,
		TraceID:         res.TraceId,
	}, nil
}
