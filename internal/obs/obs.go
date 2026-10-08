// Package obs holds every Datadog SDK call in the project.
//
// It is deliberately the only package that imports dd-trace-go. The Agent
// Observability SDK is explicitly experimental, so confining it here means an
// API break is a one-file fix rather than a change scattered across the
// pipeline.
//
// A nil *Tracer is valid and does nothing. That matters: the demo has to run
// with no Datadog agent present, so the pipeline is instrumented
// unconditionally and the decision about whether telemetry goes anywhere is
// made once, here, at startup.
package obs

import (
	"fmt"
	"net"
	"strings"

	"github.com/DataDog/datadog-go/v5/statsd"
	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
)

// DefaultMLApp groups this project's traces in the Agent Observability views.
const DefaultMLApp = "voice-demo"

// Config describes how telemetry is reported.
type Config struct {
	// Enabled is the master switch. When false, nothing is started and every
	// method on the returned Tracer is a no-op.
	Enabled bool

	// Service, Env and Version are the standard Datadog unified-service tags.
	Service string
	Env     string
	Version string

	// AgentHost and TracePort locate the trace agent; StatsdPort locates
	// DogStatsD.
	AgentHost  string
	TracePort  string
	StatsdPort string

	// LLMObsEnabled turns on Agent Observability. It is separate from Enabled
	// so a run can ship APM and metrics without LLM Obs, which is useful when
	// comparing what each view does and does not show.
	LLMObsEnabled bool

	// MLApp names the application in the Agent Observability views.
	MLApp string

	// Agentless submits LLM Obs data straight to Datadog instead of through
	// the agent. It requires DD_API_KEY and is the fallback when no agent is
	// reachable.
	Agentless bool
}

func (c *Config) applyDefaults() {
	if c.Service == "" {
		c.Service = "voicegw"
	}
	if c.Env == "" {
		c.Env = "demo"
	}
	if c.Version == "" {
		c.Version = "0.1.0"
	}
	if c.AgentHost == "" {
		c.AgentHost = "localhost"
	}
	if c.TracePort == "" {
		c.TracePort = "8126"
	}
	if c.StatsdPort == "" {
		c.StatsdPort = "8125"
	}
	if c.MLApp == "" {
		c.MLApp = DefaultMLApp
	}
}

// Validate rejects a configuration that cannot work.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Service == "" {
		return fmt.Errorf("obs: Service is required")
	}
	if c.LLMObsEnabled && c.MLApp == "" {
		return fmt.Errorf("obs: MLApp is required when LLM Observability is enabled")
	}
	return nil
}

// Tracer is the instrumentation handle. A nil *Tracer is a valid no-op.
type Tracer struct {
	cfg    Config
	statsd statsd.ClientInterface
	// llmobs records whether Agent Observability spans should be created.
	// Calling into the SDK while it is disabled logs a warning per span, so
	// this flag keeps a disabled run quiet rather than merely harmless.
	llmobs bool
}

// Start initializes the tracer and the DogStatsD client. A disabled Config
// returns a working no-op Tracer and no error, so callers never need to branch.
func Start(cfg Config) (*Tracer, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return &Tracer{cfg: cfg, statsd: &statsd.NoOpClient{}}, nil
	}

	opts := []tracer.StartOption{
		tracer.WithService(cfg.Service),
		tracer.WithEnv(cfg.Env),
		tracer.WithServiceVersion(cfg.Version),
		tracer.WithAgentAddr(net.JoinHostPort(cfg.AgentHost, cfg.TracePort)),
	}
	if cfg.LLMObsEnabled {
		opts = append(opts,
			tracer.WithLLMObsEnabled(true),
			tracer.WithLLMObsMLApp(cfg.MLApp),
			tracer.WithLLMObsAgentlessEnabled(cfg.Agentless),
		)
	}
	if err := tracer.Start(opts...); err != nil {
		// Enabling LLM Observability in agent mode requires a reachable agent
		// that supports it; the SDK refuses to start otherwise. Failing here is
		// the right behavior for a demo — discovering mid-presentation that the
		// dashboards are empty is far worse than failing at launch — but the
		// error has to say what to do about it.
		return nil, fmt.Errorf("obs: starting the tracer: %w\n"+
			"  the agent at %s:%s must be running and support Agent Observability.\n"+
			"  options: start it with `make up`, pass -dd-agentless with DD_API_KEY set\n"+
			"  to bypass the agent, pass -dd-llmobs=false for APM and metrics only,\n"+
			"  or drop -dd to run without telemetry",
			err, cfg.AgentHost, cfg.TracePort)
	}

	client, err := statsd.New(net.JoinHostPort(cfg.AgentHost, cfg.StatsdPort),
		statsd.WithNamespace(metricNamespace),
		statsd.WithTags(unifiedTags(cfg)),
	)
	if err != nil {
		tracer.Stop()
		return nil, fmt.Errorf("obs: connecting to DogStatsD: %w", err)
	}

	return &Tracer{cfg: cfg, statsd: client, llmobs: cfg.LLMObsEnabled}, nil
}

// unifiedTags are the standard Datadog tags applied to every metric.
func unifiedTags(cfg Config) []string {
	return []string{
		"service:" + cfg.Service,
		"env:" + cfg.Env,
		"version:" + cfg.Version,
	}
}

// Stop flushes and shuts down. It is safe on a nil or disabled Tracer.
func (t *Tracer) Stop() {
	if t == nil {
		return
	}
	if t.statsd != nil {
		t.statsd.Flush()
		t.statsd.Close()
	}
	if t.cfg.Enabled {
		tracer.Stop()
	}
}

// Flush forces pending spans and metrics out. Tests and short-lived demo runs
// need it because a process that exits immediately would otherwise drop
// everything still buffered.
func (t *Tracer) Flush() {
	if t == nil {
		return
	}
	if t.statsd != nil {
		t.statsd.Flush()
	}
	if t.cfg.Enabled {
		tracer.Flush()
	}
}

// Enabled reports whether telemetry is actually being reported.
func (t *Tracer) Enabled() bool { return t != nil && t.cfg.Enabled }

// LLMObsEnabled reports whether Agent Observability spans are being created.
func (t *Tracer) LLMObsEnabled() bool { return t != nil && t.llmobs }

// Describe summarizes the configuration for a startup log line.
func (t *Tracer) Describe() string {
	if t == nil || !t.cfg.Enabled {
		return "datadog: disabled"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "datadog: service=%s env=%s version=%s agent=%s:%s statsd=%s",
		t.cfg.Service, t.cfg.Env, t.cfg.Version,
		t.cfg.AgentHost, t.cfg.TracePort, t.cfg.StatsdPort)
	if t.llmobs {
		fmt.Fprintf(&b, " llmobs=on ml_app=%s agentless=%v", t.cfg.MLApp, t.cfg.Agentless)
	} else {
		b.WriteString(" llmobs=off")
	}
	return b.String()
}

// StartForTest returns a Tracer that reports telemetry as enabled without
// starting the dd-trace tracer or opening a DogStatsD socket.
//
// It exists for tests that drive the real SDK through testtracer, which starts
// the tracer itself against a fake agent. Calling Start in that situation would
// start a second tracer and try to reach a real DogStatsD endpoint. Metrics are
// discarded; tests that assert on metrics construct a Tracer directly within
// this package.
func StartForTest(cfg Config) (*Tracer, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Tracer{
		cfg:    cfg,
		statsd: &statsd.NoOpClient{},
		llmobs: cfg.Enabled && cfg.LLMObsEnabled,
	}, nil
}
