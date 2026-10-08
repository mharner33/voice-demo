// Package control implements the call-setup and teardown plane.
//
// It mirrors SIP's INVITE/BYE semantics over gRPC rather than implementing SIP,
// because the signaling protocol is not what this demo is about — but the
// *existence* of signaling is, since four things cannot be done without it:
// detecting trailing packet loss, knowing exactly when a call ended, sending
// audio back to the caller, and labeling a call with the network conditions it
// was sent under.
package control

import (
	"fmt"
	"time"

	"github.com/mharner33/voice-demo/internal/codec"
	controlpb "github.com/mharner33/voice-demo/proto/controlpb"
)

// SupportedPtimeMs is the only packetization interval this demo handles.
// Anything else is rejected rather than silently producing wrong timestamp
// arithmetic.
const SupportedPtimeMs = 20

// CallParams are the negotiated parameters of one call.
type CallParams struct {
	CallID string
	From   string
	To     string
	Codec  codec.Codec
	SSRC   uint32

	// ReturnPort is where the caller is listening for synthesized audio. Zero
	// means it is not listening and nothing should be sent back.
	ReturnPort int

	// NetworkProfile is the impairment the caller declared it is applying to
	// its own outbound stream. The gateway cannot discover this from the media.
	NetworkProfile string

	StartedAt time.Time
}

// WantsReturnAudio reports whether the caller is listening for a reply.
func (p CallParams) WantsReturnAudio() bool { return p.ReturnPort > 0 }

// SenderReport is the caller's own account of what it transmitted, delivered at
// teardown. Comparing it against what the gateway measured independently is
// what demonstrates the loss accounting is correct.
type SenderReport struct {
	FramesOffered  uint64
	PacketsSent    uint64
	PacketsDropped uint64

	// FirstSequence and FinalSequence bracket the range the sender emitted,
	// counting packets it dropped itself. Both are needed: a receiver cannot
	// see loss at either edge of a stream, and a dropped first packet makes it
	// silently start counting one packet in.
	FirstSequence uint16
	FinalSequence uint16
	HaveRange     bool

	Reason string
}

// CallSummary is the gateway's view of a finished call, returned to the caller
// so the two accounts can be reconciled.
type CallSummary struct {
	PacketsReceived uint64
	PacketsExpected uint64
	PacketsLost     uint64
	LossPct         float64
	JitterMs        float64
	MOS             float64

	FramesConcealed uint64
	ConcealPct      float64

	Turns       int
	ToolCalls   int
	Transcripts []string
	Replies     []string

	TraceID string
}

// codecFromProto converts the wire enum, rejecting anything unsupported.
func codecFromProto(c controlpb.Codec) (codec.Codec, error) {
	switch c {
	case controlpb.Codec_CODEC_PCMU:
		return codec.PCMU, nil
	case controlpb.Codec_CODEC_PCMA:
		return codec.PCMA, nil
	case controlpb.Codec_CODEC_UNSPECIFIED:
		// An unspecified codec defaults to u-law, which is the near-universal
		// telephony default and what a bare RTP payload type 0 means.
		return codec.PCMU, nil
	default:
		return "", fmt.Errorf("control: unsupported codec %v", c)
	}
}

// codecToProto converts back to the wire enum.
func codecToProto(c codec.Codec) controlpb.Codec {
	if c == codec.PCMA {
		return controlpb.Codec_CODEC_PCMA
	}
	return controlpb.Codec_CODEC_PCMU
}

// validateStart checks a setup request before any state is created, so a
// malformed request fails cleanly rather than half-establishing a call.
func validateStart(req *controlpb.StartCallRequest) error {
	if req == nil {
		return fmt.Errorf("control: nil StartCall request")
	}
	if req.Ssrc == 0 {
		// The SSRC is what binds the signaled call to the media that follows.
		// Zero is a legal RTP value but useless as a key, and almost always
		// means the client forgot to set it.
		return fmt.Errorf("control: ssrc is required; it binds the call to its RTP stream")
	}
	if p := req.PtimeMs; p != 0 && p != SupportedPtimeMs {
		return fmt.Errorf("control: ptime %d ms is not supported, only %d ms",
			p, SupportedPtimeMs)
	}
	if req.ReturnPort > 65535 {
		return fmt.Errorf("control: return_port %d is out of range", req.ReturnPort)
	}
	if _, err := codecFromProto(req.Codec); err != nil {
		return err
	}
	return nil
}
