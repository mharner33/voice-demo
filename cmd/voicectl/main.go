// Command voicectl is the client: it sets up a call over the gateway's control
// plane, sends audio as RTP/G.711 while optionally impairing the stream, tears
// the call down, and receives the agent's spoken reply.
//
// File and synthetic tone sources are supported. Microphone capture will be
// host-only when it arrives, since macOS containers cannot reach the
// microphone. See docs/plan.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/control"
	"github.com/mharner33/voice-demo/internal/rtp"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "send":
		if err := runSend(os.Args[2:]); err != nil {
			log.Fatalf("voicectl send: %v", err)
		}
	case "profiles":
		listProfiles()
	case "-h", "--help", "help":
		usage()
	default:
		log.Printf("unknown subcommand %q", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `voicectl — RTP/G.711 test client

Usage:
  voicectl send [flags]     send audio to a gateway over RTP
  voicectl profiles         list the named impairment profiles

Run "voicectl send -h" for the send flags.
`)
}

func listProfiles() {
	for _, name := range chaos.ProfileNames() {
		p := chaos.Profiles[name]
		fmt.Printf("%-18s %s\n", name, p.Description)
		n := p.Network
		if n.IsClean() {
			fmt.Printf("%-18s   network: untouched\n", "")
		} else {
			fmt.Printf("%-18s   network: loss=%.1f%% burst=%v jitter=%.0fms latency=%.0fms reorder=%.1f%% dup=%.1f%%\n",
				"", n.LossPct, n.LossBurst, n.JitterMs, n.LatencyMs, n.ReorderPct, n.DupPct)
		}
	}
}

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	var (
		to       = fs.String("to", "", "gateway RTP address host:port; derived from the control plane when signaling")
		ctrlAddr = fs.String("control", "127.0.0.1:50051", "gateway control-plane address")
		noSignal = fs.Bool("no-signaling", false,
			"skip StartCall/EndCall and just send RTP; trailing loss becomes undetectable and no reply audio is possible")
		from      = fs.String("from", "+15551234567", "caller identifier, as a SIP From would be")
		callee    = fs.String("to-number", "+18005550100", "callee identifier, as a SIP To would be")
		saveReply = fs.String("save-reply", "", "write the agent's reply audio to this WAV file")
		replyPort = fs.Int("reply-port", 0, "port to receive reply audio on; 0 lets the kernel choose")
		noReply   = fs.Bool("no-reply", false, "do not listen for reply audio")
		file      = fs.String("file", "", "WAV file to send (omit to send a synthetic tone)")
		tone      = fs.Float64("tone", 440, "synthetic tone frequency in Hz")
		dur       = fs.Duration("duration", 5*time.Second, "synthetic tone duration")
		codecArg  = fs.String("codec", "PCMU", "G.711 variant: PCMU or PCMA")
		profile   = fs.String("profile", "clean", "impairment profile ("+strings.Join(chaos.ProfileNames(), ", ")+")")
		pace      = fs.Duration("pace", codec.FrameDuration, "interval between frames; below 20ms compresses the call but inflates measured jitter")
		seed      = fs.Int64("seed", time.Now().UnixNano(), "impairment RNG seed; fix it to make a run reproducible")

		// Per-knob overrides, applied on top of the profile.
		lossPct    = fs.Float64("loss-pct", -1, "override packet loss percentage")
		jitterMs   = fs.Float64("jitter-ms", -1, "override jitter in ms")
		latencyMs  = fs.Float64("latency-ms", -1, "override added latency in ms")
		reorderPct = fs.Float64("reorder-pct", -1, "override reorder percentage")
		dupPct     = fs.Float64("dup-pct", -1, "override duplication percentage")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := codec.Codec(strings.ToUpper(*codecArg))
	if c != codec.PCMU && c != codec.PCMA {
		return fmt.Errorf("codec must be PCMU or PCMA, got %q", *codecArg)
	}

	prof, err := chaos.LookupProfile(*profile)
	if err != nil {
		return err
	}
	net0 := prof.Network
	for _, o := range []struct {
		val float64
		dst *float64
	}{
		{*lossPct, &net0.LossPct},
		{*jitterMs, &net0.JitterMs},
		{*latencyMs, &net0.LatencyMs},
		{*reorderPct, &net0.ReorderPct},
		{*dupPct, &net0.DupPct},
	} {
		if o.val >= 0 {
			*o.dst = o.val
		}
	}
	if err := net0.Validate(); err != nil {
		return err
	}

	audio, err := loadAudio(*file, *tone, *dur)
	if err != nil {
		return err
	}
	if audio.SampleRate != codec.SampleRate8k {
		resampled, err := codec.Resample(audio, codec.SampleRate8k)
		if err != nil {
			return fmt.Errorf("source is %d Hz and cannot be resampled to 8 kHz: %w",
				audio.SampleRate, err)
		}
		log.Printf("resampled %d Hz -> 8 kHz", audio.SampleRate)
		audio = resampled
	}

	pcmFrames := codec.FramePCM(audio.PCM)
	payloads := make([][]byte, len(pcmFrames))
	for i, f := range pcmFrames {
		payloads[i] = c.Encode(f)
	}

	ssrc, err := rtp.NewSSRC()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Listen for the agent's reply before signaling, so the port can be
	// declared at call setup. The gateway sends audio to the address the media
	// came from combined with this port.
	var listener *replyListener
	if !*noReply && !*noSignal {
		listener, err = newReplyListener(*replyPort, c)
		if err != nil {
			return err
		}
		defer listener.Close()
	}

	mediaAddr := *to
	var (
		ctrl   *control.Client
		callID string
	)

	if *noSignal {
		if mediaAddr == "" {
			mediaAddr = "127.0.0.1:5004"
		}
		log.Printf("sending without signaling; the gateway cannot detect trailing " +
			"loss or send a reply")
	} else {
		ctrl, err = control.Dial(*ctrlAddr)
		if err != nil {
			return err
		}
		defer ctrl.Close()

		returnPort := 0
		if listener != nil {
			returnPort = listener.Port()
		}

		setupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		res, err := ctrl.StartCall(setupCtx, control.StartRequest{
			From:           *from,
			To:             *callee,
			Codec:          c,
			SSRC:           ssrc,
			ReturnPort:     returnPort,
			NetworkProfile: *profile,
		})
		cancel()
		if err != nil {
			return err
		}
		callID = res.CallID

		// The gateway answers with where to send media, which is the whole
		// point of negotiating rather than assuming a port.
		if mediaAddr == "" {
			host, _, splitErr := net.SplitHostPort(*ctrlAddr)
			if splitErr != nil {
				return fmt.Errorf("cannot derive the media host from %q: %w", *ctrlAddr, splitErr)
			}
			mediaAddr = net.JoinHostPort(host, strconv.Itoa(res.MediaPort))
		}

		log.Printf("call %s established: media to %s, codec %s", res.CallID, mediaAddr, res.Codec)
		if res.TraceID != "" {
			log.Printf("call %s trace: %s", res.CallID, res.TraceID)
		}
	}

	conn, err := net.Dial("udp", mediaAddr)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", mediaAddr, err)
	}
	defer conn.Close()

	imp, err := chaos.NewImpairer(net0, *seed)
	if err != nil {
		return err
	}
	sender, err := rtp.NewSender(conn, c, ssrc, imp)
	if err != nil {
		return err
	}

	log.Printf("sending %d frames (%.1fs of %s audio) to %s as ssrc=%#08x, profile=%s, seed=%d",
		len(payloads), audio.Duration(), c, mediaAddr, ssrc, *profile, *seed)

	start := time.Now()
	streamErr := sender.Stream(ctx, payloads, *pace)
	sender.Drain()
	elapsed := time.Since(start)

	st := sender.Stats()
	log.Printf("sent %d packets in %s (offered %d, dropped %d, duplicated %d, delayed %d, write errors %d)",
		st.PacketsSent, elapsed.Round(time.Millisecond), st.FramesOffered,
		st.Dropped, st.Duplicated, st.Delayed, st.WriteErrors)

	if ctrl != nil && callID != "" {
		// Teardown carries the sender's own accounting, including both ends of
		// the sequence range it emitted. Those are what let the gateway
		// account for loss at the edges of the stream, which it cannot
		// otherwise see: nothing later reveals a trailing gap, and a dropped
		// first packet makes it start counting one packet in.
		firstSeq, finalSeq, haveRange := sender.SeqRange()

		byeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		sum, err := ctrl.EndCall(byeCtx, callID, control.SenderReport{
			FramesOffered:  st.FramesOffered,
			PacketsSent:    st.PacketsSent,
			PacketsDropped: st.Dropped,
			FirstSequence:  firstSeq,
			FinalSequence:  finalSeq,
			HaveRange:      haveRange,
			Reason:         endReason(streamErr, ctx),
		})
		cancel()
		if err != nil {
			return err
		}
		reportSummary(callID, st, sum, listener, *saveReply, c)
	} else if listener != nil {
		listener.Close()
	}

	if streamErr != nil && ctx.Err() == nil {
		return streamErr
	}
	return nil
}

func endReason(streamErr error, ctx context.Context) string {
	switch {
	case ctx.Err() != nil:
		return "cancelled"
	case streamErr != nil:
		return "error"
	default:
		return "normal"
	}
}

// reportSummary prints the two sides' accounting next to each other. Agreement
// between them is the evidence that the loss figures mean something.
func reportSummary(callID string, sent rtp.SenderStats, sum control.CallSummary,
	listener *replyListener, savePath string, c codec.Codec) {

	fmt.Printf("\ncall %s\n", callID)
	fmt.Printf("  sender:   offered=%d sent=%d dropped=%d\n",
		sent.FramesOffered, sent.PacketsSent, sent.Dropped)
	fmt.Printf("  gateway:  received=%d expected=%d lost=%d (%.2f%%) jitter=%.1fms mos=%.2f\n",
		sum.PacketsReceived, sum.PacketsExpected, sum.PacketsLost,
		sum.LossPct, sum.JitterMs, sum.MOS)

	// The reconciliation. These agree exactly when the final sequence number
	// reached the gateway; without it the gateway undercounts by however many
	// packets were lost after the last one that arrived.
	switch {
	case sum.PacketsLost == sent.Dropped:
		fmt.Printf("  loss reconciles exactly: the gateway measured all %d dropped packets\n",
			sent.Dropped)
	default:
		fmt.Printf("  loss differs: sender dropped %d, gateway measured %d\n",
			sent.Dropped, sum.PacketsLost)
	}

	fmt.Printf("  buffer:   concealed=%d (%.2f%%)\n", sum.FramesConcealed, sum.ConcealPct)
	fmt.Printf("  agent:    turns=%d tools=%d\n", sum.Turns, sum.ToolCalls)
	for i, t := range sum.Transcripts {
		fmt.Printf("    %d. heard: %q\n", i, t)
		if i < len(sum.Replies) {
			fmt.Printf("       said:  %q\n", sum.Replies[i])
		}
	}
	if sum.TraceID != "" {
		fmt.Printf("  trace:    %s\n", sum.TraceID)
	}

	if listener == nil {
		return
	}

	// Give the reply a moment to finish arriving: synthesis runs after the last
	// inbound packet, so the return audio trails the call.
	deadline := time.Now().Add(3 * time.Second)
	last := -1
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if n := listener.Frames(); n == last && n > 0 {
			break
		} else {
			last = n
		}
	}

	fmt.Printf("  reply:    %d frames of audio received\n", listener.Frames())
	if st, ok := listener.Stats(); ok && st.Received > 0 {
		fmt.Printf("            return stream: lost=%d (%.2f%%) jitter=%.1fms\n",
			st.Lost, st.LossPct, st.JitterMs)
	}

	if savePath == "" {
		return
	}
	reply := listener.Audio()
	if len(reply.PCM) == 0 {
		fmt.Printf("            nothing to save: no reply audio arrived\n")
		return
	}
	if err := codec.WriteWAVFile(savePath, reply); err != nil {
		log.Printf("saving the reply: %v", err)
		return
	}
	fmt.Printf("            saved %.2fs of reply audio to %s\n", reply.Duration(), savePath)
}

func loadAudio(file string, toneHz float64, dur time.Duration) (codec.Audio, error) {
	if file == "" {
		log.Printf("no -file given; generating a %.0f Hz tone for %s", toneHz, dur)
		return codec.Tone(toneHz, codec.SampleRate8k, dur, 0.5), nil
	}
	audio, err := codec.ReadWAVFile(file)
	if err != nil {
		return codec.Audio{}, fmt.Errorf("reading %s: %w", file, err)
	}
	log.Printf("loaded %s: %.2fs at %d Hz", file, audio.Duration(), audio.SampleRate)
	return audio, nil
}
