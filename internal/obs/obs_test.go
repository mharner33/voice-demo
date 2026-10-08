package obs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DataDog/datadog-go/v5/statsd"
)

// captureStatsd records what was emitted instead of sending it. It embeds
// NoOpClient so the large ClientInterface surface is satisfied without
// implementing methods this project never calls.
type captureStatsd struct {
	statsd.NoOpClient

	mu      sync.Mutex
	counts  map[string]int64
	dists   map[string][]float64
	tags    map[string][]string
	incrs   map[string]int
	flushes int
}

func newCapture() *captureStatsd {
	return &captureStatsd{
		counts: map[string]int64{},
		dists:  map[string][]float64{},
		tags:   map[string][]string{},
		incrs:  map[string]int{},
	}
}

func (c *captureStatsd) Count(name string, v int64, tags []string, _ float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[name] += v
	c.tags[name] = tags
	return nil
}

func (c *captureStatsd) Distribution(name string, v float64, tags []string, _ float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dists[name] = append(c.dists[name], v)
	c.tags[name] = tags
	return nil
}

func (c *captureStatsd) Incr(name string, tags []string, _ float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.incrs[name]++
	c.tags[name] = tags
	return nil
}

func (c *captureStatsd) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushes++
	return nil
}

func (c *captureStatsd) count(name string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.counts[name]
	return v, ok
}

func (c *captureStatsd) dist(name string) ([]float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.dists[name]
	return v, ok
}

func (c *captureStatsd) tagsFor(name string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tags[name]
}

// newTestTracer builds a Tracer whose metrics are captured rather than sent.
// It does not start the real dd-trace tracer, which keeps these tests fast and
// hermetic; span behavior is covered separately in span_test.go.
func newTestTracer(cfg Config) (*Tracer, *captureStatsd) {
	cfg.applyDefaults()
	cap := newCapture()
	return &Tracer{cfg: cfg, statsd: cap, llmobs: cfg.LLMObsEnabled && cfg.Enabled}, cap
}

// TestNilTracerIsSafe is the property the whole design rests on: the pipeline
// is instrumented unconditionally, so every method has to tolerate there being
// no tracer at all.
func TestNilTracerIsSafe(t *testing.T) {
	var tr *Tracer

	if tr.Enabled() {
		t.Error("a nil Tracer reports enabled")
	}
	if tr.LLMObsEnabled() {
		t.Error("a nil Tracer reports LLM Obs enabled")
	}
	if got := tr.Describe(); !strings.Contains(got, "disabled") {
		t.Errorf("Describe() = %q, want it to say disabled", got)
	}

	// None of these may panic, and none may produce a span.
	ctx := context.Background()
	for name, start := range map[string]func() (*Span, context.Context){
		"apm":      func() (*Span, context.Context) { return tr.StartAPM(ctx, "n", nil) },
		"apm-at":   func() (*Span, context.Context) { return tr.StartAPMAt(ctx, "n", time.Now(), nil) },
		"workflow": func() (*Span, context.Context) { return tr.StartWorkflow(ctx, "n", "s") },
		"agent":    func() (*Span, context.Context) { return tr.StartAgent(ctx, "n") },
		"tool":     func() (*Span, context.Context) { return tr.StartTool(ctx, "n") },
		"llm":      func() (*Span, context.Context) { return tr.StartLLM(ctx, "n", "m", "p") },
		"llm-at":   func() (*Span, context.Context) { return tr.StartLLMAt(ctx, "n", "m", "p", time.Now()) },
	} {
		span, gotCtx := start()
		if span != nil {
			t.Errorf("%s: got a span from a nil Tracer", name)
		}
		if gotCtx != ctx {
			t.Errorf("%s: the context was modified by a nil Tracer", name)
		}
	}

	tr.RecordNetwork(NetworkStats{}, CallTags{})
	tr.RecordBuffer(BufferStats{}, CallTags{})
	tr.RecordCall(PipelineStats{}, time.Second, CallTags{})
	tr.RecordTurn(TurnStats{}, CallTags{})
	tr.Incr("x", nil)
	tr.Flush()
	tr.Stop()
}

// TestNilSpanIsSafe covers the other half: when telemetry is off the pipeline
// holds nil spans and keeps calling methods on them.
func TestNilSpanIsSafe(t *testing.T) {
	var s *Span

	s.TextIO("in", "out", map[string]any{"k": "v"})
	s.LLMIO(
		[]Message{{Role: "user", Content: "in"}},
		[]Message{{Role: "assistant", Content: "out"}},
		Metrics{InputTokens: 1}, nil,
	)
	s.SetTags(map[string]string{"k": "v"})
	s.SetAPMTag("k", 1)
	s.Finish(nil)
	s.Finish(context.Canceled)

	if got := s.APMTraceID(); got != "" {
		t.Errorf("APMTraceID() = %q on a nil Span, want empty", got)
	}
	if got := s.APMSpanID(); got != 0 {
		t.Errorf("APMSpanID() = %d on a nil Span, want 0", got)
	}
}

// TestDisabledTracerEmitsNothing checks the master switch. A disabled run must
// be genuinely silent, not merely harmless, because the SDK logs a warning on
// every span it is asked to create while off.
func TestDisabledTracerEmitsNothing(t *testing.T) {
	tr, err := Start(Config{Enabled: false})
	if err != nil {
		t.Fatalf("Start with Enabled=false: %v", err)
	}
	defer tr.Stop()

	if tr == nil {
		t.Fatal("Start returned a nil Tracer; callers would have to branch")
	}
	if tr.Enabled() {
		t.Error("a disabled Tracer reports enabled")
	}
	if tr.LLMObsEnabled() {
		t.Error("a disabled Tracer reports LLM Obs enabled")
	}

	ctx := context.Background()
	span, gotCtx := tr.StartWorkflow(ctx, "voice_call", "c-1")
	if span != nil {
		t.Error("a disabled Tracer produced a span")
	}
	if gotCtx != ctx {
		t.Error("a disabled Tracer modified the context")
	}
	tr.RecordNetwork(NetworkStats{Received: 10}, CallTags{})
}

func TestConfigDefaults(t *testing.T) {
	var cfg Config
	cfg.applyDefaults()

	for name, got := range map[string]string{
		"Service":    cfg.Service,
		"Env":        cfg.Env,
		"Version":    cfg.Version,
		"AgentHost":  cfg.AgentHost,
		"TracePort":  cfg.TracePort,
		"StatsdPort": cfg.StatsdPort,
		"MLApp":      cfg.MLApp,
	} {
		if got == "" {
			t.Errorf("%s is empty after applyDefaults", name)
		}
	}
	if cfg.MLApp != DefaultMLApp {
		t.Errorf("MLApp = %q, want %q", cfg.MLApp, DefaultMLApp)
	}
	if cfg.TracePort != "8126" {
		t.Errorf("TracePort = %q, want 8126", cfg.TracePort)
	}
	if cfg.StatsdPort != "8125" {
		t.Errorf("StatsdPort = %q, want 8125", cfg.StatsdPort)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"disabled needs nothing", Config{Enabled: false}, false},
		{"enabled with defaults", Config{Enabled: true, Service: "s"}, false},
		{"enabled without service", Config{Enabled: true}, true},
		{"llmobs without ml app", Config{Enabled: true, Service: "s", LLMObsEnabled: true}, true},
		{"llmobs with ml app", Config{
			Enabled: true, Service: "s", LLMObsEnabled: true, MLApp: "a",
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestMLAppDefaultSatisfiesValidation checks that the LLM Obs requirement is met
// by the default rather than forcing every caller to set it. Validate runs
// after applyDefaults in Start, so an unset MLApp is valid there even though it
// is invalid on a raw Config.
func TestMLAppDefaultSatisfiesValidation(t *testing.T) {
	raw := Config{Enabled: true, Service: "s", LLMObsEnabled: true}
	if err := raw.Validate(); err == nil {
		t.Error("a raw Config with no MLApp passed Validate")
	}

	defaulted := raw
	defaulted.applyDefaults()
	if err := defaulted.Validate(); err != nil {
		t.Errorf("Validate after applyDefaults = %v, want nil", err)
	}
	if defaulted.MLApp != DefaultMLApp {
		t.Errorf("MLApp = %q, want %q", defaulted.MLApp, DefaultMLApp)
	}
}

func TestDescribe(t *testing.T) {
	tr, _ := newTestTracer(Config{
		Enabled: true, Service: "voicegw", Env: "demo", LLMObsEnabled: true, MLApp: "voice-demo",
	})
	got := tr.Describe()
	for _, want := range []string{"voicegw", "demo", "llmobs=on", "voice-demo"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}

	off, _ := newTestTracer(Config{Enabled: true, Service: "voicegw"})
	if got := off.Describe(); !strings.Contains(got, "llmobs=off") {
		t.Errorf("Describe() with LLM Obs off = %q", got)
	}
}

func TestUnifiedTags(t *testing.T) {
	got := unifiedTags(Config{Service: "voicegw", Env: "demo", Version: "1.2.3"})
	want := map[string]bool{"service:voicegw": true, "env:demo": true, "version:1.2.3": true}
	if len(got) != len(want) {
		t.Fatalf("got %d tags, want %d: %v", len(got), len(want), got)
	}
	for _, tag := range got {
		if !want[tag] {
			t.Errorf("unexpected tag %q", tag)
		}
	}
}
