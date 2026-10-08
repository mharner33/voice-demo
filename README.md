# voice-demo

Simulated SIP voice calls over RTP/G.711, transcribed by Google STT, answered by an LLM,
and synthesized back to audio — instrumented with Datadog APM and Agent (LLM)
Observability, with injectable network and provider faults.

The point of the project is **demonstrating how to instrument this flow with Datadog**.
The media path is authentic because that is what makes the loss/jitter/MOS metrics real;
SIP signaling is deliberately out of scope.

See [docs/plan.md](docs/plan.md) for the full design and phase plan.

## Status

| Phase | Scope | State |
|---|---|---|
| 0 | Scaffold, Makefile, compose, toolchain | done |
| 1 | G.711 codec, WAV I/O, resampling | done |
| 2 | RTP transport, loss/jitter accounting, chaos injection | done |
| 3 | Adaptive jitter buffer | done |
| 4 | Provider interfaces + mocks | next |
| 5 | Datadog instrumentation | |
| 6 | LLM in the loop (agent + tool spans) | |
| 7 | Real Google STT/TTS | |
| 8 | Load generator, demo script, dashboard | |
| 9 | Demo polish | |

Nothing is wired to Datadog yet — that is phase 5. What works today is the media path and
its measurement.

## Quick start

Build:

```bash
make build
```

Start the gateway:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -report-interval 2s
```

Jitter buffer flags: `-jbuf-target` (prebuffer depth in 20 ms frames, default 3),
`-jbuf-max` (hard cap, default 25), `-jbuf-adaptive`, and `-conceal` (`repeat`, `silence`,
or `noise`).

Send a clean call, then a degraded one:

```bash
./bin/voicectl send -to 127.0.0.1:5004 -duration 5s -profile clean
```

```bash
./bin/voicectl send -to 127.0.0.1:5004 -duration 5s -profile lossy-wan
```

The gateway reports per-stream network measurements, derived only from what arrived,
alongside what the jitter buffer did with them:

```
[ended] ssrc=0xa7087cbb | net: recv=184 expected=200 lost=16 (8.00%) dup=0 reorder=62
        jitter=26.1ms mos=1.64 dur=4.041s | jbuf: played=210 concealed=26 (12.38%)
        late=0 evicted=0 depth=0/200ms peak=12
```

### Seeing the latency/loss trade-off

This is the clearest single demonstration of why buffer depth matters. Run the same
impaired stream through a shallow and a deep buffer:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -jbuf-target 2 -idle-timeout 1s
```

```bash
./bin/voicectl send -to 127.0.0.1:5004 -duration 4s -profile lossy-wan -seed 7
```

Then repeat with `-jbuf-target 10`. On an identical stream (200 packets, 16 genuinely lost
by the network, 63 reordered):

| Buffer depth | Late-dropped | Concealed | Self-inflicted loss |
|---|---|---|---|
| 2 frames (40 ms) | 4 | 29 (13.9%) | 4 packets that had arrived |
| 10 frames (200 ms) | 0 | 26 (12.4%) | none |

The deep buffer's 26 concealed slots are exactly the 16 packets the network lost plus the
10-frame playout hangover — it threw nothing away itself. The shallow buffer discarded 4
packets that had already arrived, trading audio quality for 160 ms less latency.

List the impairment profiles:

```bash
./bin/voicectl profiles
```

Send a WAV file instead of a tone:

```bash
./bin/voicectl send -to 127.0.0.1:5004 -file testdata/sample.wav -profile mobile
```

Override individual knobs on top of a profile:

```bash
./bin/voicectl send -to 127.0.0.1:5004 -profile clean -loss-pct 12 -jitter-ms 40 -seed 42
```

Fixing `-seed` makes a run reproducible, which is what the loss-accounting tests rely on.

## Make targets

```bash
make help
```

`build` `test` `test-integration` `cover` `fuzz` `lint` `tidy` `proto` `up` `down` `logs`
`agent-status` `demo`

`make test` is fully offline — no cloud credentials, no API spend. Tests that hit real
providers live behind `-tags=integration` and run via `make test-integration`.

## Three measurement artifacts worth knowing

All three are consequences of how RTP works, not bugs, and all three will show up in a demo:

**Trailing loss is invisible.** A receiver reconstructs loss from sequence-number gaps, so
it cannot detect packets lost at the very end of a stream — it never saw the sequence
numbers that would reveal the gap. In the run above the sender dropped 8 packets but the
receiver measured 6, because 2 of the drops were in the tail. Real endpoints close this gap
with RTCP; this demo will close it with the phase-5 control plane's end-of-call message.

**The conceal rate includes a playout hangover.** When a stream goes quiet the playout
loop keeps filling slots for 200 ms before idling, because it cannot distinguish "the far
end paused" from "packets are missing". Those 10 frames count as concealed, so a short call
reports a conceal rate a few points above its true loss. Phase 5's end-of-call control
message removes the ambiguity.

**`-pace` below 20 ms inflates jitter.** The flag compresses a call into less wall-clock
time by sending frames faster while RTP timestamps still advance at the true 20 ms rate.
Jitter is defined as the difference between arrival spacing and timestamp spacing, so
compressing the former while leaving the latter alone registers as jitter by definition.
Use the default 20 ms pacing whenever you care about the jitter number.

## Architecture notes

- **Transport metrics belong in APM; AI metrics belong in Agent Observability.** Jitter,
  loss, and buffer depth are APM spans and DogStatsD metrics. Only STT/LLM/TTS become LLM
  Obs spans. Mixing them degrades both views.
- **Sender and receiver measure independently.** The receiver never sees the sender's drop
  count; it reconstructs loss from sequence gaps alone, exactly as a real gateway must.
  The phase-2 acceptance test asserts the two agree exactly — a demo that reported the
  sender's own numbers would prove nothing.
- **Microphone capture will be host-only.** macOS containers cannot reach the microphone.
  File and synthetic sources work anywhere.
- **RTP/UDP into podman on macOS crosses a VM boundary.** `deploy/compose.yml` maps the UDP
  port explicitly; if the host client cannot reach the gateway, run the client inside the
  compose network instead.
