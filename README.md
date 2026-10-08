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
| 4 | Provider interfaces + mocks + pipeline | done |
| 5 | Datadog instrumentation | next |
| 6 | LLM in the loop (agent + tool spans) | |
| 7 | Real Google STT/TTS | |
| 8 | Load generator, demo script, dashboard | |
| 9 | Demo polish | |

Nothing is wired to Datadog yet — that is phase 5. What works today is the whole call
path: RTP in, jitter buffer, transcript, agent reply with a tool call, synthesized audio
out. Every provider is a deterministic mock, so `make test` needs no credentials and
spends nothing.

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

Provider fault flags: `-provider-profile` (use `provider-degraded` to add STT and LLM
latency), plus `-stt-latency`, `-stt-error-rate`, `-llm-latency`, `-llm-error-rate`,
`-tts-latency` to override individual knobs.

Send a clean call, then a degraded one:

```bash
./bin/voicectl send -to 127.0.0.1:5004 -duration 5s -profile clean
```

```bash
./bin/voicectl send -to 127.0.0.1:5004 -duration 5s -profile lossy-wan
```

The gateway reports what the network did, what the jitter buffer did about it, and what
the AI pipeline made of the result:

```
[ended] c-076eb2a3 | net: recv=284 lost=16 (5.33%) reorder=95 jitter=29.8ms mos=2.06
        dur=5.936s | jbuf: played=310 concealed=26 (8.39%) late=0 | pipe: dropped=0
   c-076eb2a3 | ai: turns=3 tools=2 errors=0 tokens=96/86 first_partial=621ms
               concealed_in=5.0% frames_out=1070
   c-076eb2a3 | turn 0: "hello I'm calling about my account balance" (conf 0.87)
               -> "I've pulled up your account. Account 4729 belongs to Dana Okafor,
                   the balance is $1,284.52, and it is in good standing."
               [tools=[lookup_account] llm=1ms tts_fb=0s]
```

Two things to notice, because they are the connection the demo exists to draw:

- **`conf 0.87`** instead of the clean 0.95. The recognizer's confidence is degraded in
  proportion to how much of the utterance was jitter-buffer filler, so packet loss shows up
  as an AI-quality number rather than only as a loss percentage on a network chart.
- **`concealed_in=5.0%`** against the network's measured 5.33% loss. The AI layer and the
  network layer agree, independently measured.

### Seeing a degraded provider

```bash
./bin/voicegw -addr 127.0.0.1:5004 -provider-profile provider-degraded -idle-timeout 1s
```

The same call then reports `first_partial=2.001s` instead of 621 ms, and `llm=6.005s` on
the tool-calling turn — two round trips at 3 s each. The injected latency is applied before
any injected error, so a failing provider still costs what a real timeout would.

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

## Measurement artifacts worth knowing

These are consequences of how RTP and streaming recognition work, not bugs, and all of them
will show up in a demo:

**Trailing loss is invisible.** A receiver reconstructs loss from sequence-number gaps, so
it cannot detect packets lost at the very end of a stream — it never saw the sequence
numbers that would reveal the gap. In the run above the sender dropped 8 packets but the
receiver measured 6, because 2 of the drops were in the tail. Real endpoints close this gap
with RTCP; this demo will close it with the phase-5 control plane's end-of-call message.

**The jitter buffer's conceal rate includes a playout hangover.** When a stream goes quiet
the playout loop keeps filling slots for 200 ms before idling, because it cannot distinguish
"the far end paused" from "packets are missing". Those 10 frames count as concealed, so the
`jbuf` conceal rate sits a few points above the true loss. The pipeline's `concealed_in` is
*not* affected: starved filler is deliberately never forwarded to the recognizer, which is
why the two numbers differ (8.39% versus 5.0% in the output above). Phase 5's end-of-call
control message removes the ambiguity.

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
- **Every provider is behind an interface with a deterministic mock.** `internal/stt`,
  `internal/llm` and `internal/tts` each expose one seam plus a mock whose output is a
  function of logical progress, never of wall-clock time or of audio content. Phases 6 and 7
  swap in the real LLM and Google STT/TTS without anything above the interfaces changing.
- **Synthesized audio is not yet sent back to the caller.** The pipeline produces it and
  counts it, but establishing the return media path is the control plane's job in phase 5.
