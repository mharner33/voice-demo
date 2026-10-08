// Command voicectl is the client: it sources audio and sends it to the gateway
// as RTP/G.711, optionally impairing the stream on the way out.
//
// Phase 2 scope: file and synthetic tone sources. Microphone capture arrives with
// the client work in a later phase and will be host-only, since macOS containers
// cannot reach the microphone. See docs/plan.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
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
		to       = fs.String("to", "127.0.0.1:5004", "gateway address host:port")
		file     = fs.String("file", "", "WAV file to send (omit to send a synthetic tone)")
		tone     = fs.Float64("tone", 440, "synthetic tone frequency in Hz")
		dur      = fs.Duration("duration", 5*time.Second, "synthetic tone duration")
		codecArg = fs.String("codec", "PCMU", "G.711 variant: PCMU or PCMA")
		profile  = fs.String("profile", "clean", "impairment profile ("+strings.Join(chaos.ProfileNames(), ", ")+")")
		pace     = fs.Duration("pace", codec.FrameDuration, "interval between frames; below 20ms compresses the call but inflates measured jitter")
		seed     = fs.Int64("seed", time.Now().UnixNano(), "impairment RNG seed; fix it to make a run reproducible")

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

	conn, err := net.Dial("udp", *to)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", *to, err)
	}
	defer conn.Close()

	imp, err := chaos.NewImpairer(net0, *seed)
	if err != nil {
		return err
	}
	ssrc, err := rtp.NewSSRC()
	if err != nil {
		return err
	}
	sender, err := rtp.NewSender(conn, c, ssrc, imp)
	if err != nil {
		return err
	}

	log.Printf("sending %d frames (%.1fs of %s audio) to %s as ssrc=%#08x, profile=%s, seed=%d",
		len(payloads), audio.Duration(), c, *to, ssrc, *profile, *seed)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	streamErr := sender.Stream(ctx, payloads, *pace)
	sender.Drain()
	elapsed := time.Since(start)

	st := sender.Stats()
	log.Printf("sent %d packets in %s (offered %d, dropped %d, duplicated %d, delayed %d, write errors %d)",
		st.PacketsSent, elapsed.Round(time.Millisecond), st.FramesOffered,
		st.Dropped, st.Duplicated, st.Delayed, st.WriteErrors)

	if streamErr != nil && ctx.Err() == nil {
		return streamErr
	}
	return nil
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
