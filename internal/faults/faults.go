// Package faults injects latency and errors into provider calls, so a demo can
// show what a sick STT, LLM, or TTS backend looks like in Datadog on command.
//
// It is a separate package from chaos because the two impair different layers
// and are observed differently: chaos degrades packets on the wire and shows up
// as loss and jitter, while these faults degrade API calls and show up as span
// duration and error rates.
package faults

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ErrInjected is returned by a deliberately failed call. Callers use
// errors.Is to distinguish an injected fault from a genuine provider failure,
// which matters when deciding whether a demo is broken or behaving as asked.
var ErrInjected = errors.New("faults: injected provider error")

// Config describes the impairment applied to one provider.
type Config struct {
	// ExtraLatency is added to every call before it runs.
	ExtraLatency time.Duration
	// ErrorRate is the fraction of calls that fail, from 0 to 1.
	ErrorRate float64
	// Seed makes the failure pattern reproducible.
	Seed int64
}

// IsClean reports whether this config leaves calls untouched, letting callers
// skip wrapping entirely.
func (c Config) IsClean() bool {
	return c.ExtraLatency <= 0 && c.ErrorRate <= 0
}

// Validate rejects nonsense early, so a bad flag fails at startup rather than
// midway through a demo.
func (c Config) Validate() error {
	if c.ExtraLatency < 0 {
		return fmt.Errorf("faults: ExtraLatency = %v, want >= 0", c.ExtraLatency)
	}
	if c.ErrorRate < 0 || c.ErrorRate > 1 {
		return fmt.Errorf("faults: ErrorRate = %v, want 0-1", c.ErrorRate)
	}
	return nil
}

// Stats records what was actually injected, for reporting at end of call and
// for asserting in tests.
type Stats struct {
	Calls        uint64
	Errors       uint64
	TotalLatency time.Duration
}

// Injector applies a Config to successive calls. Safe for concurrent use: one
// injector may guard a provider shared across calls.
type Injector struct {
	// Sleeper performs the latency delay. It defaults to a context-aware sleep;
	// tests replace it to keep themselves instant while still asserting the
	// duration that would have been waited.
	Sleeper func(ctx context.Context, d time.Duration) error

	mu  sync.Mutex
	cfg Config
	rng *rand.Rand
	st  Stats
}

// New creates an Injector.
func New(cfg Config) (*Injector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Injector{
		Sleeper: Sleep,
		cfg:     cfg,
		rng:     rand.New(rand.NewSource(cfg.Seed)),
	}, nil
}

// Sleep waits for d or until ctx is done, whichever comes first. A provider
// call that is being artificially delayed must still honor cancellation, or a
// hung call could outlive the context that was supposed to bound it.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Apply impairs one call. It waits out any configured latency and then returns
// ErrInjected if this call is selected to fail.
//
// Latency is applied before the error so that a failing call still costs what a
// real timeout would. Returning instantly would make a provider-degraded demo
// look faster than a healthy one, which is backwards.
func (i *Injector) Apply(ctx context.Context) error {
	i.mu.Lock()
	cfg := i.cfg
	fail := cfg.ErrorRate > 0 && i.rng.Float64() < cfg.ErrorRate
	i.st.Calls++
	if cfg.ExtraLatency > 0 {
		i.st.TotalLatency += cfg.ExtraLatency
	}
	if fail {
		i.st.Errors++
	}
	sleeper := i.Sleeper
	i.mu.Unlock()

	if cfg.ExtraLatency > 0 {
		if sleeper == nil {
			sleeper = Sleep
		}
		if err := sleeper(ctx, cfg.ExtraLatency); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fail {
		return fmt.Errorf("%w (rate %.0f%%)", ErrInjected, cfg.ErrorRate*100)
	}
	return nil
}

// SetConfig retunes the injector mid-call, for the live /chaos endpoint.
func (i *Injector) SetConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cfg = cfg
	return nil
}

// Config returns the current configuration.
func (i *Injector) Config() Config {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cfg
}

// Stats snapshots what has been injected.
func (i *Injector) Stats() Stats {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.st
}
