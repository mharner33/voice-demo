package tts

import (
	"context"

	"github.com/mharner33/voice-demo/internal/faults"
)

type faulty struct {
	inner Synthesizer
	inj   *faults.Injector
}

// WithFaults returns a Synthesizer that impairs the wrapped one. A clean config
// returns the original unchanged.
//
// The fault is applied before the first chunk, which is where it belongs: the
// perceived failure of a slow synthesizer is the silence before the agent
// starts speaking, not the rate at which audio arrives afterward.
func WithFaults(s Synthesizer, cfg faults.Config) (Synthesizer, error) {
	if cfg.IsClean() {
		return s, nil
	}
	inj, err := faults.New(cfg)
	if err != nil {
		return nil, err
	}
	return &faulty{inner: s, inj: inj}, nil
}

func (f *faulty) Synthesize(ctx context.Context, text string, opts Options) (<-chan Audio, error) {
	if err := f.inj.Apply(ctx); err != nil {
		return nil, err
	}
	return f.inner.Synthesize(ctx, text, opts)
}

func (f *faulty) Info() Info { return f.inner.Info() }

// Injector exposes the fault injector for live retuning and end-of-call
// reporting.
func (f *faulty) Injector() *faults.Injector { return f.inj }
