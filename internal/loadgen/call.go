// Package loadgen places calls against a gateway: one at a time for a single
// test call, or many at once for a load run.
//
// It exists as a package rather than living in the client command because the
// two paths have to be the same path. A load run that set its calls up
// differently from `voicectl send` would be measuring something other than
// what the single-call demo shows, and the whole argument of the demo rests on
// the two agreeing.
package loadgen

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/control"
	"github.com/mharner33/voice-demo/internal/rtp"
)

// Timeouts for the control plane. Setup is short because a gateway that cannot
// answer in ten seconds is not going to serve a call; teardown is long because
// it waits for the pipeline to finish the last utterance, and a degraded
// provider profile deliberately adds seconds per turn.
const (
	SetupTimeout    = 10 * time.Second
	TeardownTimeout = 60 * time.Second
)

// Spec is one call to place.
type Spec struct {
	// From and To identify the caller and callee, as a SIP From and To would.
	// They are tagged on the call's APM root span, so varying them across a
	// load run is what makes the trace list look like a switchboard rather
	// than one number dialing itself a thousand times.
	From string
	To   string

	Codec codec.Codec

	// Profile is the name of the impairment the client is applying, declared
	// at setup. The gateway cannot discover it: nothing in an RTP stream says
	// how it was degraded.
	Profile string

	// Network is the impairment itself, applied on this side.
	Network chaos.Network

	// Seed makes one call's impairment pattern reproducible.
	Seed int64

	// Frames are the encoded payloads to send, and Pace is the interval
	// between them. A pace below the packetization interval compresses the
	// call but inflates the jitter the gateway measures, so a demo run must
	// leave it at codec.FrameDuration.
	Frames [][]byte
	Pace   time.Duration

	// Fixture names the audio, for the load report. It never reaches the
	// gateway.
	Fixture string

	// ReturnPort is where this client is listening for the agent's reply, or
	// zero if it is not listening.
	ReturnPort int

	// MediaAddr overrides the media destination. Empty means use the port the
	// gateway negotiated, which is the point of having a control plane.
	MediaAddr string

	// OnSetup, if set, is called once the call is established and before any
	// media flows. It exists so a single-call client can report the
	// negotiated parameters when they are negotiated rather than at the end,
	// which is when Place returns.
	OnSetup func(SetupInfo)
}

// SetupInfo is what the gateway answered at call setup, plus the SSRC that
// binds the call to the media about to follow it.
type SetupInfo struct {
	CallID    string
	MediaAddr string
	Codec     codec.Codec
	TraceID   string
	SSRC      uint32
}

// Outcome is what happened to one call: both sides' accounting, so they can be
// reconciled rather than taken on trust.
type Outcome struct {
	CallID    string
	SSRC      uint32
	MediaAddr string
	Fixture   string
	Profile   string

	// TraceID is the APM trace the gateway opened for this call.
	TraceID string

	Sent    rtp.SenderStats
	Summary control.CallSummary

	// Elapsed is wall-clock time from setup to teardown.
	Elapsed time.Duration

	// Err is set when the call failed. The rest of the fields are filled in as
	// far as the call got, so a failure is still diagnosable.
	Err error
}

// LossReconciles reports whether the gateway independently measured exactly
// the packets this client dropped. It is the single most useful line in a load
// report: when it holds, every loss figure on the dashboard is trustworthy.
func (o Outcome) LossReconciles() bool {
	return o.Err == nil && o.Summary.PacketsLost == o.Sent.Dropped
}

// Caller places calls against one gateway. Safe for concurrent use: the gRPC
// client multiplexes, and each call gets its own UDP socket and SSRC.
type Caller struct {
	ctrl *control.Client
	addr string
}

// Dial connects to a gateway's control plane.
func Dial(controlAddr string) (*Caller, error) {
	ctrl, err := control.Dial(controlAddr)
	if err != nil {
		return nil, err
	}
	return &Caller{ctrl: ctrl, addr: controlAddr}, nil
}

// Close releases the control-plane connection.
func (c *Caller) Close() error { return c.ctrl.Close() }

// Place runs one call from setup to teardown and returns both sides' numbers.
//
// The returned error is also recorded on the Outcome, because a load run wants
// to keep going and report the failure while a single call wants to exit with
// it.
func (c *Caller) Place(ctx context.Context, spec Spec) (Outcome, error) {
	if len(spec.Frames) == 0 {
		return Outcome{}, fmt.Errorf("loadgen: no audio to send")
	}
	if spec.Pace <= 0 {
		spec.Pace = codec.FrameDuration
	}
	if spec.Codec == "" {
		spec.Codec = codec.PCMU
	}

	out := Outcome{Fixture: spec.Fixture, Profile: spec.Profile}
	start := time.Now()

	ssrc, err := rtp.NewSSRC()
	if err != nil {
		return out, err
	}
	out.SSRC = ssrc

	setupCtx, cancel := context.WithTimeout(ctx, SetupTimeout)
	res, err := c.ctrl.StartCall(setupCtx, control.StartRequest{
		From:           spec.From,
		To:             spec.To,
		Codec:          spec.Codec,
		SSRC:           ssrc,
		ReturnPort:     spec.ReturnPort,
		NetworkProfile: spec.Profile,
	})
	cancel()
	if err != nil {
		out.Err = fmt.Errorf("setup: %w", err)
		out.Elapsed = time.Since(start)
		return out, out.Err
	}
	out.CallID = res.CallID
	out.TraceID = res.TraceID

	mediaAddr := spec.MediaAddr
	if mediaAddr == "" {
		// The gateway answers with where to send media, which is the whole
		// point of negotiating rather than assuming a port.
		host, _, splitErr := net.SplitHostPort(c.addr)
		if splitErr != nil {
			out.Err = fmt.Errorf("cannot derive the media host from %q: %w", c.addr, splitErr)
			return out, out.Err
		}
		mediaAddr = net.JoinHostPort(host, strconv.Itoa(res.MediaPort))
	}
	out.MediaAddr = mediaAddr

	if spec.OnSetup != nil {
		spec.OnSetup(SetupInfo{
			CallID:    res.CallID,
			MediaAddr: mediaAddr,
			Codec:     res.Codec,
			TraceID:   res.TraceID,
			SSRC:      ssrc,
		})
	}

	conn, err := net.Dial("udp", mediaAddr)
	if err != nil {
		out.Err = fmt.Errorf("dialing %s: %w", mediaAddr, err)
		return out, out.Err
	}
	defer conn.Close()

	imp, err := chaos.NewImpairer(spec.Network, spec.Seed)
	if err != nil {
		out.Err = err
		return out, err
	}
	sender, err := rtp.NewSender(conn, spec.Codec, ssrc, imp)
	if err != nil {
		out.Err = err
		return out, err
	}

	streamErr := sender.Stream(ctx, spec.Frames, spec.Pace)
	// Drain before teardown, not after: delayed packets still in flight would
	// otherwise arrive after the gateway has already reported the call, and
	// the loss figure would count them missing.
	sender.Drain()
	_ = sender.Close()
	out.Sent = sender.Stats()

	first, final, haveRange := sender.SeqRange()

	// Teardown runs on a context of its own so that a cancelled run still
	// closes its calls. A call left open is reaped by the idle timeout
	// instead, which loses the sequence range and with it the exact loss
	// figure — the thing the control plane exists for.
	byeCtx, byeCancel := context.WithTimeout(context.WithoutCancel(ctx), TeardownTimeout)
	sum, err := c.ctrl.EndCall(byeCtx, res.CallID, control.SenderReport{
		FramesOffered:  out.Sent.FramesOffered,
		PacketsSent:    out.Sent.PacketsSent,
		PacketsDropped: out.Sent.Dropped,
		FirstSequence:  first,
		FinalSequence:  final,
		HaveRange:      haveRange,
		Reason:         EndReason(streamErr, ctx),
	})
	byeCancel()

	out.Elapsed = time.Since(start)
	switch {
	case err != nil:
		out.Err = fmt.Errorf("teardown: %w", err)
	case streamErr != nil && ctx.Err() == nil:
		out.Err = fmt.Errorf("sending: %w", streamErr)
	}
	out.Summary = sum

	return out, out.Err
}

// EndReason classifies how a call finished, for the teardown message and the
// gateway's call log.
func EndReason(streamErr error, ctx context.Context) string {
	switch {
	case ctx.Err() != nil:
		return "cancelled"
	case streamErr != nil:
		return "error"
	default:
		return "normal"
	}
}
