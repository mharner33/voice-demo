package llm

import (
	"context"

	"github.com/mharner33/voice-demo/internal/faults"
)

type faulty struct {
	inner Agent
	inj   *faults.Injector
}

// WithFaults returns an Agent that impairs the wrapped one. A clean config
// returns the original unchanged.
//
// Unlike the streaming transcriber, the fault applies to every Reply call,
// because each one is a separate round trip to the model. A turn that needs a
// tool therefore pays the latency twice, which is exactly what happens with a
// genuinely slow model and is worth seeing in a trace.
func WithFaults(a Agent, cfg faults.Config) (Agent, error) {
	if cfg.IsClean() {
		return a, nil
	}
	inj, err := faults.New(cfg)
	if err != nil {
		return nil, err
	}
	return &faulty{inner: a, inj: inj}, nil
}

func (f *faulty) Reply(ctx context.Context, req Request) (Reply, error) {
	if err := f.inj.Apply(ctx); err != nil {
		return Reply{}, err
	}
	return f.inner.Reply(ctx, req)
}

func (f *faulty) Info() Info { return f.inner.Info() }

// Injector exposes the fault injector for live retuning and end-of-call
// reporting.
func (f *faulty) Injector() *faults.Injector { return f.inj }
