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
| 3 | Jitter buffer | next |
| 4 | Provider interfaces + mocks | |
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

Send a clean call, then a degraded one:

```bash
./bin/voicectl send -to 127.0.0.1:5004 -duration 5s -profile clean
```

```bash
./bin/voicectl send -to 127.0.0.1:5004 -duration 5s -profile lossy-wan
```

The gateway reports per-stream measurements derived only from what arrived:

```
[live] ssrc=0x020ffeb0 recv=250 expected=250 lost=0 (0.00%) dup=0 reorder=0 jitter=0.42ms mos=4.41
[live] ssrc=0xf16e6ff0 recv=231 expected=244 lost=13 (5.33%) dup=0 reorder=58 jitter=74.10ms mos=1.94
```

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

## Two measurement artifacts worth knowing

Both are real properties of RTP, not bugs, and both will show up in a demo:

**Trailing loss is invisible.** A receiver reconstructs loss from sequence-number gaps, so
it cannot detect packets lost at the very end of a stream — it never saw the sequence
numbers that would reveal the gap. In the run above the sender dropped 8 packets but the
receiver measured 6, because 2 of the drops were in the tail. Real endpoints close this gap
with RTCP; this demo will close it with the phase-5 control plane's end-of-call message.

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
