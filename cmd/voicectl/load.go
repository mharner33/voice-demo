package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/loadgen"
)

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	var (
		ctrlAddr = fs.String("control", "127.0.0.1:50051", "gateway control-plane address")
		mediaTo  = fs.String("to", "", "gateway RTP address host:port; derived from the control plane by default")

		calls       = fs.Int("calls", 50, "how many calls to place; 0 with -duration runs until the time is up")
		concurrency = fs.Int("concurrency", loadgen.DefaultConcurrency, "how many calls in flight at once")
		duration    = fs.Duration("duration", 0, "stop after this long; 0 means place exactly -calls calls")
		stagger     = fs.Duration("stagger", 100*time.Millisecond,
			"delay between each worker's first call, so a run does not set up every call in the same millisecond")

		profileList = fs.String("profile", "clean",
			"impairment profile, or a comma-separated list cycled across calls ("+
				strings.Join(chaos.ProfileNames(), ", ")+")")
		fixtureDir = fs.String("fixtures", "",
			"directory of WAV files to call with; the built-in synthetic set is used by default")
		codecArg = fs.String("codec", "PCMU", "G.711 variant: PCMU or PCMA")
		pace     = fs.Duration("pace", codec.FrameDuration,
			"interval between frames; leave at 20ms for any run whose jitter figures will be looked at")
		seed    = fs.Int64("seed", 1, "impairment RNG seed; the same seed replays the same run")
		quiet   = fs.Bool("quiet", false, "do not print a line per call")
		verbose = fs.Bool("verbose", false, "print each call's transcript and reply")

		netFlags = registerNetworkFlags(fs)
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := parseCodec(*codecArg)
	if err != nil {
		return err
	}

	profiles := splitProfiles(*profileList)
	if len(profiles) == 0 {
		return fmt.Errorf("no profile given")
	}

	// The per-knob overrides apply to every profile in the list, which is the
	// only sensible reading of "-profile clean,mobile -loss-pct 20": the
	// profiles still differ in jitter, and both now lose 20%.
	overridden := make([]chaos.Profile, 0, len(profiles))
	for _, name := range profiles {
		p, err := chaos.LookupProfile(name)
		if err != nil {
			return err
		}
		p.Network, err = netFlags.apply(p.Network)
		if err != nil {
			return err
		}
		overridden = append(overridden, p)
	}

	fixtures := loadgen.SyntheticFixtures()
	if *fixtureDir != "" {
		fixtures, err = loadgen.LoadFixtures(*fixtureDir)
		if err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("%s against %s: %d concurrent, codec %s, profiles %s, %d fixtures, seed %d",
		describeVolume(*calls, *duration), *ctrlAddr, *concurrency, c,
		strings.Join(profiles, ","), len(fixtures), *seed)
	for _, f := range fixtures {
		log.Printf("  fixture %-20s %.1fs", f.Name, f.Duration().Seconds())
	}
	if *pace != codec.FrameDuration {
		log.Printf("WARNING: pace is %v rather than %v, so the jitter the gateway "+
			"measures will be inflated — see plan finding 6", *pace, codec.FrameDuration)
	}

	var done atomic.Int64
	report, runErr := loadgen.Run(ctx, loadgen.Config{
		ControlAddr: *ctrlAddr,
		MediaAddr:   *mediaTo,
		Calls:       *calls,
		Duration:    *duration,
		Concurrency: *concurrency,
		Profiles:    overridden,
		Fixtures:    fixtures,
		Codec:       c,
		Pace:        *pace,
		Stagger:     *stagger,
		Seed:        *seed,
		OnOutcome: func(out loadgen.Outcome) {
			n := done.Add(1)
			if *quiet {
				return
			}
			printOutcome(n, out, *verbose)
		},
	})

	printReport(report)

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	if report.Failed > 0 {
		// A load run that could not place its calls has failed, and the exit
		// status has to say so: the scripted demo runs these in sequence and
		// must stop rather than narrate a beat that never happened.
		return fmt.Errorf("%d of %d calls failed", report.Failed, report.Calls)
	}
	return nil
}

func describeVolume(calls int, duration time.Duration) string {
	switch {
	case calls > 0 && duration > 0:
		return fmt.Sprintf("up to %d calls over %v", calls, duration)
	case duration > 0:
		return fmt.Sprintf("calls for %v", duration)
	default:
		return fmt.Sprintf("%d calls", calls)
	}
}

func splitProfiles(list string) []string {
	var out []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// printOutcome is one line per call: enough to watch a run progress, little
// enough that fifty of them still fit on a screen.
func printOutcome(n int64, out loadgen.Outcome, verbose bool) {
	if out.Err != nil {
		fmt.Printf("%4d  %-10s %-18s FAILED: %v\n", n, out.CallID, out.Profile, out.Err)
		return
	}

	recon := "="
	if !out.LossReconciles() {
		// Flagged rather than hidden: a call whose two accounts disagree is
		// the one case where the numbers on the dashboard cannot be trusted.
		recon = "≠"
	}
	fmt.Printf("%4d  %-10s %-18s %-18s sent=%-4d lost=%-3d %s%-3d loss=%5.2f%% jitter=%6.1fms mos=%.2f turns=%d\n",
		n, out.CallID, out.Profile, out.Fixture,
		out.Sent.PacketsSent, out.Summary.PacketsLost, recon, out.Sent.Dropped,
		out.Summary.LossPct, out.Summary.JitterMs, out.Summary.MOS, out.Summary.Turns)

	if !verbose {
		return
	}
	for i, t := range out.Summary.Transcripts {
		fmt.Printf("        heard: %q\n", t)
		if i < len(out.Summary.Replies) {
			fmt.Printf("        said:  %q\n", out.Summary.Replies[i])
		}
	}
}

// printReport is what a demo reads out loud: the per-profile comparison, and
// whether the loss figures can be believed.
func printReport(r loadgen.Report) {
	fmt.Printf("\n%d calls in %v", r.Calls, r.Elapsed.Round(time.Millisecond))
	if r.Elapsed > 0 && r.Calls > 0 {
		fmt.Printf(" (%.1f calls/s)", float64(r.Calls)/r.Elapsed.Seconds())
	}
	fmt.Println()

	fmt.Printf("  packets:   sent=%d dropped=%d | gateway received=%d lost=%d\n",
		r.PacketsSent, r.PacketsDropped, r.PacketsReceived, r.PacketsLost)

	switch {
	case r.Calls == 0:
		fmt.Println("  no calls were placed")
	case r.Reconciled == r.Calls:
		fmt.Printf("  loss reconciles on all %d calls: every loss figure is exact\n", r.Calls)
	default:
		fmt.Printf("  loss reconciles on %d of %d calls; the rest are lower bounds\n",
			r.Reconciled, r.Calls)
	}

	fmt.Printf("  agent:     turns=%d tool calls=%d\n", r.Turns, r.ToolCalls)

	if len(r.ByProfile) > 0 {
		fmt.Println("\n  profile              calls  loss      jitter      mos   conceal   turns")
		for _, p := range r.ByProfile {
			fmt.Printf("  %-20s %5d  %6.2f%%  %7.1fms  %5.2f  %6.2f%%  %6d\n",
				p.Profile, p.Calls, p.MeanLossPct, p.MeanJitterMs,
				p.MeanMOS, p.MeanConcealPct, p.Turns)
		}
	}

	if r.Failed > 0 {
		fmt.Printf("\n  %d calls failed:\n", r.Failed)
		msgs := make([]string, 0, len(r.Errors))
		for msg := range r.Errors {
			msgs = append(msgs, msg)
		}
		sort.Slice(msgs, func(i, j int) bool { return r.Errors[msgs[i]] > r.Errors[msgs[j]] })
		for _, msg := range msgs {
			fmt.Printf("    %4d × %s\n", r.Errors[msg], msg)
		}
	}
}

func runFixtures(args []string) error {
	fs := flag.NewFlagSet("fixtures", flag.ExitOnError)
	dir := fs.String("dir", "testdata/fixtures", "directory to write the WAV files into")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fixtures := loadgen.SyntheticFixtures()
	if err := loadgen.WriteFixtures(*dir, fixtures); err != nil {
		return err
	}

	abs := *dir
	if wd, err := os.Getwd(); err == nil && !strings.HasPrefix(abs, "/") {
		abs = wd + "/" + abs
	}
	fmt.Printf("wrote %d fixtures to %s\n", len(fixtures), abs)
	for _, f := range fixtures {
		fmt.Printf("  %-20s %.1fs\n", f.Name+".wav", f.Duration().Seconds())
	}
	fmt.Printf("\nPass one back with: voicectl send -file %s/%s.wav\n", *dir, fixtures[0].Name)
	return nil
}
