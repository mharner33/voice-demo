package stt

import (
	"context"

	"github.com/mharner33/voice-demo/internal/faults"
)

// faulty wraps a Transcriber with injected latency and errors.
type faulty struct {
	inner Transcriber
	inj   *faults.Injector
}

// WithFaults returns a Transcriber that impairs the wrapped one. A clean config
// returns the original unchanged, so the healthy path carries no overhead.
//
// The fault is applied when the stream is opened rather than per result: a
// degraded recognizer shows up as a slow or failed connection, which is what
// actually happens when a provider is unhealthy, and it keeps the injected
// latency attributable to one span instead of smeared across every partial.
func WithFaults(t Transcriber, cfg faults.Config) (Transcriber, error) {
	if cfg.IsClean() {
		return t, nil
	}
	inj, err := faults.New(cfg)
	if err != nil {
		return nil, err
	}
	return &faulty{inner: t, inj: inj}, nil
}

func (f *faulty) Stream(ctx context.Context, audio <-chan Audio) (<-chan Result, error) {
	if err := f.inj.Apply(ctx); err != nil {
		return nil, err
	}
	return f.inner.Stream(ctx, audio)
}

func (f *faulty) Info() Info { return f.inner.Info() }

// Injector exposes the fault injector, so the control endpoint can retune it
// mid-call and the end-of-call report can say what was injected.
func (f *faulty) Injector() *faults.Injector { return f.inj }
