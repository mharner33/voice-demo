package jbuf

import (
	"sort"
	"testing"
	"time"

	pionrtp "github.com/pion/rtp"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/codec"
	"github.com/mharner33/voice-demo/internal/rtp"
)

// arrival is a packet and the moment the network would have delivered it.
type arrival struct {
	pkt *pionrtp.Packet
	at  time.Duration
}

// simulateNetwork runs real audio through the real packetizer and the real
// impairment model, then sorts by delivery time to produce the packet order a
// receiver would actually see. Applying the impairer's delays as an ordering
// rather than as wall-clock sleeps keeps the test deterministic and instant
// while still exercising the production components.
func simulateNetwork(t *testing.T, pcm []int16, c codec.Codec, netCfg chaos.Network, seed int64) ([]arrival, chaos.SendStats) {
	t.Helper()

	imp, err := chaos.NewImpairer(netCfg, seed)
	if err != nil {
		t.Fatalf("NewImpairer: %v", err)
	}
	pktz := rtp.NewPacketizerAt(0xABCDEF01, c, 1000, 4_000_000)

	var out []arrival
	for i, frame := range codec.FramePCM(pcm) {
		pkt := pktz.Packetize(c.Encode(frame))
		sent := time.Duration(i) * codec.FrameDuration

		action := imp.Next()
		if action.Drop {
			continue
		}
		out = append(out, arrival{pkt: pkt, at: sent + action.Delay})
		if action.Duplicate {
			out = append(out, arrival{pkt: pkt, at: sent + action.Delay})
		}
	}

	// A receiver sees packets ordered by arrival, not by sequence. This sort is
	// what turns the impairer's per-packet delays into real reordering.
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out, imp.Stats()
}

// TestNetworkImpairmentIsRepairedEndToEnd is the capstone for phases 1 through 3:
// real audio, real G.711 encoding, real RTP packetization, the real burst-loss
// and jitter model, and the real jitter buffer. Whatever the network does to the
// ordering, the audio that comes out must be correctly sequenced, and every
// frame that survived must carry exactly the samples that were sent.
func TestNetworkImpairmentIsRepairedEndToEnd(t *testing.T) {
	const seed = 20260108
	audio := codec.Tone(440, codec.SampleRate8k, 4*time.Second, 0.5)
	frames := codec.FramePCM(audio.PCM)

	// lossy-wan is the worst profile the demo ships: bursty loss, 80 ms of
	// jitter, and 2% reordering.
	netCfg := chaos.Profiles["lossy-wan"].Network

	arrivals, sent := simulateNetwork(t, audio.PCM, codec.PCMU, netCfg, seed)
	if sent.Dropped == 0 {
		t.Fatal("the profile dropped nothing; the test would prove little")
	}

	// 80 ms of jitter plus a 30 ms reorder hold needs more than 110 ms of depth
	// to absorb, so the target is set to 8 frames (160 ms).
	b := newTestBuffer(t, Config{
		Codec:       codec.PCMU,
		TargetDepth: 8,
		MaxDepth:    40,
		Conceal:     ConcealRepeat,
	})

	var played []Frame
	for _, a := range arrivals {
		b.Push(a.pkt)
		if f, ok := b.Pop(); ok {
			played = append(played, f)
		}
	}
	played = append(played, b.Drain()...)

	if len(played) == 0 {
		t.Fatal("nothing was played")
	}

	// The output contract: playout timestamps form an unbroken ladder.
	for i := 1; i < len(played); i++ {
		if delta := played[i].Timestamp - played[i-1].Timestamp; delta != codec.SamplesPerFrame {
			t.Fatalf("slot %d advanced the timestamp by %d, want %d",
				i, delta, codec.SamplesPerFrame)
		}
	}

	// Every frame that was not concealed must hold the exact samples that were
	// encoded for its slot. The comparison is against the G.711 round trip,
	// since companding is lossy but deterministic.
	firstTS := played[0].Timestamp
	var verified int
	for _, f := range played {
		if f.Concealed {
			continue
		}
		idx := int((f.Timestamp - firstTS) / codec.SamplesPerFrame)
		if idx < 0 || idx >= len(frames) {
			t.Fatalf("slot timestamp %d maps to frame %d, outside 0..%d",
				f.Timestamp, idx, len(frames)-1)
		}
		want := codec.PCMU.Decode(codec.PCMU.Encode(frames[idx]))
		for j := range want {
			if f.PCM[j] != want[j] {
				t.Fatalf("frame %d sample %d = %d, want %d — audio was corrupted "+
					"or placed in the wrong slot", idx, j, f.PCM[j], want[j])
			}
		}
		verified++
	}

	st := b.Stats()
	t.Logf("4s call over lossy-wan: %d sent, %d dropped by the network, "+
		"%d slots played, %d verified byte-exact, %d concealed (%.1f%%), "+
		"%d late, %d duplicates rejected, %d evicted",
		sent.Offered, sent.Dropped, len(played), verified, st.Concealed,
		st.ConcealRate()*100, st.Late, st.Duplicate, st.Evicted)

	if verified == 0 {
		t.Fatal("no frames were verified byte-exact")
	}
	// A 5% loss profile should leave the great majority of audio intact. A much
	// higher conceal rate would mean the buffer is discarding packets that
	// arrived, not merely filling for ones that did not.
	if st.ConcealRate() > 0.25 {
		t.Errorf("ConcealRate = %.1f%% against a ~5%% loss profile; the buffer is "+
			"losing audio that arrived", st.ConcealRate()*100)
	}
}

// TestDeepBufferAbsorbsJitterThatShallowBufferDrops demonstrates the central
// trade-off on identical input: the same impaired stream, played through a
// shallow and a deep buffer. The shallow one discards late packets; the deep one
// plays them, at the cost of added mouth-to-ear latency. This is the comparison
// the phase-8 demo is built to show.
func TestDeepBufferAbsorbsJitterThatShallowBufferDrops(t *testing.T) {
	const seed = 99
	audio := codec.Tone(440, codec.SampleRate8k, 3*time.Second, 0.5)

	// Jitter well beyond one frame interval, with no loss, so that every
	// concealed slot is the buffer's own doing rather than a missing packet.
	netCfg := chaos.Network{JitterMs: 100}

	arrivals, sent := simulateNetwork(t, audio.PCM, codec.PCMU, netCfg, seed)
	if sent.Dropped != 0 {
		t.Fatalf("the network dropped %d packets; this test needs lossless input",
			sent.Dropped)
	}

	run := func(target int) Stats {
		b := newTestBuffer(t, Config{TargetDepth: target, MaxDepth: 50})
		for _, a := range arrivals {
			b.Push(a.pkt)
			b.Pop()
		}
		b.Drain()
		return b.Stats()
	}

	shallow := run(2) // 40 ms
	deep := run(10)   // 200 ms, enough for 100 ms of jitter

	t.Logf("shallow (40ms): %d late, %.1f%% concealed", shallow.Late, shallow.ConcealRate()*100)
	t.Logf("deep (200ms):   %d late, %.1f%% concealed", deep.Late, deep.ConcealRate()*100)

	if shallow.Late == 0 {
		t.Error("the shallow buffer dropped nothing as late; 100ms of jitter " +
			"should overwhelm a 40ms buffer")
	}
	if deep.Late >= shallow.Late {
		t.Errorf("deep buffer had %d late packets vs shallow %d; a deeper buffer "+
			"must absorb more jitter", deep.Late, shallow.Late)
	}
	if deep.ConcealRate() >= shallow.ConcealRate() {
		t.Errorf("deep buffer concealed %.1f%% vs shallow %.1f%%; absorbing more "+
			"jitter must mean less concealment",
			deep.ConcealRate()*100, shallow.ConcealRate()*100)
	}
}

// TestAdaptiveBufferRecoversFromJitterOnset shows the adaptive controller doing
// the thing it exists for: a buffer sized for a clean network, hit with sudden
// jitter, deepens itself and stops discarding late packets.
func TestAdaptiveBufferRecoversFromJitterOnset(t *testing.T) {
	audio := codec.Tone(440, codec.SampleRate8k, 2*time.Second, 0.5)

	clean, _ := simulateNetwork(t, audio.PCM, codec.PCMU, chaos.Network{}, 1)
	jittery, _ := simulateNetwork(t, audio.PCM, codec.PCMU, chaos.Network{JitterMs: 90}, 2)

	b := newTestBuffer(t, Config{
		TargetDepth:       2,
		MaxDepth:          50,
		Adaptive:          true,
		AdaptiveMaxTarget: 12,
	})

	// A clean stretch first: the target must stay where it started.
	for _, a := range clean {
		b.Push(a.pkt)
		b.Pop()
	}
	if got := b.Stats().TargetDepth; got != 2 {
		t.Errorf("TargetDepth = %d after a clean stretch, want it unchanged at 2", got)
	}

	// Now the network degrades.
	for _, a := range jittery {
		b.Push(a.pkt)
		b.Pop()
	}

	st := b.Stats()
	if st.TargetDepth <= 2 {
		t.Errorf("TargetDepth = %d after jitter onset, want it to have grown", st.TargetDepth)
	}
	t.Logf("adaptive target: 2 frames (40ms) on a clean network -> %d frames (%.0fms) "+
		"after 90ms jitter onset, %d late packets",
		st.TargetDepth, st.TargetDepthMs(), st.Late)
}
