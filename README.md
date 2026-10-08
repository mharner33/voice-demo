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
| 5 | Datadog instrumentation | done |
| 6 | LLM in the loop (agent + tool spans) | next |
| 7 | Real Google STT/TTS | |
| 8 | Load generator, demo script, dashboard | |
| 9 | Demo polish | |

The whole call path works and is fully instrumented: RTP in, jitter buffer, transcript,
agent reply with a tool call, synthesized audio out — reported as APM traces, Agent
Observability spans, DogStatsD metrics and a correlated call log. Every provider is a
deterministic mock, so `make test` needs no credentials and spends nothing. Telemetry is
off by default, so the demo also runs with no Datadog agent at all.

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

## Datadog

Telemetry is **off by default**. Turn it on with `-dd` (or `VOICE_DD_ENABLED=true`):

```bash
./bin/voicegw -addr 127.0.0.1:5004 -dd -dd-env demo
```

Start the agent and the gateway together, then upload the dashboard:

```bash
make up
```

```bash
make dashboard
```

`make dashboard` needs `DD_API_KEY` and `DD_APP_KEY`. The dashboard definition lives in
[deploy/datadog/dashboard.json](deploy/datadog/dashboard.json), and a test asserts every
metric it charts is one the code actually emits — an empty graph during a demo is
indistinguishable from a healthy system, so the two cannot be allowed to drift.

### What gets reported

**One APM trace per call.** `voice.call` is the root, with backdated children
`voice.rtp.ingest` and `voice.jitter_buffer` carrying the loss, jitter and buffer figures
as tags. The transport stages run for the whole call rather than as a nested call stack, so
their spans are created at the end and backdated to cover it.

**Agent Observability spans** for the AI stages, grouped into one session per call:

```
workflow  voice_call              session_id = call_id
├── llm   stt.transcribe          one per utterance, backdated to its start
│                                 time_to_first_token, billable_character_count
├── agent voice_agent
│   ├── llm  agent.reply          one span per model round trip
│   │                             input_tokens, output_tokens, total_tokens
│   └── tool lookup_account
└── llm   tts.synthesize          time_to_first_token = first audio byte
```

Transport metrics belong in APM; AI metrics belong in Agent Observability. They share one
APM trace ID, which is what lets you pivot between them — and what the call log records so
a log line jumps to its trace.

**Token counts are deliberately not sent over DogStatsD.** Datadog derives
`ml_obs.span.llm.*.tokens` from the span attributes, so emitting them again would double
count. DogStatsD carries the transport and call-level figures, which have no Agent
Observability equivalent.

**A call log line** per call, JSON on stdout, holding the network, buffer and AI figures
together with `dd.trace_id` and `dd.span_id`:

```json
{"event":"call.end","call_id":"c-658948e7","packets_rx":234,"packets_lost":16,
 "loss_pct":6.4,"jitter_ms":31.4,"mos":1.86,"frames_concealed":26,"conceal_pct":10,
 "turn_count":3,"tool_calls":2,"input_tokens":96,"output_tokens":86,
 "first_partial_ms":621,"concealed_in_pct":6.02,
 "turns":[{"transcript":"hello I'm calling about my account balance","confidence":0.874,
           "reply":"I've pulled up your account. Account 4729 belongs to Dana Okafor...",
           "tool_calls":["lookup_account"],"tool_rounds":2}],
 "dd.trace_id":"6ac80e91000000002920ab6f7a1b6a9f","service":"voicegw","env":"demo"}
```

### If the agent is missing, the gateway refuses to start

With LLM Observability enabled in agent mode and no agent reachable, `tracer.Start` fails
and `voicegw` exits with a message naming the four remedies. That is deliberate:
discovering mid-presentation that the dashboards are empty is worse than failing at launch.
Run without `-dd` to skip telemetry entirely, or `-dd-llmobs=false` for APM and metrics
only (which tolerates a missing agent).

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

**`provider_profile` is the gateway's profile, not the client's.** The network profile —
loss, jitter, reordering — is chosen by `voicectl`, and nothing in an RTP stream says how a
stream was degraded, so the gateway cannot tag it. Tagging the gateway's provider profile as
though it were the network profile would attribute client-side impairment to the server.
The control plane will carry the real one.

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
- **Every Datadog SDK call lives in `internal/obs`.** The Agent Observability SDK is
  explicitly experimental, so confining it to one package means an API break — or a
  workaround for an SDK bug — is a one-file fix. That paid for itself immediately: see the
  `llmobs.WithError(nil)` panic in [docs/plan.md §7](docs/plan.md) finding 18.
- **Synthesized audio is not yet sent back to the caller.** The pipeline produces it and
  counts it, but establishing the return media path needs the control plane.
