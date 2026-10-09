# voice-demo — SIP/RTP → STT → LLM → TTS with Datadog Agent Observability

A Go client/server demo simulating voice calls arriving over RTP/G.711, transcribed by
Google streaming STT, answered by an LLM with tool calls, and synthesized back to audio
via Google TTS — fully instrumented with Datadog APM + Agent (LLM) Observability, with
injectable network and provider faults.

**Primary goal: demonstrate how to instrument this flow with Datadog.** Authenticity of
the media path serves that goal; authenticity of SIP signaling does not, so full signaling
is out of scope (see [Appendix A](#appendix-a--deferred-real-sip-signaling)).

## Decisions locked in

| Question | Decision |
|---|---|
| STT/TTS provider | **Google** — Speech-to-Text v2 `StreamingRecognize` + Cloud Text-to-Speech (unary; see finding 38) |
| LLM in the loop | **Yes, moved forward to phase 6** — STT → LLM (with tool call) → TTS |
| SIP signaling | **Not needed.** gRPC control plane mirrors INVITE/BYE semantics; RTP/G.711 for media |
| Audience | Customer-facing eventually → invest in dashboard + scripted demo. **UI deferred**; CLI only for now |

## 1. Architecture

```
┌─────────────────────────── host (macOS) ───────────────────────────┐
│  voicectl (CLI client)                                             │
│   • mic capture (malgo)  │ wav file  │ synthetic tone              │
│   • PCM → G.711 µ-law encode → 20ms frames                         │
│   • RTP packetize (pion/rtp)                                       │
│   • impairment layer: loss / jitter / reorder / dup / burst        │
└──────────┬─────────────────────────────────┬───────────────────────┘
           │ control: gRPC (call setup)      │ media: RTP/UDP :5004
           ▼                                 ▼
┌──────────────────────── podman network ────────────────────────────┐
│  voicegw (server)                                                  │
│   ├─ control plane: gRPC StartCall/EndCall → call_id, media port   │
│   ├─ RTP ingest → seq/ts tracking → loss & RFC3550 jitter calc     │
│   ├─ adaptive jitter buffer (target/max depth, late-drop)          │
│   ├─ G.711 → PCM16 decode → resample 8k→16k                        │
│   ├─ STT stream  (iface: mock | google)                            │
│   ├─ LLM turn    (iface: mock | anthropic) + mock tool calls       │
│   ├─ TTS synth   (iface: mock | google) → PCM → G.711 → RTP out    │
│   ├─ call log: JSON lines w/ call_id + dd.trace_id                 │
│   └─ chaos control: HTTP /chaos (live-tunable mid-call)            │
│                                                                     │
│  datadog-agent (APM 8126, DogStatsD 8125, logs)                    │
└────────────────────────────────────────────────────────────────────┘
```

### Why this split

- **Media over real RTP/G.711** gives the authentic SIP-channel feel: sequence numbers,
  timestamps, SSRC, 20ms ptime, 160-byte payloads — all the things that make
  loss/jitter/MOS genuinely measurable rather than simulated. This is what makes the
  Datadog network-layer metrics believable.
- **Control over gRPC, not SIP.** The gRPC control plane mirrors INVITE/BYE semantics
  (`StartCall` → media params + `call_id`, `EndCall` → BYE) so the call lifecycle and
  trace shape are identical to a real SIP deployment, without a SIP stack.
- **Mic capture must run on the host.** macOS containers cannot access the microphone.
  File/synthetic modes work from anywhere; mic mode is host-only. Hard constraint.

### Repo layout

```
cmd/voicectl/          CLI client (send, load, chaos subcommands)
cmd/voicegw/           server
internal/codec/        G.711 µ-law/A-law, WAV, resampling
internal/rtp/          packetize, depacketize, loss & jitter accounting
internal/jbuf/         jitter buffer
internal/chaos/        impairment models + profiles
internal/stt/          Transcriber iface + mock, google
internal/llm/          Agent iface + mock, anthropic; tool definitions
internal/tts/          Synthesizer iface + mock, google
internal/obs/          ALL Datadog SDK calls live here (tracer, llmobs, statsd, logs)
internal/call/         call session orchestration + call log
proto/                 gRPC control plane
deploy/                compose.yml, Dockerfile, datadog/dashboard.json
testdata/              WAV fixtures, golden transcripts, scripted packet sequences
docs/                  this plan, README narrative
```

## 2. Observability design

The key distinction: **transport belongs in APM, AI belongs in Agent Observability.**
Mixing them degrades both views.

### APM trace shape (one trace per call)

```
voice.call                      (root, server; tags: call_id, codec, from, to, chaos_profile)
├── voice.rtp.ingest            (loss/jitter metrics as span tags)
├── voice.jitter_buffer         (depth, late-drops, underruns)
├── voice.decode.g711
├── [LLM Obs spans — linked via APMTraceID]
└── voice.rtp.egress
```

### Agent Observability span shape

Using `github.com/DataDog/dd-trace-go/v2/llmobs`. The SDK is explicitly **experimental**;
authoritative reference is [pkg.go.dev](https://pkg.go.dev/github.com/DataDog/dd-trace-go/v2/llmobs)
(Go support landed in dd-trace-go v2.3.0). Every SDK call is confined to `internal/obs`
so an API break is a one-file fix.

```
workflow "voice_call"                  StartWorkflowSpan, WithSessionID(callID)
├── llm  "stt.transcribe"              model=telephony, provider=google
│       AnnotateLLMIO(in: "<audio 4.2s, 8kHz µ-law>", out: transcript)
│       metrics: time_to_first_token (→ first partial), billable_character_count
│       metadata: audio_ms, sample_rate, codec, partials_count, confidence
├── agent "voice_agent"                 StartAgentSpan
│   ├── llm  "agent.reply"             model=claude-*, provider=anthropic
│   │       metrics: input_tokens, output_tokens, total_tokens
│   └── tool "lookup_account"          StartToolSpan
│           AnnotateTextIO(in: args JSON, out: result JSON)
└── llm  "tts.synthesize"              model=neural2-C, provider=google
        AnnotateLLMIO(in: reply text, out: "<audio 3.1s>")
        metrics: time_to_first_token, billable_character_count
```

Notes:
- The STT span's sample rate is 8 kHz, the rate actually sent — see finding 37.
- STT/TTS are not token-based. `MetricKeyBillableCharacterCount` and
  `MetricKeyTimeToFirstToken` are the right fits; skip the token keys there. The LLM turn
  is where real token metrics and cost tracking appear.
- `WithSessionID(callID)` groups every span of a call in the LLM Obs session view — this
  is the view to open first in a demo.
- `span.APMTraceID()` ties LLM Obs spans back to the APM trace for the drill-down.
- Each call submits three evaluations against its workflow span via
  `SubmitEvaluationFromSpan`: `transcript_confidence` (score), `audio_quality`
  (categorical), `replied_every_turn` (boolean). All three are derived from pipeline data,
  not from an LLM judge, so the confidence figure moves with packet loss — see finding 34.
- Cost rides the LLM span as the `input_cost` / `output_cost` custom metric keys, which
  have no SDK constants (finding 33).

### DogStatsD metrics (tagged `call_id`, `codec`, `chaos_profile`)

| Metric | Type | Meaning |
|---|---|---|
| `voice.rtp.packets.received` / `.lost` / `.reordered` / `.duplicated` | count | derived from seq gaps |
| `voice.rtp.jitter_ms` | gauge | RFC 3550 interarrival jitter |
| `voice.jbuf.depth_ms` / `.late_drops` / `.underruns` | gauge / count | buffer health |
| `voice.mos.estimate` | gauge | simplified ITU E-model from loss + latency |
| `voice.stt.first_partial_ms` / `.final_ms` | distribution | STT responsiveness |
| `voice.llm.ttft_ms` / `voice.llm.tokens` | distribution | LLM turn |
| `voice.tts.first_byte_ms` | distribution | TTS responsiveness |
| `voice.call.duration_ms` / `.e2e_latency_ms` | distribution | end-to-end |

### Call log (JSON lines → Datadog Logs)

```json
{"ts":"...","call_id":"c-7f3a","event":"call.end","from":"+15551234","to":"+18005550100",
 "codec":"PCMU","duration_ms":8420,"packets_rx":418,"packets_lost":3,"loss_pct":0.71,
 "jitter_ms_p95":18.4,"mos":4.1,"stt_final_ms":640,"llm_ms":1120,"tts_first_byte_ms":310,
 "transcript":"...","reply":"...","tool_calls":["lookup_account"],
 "dd.trace_id":"...","dd.span_id":"...","chaos_profile":"lossy-wan"}
```

Log-trace correlation via `dd.trace_id` injection. The `sip_log_generator.py` from the
rum-install repo can be adapted to emit correlated SIP *signaling* logs using the same
`call_id` — giving the demo signaling + media + AI in a single view without implementing
SIP itself.

## 3. Chaos / realism toggles

Two independent injection points. The provider side is live-tunable via `POST /chaos`
(built in phase 8 — finding 45) so conditions can be flipped mid-demo while a dashboard is
on screen; the network side belongs to the client and is set per run, because the gateway
cannot degrade or even observe the impairment of a stream it only receives (finding 24).

**Client-side (network — most realistic):**
`--loss-pct`, `--loss-burst` (Gilbert-Elliott burst model), `--jitter-ms`,
`--reorder-pct`, `--dup-pct`, `--latency-ms`

**Server-side (provider):**
`--stt-extra-latency-ms`, `--stt-error-rate`, `--llm-extra-latency-ms`,
`--llm-error-rate`, `--tts-extra-latency-ms`, `--stt-fail-after` (mid-call stream death,
which an error rate cannot express — finding 48)

**Named profiles** so a demo beat is one flag:

| Profile | Conditions |
|---|---|
| `clean` | no impairment — baseline |
| `mobile` | 1% loss, 30ms jitter |
| `lossy-wan` | 5% burst loss, 80ms jitter, 2% reorder |
| `provider-degraded` | STT +2s, 10% STT errors, LLM +3s |
| `stt-dropout` | the recognition stream dies 3s into every call (finding 48) |

## 4. Build plan — 9 phases

Every phase ends green before the next starts. The `mock` providers land in phase 4, so
nothing after it requires cloud credentials or incurs API spend to test.

**Progress: all nine phases are complete, plus the control plane** (which was
originally deferred to Appendix A as "real SIP signaling" but turned out to be load-bearing
for four separate measurement problems — see findings 25-28). See
[§7 Findings](#7-findings-from-the-implementation) for what the implementation taught us,
including corrections to assumptions recorded in this plan.

### Phase 0 — Scaffold & toolchain

- `go mod init`, repo layout above
- `Makefile`: `build test lint proto up down demo`
- Install `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc` (**none present on this machine today**)
- `deploy/compose.yml` with `voicegw` + `datadog-agent`; `.env.example`

**Test:** `make build` produces both binaries; `make up` starts the agent and
`podman exec … agent status` shows APM + DogStatsD ready.

### Phase 1 — Codec + framing (pure functions, no I/O)

- G.711 µ-law/A-law encode/decode, 8kHz PCM16 ↔ 160-byte frames
- WAV reader/writer, stdlib only (no ffmpeg/sox dependency — neither is installed)
- 8k ↔ 16k resampling for Google STT

**Test:** table-driven tests against ITU G.711 reference vectors; WAV → µ-law → WAV
round-trip with an SNR assertion; fuzz the decoder on random bytes.

### Phase 2 — RTP send/receive + loss & jitter accounting

- Client: packetize frames (`pion/rtp`), monotonic seq/ts, random SSRC, 20ms pacing
- Server: UDP listener, per-SSRC session, seq-gap loss detection, RFC 3550 jitter
- Chaos impairment layer on the sender

**Test (the one that matters most):** loopback integration test — send 500 known frames
with 5% scripted loss and fixed jitter, assert the server's computed loss % and jitter land
within tolerance. This is what proves the dashboard numbers aren't lying.

### Phase 3 — Jitter buffer (done)

- Map keyed on extended RTP timestamp; configurable target/max depth; prebuffering;
  late-packet drop; duplicate rejection; overflow eviction with playout advance; hole
  concealment (repeat with fade / silence / comfort noise); optional adaptive depth
- `voicegw` runs a per-stream buffer with a 20 ms playout loop

**Test:** scripted packet sequences (in-order, reordered, duplicated, gapped,
late-beyond-depth, overflowing, across the timestamp wrap) asserting correctly ordered
PCM output and expected drop/conceal counts, plus an end-to-end pass over the real
packetizer and impairment model.

**No fake clock was needed.** A pull-driven design made it unnecessary — see finding 8.

### Phase 4 — Provider interfaces + mock implementations (done)

```go
type Transcriber interface {
    Stream(ctx context.Context, audio <-chan Frame) (<-chan Transcript, error)
}
type Agent interface {
    Reply(ctx context.Context, transcript string, tools []Tool) (Reply, error)
}
type Synthesizer interface {
    Synthesize(ctx context.Context, text string, opts Opts) (<-chan Frame, error)
}
```

- `mock` STT: deterministic transcript from a fixture map keyed by audio hash; emits
  scripted partials with configurable delays
- `mock` LLM: canned reply + canned tool call
- `mock` TTS: known PCM pattern, length proportional to text
- Fault-injection decorators (latency, error rate) wrapping any implementation

Delivered as `internal/stt`, `internal/llm`, `internal/tts`, `internal/faults` (the shared
latency/error primitive) and `internal/call` (the pipeline, which also measures the timings
phase 5 turns into spans). `voicegw` now runs the full pipeline per stream.

**Test:** full pipeline, zero network — audio in → RTP → jbuf → mock STT → mock LLM (with a
real tool call) → mock TTS → RTP out over a real socket, asserting the transcript, the tool
result in the reply, the degraded confidence, and the egress tone frequency that identifies
the exact reply text. This is the CI regression test for every later phase.

### Phase 5 — Datadog instrumentation (done)

- `internal/obs`: tracer start (`tracer.WithLLMObsEnabled()`,
  `WithLLMObsMLApp("voice-demo")`), DogStatsD client, structured logger with trace injection
- APM spans per the shape above
- LLM Obs workflow + llm/agent/tool spans around STT/LLM/TTS
- Call-log writer

Delivered as `internal/obs`, the only package that imports dd-trace-go. Plus
`deploy/datadog/dashboard.json` and a `make dashboard` target.

**Test:** not `mocktracer` — it does not capture Agent Observability spans. The right tool
turned out to be `instrumentation/testutils/testtracer`, which stands up a fake agent and
captures the actual submitted payloads, including LLM Obs span events. The acceptance test
runs the real pipeline against it and asserts the span kinds, the nesting, the session
grouping, the metric keys and their units. Verified additionally against a real local agent
(7.84.2): see finding 21.

### Phase 6 — LLM in the loop (done)

This is what turns the demo from a pipeline into an actual voice agent, and what makes the
Agent Observability views worth showing.

Two of the four items shipped early, in phase 4: the `agent` → `llm` + `tool` span tree
with token metrics, and TTS consuming the agent's reply. What phase 6 added:

- **A real agent** — `internal/llm/anthropic.go`, Claude via the official Go SDK, behind
  `-real-llm`. The mock stays the default (finding 32).
- **`WithAnnotatedToolDefinitions`** on every model round, so a turn where the agent
  decided it did not need a tool is distinguishable from one where no tool was offered.
- **Cost** as `input_cost` / `output_cost` custom metrics on the LLM span, computed from a
  per-model price list carried on `llm.Info` (finding 33).
- **Three evaluations per call** via `SubmitEvaluationFromSpan`, joined to the workflow
  span: `transcript_confidence` (score), `audio_quality` (categorical), and
  `replied_every_turn` (boolean). Derived from pipeline data rather than an LLM judge,
  deliberately (finding 34).

**Test:** `internal/llm/anthropic_test.go` drives the real SDK against a local HTTP server,
covering the request this program builds and the tool round trip with no credentials and no
spend. `internal/call/evaluation_test.go` asserts the tool definitions, cost, and all three
evaluations reach the capturing fake agent. `internal/llm/anthropic_integration_test.go`
(behind `-tags=integration`) checks what only a live model can: that it keeps replies short
enough to speak, calls the tool instead of inventing a balance, and asks for a repeat when
handed a garbled transcript.

### Phase 7 — Real Google STT/TTS (done)

- `internal/stt/google.go` — Speech-to-Text v2 `StreamingRecognize` behind `-real-stt`,
  with interim results on so the first partial really is a first-token analogue. The model
  is `telephony`, not the `latest_long` named above: that is a v1 name (finding 37). Audio
  goes out at the wire's own 8 kHz, un-resampled, per Google's own guidance (finding 37).
- `internal/tts/google.go` — Cloud Text-to-Speech behind `-real-tts`, unary rather than
  streaming and asking for headerless PCM rather than LINEAR16 (findings 38, 39).
- Credentials via ADC or a mounted service-account JSON. Both APIs also need a **project**
  named on the request, which is not merely a billing nicety (finding 40).
- `-real-stt` and `-real-tts` are independent flags, so one stage can be real while the
  other stays reproducible. The mocks remain the defaults, for the reason finding 32 gives
  about the agent.
- `stt.Result` gained `ConfidenceUnknown` and `Err`, because a real provider can decline to
  score a transcript and can die mid-call; both were previously inexpressible (findings 41,
  42).
- `stt.WordErrorRate` — the comparison the integration tests assert on.

**Test:** the offline tests (`internal/stt/google_test.go`, `internal/tts/google_test.go`)
drive the real SDKs against local gRPC servers speaking each service, so the recognizer
path, decoding config, audio batching, byte order, chunking and failure handling are all
pinned with no credentials and no spend — the same technique as phase 6's HTTP server, and
the only way to assert what Google actually receives.

The live tests (`internal/call/google_integration_test.go`, behind `-tags=integration`)
synthesize a phrase with the real voice, push it through G.711 and the real recognizer, and
compare by **word error rate** rather than equality. The golden audio is generated by the
other provider under test rather than committed as a fixture, so the two halves of the
phase check each other. A second test conceals a third of the frames and asserts the
transcript gets *worse*, which is the demo's central claim checked against a real
recognizer instead of a mock written to degrade.

**Not yet verified against the live APIs on this machine**: `gcloud` needs an interactive
re-login here and the local ADC has no quota project, so the live tests skip. The failures
seen while getting that far are what produced finding 40.

### Phase 8 — Load generator + demo script + dashboard (done)

- `internal/loadgen` — one call's setup/stream/teardown, and a concurrency-limited run over
  many of them. `voicectl send` was refactored onto the same code, because a load run that
  set its calls up differently from the single-call demo would be measuring something else
  (finding 43).
- `voicectl load -calls 50 -concurrency 10 -profile clean,mobile,lossy-wan` — profiles and
  fixtures cycle across calls, and From/To come from a small pool so the trace list looks
  like a switchboard. Reproducible: the same seed replays the same run.
- Fixtures vary in **duration**, not content, and finding 44 explains why that is the only
  dimension that matters with the mock recognizer. `voicectl fixtures` writes them out as
  WAVs; `-fixtures <dir>` takes real speech for a `-real-stt` run.
- **A live `/chaos` endpoint** on the gateway, which §3 always called for and which turned
  out to be the thing that makes a scripted demo possible at all (finding 45). Plus
  `/healthz`, which the demo script waits on.
- `deploy/sip/sip_log_generator.py` reads the gateway's call log and emits the SIP signaling
  those calls would have produced, carrying the same `call_id` in the `Call-ID` header.
- `make demo` → `deploy/demo.sh`: four beats, no gateway restart between them, each one
  printing what to look at (finding 46).
- `network_profile` is now a metric tag, which is what lets the dashboard put the beats side
  by side instead of separating them by time (finding 47). Four monitors in
  `deploy/datadog/monitors/`, with a test holding their thresholds against the impairment
  the demo actually produces.

**Test:** `internal/loadgen` runs 50 calls ten at a time against a miniature real gateway —
a real control plane, a real UDP media port, real per-SSRC RTP accounting, with only the AI
pipeline removed — and asserts that every call completes, that peak concurrency respects the
cap, and that **every loss figure reconciles**: the gateway independently measured exactly
the packets the client dropped, on all fifty. `goleak` runs over the whole package via
`TestMain`. Separate tests pin that the profiles are distinguishable (`lossy-wan` must lose
more than `clean` and score worse MOS, or the demo has nothing to show), that a run is
reproducible, and that a cancelled run still tears its calls down.

Verified end to end against the real gateway and the full pipeline: four beats, 16 calls,
loss reconciling on every one.

| beat (network / provider) | loss | concealed | MOS | confidence | max turn |
|---|---|---|---|---|---|
| clean / clean | 0.00% | 0.00% | 4.41 | 0.950 | — |
| mobile / clean | 1.46% | 1.46% | 3.56 | 0.940 | — |
| lossy-wan / clean | 5.71% | 6.70% | 2.08 | 0.922 | — |
| clean / provider-degraded | 0.00% | 0.00% | 4.41 | 0.950 | 6001 ms |

The last row is the one that makes the whole design legible: a pristine network with a sick
provider leaves every transport figure untouched and moves only the span durations.

### Phase 9 — Demo polish (done)

- [docs/demo-runbook.md](demo-runbook.md) — the session script: a ten-second pre-flight,
  what to open in what order, what to point at and say per beat, the failure gallery, and a
  table of what to do when something breaks mid-session. Every figure in it is measured
  (finding 50).
- **Dashboard tuning.** The beat row moved to the top, since it is the row a demo reads and
  a dashboard whose first row does not state its own conditions produces unattributable
  screenshots. Threshold lines now match the monitors exactly, with a test holding them
  together (finding 49), and the y-axis ranges are pinned so two runs are comparable.
- **A mid-call recognizer failure is now producible** — `internal/stt/dropout.go` and the
  `stt-dropout` profile. The plan asked what the trace looks like when STT times out
  mid-call, and the honest answer was that nothing in the system could make that happen
  (finding 48).
- The call's APM root span carries `call.stt_error` when a recognition stream died, so
  `@call.stt_error:*` finds every affected trace without making the whole call an error.

**Test:** `internal/stt/dropout_test.go` pins the behaviour that matters — exactly one error
and it is last, hypotheses already produced survive, a call shorter than the fault point is
untouched, the wrapper keeps draining audio after the recognizer is gone (a wrapper that
stopped reading would stall the gateway's playout loop), the fault point is fixed for the
duration of a call, and two identical runs fail identically. `cmd/voicegw/http_test.go`
covers arming it over `/chaos` and the guarantee that a rejected request cannot leave a
recognizer set to die. `internal/obs/monitors_test.go` holds the dashboard's threshold lines
against the monitors' thresholds.

Rehearsed against the real gateway rather than reasoned about: every row of the runbook's
failure gallery is observed output.

**UI is deliberately deferred.** CLI + Datadog dashboards are the interface for now; a web
UI can be added later if the customer session calls for it.

## 5. Risks / things that will bite

| Risk | Mitigation |
|---|---|
| macOS mic from container — impossible | Mic mode host-only; file/synthetic modes everywhere |
| RTP/UDP through podman's VM on macOS | Fixed mapping `-p 5004:5004/udp`; fallback = run client inside the compose network |
| `llmobs` SDK is EXPERIMENTAL | Pin dd-trace-go v2; confine all SDK calls to `internal/obs` |
| STT/TTS don't map cleanly to "LLM" semantics | Use `billable_character_count` + `time_to_first_token`; the phase-6 LLM turn carries the token/cost story |
| Google STT cost during load tests | Mock providers are the default; real providers opt-in per run and behind a build tag in CI |
| A dashboard cleared by restarting the gateway between demo beats | `POST /chaos` retunes provider impairment live; the network side is per-run on the client, so no beat needs a restart |
| Google streaming recognition's 5-minute cap | The provider stops sending at 4m30s and lets results drain, so recognition ends cleanly instead of the service aborting the stream mid-call |
| Flaky timing-dependent tests | Fake clock in the jitter buffer; tolerance-based assertions elsewhere |
| Google streaming API 5-minute stream limit | Cap synthetic call duration; document the constraint |

## 7. Findings from the implementation

What the implementation changed about the plan. Findings 1-7 are from phases 0-2;
8-11 are from phase 3.

1. **"ITU reference vectors" was the wrong test.** The actual ITU vector files are not
   public, so phase 1 instead asserts the structural properties every conformant G.711
   implementation must satisfy: the published anchor values (u-law `0x00` → -32124, `0xFF`
   → 0; A-law `0xD5` → 8, `0x00` → -5504), canonical silence encodings, per-code
   idempotency, decode stability, and bounded relative error. This is stronger than a
   vector file, because it covers all 256 codes and the full int16 input range.

2. **G.711 is not symmetric about zero, and u-law is not fully idempotent.** The reference
   encoder shifts before negating (`pcm >> 2`, then negate), so `-953 >> 2` is -239 while
   `953 >> 2` is 238 — near a bucket boundary the two land in different quantization
   intervals. Measured worst asymmetry is one top-segment step (1024). Separately, u-law
   code `0x7F` is negative zero: it decodes to 0, which re-encodes to `0xFF`, so it is the
   single non-idempotent code. A-law has no such code. Both are properties of G.711, and
   the tests now assert them explicitly rather than assuming they are absent.

3. **Bursty loss is *worse* than uniform loss, not better.** An earlier comment in `mos.go`
   had this backwards. The E-model's `Ie_eff = Ie + (95-Ie)·Ppl/(Ppl/BurstR + Bpl)` makes
   a higher burst ratio *increase* the impairment: loss concealment can hide one missing
   frame but not a run of them, so bursts destroy whole syllables. This is the
   justification for the burst loss model, and the test now pins the sign of the
   relationship.

4. **The jitter estimator is exact against a closed form — but only below the frame
   interval.** With arrivals alternating between nominal and nominal+j, every interarrival
   delta differs from its timestamp delta by exactly j, so the RFC 3550 EWMA converges to
   j. Verified to three decimal places at 1, 5, and 15 ms. Above 20 ms the offset physically
   reorders packets on the wire, the closed form stops describing what the receiver sees,
   and measurement moves to a separate reordering test.

5. **Trailing loss is undetectable, and the acceptance test has to account for it.** Loss is
   reconstructed from sequence-number gaps, so packets dropped at the very end of a stream
   leave no evidence — the receiver never saw the sequence numbers that would reveal the
   gap. The loopback test sends a few guaranteed-delivered sentinel frames to make the top
   of the sequence range observable, which is what a real endpoint's RTCP BYE accomplishes.
   **Phase 5's control plane must send an end-of-call packet count**, or the demo will
   under-report loss on every call.

6. **Accelerated pacing inflates measured jitter.** `voicectl -pace` compresses a call into
   less wall-clock time by sending frames faster while RTP timestamps still advance at the
   true 20 ms rate. Since jitter is by definition the difference between arrival spacing
   and timestamp spacing, this registers as jitter. Harmless in tests, but the scripted
   demo in phase 8 must use true 20 ms pacing or the jitter numbers on screen will be
   wrong.

7. **Sender and receiver agree exactly.** The phase-2 acceptance test over real UDP
   injected 28 drops and the receiver independently measured 28 lost, working only from
   sequence gaps. Verified stable across repeated runs.

8. **The fake clock was unnecessary — a pull-driven buffer is better.** The plan called
   for driving the jitter buffer with a fake clock. Instead the buffer is pull-driven: the
   consumer calls `Pop` once per packetization interval, exactly as an audio device's
   callback does, which makes the pull cadence itself the clock. Every decision — is this
   packet late, is this slot a hole, is the buffer over its latency budget — became a
   function of logical playout position rather than wall-clock time. The result is fully
   deterministic with no clock abstraction to build or inject.

9. **Extended-timestamp arithmetic broke in three places, all found by tests.** Emitted
   frames carried truncated timestamps, because `uint32` of a 2^40-based extended value
   discards exactly the wrong bits (2^40 mod 2^32 is 0). The frame-grid check underflowed
   for any packet behind playout, because unsigned subtraction wraps and 2^64 is not
   divisible by 160, so late packets were misreported as off-grid. And the grid anchor had
   to be *fixed* for the whole call rather than following the advancing playout position.
   Extended timestamps need a stored origin for both the wire value and the grid.

10. **A playout loop must bound its concealment hangover.** The first `voicegw` playout
    loop kept popping after the audio ended, emitting roughly a second of filler and
    driving the reported conceal rate to 31% against a true 8% network loss. The loop now
    goes idle after 10 consecutive starved slots (200 ms, comfortably more than the
    lossy-wan profile's ~4-frame bursts). The remaining 10-frame tail still inflates the
    end-of-call conceal rate slightly; **phase 5's end-of-call control message removes it**,
    the same message finding 5 already requires for the loss count.

11. **The latency/loss trade-off is measurable and reconciles exactly.** On an identical
    impaired stream, a 40 ms buffer discarded 4 packets as late and concealed 29 slots,
    while a 200 ms buffer discarded none and concealed 26 — exactly the 16 packets the
    network actually lost plus the 10-frame hangover, i.e. zero self-inflicted loss. This
    comparison is the phase-8 demo's clearest single illustration of why buffer depth
    matters.

12. **Mocks keyed on audio content would have been a mistake.** The plan called for the
    mock recognizer to look up transcripts by a hash of the audio. That breaks the moment
    loss is injected: the same call would transcribe differently clean versus lossy, and no
    pipeline test could assert a fixed transcript. The mock is instead driven by *how much*
    audio it has consumed — a partial every N frames, a final every M — which is
    deterministic, independent of wall-clock time, and still produces realistic growing
    partials. Same conclusion as finding 8: make the mock a function of logical progress,
    not of content or of time.

13. **The mocks must make loss visible in the AI layer.** A pure plumbing mock would have
    made phases 5-8 testable but would not have demonstrated anything. The mock recognizer
    therefore degrades its reported confidence in proportion to how much of the utterance
    was jitter-buffer filler, and the mock synthesizer derives its tone frequency from the
    reply text. The first makes packet loss show up as an AI-quality metric; the second
    lets a test prove the agent's *specific* reply reached the caller as audio, rather than
    merely that some audio was sent.

14. **Never feed concealed audio to a recognizer when the buffer is empty.** The first
    `voicegw` pipeline forwarded every popped frame, including the playout hangover's
    trailing filler. Because that filler is entirely invented, the last utterance of every
    call consisted only of concealment and reported **zero confidence** despite its speech
    having been recognized correctly — and it manufactured a spurious extra turn. Mid-call
    holes with audio queued behind them are still forwarded to keep the timeline intact,
    but a starved buffer now pauses the stream instead. This also brought the AI-layer and
    network-layer numbers into agreement: `concealed_in` fell from 8.4% to 5.0% against an
    actual network loss of 5.33%. A third consequence of the hangover (see finding 10), and
    another reason phase 5's end-of-call message matters.

15. **The audio queue into the pipeline needs a drop policy, not backpressure.** A slow
    agent turn (the provider-degraded profile adds 3 s per LLM round trip, 6 s on a
    tool-calling turn) would block the playout loop if the queue were unbuffered. Blocking
    is wrong: a caller's voice cannot be paused, so unbounded queueing would just become a
    second, invisible jitter buffer. The queue is bounded at 2 s and drops with a counter
    when full, which is reported per call.

16. **A random starting sequence number needs wrap-safe test code.** The phase-2 loopback
    test reassembled audio by numerically sorting raw `uint16` sequence numbers. Because
    `NewSender` picks a random start per RFC 3550, roughly 0.4% of runs began within 250 of
    65535, wrapped mid-stream, and sorted the post-wrap frames first — rotating the audio
    and failing. It surfaced once during phase 3 and would otherwise have looked like a
    mystery flake. The test now pins the starting sequence and orders frames by signed
    delta, with one case that always wraps. Worth remembering for the phase-8 load
    generator: anything that reassembles a stream must use signed-delta arithmetic, never a
    numeric sort.

17. **`mocktracer` cannot test Agent Observability spans; `testtracer` can.** The plan
    called for `mocktracer`, which only records APM spans. LLM Obs spans travel a separate
    submission path, so a `mocktracer` assertion would have passed while the Agent
    Observability views stayed empty. `instrumentation/testutils/testtracer` stands up a
    fake agent and exposes `WaitForLLMObsSpans`, which returns the real submitted span
    events — kinds, parents, session IDs, metrics and all. Asserting on the wrapper instead
    would have proven nothing about what Datadog receives.

18. **`llmobs.WithError(nil)` panics, despite the documentation promising it is a no-op.**
    This is a real defect in dd-trace-go v2.11.1. `WithError` calls `errortrace.WrapN`,
    which correctly returns a nil `*TracerError`; assigning that typed nil to the config's
    `error`-typed field leaves a **non-nil** interface, and `Finish` then calls `Error()`
    on it and dereferences the nil inner error. The consequence is severe: the
    single-deferred-`Finish(err)` pattern the SDK docs explicitly recommend **panics on
    every successful span**. The APM side is unaffected, since `tracer.WithError` assigns
    the error directly. `Span.Finish` guards against it, and two tests pin the behavior —
    one asserting no panic, one reproducing the upstream defect so a future fixed release
    makes the workaround's removal visible. This is the clearest argument for confining
    the SDK to one package: the workaround is three lines in one file.

19. **Time to first token is in seconds, and nothing in the API says so.**
    `MetricKeyTimeToFirstToken` takes a bare `float64`. The Go SDK reference's example
    passes `0.25`, which only makes sense as seconds. Sending milliseconds would overstate
    latency a thousandfold and quietly ruin every responsiveness chart. The conversion
    happens once, in `obs.Metrics`, and a test asserts `621ms` arrives as `0.621`.

20. **Token counts must not also go over DogStatsD.** Datadog derives platform metrics
    (`ml_obs.span.llm.input.tokens` and friends) from the recognized token attributes on
    LLM Obs spans. Emitting them again as custom metrics would double count. The split is
    therefore: transport and call-level figures over DogStatsD, AI figures as span
    attributes. A test asserts no metric with "token" in its name is ever emitted.

21. **Verified against a real agent, and the 403 was the proof.** Pointing the gateway at
    a local agent 7.84.2 with a deliberately invalid API key produced
    `llmobs: failed to push span events: ... 403 ... API key is missing or invalid`. That
    error is the confirmation: the app built the span events, the agent accepted them and
    proxied them to Datadog's intake via `evp_proxy`, and only authentication failed.
    DogStatsD separately logged 987 metric packets with zero parse errors. With a valid
    key the data lands.

22. **A missing agent must fail loudly, not silently.** With LLM Obs enabled in agent mode
    and no agent reachable, `tracer.Start` returns an error and the gateway exits. That is
    deliberate: discovering mid-presentation that the dashboards are empty is far worse
    than failing at launch. The error names all four remedies. Telemetry is off by default
    so the demo still runs with no agent at all.

23. **Starting a span reassigns `ctx`, which raced with an existing goroutine.** Adding the
    workflow span to the pipeline introduced `wf, ctx := StartWorkflow(ctx, ...)` *after*
    the audio-tee goroutine had already captured the `ctx` variable — a closure-capture
    data race the detector caught immediately. The fix is also what the SDK documents:
    open the span before launching any goroutine, and hand the goroutine the context the
    constructor returned.

24. **The gateway cannot know the client's network profile.** The call tags originally
    labeled the gateway's provider-impairment profile as `chaos_profile`, which attributed
    client-side impairment to the server: nothing in an RTP stream says how it was
    degraded. Renamed to `provider_profile`. The network profile becomes taggable once the
    control plane carries it at call setup — a fourth thing waiting on that message.

25. **The loss blind spot is symmetric, and the design only fixed half of it.** Trailing
    loss was the known problem: a receiver cannot see a gap after the last packet that
    arrived. The leading case is identical and easy to miss — a receiver whose *first*
    packet was dropped anchors its sequence base on the second one and silently starts
    counting a packet in. `NoteFinalSequence` fixed only the tail.

    This was caught empirically, not by reasoning: running the real client against the
    real gateway across fifteen seeds, two of them under-counted by exactly one packet with
    `expected=99` on a 100-frame stream. The fix is `NoteSequenceRange(first, final)`, and
    `EndCall` now rejects a half-range outright — accepting one end would leave the other
    edge uncounted while reporting the figure as exact, which is worse than not fixing it.
    A test now shows 20 dropped packets of which only 9 are countable from the stream alone.

26. **A latent bug lived in the same arithmetic.** The expected range was anchored on the
    first packet to *arrive*, not the lowest sequence number seen. With reordering at the
    very start of a stream — packet 5 arriving before packet 0 — `Expected` undercounted
    and could in principle fall *below* `Received`, which is nonsense. The session now
    tracks a minimum as well as a maximum extended sequence.

27. **EndCall removes the hangover entirely, which makes two numbers agree.** With an
    explicit teardown the playout loop stops exactly when the caller stopped, so no
    trailing concealment is emitted at all: `underruns` went from about 10 per call to 0.
    The visible consequence is that the jitter buffer's conceal rate and the pipeline's
    `concealed_in_pct` are now *identical* (6.53% and 6.53% on the verification run),
    where before they differed (8.39% versus 5.0%) purely because of hangover filler. The
    hangover bound remains for unsignaled calls and mid-call gaps, where the end still has
    to be inferred from silence.

28. **The return media path needs the observed source address, not a declared one.** The
    caller declares a port at setup; the gateway pairs it with the address the RTP actually
    came from. That is what SDP plus RTP does in practice and what makes the path work from
    behind NAT. `voicectl -save-reply` writes the agent's spoken reply to a WAV, which is
    the first artifact in this project a person can simply listen to.

29. **Unsignaled calls still work, deliberately.** RTP from an SSRC that never called
    StartCall creates a call anyway, tagged `signaled=false`, with no trailing-loss
    accounting and no return audio. Keeping that path means every test and demo built
    before the control plane existed still runs, and it makes the contrast demonstrable:
    `voicectl send -no-signaling` is the before picture. `-require-signaling` on the
    gateway turns it off.

30. **Tool definitions are accepted on LLM spans only.** `WithAnnotatedToolDefinitions`
    on a workflow or agent span is dropped with a log warning, not an error — the SDK
    checks `spanKind` first. The agent span is the more natural home for "what this agent
    can do", so this is worth knowing before reaching for it. They go on the per-round
    `agent.reply` LLM span instead, which has the side benefit of showing the tool set as
    it was on each round. `TestToolDefinitionsAreOnlyValidOnLLMSpans` pins the behavior so
    a future SDK release that relaxes it is visible.

31. **The `Agent` interface is stateless; a real model is not.** The interface takes one
    transcript at a time, but the Messages API is given the whole conversation on every
    request, and a turn that calls a tool *must* replay the assistant message that
    requested it — results are matched to calls by `tool_use` ID, and a reconstructed
    turn missing them is rejected. So the provider keeps history keyed by call ID, with
    `Message.ToParam()` echoing each response back verbatim so thinking blocks and tool
    IDs survive. The side benefit is a coherent agent: the caller can say "what about the
    other one" and be understood. The gateway calls `Forget` at teardown rather than
    waiting for the TTL to reap it.

    Two bugs here were found by tests, not by inspection. The turn-boundary rule for
    trimming history assumed tool results arrive as *consecutive* user messages; they do
    not — the sequence is user, assistant(tool_use), user(tool_result) — so every tool
    turn was counted as two and trimming cut in the wrong place. And the trim then kept
    one turn too many, slicing at the oldest collected boundary instead of the
    *maxTurns*-th newest. Both would have produced a request the API rejects, only on a
    call long enough to trim.

32. **The mock stays the default, and that is a feature.** `-real-llm` is opt-in. Beyond
    cost and credentials, the scripted agent is what makes the transport story
    reproducible: it returns the same reply for the same transcript, so a change in the
    dashboards between two runs is a change in the *network*, not in the model's mood.
    The demo's central claim — that packet loss degrades AI quality — is only legible
    against a fixed agent.

33. **Cost has no SDK constant; `input_cost` and `output_cost` are custom metric keys.**
    Datadog recognizes them and renders them in the cost views, but a typo would be
    invisible: the span submits fine and the view simply stays empty. `TestCostMetricsUse
    TheExpectedKeys` pins both strings. Pricing lives on `llm.Info` so the provider owns
    its own price list, and an unknown model reports *no* cost rather than a guessed one
    — a missing figure is recoverable, a wrong one quietly misinforms every downstream
    dashboard. A free provider omits the metric entirely rather than sending zero, which
    would drag down any average taken across mock and real calls.

34. **The evaluations are derived from pipeline data, not from an LLM judge.** A judge
    would be a more impressive demo of evaluations and a much worse demo of *this* system,
    because the figure that makes the argument here is one that moves with packet loss. A
    judge would mostly measure the judge. `transcript_confidence` is the recognizer's own
    mean confidence, `audio_quality` buckets the call by concealed fraction, and
    `replied_every_turn` catches the failure the transport metrics cannot see — a turn
    that produced silence. All three are submitted even on a call that transcribed
    nothing, since omitting them would bias the series toward calls that went well.

    `TestConcealedAudioDegradesTheEvaluation` makes the demo's claim falsifiable: clean
    audio scores 0.950, audio with every fourth frame concealed scores 0.712 and is
    labeled `poor`. If that test ever passes trivially, the thing the dashboards are built
    to show has stopped happening.

35. **Thinking is left on, at low effort, rather than disabled.** Disabling it would be the
    obvious choice for a latency-critical phone call, and it has a failure mode that would
    be both invisible here and ruinous: on this model family the model occasionally writes
    a tool call into its *spoken text* instead of emitting a real tool call. The turn
    "succeeds", the lookup never runs, and the caller is read a balance that was never
    fetched. Low effort buys most of the latency saving without that risk. Relatedly,
    `max_tokens` has to leave headroom because thinking shares the output budget with the
    spoken text — a tight cap truncates the reply mid-sentence, which the caller hears.

36. **A refusal is a successful response with no content.** `stop_reason: "refusal"`
    arrives as HTTP 200, so code that reads `content[0]` unconditionally would hand the
    caller silence — the one failure mode a phone line must never have. The provider
    checks the stop reason *before* the content and speaks a handoff line instead. The
    server-side `fallbacks` parameter was deliberately not adopted: it is beta-endpoint
    only, which would move the whole provider onto beta param types for a risk this
    content does not carry. Handling the refusal gracefully defends against the actual
    failure mode. Reversible if a demo ever needs it.

37. **Three of the plan's four Google details were wrong, and one of them was Google's own
    advice.** `latest_long` is a v1 model name; Speech v2's catalogue is `long`, `short`,
    `telephony` and the chirp family, and `telephony` is the one trained on 8 kHz phone
    audio — exactly this demo's input. The plan also called for upsampling 8 kHz to 16 kHz
    because Google's docs recommend 16 kHz, but the same field's documentation finishes the
    thought: "if that's not possible, use the native sample rate of the audio source
    (instead of resampling)". Interpolating telephony audio adds no information a recognizer
    can use, doubles the bytes on the wire, and would make the sample rate reported on every
    STT span a fiction. So no resampling at all, and the 8k↔16k converter from phase 1 goes
    unused on this path. The decoding config is explicit rather than auto-detected, since
    headerless PCM off the wire gives a detector nothing to work with. And the recognizer
    resource needs a project and a location even for an ad-hoc request: `.../recognizers/_`
    means "configure from this request", but the path still has to name a project.

38. **Streaming synthesis was the obvious choice and the wrong one.** The `Synthesizer`
    interface streams, so `StreamingSynthesize` looks like the match. Two things rule it
    out: it supports only a subset of voices, so adopting it would silently narrow which
    voices work and fail at request time for the rest; and its responses are documented as
    24 kHz LINEAR16 regardless of the rate requested, which would need a 3:1 resampler this
    project does not have — the only rates here are 8 kHz on the wire and the 16 kHz speech
    services prefer. The unary call chunked into 20 ms frames keeps every voice and keeps
    the metric honest: time to first byte is now the whole request's latency, which is
    exactly the silence the caller hears. What is given up is a genuinely lower one.

39. **`LINEAR16` from the TTS API carries a WAV header; `PCM` does not.** They are the same
    samples. A header companded into G.711 and sent to the caller is a short burst of noise
    at the start of every reply — audible, and the kind of thing that gets blamed on the
    network during a demo. The provider asks for `PCM` and rejects a WAV-wrapped response
    rather than stripping the header, because accepting it would mean the request was
    misconfigured in other ways that matter too, including the sample rate the span reports.

40. **Both APIs need a project on the request, and user credentials make that a hard
    error.** This was found by running the integration test, not by reading: with a laptop's
    `gcloud` ADC, Text-to-Speech returns `PermissionDenied ... requires a quota project,
    which is not set by default`. A service account carries its own project; user
    credentials carry none. So the project is passed as `option.WithQuotaProject` on both
    clients — the recognizer's project in the resource path does not satisfy it, and
    Text-to-Speech has no resource path at all. `gcloud auth application-default
    set-quota-project` is the other half of the fix, and it belongs in the README rather
    than in someone's memory.

41. **A real recognizer may decline to report confidence, and zero is not the answer.**
    Google documents the field as set only on finals, "not guaranteed to be accurate", with
    0.0 as a sentinel for unset. Passing that through would have read as total certainty
    that the transcript is wrong — and `transcript_confidence` is the call's headline
    evaluation, the one the demo points at to argue that packet loss degrades AI quality. A
    fabricated zero there is worse than a gap: it looks like a call that went badly. So
    `stt.Result` carries `ConfidenceUnknown`, unscored turns are left out of the mean rather
    than counted as zero, the evaluation is omitted when a call scored nothing at all, and
    the call log omits the field instead of logging a zero. The flag is false by default, so
    the mock — which always reports a figure — needed no changes.

42. **A recognizer that dies mid-call looked exactly like a caller who stopped talking.**
    Both close the results channel. The `Transcriber` interface could only report a failure
    from `Stream` itself, which covers a rejected configuration and nothing after it, so a
    quota error forty seconds into a call would have ended recognition silently and the call
    log would have called it a normal hangup. `Result.Err` fixes it, and the pipeline records
    it as `Result.STTErr` — separately from the turns, because it belongs to no turn — marks
    the workflow span errored, and logs it as `stt_error`. The call is *not* reported as
    failed overall: the turns that completed before the recognizer quit really happened.

    Getting the error to say something useful took one more step. gRPC reports a stream the
    server has aborted as `io.EOF` from `Send`, with the real status available only from the
    receiving side, so the first version faithfully reported `EOF` for a quota failure. The
    send side's error now gives way to the receive side's whenever it is uninformative, and
    a cancelled context — which is how *every* call ends — is not reported as a failure at
    all. Two tests pin both halves.

43. **The load generator had to be the single-call path, not a second one.** Writing
    `voicectl load` as its own implementation would have been easier and would have
    quietly invalidated the demo: two code paths setting calls up differently measure
    different things, and the argument the dashboards make depends on a load run and a
    single call being the same call. So `internal/loadgen` owns one `Place`, and
    `voicectl send` was refactored onto it, keeping only what is genuinely single-call —
    the reply listener, the WAV save, the two-sided reconciliation printout. The
    `-no-signaling` path stayed separate on purpose: it has no control plane at all, which
    is the entire point of it.

44. **Varied audio fixtures do not vary the conversation; varied *lengths* do.** The plan
    asked for several distinct WAV files so transcripts and replies would differ across a
    load run. With the mock recognizer that reasoning does not hold: the mock finalizes an
    utterance every N frames *consumed* and cycles its phrase list from the start of each
    call (finding 12 — deliberately a function of logical progress, not content). So two
    different waveforms of the same length produce byte-identical conversations, while a
    2.5 s call and an 11 s call produce two turns and six. The built-in fixtures therefore
    vary duration, and vary pitch only so a person can tell them apart by ear. Content
    matters again under `-real-stt`, which is what `-fixtures <dir>` is for. Verified: the
    four-beat run produced 2, 3, 4, 5 and 6-turn calls from the five fixtures.

45. **The demo needed the live endpoint more than it needed the load generator.** `/chaos`
    was in §3 from the start and never built, because nothing until now required it. It
    turns out to be load-bearing for the scripted demo: flipping a beat by restarting the
    gateway clears every live graph and the call list, which costs the audience's attention
    at exactly the moment the comparison is being made. Three things fell out of building
    it. The fault wrappers had to survive a *clean* start — `faults.Config.Tunable`, since
    `WithFaults` otherwise returns the provider unwrapped and a gateway started clean has
    nothing to turn on. The profile name had to become a live value rather than a startup
    constant, because it is a metric tag: a beat's latency tagged with the previous beat's
    profile would have made the dashboard actively wrong. And the retune has to be atomic —
    a half-applied request leaves conditions no profile describes, which is the worst thing
    to be looking at on a dashboard.

46. **A demo script's hardest part is the order of the beats, not the automation.** The four
    beats are ordered so each answers the question the previous one raises: `clean`
    establishes what healthy looks like (without it every later number is unanchored),
    `mobile` shows light impairment is visible at all, `lossy-wan` makes the central claim
    that packet loss degrades AI quality, and `provider-degraded` is the control — because
    an audience that has just watched the network hurt the agent will ask how you would
    tell a sick network from a sick provider. The answer is on screen: transport panels
    return to baseline while span durations blow out. Each beat prints what to watch, so
    the script is also the narration.

47. **The beats were invisible to the metrics until the client's profile became a tag.**
    Every metric carried `provider_profile` but nothing carried the *network* profile, so a
    dashboard could only separate beats by time — and "the numbers changed around 14:32" is
    a far weaker claim than two labeled series in one graph. The control plane has carried
    the client's declared profile since the phase-5 work (finding 24 anticipated exactly
    this), so the fix was to put it on `obs.CallTags` and use it. The dashboard gained a
    beat-comparison row split by it, and `$network_profile` as a template variable.

48. **The plan asked for a trace of a mid-call STT timeout, and nothing could produce
    one.** The fault injector decides a provider's fate when the stream *opens*, which was
    deliberate — it keeps injected latency attributable to one span — but it only models a
    provider that was never reachable. The failure a voice pipeline is actually asked about
    is the recognizer that was working and then stopped, mid-utterance, with the caller
    still talking. So phase 9 had to build it: `internal/stt/dropout.go`, wrapping any
    Transcriber, with the fault point measured in *audio consumed* rather than wall-clock
    for the same reason the mock recognizer is (finding 12) — a demo that failed at a
    different point each run would be unrepeatable.

    Two things about it were not obvious. The wrapper has to keep draining the caller's
    audio after the recognizer is gone, because the gateway's playout loop hands every
    frame to that channel and a wrapper that stopped reading would stall playout — which is
    also exactly what happens to a caller's voice when the far end has stopped listening.
    And the error and the provider's last hypothesis become ready at the same instant, so a
    naive select kept the final transcript on some runs and dropped it on others; the
    wrapper now delivers everything the provider already produced and reports the failure
    after it, with a grace bound for a provider that will not wind down.

    What the rehearsal then showed is the part worth saying out loud in a demo: an
    eight-second call produced two turns instead of four, with `packets_rx=400`,
    `packets_lost=0` and `mos=4.41` — pristine transport — and `stt_error` naming the
    failure and where in the call it happened. But `mean_confidence` stayed at 0.95 and
    `audio_quality` still said `clean`, because the turns that *did* complete were fine.
    **None of the three evaluations catches a recognizer that stopped early.** What catches
    it is the errored workflow span, `stt_error` in the log, and `voice.call.errors` tripping
    the failed-turns monitor. That is an honest thing to tell an audience about what
    evaluations are for: they grade the output that exists, not the output that never
    happened.

49. **A dashboard line and an alert threshold that disagree are worse than neither.** The
    dashboard now draws its threshold lines at exactly the monitors' values, and a test
    fails the build if a line is drawn that nothing alerts on, or if a line is labelled as
    anything other than a monitor threshold. The failure mode this prevents is specific and
    bad: a graph showing a call sitting comfortably under its line while an alert fires
    about it, mid-demo, which is the only thing anyone in the room would remember
    afterwards.

50. **Every number in the runbook is measured, and that is the point of writing it last.**
    A runbook of plausible figures is worse than no runbook, because the first number that
    does not match is the moment the audience stops believing the rest. So each beat and
    each failure in [docs/demo-runbook.md](demo-runbook.md) was run against the real
    gateway and the observed output pasted in — which is also how finding 48's evaluation
    gap was discovered, by reading what the call log actually said rather than what it was
    expected to say.

Also worth noting for later phases: `Session` reports current and maximum jitter but not
percentiles, because computing p95 would require retaining per-packet samples. Phase 5
should send jitter as a DogStatsD **distribution** and let the agent aggregate percentiles.

## Appendix A — Deferred: real SIP signaling

**Partly built.** A gRPC control plane mirroring INVITE/BYE now exists in
`internal/control`, because four measurement problems turned out to be unsolvable without
signaling of some kind (findings 25-28). What remains deferred is *SIP specifically*:

Out of scope per decision 3, recorded in case the audience later cares about the protocol
itself rather than the capability:

- `emiago/sipgo` UAS: handle INVITE/ACK/BYE, parse SDP, negotiate PCMU, bind an RTP port
- SIPp as external traffic generator using `play_pcap_audio` for media

Caveat: SIPp + RTP into podman on macOS crosses a VM boundary — expect to pin UDP port
ranges, and likely to run SIPp inside the podman network rather than on the host. The
gRPC control plane is intentionally shaped so this swaps in at the edge without touching
the media path or the instrumentation.
