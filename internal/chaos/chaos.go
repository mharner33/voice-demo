// Package chaos injects controlled network and provider impairments so a demo can
// show what degraded voice quality looks like in Datadog on command.
//
// Network impairments are applied on the sending side, which is where real packet
// loss and jitter originate. Provider impairments are consumed by the STT/LLM/TTS
// wrappers in later phases; they live here so that a named profile can describe a
// whole scenario in one place.
package chaos

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Network describes impairments applied to outbound RTP packets.
type Network struct {
	// LossPct is the percentage of packets to drop, 0-100.
	LossPct float64
	// LossBurst switches from independent per-packet loss to a Gilbert-Elliott
	// two-state burst model. Real networks lose packets in bursts, and bursts
	// hurt speech recognition far more than the same loss spread evenly.
	LossBurst bool
	// BurstMeanLen is the mean burst length in packets when LossBurst is set.
	BurstMeanLen float64
	// LatencyMs is a constant one-way delay added to every packet.
	LatencyMs float64
	// JitterMs is the width of a uniform random delay added on top of LatencyMs.
	JitterMs float64
	// ReorderPct is the percentage of packets held back far enough to arrive
	// after their successor.
	ReorderPct float64
	// DupPct is the percentage of packets transmitted twice.
	DupPct float64
}

// Provider describes impairments applied to the AI provider calls. These are read
// by the STT/LLM/TTS fault-injection wrappers.
type Provider struct {
	STTExtraLatencyMs float64
	STTErrorRate      float64
	LLMExtraLatencyMs float64
	LLMErrorRate      float64
	TTSExtraLatencyMs float64
}

// Profile is a named scenario: one flag that sets up a whole demo beat.
type Profile struct {
	Name        string
	Description string
	Network     Network
	Provider    Provider
}

// Profiles are the scenarios the scripted demo steps through.
var Profiles = map[string]Profile{
	"clean": {
		Name:        "clean",
		Description: "no impairment — the baseline everything else is compared against",
	},
	"mobile": {
		Name:        "mobile",
		Description: "a decent cellular connection: light loss, moderate jitter",
		Network:     Network{LossPct: 1, JitterMs: 30, LatencyMs: 20},
	},
	"lossy-wan": {
		Name:        "lossy-wan",
		Description: "a congested WAN: bursty loss, heavy jitter, some reordering",
		Network: Network{
			LossPct:      5,
			LossBurst:    true,
			BurstMeanLen: 4,
			JitterMs:     80,
			LatencyMs:    60,
			ReorderPct:   2,
		},
	},
	"provider-degraded": {
		Name:        "provider-degraded",
		Description: "clean network, sick AI providers — isolates provider latency",
		Provider: Provider{
			STTExtraLatencyMs: 2000,
			STTErrorRate:      0.10,
			LLMExtraLatencyMs: 3000,
		},
	},
}

// ProfileNames lists the available profile names in a stable order.
func ProfileNames() []string {
	return []string{"clean", "mobile", "lossy-wan", "provider-degraded"}
}

// LookupProfile resolves a profile by name.
func LookupProfile(name string) (Profile, error) {
	p, ok := Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("chaos: unknown profile %q (have %v)", name, ProfileNames())
	}
	return p, nil
}

// Validate rejects nonsensical settings early, so a typo in a demo flag fails at
// startup instead of producing quietly wrong metrics.
func (n Network) Validate() error {
	for _, f := range []struct {
		name string
		val  float64
	}{
		{"LossPct", n.LossPct},
		{"ReorderPct", n.ReorderPct},
		{"DupPct", n.DupPct},
	} {
		if f.val < 0 || f.val > 100 {
			return fmt.Errorf("chaos: %s = %v, want 0-100", f.name, f.val)
		}
	}
	if n.LatencyMs < 0 {
		return fmt.Errorf("chaos: LatencyMs = %v, want >= 0", n.LatencyMs)
	}
	if n.JitterMs < 0 {
		return fmt.Errorf("chaos: JitterMs = %v, want >= 0", n.JitterMs)
	}
	if n.LossBurst && n.BurstMeanLen < 1 {
		return fmt.Errorf("chaos: BurstMeanLen = %v, want >= 1 when LossBurst is set", n.BurstMeanLen)
	}
	return nil
}

// IsClean reports whether this config leaves traffic untouched, letting callers
// skip the impairment path entirely.
func (n Network) IsClean() bool {
	return n.LossPct == 0 && n.LatencyMs == 0 && n.JitterMs == 0 &&
		n.ReorderPct == 0 && n.DupPct == 0
}

// Action is the verdict for one packet.
type Action struct {
	// Drop means the packet is discarded and never transmitted.
	Drop bool
	// Duplicate means the packet is transmitted a second time.
	Duplicate bool
	// Delay is how long to hold the packet before transmitting.
	Delay time.Duration
}

// reorderHold is how far a reordered packet is held back. Slightly more than one
// packetization interval guarantees it lands after its successor without being
// so late that a jitter buffer discards it outright.
const reorderHold = 30 * time.Millisecond

// Impairer decides what happens to each outbound packet. It is safe for
// concurrent use: the /chaos endpoint can retune it while a call is in flight.
type Impairer struct {
	mu  sync.Mutex
	cfg Network
	rng *rand.Rand

	// Gilbert-Elliott state.
	inBurst    bool
	pGoodToBad float64
	pBadToGood float64

	// Counters, for asserting in tests and reporting at end of call.
	offered    uint64
	dropped    uint64
	duplicated uint64
	reordered  uint64
}

// NewImpairer builds an Impairer. A fixed seed makes a run reproducible, which is
// what the loss-accounting tests rely on.
func NewImpairer(cfg Network, seed int64) (*Impairer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	i := &Impairer{
		cfg: cfg,
		rng: rand.New(rand.NewSource(seed)),
	}
	i.recomputeBurst()
	return i, nil
}

// recomputeBurst derives the Gilbert-Elliott transition probabilities from the
// target loss rate and mean burst length.
//
// With mean burst length B, the bad->good probability is r = 1/B. For the chain's
// steady-state time in the bad state to equal the target loss fraction L,
// p/(p+r) = L, so p = L*r/(1-L).
func (i *Impairer) recomputeBurst() {
	if !i.cfg.LossBurst {
		i.pGoodToBad, i.pBadToGood, i.inBurst = 0, 0, false
		return
	}
	b := i.cfg.BurstMeanLen
	if b < 1 {
		b = 1
	}
	l := i.cfg.LossPct / 100
	r := 1 / b
	switch {
	case l <= 0:
		i.pGoodToBad, i.pBadToGood = 0, r
	case l >= 1:
		i.pGoodToBad, i.pBadToGood = 1, 0
	default:
		i.pGoodToBad, i.pBadToGood = l*r/(1-l), r
	}
}

// SetNetwork retunes the impairer mid-call.
func (i *Impairer) SetNetwork(cfg Network) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cfg = cfg
	i.recomputeBurst()
	return nil
}

// Network returns the current config.
func (i *Impairer) Network() Network {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cfg
}

// Next decides the fate of the next packet and advances the model's state.
func (i *Impairer) Next() Action {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.offered++

	if i.shouldDropLocked() {
		i.dropped++
		return Action{Drop: true}
	}

	var a Action
	a.Delay = time.Duration(i.cfg.LatencyMs * float64(time.Millisecond))
	if i.cfg.JitterMs > 0 {
		a.Delay += time.Duration(i.rng.Float64() * i.cfg.JitterMs * float64(time.Millisecond))
	}
	if i.cfg.ReorderPct > 0 && i.rng.Float64()*100 < i.cfg.ReorderPct {
		a.Delay += reorderHold
		i.reordered++
	}
	if i.cfg.DupPct > 0 && i.rng.Float64()*100 < i.cfg.DupPct {
		a.Duplicate = true
		i.duplicated++
	}
	return a
}

// shouldDropLocked advances the loss model by one packet. The caller holds the mutex.
func (i *Impairer) shouldDropLocked() bool {
	if i.cfg.LossPct <= 0 {
		return false
	}
	if !i.cfg.LossBurst {
		return i.rng.Float64()*100 < i.cfg.LossPct
	}
	// Gilbert-Elliott: transition first, then drop iff we are in the bad state.
	if i.inBurst {
		if i.rng.Float64() < i.pBadToGood {
			i.inBurst = false
		}
	} else if i.rng.Float64() < i.pGoodToBad {
		i.inBurst = true
	}
	return i.inBurst
}

// SendStats is what the impairer actually did, for comparison against what the
// receiver independently measured.
type SendStats struct {
	Offered    uint64
	Dropped    uint64
	Duplicated uint64
	Reordered  uint64
}

// Stats snapshots the counters.
func (i *Impairer) Stats() SendStats {
	i.mu.Lock()
	defer i.mu.Unlock()
	return SendStats{
		Offered:    i.offered,
		Dropped:    i.dropped,
		Duplicated: i.duplicated,
		Reordered:  i.reordered,
	}
}
