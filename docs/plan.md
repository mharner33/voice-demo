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
| STT/TTS provider | **Google** — Speech-to-Text v2 `StreamingRecognize` + Cloud Text-to-Speech |
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
├── llm  "stt.transcribe"              model=latest_long, provider=google
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
- STT/TTS are not token-based. `MetricKeyBillableCharacterCount` and
  `MetricKeyTimeToFirstToken` are the right fits; skip the token keys there. The LLM turn
  is where real token metrics and cost tracking appear.
- `WithSessionID(callID)` groups every span of a call in the LLM Obs session view — this
  is the view to open first in a demo.
- `span.APMTraceID()` ties LLM Obs spans back to the APM trace for the drill-down.
- Phase 6 also submits an evaluation (`SubmitEvaluationFromSpan`) — e.g. a transcript
  confidence score — so the Evaluations view isn't empty.

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

Two independent injection points, both live-tunable via `POST /chaos` (and settable by
env) so conditions can be flipped mid-demo while a dashboard is on screen.

**Client-side (network — most realistic):**
`--loss-pct`, `--loss-burst` (Gilbert-Elliott burst model), `--jitter-ms`,
`--reorder-pct`, `--dup-pct`, `--latency-ms`

**Server-side (provider):**
`--stt-extra-latency-ms`, `--stt-error-rate`, `--llm-extra-latency-ms`,
`--llm-error-rate`, `--tts-extra-latency-ms`

**Named profiles** so a demo beat is one flag:

| Profile | Conditions |
|---|---|
| `clean` | no impairment — baseline |
| `mobile` | 1% loss, 30ms jitter |
| `lossy-wan` | 5% burst loss, 80ms jitter, 2% reorder |
| `provider-degraded` | STT +2s, 10% STT errors, LLM +3s |

## 4. Build plan — 9 phases

Every phase ends green before the next starts. The `mock` providers land in phase 4, so
nothing after it requires cloud credentials or incurs API spend to test.

**Progress: phases 0 through 4 are complete.** See
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

### Phase 5 — Datadog instrumentation

- `internal/obs`: tracer start (`tracer.WithLLMObsEnabled()`,
  `WithLLMObsMLApp("voice-demo")`), DogStatsD client, structured logger with trace injection
- APM spans per the shape above
- LLM Obs workflow + llm/agent/tool spans around STT/LLM/TTS
- Call-log writer

**Test:** assert span names/tags/metrics with dd-trace-go's `mocktracer` in unit tests;
run against the local agent with `DD_TRACE_DEBUG=1`. Then verify manually in Datadog: one
trace per call, LLM Obs session grouped by `call_id`, metrics flowing, logs correlated.

### Phase 6 — LLM in the loop (moved forward)

This is what turns the demo from a pipeline into an actual voice agent, and what makes the
Agent Observability views worth showing.

- Real LLM turn: transcript → reply, with a tool definition (`lookup_account`) backed by a
  mock implementation so the trace shows `agent` → `llm` + `tool` spans
- `WithAnnotatedToolDefinitions`, token metrics, cost tags
- One submitted evaluation per call so the Evaluations view is populated
- TTS consumes the LLM reply rather than echoing the transcript

**Test:** integration test with mock STT/TTS and a real LLM, asserting the tool is invoked
and the full span tree appears. Verify in Datadog that token costs, tool calls, and the
evaluation all render.

### Phase 7 — Real Google STT/TTS

- Google STT v2 `StreamingRecognize` (streaming partials → `time_to_first_token`)
- Google Cloud Text-to-Speech
- Credentials via mounted service-account JSON / ADC; `gcloud` SDK is already installed

**Test:** golden-audio tests behind a `-tags=integration` build flag so CI stays offline
and free. Compare transcripts using a **word-error-rate threshold**, not exact match.
Verify `time_to_first_token` is populated and plausible.

### Phase 8 — Load generator + demo script + dashboard

- `voicectl load --calls 50 --concurrency 10 --profile lossy-wan --duration 60s`
- Several distinct WAV fixtures so transcripts and LLM replies vary
- Adapt `sip_log_generator.py` to emit SIP signaling logs keyed to the same `call_id`
- `make demo` → scripted beats: `clean` → `mobile` → `lossy-wan` → `provider-degraded`
- Datadog dashboard JSON + a couple of monitors, checked into `deploy/datadog/`

**Test:** 50 concurrent calls with no goroutine leaks (`goleak`), no dropped calls, and
each profile change visibly reflected on the dashboard.

### Phase 9 — Demo polish (before the customer session)

- README narrative: what to open, in what order, what to point at
- Dashboard tuning so each chaos profile is unmistakable on screen
- Failure-mode rehearsal: what the trace looks like when STT times out mid-call

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

Also worth noting for later phases: `Session` reports current and maximum jitter but not
percentiles, because computing p95 would require retaining per-packet samples. Phase 5
should send jitter as a DogStatsD **distribution** and let the agent aggregate percentiles.

## Appendix A — Deferred: real SIP signaling

Out of scope per decision 3, recorded in case the audience later cares about signaling:

- `emiago/sipgo` UAS: handle INVITE/ACK/BYE, parse SDP, negotiate PCMU, bind an RTP port
- SIPp as external traffic generator using `play_pcap_audio` for media

Caveat: SIPp + RTP into podman on macOS crosses a VM boundary — expect to pin UDP port
ranges, and likely to run SIPp inside the podman network rather than on the host. The
gRPC control plane is intentionally shaped so this swaps in at the edge without touching
the media path or the instrumentation.
