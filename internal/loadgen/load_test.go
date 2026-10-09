package loadgen

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
)

// TestMain is where the no-goroutine-leaks requirement is enforced, across
// every test in the package rather than one of them.
//
// gRPC keeps background goroutines for as long as a client connection is open
// and tears them down asynchronously, so the known-benign ones are ignored by
// name. Anything this package itself leaks — a worker still sending after a
// run returned, a sender's delayed-packet timer — is not on the list and fails
// the suite.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("google.golang.org/grpc.(*ccBalancerWrapper).watcher"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/grpcsync.(*CallbackSerializer).run"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*http2Client).keepalive"),
		goleak.IgnoreAnyFunction("google.golang.org/grpc/internal/transport.newHTTP2Client.func6"),
	)
}

// fastPace compresses a test call into a fraction of its real duration. It
// inflates the jitter the gateway measures, which is why a demo must not do
// it, but these tests assert on loss and on call accounting rather than on
// jitter.
const fastPace = 200 * time.Microsecond

// shortFixtures keep the suite quick: one second of audio at the real
// packetization interval is fifty frames, and at fastPace that is milliseconds.
func shortFixtures() []Fixture {
	return []Fixture{
		{Name: "one-second", Audio: codec.Tone(300, codec.SampleRate8k, time.Second, 0.5)},
		{Name: "half-second", Audio: codec.Tone(420, codec.SampleRate8k, 500*time.Millisecond, 0.5)},
	}
}

// profilesOrFail resolves profile names for a test.
func profilesOrFail(t *testing.T, names ...string) []chaos.Profile {
	t.Helper()
	p, err := ProfilesByName(names)
	if err != nil {
		t.Fatalf("ProfilesByName: %v", err)
	}
	return p
}

func baseConfig(t *testing.T, gw *testGateway) Config {
	t.Helper()
	return Config{
		ControlAddr: gw.controlAddr(t),
		Fixtures:    shortFixtures(),
		Pace:        fastPace,
		Seed:        7,
	}
}

// TestFiftyConcurrentCalls is phase 8's acceptance test. Fifty calls, ten at a
// time, every one of them completing — and every loss figure reconciling
// against what the client knows it dropped.
func TestFiftyConcurrentCalls(t *testing.T) {
	gw := newTestGateway(t)

	cfg := baseConfig(t, gw)
	cfg.Calls = 50
	cfg.Concurrency = 10
	cfg.Profiles = profilesOrFail(t, "clean", "mobile", "lossy-wan")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rep, err := Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.Calls != 50 {
		t.Errorf("placed %d calls, want 50", rep.Calls)
	}
	if rep.Failed != 0 {
		t.Errorf("%d calls failed: %v", rep.Failed, rep.Errors)
	}
	if rep.Reconciled != rep.Calls {
		t.Errorf("only %d of %d calls reconciled their loss figures; "+
			"a dashboard built on these numbers would be lying",
			rep.Reconciled, rep.Calls)
	}
	if !rep.Succeeded() {
		t.Error("Succeeded() is false on a run with no failures and full reconciliation")
	}

	starts, ended, peak := gw.counts()
	if starts != 50 || ended != 50 {
		t.Errorf("the gateway saw %d setups and %d teardowns, want 50 and 50", starts, ended)
	}
	// Concurrency is a cap, not a target: the gateway should never have seen
	// more than ten calls live at once.
	if peak > cfg.Concurrency {
		t.Errorf("%d calls were live at once, above the configured %d", peak, cfg.Concurrency)
	}
	// And it should actually have been used, or the run was serial and the
	// test is not testing concurrency at all.
	if peak < 2 {
		t.Errorf("peak concurrency was %d; the run never overlapped two calls", peak)
	}

	t.Logf("%d calls in %v, peak %d concurrent, %d packets sent, %d dropped, %d lost",
		rep.Calls, rep.Elapsed.Round(time.Millisecond), peak,
		rep.PacketsSent, rep.PacketsDropped, rep.PacketsLost)
	for _, p := range rep.ByProfile {
		t.Logf("  %-18s calls=%d loss=%.2f%% jitter=%.1fms mos=%.2f",
			p.Profile, p.Calls, p.MeanLossPct, p.MeanJitterMs, p.MeanMOS)
	}
}

// The profiles have to be distinguishable in the report, or the scripted demo
// has nothing to show: `lossy-wan` must lose more than `clean`, and `clean`
// must lose nothing at all.
func TestProfilesAreDistinguishable(t *testing.T) {
	gw := newTestGateway(t)

	cfg := baseConfig(t, gw)
	cfg.Calls = 12
	cfg.Concurrency = 4
	cfg.Profiles = profilesOrFail(t, "clean", "lossy-wan")

	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	byName := map[string]ProfileStats{}
	for _, p := range rep.ByProfile {
		byName[p.Profile] = p
	}

	clean, ok := byName["clean"]
	if !ok {
		t.Fatal("the report has no clean profile")
	}
	lossy, ok := byName["lossy-wan"]
	if !ok {
		t.Fatal("the report has no lossy-wan profile")
	}

	if clean.MeanLossPct != 0 {
		t.Errorf("the clean profile lost %.2f%% of packets; nothing was impairing it",
			clean.MeanLossPct)
	}
	if lossy.MeanLossPct <= clean.MeanLossPct {
		t.Errorf("lossy-wan lost %.2f%% against clean's %.2f%%; the demo's beats would look identical",
			lossy.MeanLossPct, clean.MeanLossPct)
	}
	if lossy.MeanMOS >= clean.MeanMOS {
		t.Errorf("lossy-wan scored MOS %.2f against clean's %.2f; call quality should drop",
			lossy.MeanMOS, clean.MeanMOS)
	}

	t.Logf("clean loss=%.2f%% mos=%.2f | lossy-wan loss=%.2f%% mos=%.2f",
		clean.MeanLossPct, clean.MeanMOS, lossy.MeanLossPct, lossy.MeanMOS)
}

// A run is reproducible: the same seed impairs the same calls the same way.
// Without this, comparing two runs would be meaningless, and comparing runs is
// what the demo does.
func TestRunIsReproducible(t *testing.T) {
	run := func() uint64 {
		gw := newTestGateway(t)
		cfg := baseConfig(t, gw)
		cfg.Calls = 8
		cfg.Concurrency = 2
		cfg.Profiles = profilesOrFail(t, "lossy-wan")

		rep, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return rep.PacketsDropped
	}

	first, second := run(), run()
	if first != second {
		t.Errorf("the same seed dropped %d packets then %d", first, second)
	}
	if first == 0 {
		t.Error("lossy-wan dropped nothing; the impairment is not being applied")
	}
}

// Calls vary across a run — different numbers, different audio — so a trace
// list looks like a switchboard rather than one number dialing itself.
func TestCallsVaryAcrossARun(t *testing.T) {
	gw := newTestGateway(t)

	cfg := baseConfig(t, gw)
	cfg.Calls = 12
	cfg.Concurrency = 3
	cfg.Profiles = profilesOrFail(t, "clean", "mobile")

	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	froms := map[string]bool{}
	tos := map[string]bool{}
	profiles := map[string]bool{}
	ssrcs := map[uint32]bool{}
	for _, req := range gw.startRequests() {
		froms[req.From] = true
		tos[req.To] = true
		profiles[req.NetworkProfile] = true
		ssrcs[req.SSRC] = true
	}

	if len(froms) < 2 {
		t.Errorf("every call came from the same number (%v)", froms)
	}
	if len(tos) < 2 {
		t.Errorf("every call dialed the same number (%v)", tos)
	}
	if len(profiles) != 2 {
		t.Errorf("profiles seen = %v, want both configured ones", profiles)
	}
	// Each call must have its own SSRC, or the gateway would bind two calls to
	// one RTP stream.
	if len(ssrcs) != 12 {
		t.Errorf("%d distinct SSRCs across 12 calls", len(ssrcs))
	}
}

// A timed run stops on time and reports what it managed, rather than being
// treated as a failure. That is what `-duration` means.
func TestDurationBoundsTheRun(t *testing.T) {
	gw := newTestGateway(t)

	cfg := baseConfig(t, gw)
	cfg.Duration = 300 * time.Millisecond
	cfg.Concurrency = 2
	// No call count: the run ends on the clock.
	cfg.Fixtures = []Fixture{
		{Name: "two-second", Audio: codec.Tone(300, codec.SampleRate8k, 2*time.Second, 0.5)},
	}
	cfg.Pace = 2 * time.Millisecond // 100 frames at 2ms is ~200ms per call

	start := time.Now()
	rep, err := Run(context.Background(), cfg)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a run that hit its own duration returned an error: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the run took %v, well past its 300ms budget", elapsed)
	}
	if rep.Calls == 0 {
		t.Error("the run placed no calls at all")
	}
	t.Logf("%d calls in %v", rep.Calls, elapsed.Round(time.Millisecond))
}

// One failing call must not abort the run: a load test whose first teardown
// error stopped everything would report nothing about the other calls.
func TestFailuresAreReportedNotFatal(t *testing.T) {
	gw := newTestGateway(t)
	gw.endErr = errors.New("pipeline is wedged")

	cfg := baseConfig(t, gw)
	cfg.Calls = 6
	cfg.Concurrency = 2

	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.Calls != 6 {
		t.Errorf("placed %d calls, want all 6 attempted", rep.Calls)
	}
	if rep.Failed != 6 {
		t.Errorf("%d calls reported failure, want 6", rep.Failed)
	}
	// Errors are grouped, so six identical failures are one line in a report
	// rather than six.
	if len(rep.Errors) != 1 {
		t.Errorf("errors grouped into %d distinct messages, want 1: %v", len(rep.Errors), rep.Errors)
	}
	for msg, n := range rep.Errors {
		if n != 6 || !strings.Contains(msg, "wedged") {
			t.Errorf("error %q counted %d times", msg, n)
		}
	}
	if rep.Succeeded() {
		t.Error("Succeeded() is true on a run where every call failed")
	}
}

// A setup failure is reported per call too, and the run still ends cleanly
// rather than hanging on a gateway that refuses calls.
func TestSetupFailuresAreReported(t *testing.T) {
	gw := newTestGateway(t)
	gw.startErr = errors.New("at capacity")

	cfg := baseConfig(t, gw)
	cfg.Calls = 4
	cfg.Concurrency = 2

	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Failed != 4 {
		t.Errorf("%d of 4 calls failed, want 4", rep.Failed)
	}
	for msg := range rep.Errors {
		if !strings.Contains(msg, "setup") {
			t.Errorf("error %q does not say which stage failed", msg)
		}
	}
}

// Cancelling a run returns promptly and still reports what it measured, and
// the calls in flight are still torn down — a call left open loses its
// sequence range, and with it the exact loss figure.
func TestCancellationStillTearsDownCalls(t *testing.T) {
	gw := newTestGateway(t)

	cfg := baseConfig(t, gw)
	cfg.Calls = 200
	cfg.Concurrency = 4
	cfg.Fixtures = []Fixture{
		{Name: "long", Audio: codec.Tone(300, codec.SampleRate8k, 10*time.Second, 0.5)},
	}
	cfg.Pace = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()

	rep, err := Run(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
	}

	starts, ended, _ := gw.counts()
	if ended != starts {
		t.Errorf("the gateway saw %d setups but only %d teardowns; "+
			"a cancelled run must still close its calls", starts, ended)
	}
	t.Logf("cancelled after %d calls (%d set up, %d torn down)", rep.Calls, starts, ended)
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no control address", Config{Calls: 1}},
		{"neither calls nor duration", Config{ControlAddr: "x:1"}},
		{"zero concurrency", Config{ControlAddr: "x:1", Calls: 1, Concurrency: -1}},
		{"unnamed profile", Config{ControlAddr: "x:1", Calls: 1,
			Profiles: []chaos.Profile{{Network: chaos.Network{LossPct: 1}}}}},
		{"impossible impairment", Config{ControlAddr: "x:1", Calls: 1,
			Profiles: []chaos.Profile{{Name: "bad", Network: chaos.Network{LossPct: 500}}}}},
		{"empty fixture", Config{ControlAddr: "x:1", Calls: 1, Fixtures: []Fixture{{Name: "silent"}}}},
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
