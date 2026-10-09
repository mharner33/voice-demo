package faults

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// recordingSleeper stands in for real sleeping so tests stay instant while
// still asserting exactly what would have been waited.
type recordingSleeper struct {
	calls   int
	total   time.Duration
	lastCtx context.Context
}

func (r *recordingSleeper) sleep(ctx context.Context, d time.Duration) error {
	r.calls++
	r.total += d
	r.lastCtx = ctx
	return ctx.Err()
}

func newTestInjector(t *testing.T, cfg Config) (*Injector, *recordingSleeper) {
	t.Helper()
	inj, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	rs := &recordingSleeper{}
	inj.Sleeper = rs.sleep
	return inj, rs
}

func TestCleanConfigNeverImpairs(t *testing.T) {
	inj, rs := newTestInjector(t, Config{})

	for i := 0; i < 100; i++ {
		if err := inj.Apply(context.Background()); err != nil {
			t.Fatalf("call %d: Apply = %v, want nil", i, err)
		}
	}
	if rs.calls != 0 {
		t.Errorf("slept %d times with no configured latency", rs.calls)
	}

	st := inj.Stats()
	if st.Calls != 100 {
		t.Errorf("Calls = %d, want 100", st.Calls)
	}
	if st.Errors != 0 {
		t.Errorf("Errors = %d, want 0", st.Errors)
	}
	if st.TotalLatency != 0 {
		t.Errorf("TotalLatency = %v, want 0", st.TotalLatency)
	}
}

func TestLatencyIsAppliedPerCall(t *testing.T) {
	const (
		latency = 2 * time.Second
		calls   = 10
	)
	inj, rs := newTestInjector(t, Config{ExtraLatency: latency})

	for i := 0; i < calls; i++ {
		if err := inj.Apply(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if rs.calls != calls {
		t.Errorf("slept %d times, want %d", rs.calls, calls)
	}
	if want := latency * calls; rs.total != want {
		t.Errorf("slept %v in total, want %v", rs.total, want)
	}
	if got := inj.Stats().TotalLatency; got != latency*calls {
		t.Errorf("Stats().TotalLatency = %v, want %v", got, latency*calls)
	}
}

func TestErrorRateIsHonored(t *testing.T) {
	for _, rate := range []float64{0, 0.1, 0.5, 1.0} {
		inj, _ := newTestInjector(t, Config{ErrorRate: rate, Seed: 42})

		const n = 20_000
		var failures int
		for i := 0; i < n; i++ {
			if err := inj.Apply(context.Background()); err != nil {
				if !errors.Is(err, ErrInjected) {
					t.Fatalf("rate %.1f: got %v, want an ErrInjected", rate, err)
				}
				failures++
			}
		}

		got := float64(failures) / n
		if math.Abs(got-rate) > 0.02 {
			t.Errorf("ErrorRate %.2f: failed %.4f of calls, want within 0.02", rate, got)
		}
		if uint64(failures) != inj.Stats().Errors {
			t.Errorf("rate %.1f: counted %d failures but Stats says %d",
				rate, failures, inj.Stats().Errors)
		}
	}
}

// TestLatencyAppliedBeforeError pins an ordering that matters for the demo: a
// degraded provider must still be slow when it fails. Returning instantly on
// failure would make the provider-degraded profile look faster than a healthy
// one, which is backwards.
func TestLatencyAppliedBeforeError(t *testing.T) {
	inj, rs := newTestInjector(t, Config{
		ExtraLatency: time.Second,
		ErrorRate:    1.0,
	})

	err := inj.Apply(context.Background())
	if !errors.Is(err, ErrInjected) {
		t.Fatalf("Apply = %v, want ErrInjected", err)
	}
	if rs.calls != 1 {
		t.Errorf("slept %d times before failing, want 1", rs.calls)
	}
	if rs.total != time.Second {
		t.Errorf("slept %v before failing, want 1s", rs.total)
	}
}

func TestDeterministicPerSeed(t *testing.T) {
	run := func() []bool {
		inj, _ := newTestInjector(t, Config{ErrorRate: 0.3, Seed: 7})
		out := make([]bool, 500)
		for i := range out {
			out[i] = inj.Apply(context.Background()) != nil
		}
		return out
	}

	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("call %d differs between runs with the same seed", i)
		}
	}
}

// TestContextCancellationIsHonored matters because an injected delay must not
// be able to outlive the context that was supposed to bound the call.
func TestContextCancellationIsHonored(t *testing.T) {
	inj, err := New(Config{ExtraLatency: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if err := inj.Apply(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Apply with a cancelled context = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Apply took %v to notice cancellation", elapsed)
	}
}

// TestCancellationBeatsInjectedError checks that a cancelled call reports
// cancellation rather than a fabricated provider error, so a demo operator is
// not misled about why a call failed.
func TestCancellationBeatsInjectedError(t *testing.T) {
	inj, err := New(Config{ErrorRate: 1.0})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := inj.Apply(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Apply = %v, want context.Canceled", err)
	}
}

func TestRealSleepWaitsAndRespectsContext(t *testing.T) {
	start := time.Now()
	if err := Sleep(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Errorf("Sleep returned after %v, want at least ~20ms", elapsed)
	}

	// A non-positive duration is a no-op, not an error.
	if err := Sleep(context.Background(), 0); err != nil {
		t.Errorf("Sleep(0) = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Sleep past a deadline = %v, want DeadlineExceeded", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"zero value", Config{}, false},
		{"latency only", Config{ExtraLatency: time.Second}, false},
		{"full error rate", Config{ErrorRate: 1}, false},
		{"negative latency", Config{ExtraLatency: -time.Second}, true},
		{"negative rate", Config{ErrorRate: -0.1}, true},
		{"rate above one", Config{ErrorRate: 1.5}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if _, err := New(tc.cfg); (err != nil) != tc.wantErr {
				t.Errorf("New() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestIsClean(t *testing.T) {
	if !(Config{}).IsClean() {
		t.Error("zero value is not reported clean")
	}
	if (Config{ExtraLatency: time.Nanosecond}).IsClean() {
		t.Error("a latency config was reported clean")
	}
	if (Config{ErrorRate: 0.001}).IsClean() {
		t.Error("an error-rate config was reported clean")
	}
}

// TestSetConfigRetunesLive covers the /chaos endpoint's path.
func TestSetConfigRetunesLive(t *testing.T) {
	inj, _ := newTestInjector(t, Config{})

	if err := inj.Apply(context.Background()); err != nil {
		t.Fatalf("clean Apply = %v", err)
	}
	if err := inj.SetConfig(Config{ErrorRate: 1.0}); err != nil {
		t.Fatal(err)
	}
	if err := inj.Apply(context.Background()); !errors.Is(err, ErrInjected) {
		t.Errorf("Apply after retuning = %v, want ErrInjected", err)
	}
	if got := inj.Config().ErrorRate; got != 1.0 {
		t.Errorf("Config().ErrorRate = %v, want 1.0", got)
	}

	if err := inj.SetConfig(Config{ErrorRate: 5}); err == nil {
		t.Error("SetConfig accepted an invalid config")
	}
	if got := inj.Config().ErrorRate; got != 1.0 {
		t.Errorf("a rejected SetConfig changed the rate to %v", got)
	}
}

// TestConcurrentUse exercises the mutex; meaningful under -race.
func TestConcurrentUse(t *testing.T) {
	inj, _ := newTestInjector(t, Config{ErrorRate: 0.5, Seed: 1})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			inj.Apply(context.Background())
		}
	}()
	for i := 0; i < 200; i++ {
		inj.Stats()
		inj.Config()
		if err := inj.SetConfig(Config{ErrorRate: float64(i%10) / 10}); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

// A gateway started with no impairment still has to be impairable, or the live
// /chaos endpoint would have nothing to retune and a demo could not flip
// conditions with a dashboard already on screen.
func TestTunableConfigIsNeverClean(t *testing.T) {
	if (Config{Tunable: true}).IsClean() {
		t.Error("a tunable zero config reports itself clean; the wrapper would be dropped")
	}
	if !(Config{}).IsClean() {
		t.Error("a plain zero config is not clean; the healthy path would pay for nothing")
	}
}

// A tunable injector starts as a no-op and takes effect once retuned.
func TestTunableInjectorStartsClean(t *testing.T) {
	inj, err := New(Config{Tunable: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var slept time.Duration
	inj.Sleeper = func(_ context.Context, d time.Duration) error { slept += d; return nil }

	if err := inj.Apply(context.Background()); err != nil {
		t.Errorf("a clean tunable injector failed a call: %v", err)
	}
	if slept != 0 {
		t.Errorf("a clean tunable injector waited %v", slept)
	}

	if err := inj.SetConfig(Config{ExtraLatency: 2 * time.Second, Tunable: true}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := inj.Apply(context.Background()); err != nil {
		t.Errorf("Apply after retuning: %v", err)
	}
	if slept != 2*time.Second {
		t.Errorf("waited %v after retuning, want 2s", slept)
	}
}
