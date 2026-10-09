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
| — | gRPC control plane (call setup/teardown) | done |
| 6 | LLM in the loop (real agent, tool definitions, cost, evaluations) | done |
| 7 | Real Google STT/TTS | done |
| 8 | Load generator, demo script, dashboard | next |
| 9 | Demo polish | |

The whole call path works and is fully instrumented: RTP in, jitter buffer, transcript,
agent reply with a tool call, synthesized audio out — reported as APM traces, Agent
Observability spans with token counts, cost and per-call evaluations, DogStatsD metrics and
a correlated call log.

All three AI stages have a real implementation and a deterministic mock, and the mocks are
the defaults: `make test` needs no credentials and spends nothing. `-real-stt` swaps in
Google Speech-to-Text v2, `-real-tts` Google Text-to-Speech, `-real-llm` Claude — each
independently. Telemetry is off by default, so the demo also runs with no Datadog agent at
all.

## Quick start

Build:

```bash
make build
```

Start the gateway, which serves both the control plane and the media port:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -grpc-addr 127.0.0.1:50051
```

Jitter buffer flags: `-jbuf-target` (prebuffer depth in 20 ms frames, default 3),
`-jbuf-max` (hard cap, default 25), `-jbuf-adaptive`, and `-conceal` (`repeat`, `silence`,
or `noise`).

Provider fault flags: `-provider-profile` (use `provider-degraded` to add STT and LLM
latency), plus `-stt-latency`, `-stt-error-rate`, `-llm-latency`, `-llm-error-rate`,
`-tts-latency` to override individual knobs.

Place a call. The client negotiates over the control plane, so it learns the media port
rather than assuming one, and it receives the agent's spoken reply:

```bash
./bin/voicectl send -duration 5s -profile lossy-wan -save-reply reply.wav
```

The client prints both sides' accounting:

```
call c-623c122c-1
  sender:   offered=250 sent=242 dropped=8
  gateway:  received=242 expected=250 lost=8 (3.20%) jitter=0.3ms mos=2.71
  loss reconciles exactly: the gateway measured all 8 dropped packets
  buffer:   concealed=8 (3.20%)
  agent:    turns=3 tools=2
    0. heard: "hello I'm calling about my account balance"
       said:  "I've pulled up your account. Account 4729 belongs to Dana Okafor, ..."
  reply:    1070 frames of audio received
            saved 4.00s of reply audio to reply.wav
```

`reply.wav` is the agent's reply as audio — the one artifact here you can just listen to.

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

## Why there is a control plane

The gateway accepts calls over gRPC mirroring SIP's INVITE/BYE semantics. It is not a SIP
stack, and the signaling protocol is not the point — but four measurements are impossible
without signaling of some kind:

**Packet loss at the edges of a stream.** Loss is reconstructed from gaps between sequence
numbers that arrived, so loss at either end leaves no evidence: nothing later reveals a
trailing gap, and a dropped *first* packet makes the receiver silently start counting one
packet in. `EndCall` carries both ends of the range the sender emitted. Real endpoints
learn the same thing from RTCP. The call log reports `sequence_range_known` so a reader can
tell an exact figure from a lower bound.

**Knowing when a call ended.** Without a teardown message the end has to be inferred from
silence, so the playout loop keeps emitting concealment for its whole hangover window. With
`EndCall` that drops to zero, which is why the jitter buffer's conceal rate and the
pipeline's `concealed_in_pct` now agree exactly instead of differing by the hangover.

**The return media path.** The gateway synthesizes audio but has nowhere to send it until
the caller says where to listen. The declared port is paired with the source address the
RTP actually arrived from, which is what makes it work from behind NAT.

**The client's network profile.** Nothing in an RTP stream says how it was degraded.

Calls that skip signaling still work — RTP from an unknown SSRC creates a call tagged
`signaled=false`, with no trailing-loss accounting and no reply audio. That keeps the
contrast demonstrable:

```bash
./bin/voicectl send -to 127.0.0.1:5004 -no-signaling -duration 5s -loss-pct 10
```

Pass `-require-signaling` to the gateway to reject unsignaled media instead.

## Running the real speech providers

Recognition and synthesis are mocks by default; the two flags are independent, so each
stage can be real while the other stays reproducible:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -real-stt -real-tts -google-project YOUR_PROJECT -dd
```

Credentials come from Application Default Credentials. On a laptop:

```bash
gcloud auth application-default login
gcloud auth application-default set-quota-project YOUR_PROJECT
```

A project is required for both APIs, not just for billing: user credentials carry no
project of their own, and both services reject a request that does not name one to charge.
`-google-project` or `GOOGLE_CLOUD_PROJECT` supplies it; in the container,
`GOOGLE_APPLICATION_CREDENTIALS` points at a mounted service-account JSON instead and that
file carries its own project.

Enable both APIs once per project:

```bash
gcloud services enable speech.googleapis.com texttospeech.googleapis.com --project YOUR_PROJECT
```

Tuning flags: `-stt-model` (default `telephony`, the v2 model trained on 8 kHz phone
audio), `-stt-language`, `-google-location`, `-tts-voice` (default `en-US-Neural2-C`),
`-tts-language`. The language has to match the voice, or the service quietly substitutes a
different one.

Audio is sent at the 8 kHz the wire already carries, not upsampled: Google's own guidance
is to use the source's native rate rather than resample, and interpolating telephony audio
to 16 kHz adds nothing a recognizer can use. Synthesis is requested as headerless 8 kHz
PCM for the same reason — it drops straight into the return RTP stream.

Which pairing to use depends on what a run is for:

| Run | Why |
|---|---|
| mock STT + mock TTS | reproducible; a dashboard change is a *network* change |
| real STT + mock TTS | genuine transcripts, with reply audio still verifiable by tone |
| real STT + real TTS | a call a person can listen to end to end |

The gateway logs what is serving each stage at startup, so it never has to be inferred
from the dashboards:

```
stt: google telephony, en-US, 8000 Hz, project demo-proj in global
agent: mock (scripted; pass -real-llm for the Anthropic API)
tts: google en-US-Neural2-C, en-US, 8000 Hz
```

Live provider tests are behind the integration build tag and skip without
`GOOGLE_CLOUD_PROJECT` and credentials. They synthesize a phrase, run it through G.711 and
the real recognizer, and compare the transcript by **word error rate** rather than by
equality — a recognizer that hears "four seven two nine" as "4729" has not failed. One of
them conceals a third of the frames and asserts the transcript gets worse, which is the
demo's central claim checked against a real recognizer instead of a mock written to
degrade.

## Running a real agent

The agent is a scripted mock by default. `-real-llm` swaps in Claude through the official
Go SDK:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -real-llm -dd
```

Credentials come from the environment — `ANTHROPIC_API_KEY`, or a profile from `ant auth
login`, which the SDK picks up on its own. There is deliberately no `-api-key` flag, so a
key never lands in this program's command line, its logs, or anyone's shell history.

`-llm-model` and `-llm-effort` tune the call. The defaults are `claude-opus-5` at `low`
effort, because on a phone line the caller sits in silence for every token spent thinking.
Thinking is left *on* rather than disabled even so: disabling it risks the model writing a
tool call into its spoken text instead of actually calling the tool, which would have it
read out a balance it never looked up (plan finding 35).

The gateway logs which agent is answering at startup, and says when a model has no
published price so you know why the cost metrics are empty:

```
agent: anthropic claude-opus-5 at low effort
agent: mock (scripted; pass -real-llm for the Anthropic API)
```

**Why the mock is the default.** Beyond cost and credentials, the scripted agent is what
makes the transport story reproducible: it returns the same reply for the same transcript,
so a difference in the dashboards between two runs is a difference in the *network*. The
demo's central claim is only legible against a fixed agent.

Tests that call the live API are behind a build tag and skip without credentials:

```bash
make test-integration
```

They check what only a real model can: that it keeps replies short enough to speak, calls
the tool instead of inventing a balance, and asks for a repeat when handed a transcript
that packet loss has damaged.

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
│   │                             input_cost, output_cost, tool_definitions
│   └── tool lookup_account
└── llm   tts.synthesize          time_to_first_token = first audio byte
```

Plus three **evaluations** per call, joined to the workflow span, which is what populates
the Evaluations view:

| Evaluation | Kind | What it is |
|---|---|---|
| `transcript_confidence` | score | the recognizer's mean confidence across the call |
| `audio_quality` | categorical | `clean` / `degraded` / `poor`, by concealed fraction |
| `replied_every_turn` | boolean | false if any turn produced no reply — silence on the line |

These are computed from pipeline data rather than by an LLM judge, on purpose. The point of
the demo is that network damage shows up as AI quality damage, and `transcript_confidence`
is the number that moves: a clean call scores 0.95, the same call with every fourth frame
concealed scores 0.71 and is labeled `poor`. A judge would mostly measure the judge. A test
asserts the gap, so if it ever closes the demo's claim has quietly stopped being true.

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

**Loss at the edges of a stream is invisible without signaling.** A receiver reconstructs
loss from gaps between sequence numbers that arrived, so it cannot see loss at either end —
and the leading case is the easy one to miss: a dropped first packet makes the receiver
anchor on the second and start counting a packet in. The control plane's `EndCall` carries
both ends of the range, which makes the figure exact; `sequence_range_known` in the call log
says whether it is. A call sent with `-no-signaling` still has the blind spot, which is the
point of keeping that mode.

**The playout hangover only applies to unsignaled calls now.** When a stream goes quiet
without a teardown message, the playout loop keeps filling slots for 200 ms before idling,
because it cannot distinguish "the far end paused" from "packets are missing". A signaled
call ends exactly on `EndCall`, so it emits no trailing concealment at all — which is why
the `jbuf` conceal rate and the pipeline's `concealed_in_pct` agree for signaled calls and
differ for unsignaled ones.

**The return audio stream is not paced.** The gateway writes synthesized frames as fast as
the pipeline produces them rather than at one frame per 20 ms, so the *return* stream's
jitter figure reads around 20 ms regardless of conditions — arrival spacing near zero
against timestamps advancing 20 ms. It does not affect the saved WAV. A production gateway
would pace it.

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
- **Signaling is a thin gRPC plane, not SIP.** `internal/control` owns the protocol;
  `cmd/voicegw` implements the handler. Swapping in real SIP would mean replacing one
  package, because the media path and the instrumentation know nothing about it.
