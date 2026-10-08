package chaos

import (
	"math"
	"testing"
	"time"
)

// TestUniformLossHitsTargetRate checks the independent loss model converges on
// the configured rate. Over 100k packets the sampling error is tiny, so a
// generous tolerance still catches a model that is wrong by any useful margin.
func TestUniformLossHitsTargetRate(t *testing.T) {
	for _, target := range []float64{0, 1, 5, 20, 50} {
		imp, err := NewImpairer(Network{LossPct: target}, 42)
		if err != nil {
			t.Fatal(err)
		}

		const n = 100_000
		for i := 0; i < n; i++ {
			imp.Next()
		}

		got := float64(imp.Stats().Dropped) / n * 100
		if math.Abs(got-target) > 0.5 {
			t.Errorf("LossPct %.0f: dropped %.2f%%, want within 0.5pp", target, got)
		}
	}
}

// TestBurstLossHitsTargetRateAndClusters verifies both halves of the
// Gilbert-Elliott model: the long-run loss rate matches the target, and the
// losses actually arrive in bursts. Bursty loss is far more damaging to speech
// recognition than the same rate spread evenly, which is the whole reason this
// model exists.
func TestBurstLossHitsTargetRateAndClusters(t *testing.T) {
	const (
		target  = 5.0
		meanLen = 4.0
		n       = 200_000
	)
	imp, err := NewImpairer(Network{
		LossPct:      target,
		LossBurst:    true,
		BurstMeanLen: meanLen,
	}, 7)
	if err != nil {
		t.Fatal(err)
	}

	var (
		drops      int
		bursts     int
		inBurst    bool
		burstTotal int
	)
	for i := 0; i < n; i++ {
		if imp.Next().Drop {
			drops++
			burstTotal++
			if !inBurst {
				bursts++
				inBurst = true
			}
		} else {
			inBurst = false
		}
	}

	gotPct := float64(drops) / n * 100
	if math.Abs(gotPct-target) > 0.75 {
		t.Errorf("burst loss rate %.2f%%, want ~%.1f%%", gotPct, target)
	}

	gotMean := float64(burstTotal) / float64(bursts)
	if math.Abs(gotMean-meanLen) > 0.5 {
		t.Errorf("mean burst length %.2f, want ~%.1f", gotMean, meanLen)
	}
	t.Logf("burst model: %.2f%% loss across %d bursts, mean length %.2f",
		gotPct, bursts, gotMean)
}

// TestBurstLossIsActuallyBurstier is the comparison that matters: at the same
// loss rate, the burst model must produce measurably longer runs than the
// independent model. Otherwise the LossBurst flag is decorative.
func TestBurstLossIsActuallyBurstier(t *testing.T) {
	const (
		target = 10.0
		n      = 200_000
	)
	uniform, err := NewImpairer(Network{LossPct: target}, 1)
	if err != nil {
		t.Fatal(err)
	}
	burst, err := NewImpairer(Network{LossPct: target, LossBurst: true, BurstMeanLen: 5}, 1)
	if err != nil {
		t.Fatal(err)
	}

	meanRun := func(imp *Impairer) float64 {
		var runs, drops int
		in := false
		for i := 0; i < n; i++ {
			if imp.Next().Drop {
				drops++
				if !in {
					runs++
					in = true
				}
			} else {
				in = false
			}
		}
		if runs == 0 {
			return 0
		}
		return float64(drops) / float64(runs)
	}

	u, b := meanRun(uniform), meanRun(burst)
	if b <= u*2 {
		t.Errorf("burst mean run %.2f vs uniform %.2f: burst model is not "+
			"meaningfully burstier", b, u)
	}
	t.Logf("mean consecutive-loss run: uniform %.2f, burst %.2f", u, b)
}

// TestImpairerIsDeterministic is what makes the loss-accounting tests meaningful:
// the same seed must replay the same decisions exactly.
func TestImpairerIsDeterministic(t *testing.T) {
	cfg := Network{LossPct: 5, JitterMs: 30, ReorderPct: 2, DupPct: 1, LatencyMs: 20}

	run := func() []Action {
		imp, err := NewImpairer(cfg, 99)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]Action, 1000)
		for i := range out {
			out[i] = imp.Next()
		}
		return out
	}

	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("packet %d differs between runs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestDelayWithinConfiguredBounds(t *testing.T) {
	const (
		latency = 20.0
		jitter  = 30.0
	)
	imp, err := NewImpairer(Network{LatencyMs: latency, JitterMs: jitter}, 3)
	if err != nil {
		t.Fatal(err)
	}

	minD := time.Duration(latency * float64(time.Millisecond))
	maxD := time.Duration((latency + jitter) * float64(time.Millisecond))

	var sawSpread bool
	last := time.Duration(-1)
	for i := 0; i < 10_000; i++ {
		d := imp.Next().Delay
		if d < minD || d > maxD {
			t.Fatalf("delay %v outside [%v, %v]", d, minD, maxD)
		}
		if last >= 0 && d != last {
			sawSpread = true
		}
		last = d
	}
	if !sawSpread {
		t.Error("every delay was identical; jitter is not being applied")
	}
}

func TestReorderAddsHoldTime(t *testing.T) {
	// 100% reorder makes the behavior unambiguous.
	imp, err := NewImpairer(Network{ReorderPct: 100}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if d := imp.Next().Delay; d < reorderHold {
			t.Fatalf("reordered packet delay = %v, want >= %v", d, reorderHold)
		}
	}
	if got := imp.Stats().Reordered; got != 100 {
		t.Errorf("Reordered = %d, want 100", got)
	}
}

func TestDuplicationRate(t *testing.T) {
	const target = 10.0
	imp, err := NewImpairer(Network{DupPct: target}, 11)
	if err != nil {
		t.Fatal(err)
	}

	const n = 100_000
	dups := 0
	for i := 0; i < n; i++ {
		if imp.Next().Duplicate {
			dups++
		}
	}
	got := float64(dups) / n * 100
	if math.Abs(got-target) > 0.5 {
		t.Errorf("duplicated %.2f%%, want ~%.1f%%", got, target)
	}
}

// TestDroppedPacketsAreNotAlsoDelayedOrDuplicated guards the obvious logical
// slip: a dropped packet has no delay and no copy, because it does not exist.
func TestDroppedPacketsAreNotAlsoDelayedOrDuplicated(t *testing.T) {
	imp, err := NewImpairer(Network{LossPct: 100, JitterMs: 50, DupPct: 100}, 13)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		a := imp.Next()
		if !a.Drop {
			t.Fatalf("packet %d survived 100%% loss", i)
		}
		if a.Delay != 0 || a.Duplicate {
			t.Fatalf("dropped packet %d also had Delay=%v Duplicate=%v", i, a.Delay, a.Duplicate)
		}
	}
	if got := imp.Stats(); got.Duplicated != 0 {
		t.Errorf("Duplicated = %d among fully-dropped traffic, want 0", got.Duplicated)
	}
}

func TestCleanConfigPassesEverythingThrough(t *testing.T) {
	imp, err := NewImpairer(Network{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if a := imp.Next(); a.Drop || a.Duplicate || a.Delay != 0 {
			t.Fatalf("clean config impaired packet %d: %+v", i, a)
		}
	}
	st := imp.Stats()
	if st.Offered != 1000 || st.Dropped != 0 {
		t.Errorf("Stats = %+v, want 1000 offered and 0 dropped", st)
	}
}

func TestNetworkValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Network
		wantErr bool
	}{
		{"zero value", Network{}, false},
		{"full loss", Network{LossPct: 100}, false},
		{"negative loss", Network{LossPct: -1}, true},
		{"loss over 100", Network{LossPct: 101}, true},
		{"negative latency", Network{LatencyMs: -1}, true},
		{"negative jitter", Network{JitterMs: -1}, true},
		{"reorder over 100", Network{ReorderPct: 150}, true},
		{"dup over 100", Network{DupPct: 150}, true},
		{"burst without mean length", Network{LossPct: 5, LossBurst: true}, true},
		{"burst with mean length", Network{LossPct: 5, LossBurst: true, BurstMeanLen: 2}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
			if _, err := NewImpairer(tc.cfg, 1); (err != nil) != tc.wantErr {
				t.Errorf("NewImpairer() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestIsClean(t *testing.T) {
	if !(Network{}).IsClean() {
		t.Error("zero value is not reported clean")
	}
	for name, cfg := range map[string]Network{
		"loss":    {LossPct: 1},
		"latency": {LatencyMs: 1},
		"jitter":  {JitterMs: 1},
		"reorder": {ReorderPct: 1},
		"dup":     {DupPct: 1},
	} {
		if cfg.IsClean() {
			t.Errorf("%s config reported clean", name)
		}
	}
}

// TestProfilesAreValid keeps a typo in a demo profile from surfacing live.
func TestProfilesAreValid(t *testing.T) {
	for _, name := range ProfileNames() {
		p, err := LookupProfile(name)
		if err != nil {
			t.Fatalf("LookupProfile(%q): %v", name, err)
		}
		if p.Name != name {
			t.Errorf("profile %q has Name = %q", name, p.Name)
		}
		if p.Description == "" {
			t.Errorf("profile %q has no description", name)
		}
		if err := p.Network.Validate(); err != nil {
			t.Errorf("profile %q network invalid: %v", name, err)
		}
		if _, err := NewImpairer(p.Network, 1); err != nil {
			t.Errorf("profile %q rejected by NewImpairer: %v", name, err)
		}
	}

	if len(ProfileNames()) != len(Profiles) {
		t.Errorf("ProfileNames lists %d entries but Profiles has %d",
			len(ProfileNames()), len(Profiles))
	}
	if _, err := LookupProfile("nope"); err == nil {
		t.Error("LookupProfile accepted an unknown name")
	}
}

// TestProfileSeverityOrdering pins the demo narrative: each profile is worse
// than the last, so stepping through them tells a coherent story on a dashboard.
func TestProfileSeverityOrdering(t *testing.T) {
	clean := Profiles["clean"].Network
	mobile := Profiles["mobile"].Network
	lossy := Profiles["lossy-wan"].Network

	if !clean.IsClean() {
		t.Error("the clean profile is not clean")
	}
	if mobile.LossPct >= lossy.LossPct {
		t.Errorf("mobile loss %.1f%% >= lossy-wan %.1f%%", mobile.LossPct, lossy.LossPct)
	}
	if mobile.JitterMs >= lossy.JitterMs {
		t.Errorf("mobile jitter %.0fms >= lossy-wan %.0fms", mobile.JitterMs, lossy.JitterMs)
	}
	if !lossy.LossBurst {
		t.Error("lossy-wan should use the burst loss model")
	}

	// provider-degraded isolates provider latency, so its network must be clean.
	degraded := Profiles["provider-degraded"]
	if !degraded.Network.IsClean() {
		t.Error("provider-degraded should leave the network untouched")
	}
	if degraded.Provider.STTExtraLatencyMs == 0 {
		t.Error("provider-degraded should add STT latency")
	}
}

// TestSetNetworkRetunesLive covers the /chaos endpoint's path: flipping a profile
// mid-call must take effect and must reject bad input without corrupting state.
func TestSetNetworkRetunesLive(t *testing.T) {
	imp, err := NewImpairer(Network{}, 17)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if imp.Next().Drop {
			t.Fatal("clean config dropped a packet")
		}
	}

	if err := imp.SetNetwork(Network{LossPct: 100}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if !imp.Next().Drop {
			t.Fatal("packet survived after retuning to 100% loss")
		}
	}
	if got := imp.Network().LossPct; got != 100 {
		t.Errorf("Network().LossPct = %v, want 100", got)
	}

	if err := imp.SetNetwork(Network{LossPct: -5}); err == nil {
		t.Error("SetNetwork accepted an invalid config")
	}
	if got := imp.Network().LossPct; got != 100 {
		t.Errorf("a rejected SetNetwork changed the config to %v", got)
	}
}

// TestConcurrentUse exercises the mutex: the receive path calls Next while the
// control endpoint calls SetNetwork. Run with -race to make this meaningful.
func TestConcurrentUse(t *testing.T) {
	imp, err := NewImpairer(Network{LossPct: 5, JitterMs: 10}, 23)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			imp.Next()
		}
	}()
	for i := 0; i < 200; i++ {
		cfg := Network{LossPct: float64(i % 20)}
		if i%2 == 0 {
			cfg.LossBurst, cfg.BurstMeanLen = true, 3
		}
		if err := imp.SetNetwork(cfg); err != nil {
			t.Fatal(err)
		}
		imp.Stats()
		imp.Network()
	}
	<-done
}
