package loadgen

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
)

// DefaultConcurrency is how many calls run at once when none is given. Ten
// concurrent calls is enough to make a dashboard look like traffic and small
// enough to run on a laptop alongside the gateway and an agent.
const DefaultConcurrency = 10

// callerPool supplies the From and To numbers. A load run with one number
// dialing one number produces a trace list that looks like a bug; a handful of
// numbers makes it look like a switchboard, and the numbers are tags on the
// call's root span so they are filterable.
var (
	fromNumbers = []string{
		"+15551234567", "+15552223333", "+15558675309",
		"+442071838750", "+13105550123", "+16175550147",
	}
	toNumbers = []string{
		"+18005550100", // main line
		"+18005550111", // billing
		"+18005550122", // disputes
	}
)

// ProfilesByName resolves profile names, for a caller that has names rather
// than profiles.
func ProfilesByName(names []string) ([]chaos.Profile, error) {
	out := make([]chaos.Profile, 0, len(names))
	for _, name := range names {
		p, err := chaos.LookupProfile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Config describes a load run.
type Config struct {
	// ControlAddr is the gateway's control plane.
	ControlAddr string

	// MediaAddr overrides the negotiated media address. Normally empty.
	MediaAddr string

	// Calls is how many calls to place. Zero means run until Duration elapses;
	// one of the two is required.
	Calls int

	// Duration bounds the run. Zero means place exactly Calls calls however
	// long that takes.
	Duration time.Duration

	// Concurrency is how many calls are in flight at once.
	Concurrency int

	// Profiles are cycled across calls, so one run can carry several network
	// conditions and the dashboard can be split by them. Empty means clean.
	//
	// Resolved profiles rather than names, so a caller can hand over a profile
	// with per-knob overrides already applied — "-profile mobile -loss-pct 20"
	// has to remain expressible without this package having to know about
	// command-line flags.
	Profiles []chaos.Profile

	// Fixtures are cycled across calls. Empty means SyntheticFixtures.
	Fixtures []Fixture

	Codec codec.Codec

	// Pace is the interval between frames. Leave it at the packetization
	// interval for anything whose jitter figures will be looked at.
	Pace time.Duration

	// Stagger delays each worker's first call, spreading the start of a run so
	// that N calls do not all set up in the same millisecond.
	Stagger time.Duration

	// Seed makes the whole run reproducible: call i always gets the same
	// impairment pattern.
	Seed int64

	// OnOutcome is called as each call finishes, on the worker's goroutine.
	// It must be safe for concurrent use.
	OnOutcome func(Outcome)
}

func (c *Config) applyDefaults() {
	if c.Concurrency == 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.Codec == "" {
		c.Codec = codec.PCMU
	}
	if c.Pace == 0 {
		c.Pace = codec.FrameDuration
	}
	if len(c.Profiles) == 0 {
		c.Profiles = []chaos.Profile{chaos.Profiles["clean"]}
	}
	if len(c.Fixtures) == 0 {
		c.Fixtures = SyntheticFixtures()
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
}

// Validate rejects a run that cannot work.
func (c Config) Validate() error {
	if c.ControlAddr == "" {
		return fmt.Errorf("loadgen: ControlAddr is required")
	}
	if c.Calls <= 0 && c.Duration <= 0 {
		return fmt.Errorf("loadgen: set Calls, Duration, or both")
	}
	if c.Calls < 0 {
		return fmt.Errorf("loadgen: Calls = %d, want >= 0", c.Calls)
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("loadgen: Concurrency = %d, want >= 1", c.Concurrency)
	}
	if c.Stagger < 0 {
		return fmt.Errorf("loadgen: Stagger = %v, want >= 0", c.Stagger)
	}
	for i, p := range c.Profiles {
		if p.Name == "" {
			return fmt.Errorf("loadgen: profile %d has no name; the gateway is told the name", i)
		}
		if err := p.Network.Validate(); err != nil {
			return fmt.Errorf("loadgen: profile %q: %w", p.Name, err)
		}
	}
	for i, f := range c.Fixtures {
		if len(f.Audio.PCM) == 0 {
			return fmt.Errorf("loadgen: fixture %d (%q) has no audio", i, f.Name)
		}
	}
	return nil
}

// ProfileStats is one profile's slice of a run. Its whole purpose is the
// comparison between profiles: if `lossy-wan` does not show worse numbers than
// `clean` here, the demo has nothing to show either.
type ProfileStats struct {
	Profile string
	Calls   int
	Failed  int

	MeanLossPct    float64
	MeanJitterMs   float64
	MeanMOS        float64
	MeanConcealPct float64
	Turns          int
	ToolCalls      int
}

// Report is the whole run.
type Report struct {
	Calls   int
	Failed  int
	Elapsed time.Duration

	PacketsSent     uint64
	PacketsDropped  uint64
	PacketsReceived uint64
	PacketsLost     uint64

	// Reconciled counts calls where the gateway independently measured exactly
	// the packets the client dropped.
	Reconciled int

	Turns     int
	ToolCalls int

	// Errors counts failures by message, so a run that failed the same way
	// three hundred times reports one line rather than three hundred.
	Errors map[string]int

	ByProfile []ProfileStats
}

// Succeeded reports whether every call completed and every loss figure
// reconciled, which is what the phase-8 acceptance test asserts.
func (r Report) Succeeded() bool {
	return r.Failed == 0 && r.Reconciled == r.Calls && r.Calls > 0
}

// Run places calls until the configured count or duration is reached.
//
// Calls are independent: one that fails is recorded and the run continues,
// because a load test whose first teardown timeout aborted the whole run would
// report nothing about the other forty-nine.
func Run(ctx context.Context, cfg Config) (Report, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Report{}, err
	}

	// Audio is encoded once. Companding the same fixture per call would make
	// the client the slowest part of a load run.
	payloads := make([][][]byte, len(cfg.Fixtures))
	for i, f := range cfg.Fixtures {
		payloads[i] = f.Payloads(cfg.Codec)
	}

	caller, err := Dial(cfg.ControlAddr)
	if err != nil {
		return Report{}, err
	}
	defer caller.Close()

	runCtx := ctx
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	var (
		mu       sync.Mutex
		outcomes []Outcome
		next     int
		wg       sync.WaitGroup
	)

	// claim hands out the next call index, or reports that the run is over.
	// A counter rather than a channel of work items, because the run may be
	// unbounded: with only a Duration set there is no list of calls to queue.
	claim := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		if cfg.Calls > 0 && next >= cfg.Calls {
			return 0, false
		}
		i := next
		next++
		return i, true
	}

	start := time.Now()

	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()

			if cfg.Stagger > 0 {
				select {
				case <-runCtx.Done():
					return
				case <-time.After(time.Duration(worker) * cfg.Stagger):
				}
			}

			for {
				if runCtx.Err() != nil {
					return
				}
				i, ok := claim()
				if !ok {
					return
				}

				prof := cfg.Profiles[i%len(cfg.Profiles)]
				fx := cfg.Fixtures[i%len(cfg.Fixtures)]

				out, _ := caller.Place(runCtx, Spec{
					From:    fromNumbers[i%len(fromNumbers)],
					To:      toNumbers[i%len(toNumbers)],
					Codec:   cfg.Codec,
					Profile: prof.Name,
					Network: prof.Network,
					// Per-call seeds derived from the run seed, so call i is
					// impaired identically on a re-run but differently from
					// call i+1.
					Seed:    cfg.Seed + int64(i),
					Frames:  payloads[i%len(payloads)],
					Pace:    cfg.Pace,
					Fixture: fx.Name,
				})

				// A call cut short by the run's own deadline is not a failure:
				// it is what "-duration" means. Recording it as one would make
				// every timed run report failures.
				if out.Err != nil && runCtx.Err() != nil && errors.Is(out.Err, context.DeadlineExceeded) {
					return
				}

				mu.Lock()
				outcomes = append(outcomes, out)
				mu.Unlock()

				if cfg.OnOutcome != nil {
					cfg.OnOutcome(out)
				}
			}
		}(w)
	}

	wg.Wait()
	elapsed := time.Since(start)

	// A run cancelled by the caller — Ctrl-C — is reported as an error along
	// with whatever it managed to measure. A run that hit its own Duration is
	// not an error: that is how it was asked to end.
	var runErr error
	if ctx.Err() != nil {
		runErr = ctx.Err()
	}

	return summarize(outcomes, elapsed), runErr
}

// summarize folds the outcomes into a report.
func summarize(outcomes []Outcome, elapsed time.Duration) Report {
	rep := Report{
		Calls:   len(outcomes),
		Elapsed: elapsed,
		Errors:  map[string]int{},
	}

	type acc struct {
		stats                          ProfileStats
		loss, jitter, mos, conceal     float64
		lossN, jitterN, mosN, concealN int
	}
	byProfile := map[string]*acc{}

	for _, o := range outcomes {
		rep.PacketsSent += o.Sent.PacketsSent
		rep.PacketsDropped += o.Sent.Dropped
		rep.PacketsReceived += o.Summary.PacketsReceived
		rep.PacketsLost += o.Summary.PacketsLost
		rep.Turns += o.Summary.Turns
		rep.ToolCalls += o.Summary.ToolCalls

		if o.Err != nil {
			rep.Failed++
			rep.Errors[o.Err.Error()]++
		}
		if o.LossReconciles() {
			rep.Reconciled++
		}

		a := byProfile[o.Profile]
		if a == nil {
			a = &acc{stats: ProfileStats{Profile: o.Profile}}
			byProfile[o.Profile] = a
		}
		a.stats.Calls++
		a.stats.Turns += o.Summary.Turns
		a.stats.ToolCalls += o.Summary.ToolCalls
		if o.Err != nil {
			a.stats.Failed++
			continue
		}

		// Averaged only over calls that produced a figure. A failed call
		// contributes no MOS, and counting it as zero would drag the profile's
		// average down for a reason that has nothing to do with the network.
		a.loss += o.Summary.LossPct
		a.lossN++
		a.jitter += o.Summary.JitterMs
		a.jitterN++
		a.conceal += o.Summary.ConcealPct
		a.concealN++
		if o.Summary.MOS > 0 {
			a.mos += o.Summary.MOS
			a.mosN++
		}
	}

	mean := func(sum float64, n int) float64 {
		if n == 0 {
			return 0
		}
		return sum / float64(n)
	}
	for _, a := range byProfile {
		a.stats.MeanLossPct = mean(a.loss, a.lossN)
		a.stats.MeanJitterMs = mean(a.jitter, a.jitterN)
		a.stats.MeanMOS = mean(a.mos, a.mosN)
		a.stats.MeanConcealPct = mean(a.conceal, a.concealN)
		rep.ByProfile = append(rep.ByProfile, a.stats)
	}
	sort.Slice(rep.ByProfile, func(i, j int) bool {
		return rep.ByProfile[i].Profile < rep.ByProfile[j].Profile
	})

	return rep
}
