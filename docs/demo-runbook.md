# Demo runbook

What to open, in what order, what to point at, and what to do when something breaks.

Everything here has been run. The figures are measured, not estimated — if a number below
does not match what you see on the day, something has changed and the discrepancy is worth
chasing before the session rather than during it.

## 15 minutes before

```bash
make build
```

```bash
make up                      # datadog-agent + voicegw under podman
```

```bash
make dashboard && make monitors    # needs DD_API_KEY and DD_APP_KEY
```

Then the pre-flight, which is one call and takes ten seconds:

```bash
./bin/voicectl send -duration 5s -save-reply /tmp/reply.wav
```

Four things have to be true, and all four are on screen in that one command's output:

1. `loss reconciles exactly` — the gateway independently measured what the client dropped.
2. `turns=3 tools=2` for a five-second call — the agent answered and called its tool. (Turn
   count follows call length: the recognizer finalizes an utterance every two seconds of
   audio, so a three-second call gives two turns and an eleven-second call six.)
3. `reply: 1070 frames of audio received` and `saved 4.00s of reply audio` — the return
   media path works.
4. A trace ID is printed, and opening it has to show the trace in Datadog. If it does not,
   nothing else today will either. **The trace ID only appears when the gateway was started
   with `-dd`** — `make up` does that, a hand-started gateway does not, and a missing trace
   line means telemetry is off rather than broken.

Play `/tmp/reply.wav` if you want to hear the agent. It is the one artifact in this project
a person can simply listen to.

**If the gateway refuses to start** with a message about LLM Observability, that is
deliberate — it will not run with telemetry enabled and no agent reachable, because
discovering mid-session that the dashboards are empty is far worse than failing at launch.
The error names the four remedies.

## What to open, in what order

| # | What | Why it is this one |
|---|---|---|
| 1 | The **voice-demo dashboard** | the beat row is the top row; it states its own conditions |
| 2 | **APM → Traces**, `service:voicegw`, grouped by resource | one trace per call, the transport story |
| 3 | **Agent Observability → Sessions** | one session per call, the AI story, grouped by `call_id` |
| 4 | **Logs**, `service:voicegw` | the per-call JSON line that ties the other three together |

Leave all four open. The demo's entire argument is that these are views of one call, so
switching between them with the same `call_id` in hand is the demonstration.

## The four beats

```bash
make demo
```

`CALLS`, `CONCURRENCY` and `BEAT_PAUSE` tune it. For a customer session raise `BEAT_PAUSE`
to at least 60: Datadog's metric rollup makes a beat shorter than a minute hard to separate
on a dashboard, and you want time to talk.

Measured over a real run, which is what the beat row will show:

| Beat | loss | concealed | MOS | confidence | slowest turn |
|---|---|---|---|---|---|
| 1 `clean` | 0.00% | 0.00% | 4.41 | 0.950 | — |
| 2 `mobile` | 1.46% | 1.46% | 3.56 | 0.940 | — |
| 3 `lossy-wan` | 5.71% | 6.70% | 2.08 | 0.922 | — |
| 4 `provider-degraded` | 0.00% | 0.00% | 4.41 | 0.950 | 6001 ms |

### Beat 1 — clean

**Point at:** the MOS tile at 4.4, loss flat at zero, and the shape of one trace —
`voice.call` with `rtp.ingest` and `jitter_buffer` beneath it, then the AI spans.

**Say:** this is a healthy call, and every number in the next three beats is relative to
this one. Without a baseline, "5% loss" is a number nobody can judge.

Open one call's Agent Observability session here, while everything is clean. The audience
needs to see the span tree once before it is being used to diagnose something:
`workflow → stt.transcribe`, then `agent` with `agent.reply` and `lookup_account` under it,
then `tts.synthesize`.

### Beat 2 — mobile

**Point at:** loss near 1%, jitter around 30 ms, MOS dropping to about 3.6, and the first
non-zero concealment.

**Say:** this is a decent cellular connection. The jitter buffer is absorbing the variance,
which is what it is for — and the price of that absorption is latency, which is the next
thing worth showing if anyone asks why you would not just make the buffer enormous.

### Beat 3 — lossy-wan — the point of the demo

**Point at:** concealed audio climbing past its dashed monitor line, and then
`transcript_confidence` in the Evaluations view falling with it. Then open one call's
session and show the workflow span's `concealed_pct`.

**Say:** the recognizer's confidence dropped and nothing about the model changed between
beat 1 and now. The audio it was given was partly invented by the jitter buffer, because
packets did not arrive. That is a network fault showing up as an AI quality metric — and
it is only visible because the two layers are instrumented separately and correlated, not
averaged together.

The mock agent matters here, and say so if asked: it returns the same reply for the same
transcript, so a change between beats is a change in the *network* rather than in the
model's mood. With `-real-llm` the transport story gets noisier, not clearer.

### Beat 4 — provider-degraded — the control

**Point at:** the first three panels returning to beat-1 numbers while the fourth —
`agent turn latency` — blows past its 4-second line. First partial transcript goes to
2001 ms, and a tool-calling turn to 6002 ms, which is two model round trips at 3 s each.

**Say:** a clean network with sick providers. This is how you tell the two apart, and it is
why transport lives in APM while the AI layer lives in Agent Observability. Mixing them
would leave you averaging a network problem and a provider problem into one unhelpful
latency chart.

## Failure gallery

Rehearsed, with the observed output. Each is one command, and each is worth having ready
because these are the questions an audience asks.

### The recognizer dies mid-call

The one people actually ask about: a recognizer that was working and then stopped, with the
caller still talking.

```bash
curl -X POST localhost:8080/chaos -d '{"profile":"stt-dropout"}'
```

```bash
./bin/voicectl send -duration 8s
```

An eight-second call produces **two turns instead of four**, and the call log says why:

```
stt_error         stt: the recognition stream failed 3s into the call: faults: injected provider error
pipeline_errors   1
packets_rx        400      packets_lost  0      mos  4.41
turn_count        2        mean_confidence  0.95   audio_quality  clean
```

**Point at:** the transport figures are pristine. The failure is entirely in the AI layer,
the Agent Observability workflow span is marked errored, and the APM trace carries
`call.stt_error` so `@call.stt_error:*` in the trace list finds every affected call.

**Say the uncomfortable part too:** `mean_confidence` is still 0.95 and `audio_quality`
still says `clean`, because the two turns that *did* complete were fine. None of the three
evaluations catches a recognizer that stopped early — what catches it is `stt_error` in the
log, `voice.call.errors` on the dashboard, and the "calls are producing failed turns"
monitor. That is an honest answer about what evaluations are for: they grade the output
that exists, not the output that never happened.

Before phase 7 this failure was not even expressible. A dying recognizer closed its channel
exactly as a caller who had stopped talking did, and the call log called it a normal hangup.

### The recognizer is unreachable

```bash
curl -X POST localhost:8080/chaos -d '{"profile":"clean","stt_error_rate":1}'
```

The call fails at setup rather than partway through, and the log says which stage:

```
event    call.failed
error    call c-8ae63ed5-1: opening the transcriber: faults: injected provider error (rate 100%)
turns    0          packets_rx  200      mos  4.41
```

**Point at:** the difference from the previous one. `call.failed` versus `call.end` with an
`stt_error` — a provider that never answered against a provider that stopped answering.
They need separate signals because they need separate fixes.

### The agent fails every turn

```bash
curl -X POST localhost:8080/chaos -d '{"profile":"clean","llm_error_rate":1}'
```

```
turns  2    pipeline_errors  2
turn errors: agent: faults: injected provider error (rate 100%)
```

**Point at:** the transcripts are there and the replies are empty. This is the failure
`replied_every_turn` exists for, and it evaluates to **false** — the caller heard silence,
which is the one failure mode a phone line must never have.

### A call with no signaling

The before picture for the control plane, and the fastest way to justify having one:

```bash
./bin/voicectl send -to 127.0.0.1:5004 -no-signaling -duration 3s -loss-pct 10
```

```
signaled               false
end_reason             idle-timeout
packets_lost           25       packets_expected  150
sequence_range_known   false
underruns              10       frames_returned   0
```

**Point at:** `sequence_range_known: false`. The loss figure is a lower bound rather than a
measurement — packets lost at either edge of the stream leave no evidence, so a receiver
cannot see them. `frames_returned: 0`, because without signaling there is no port to send
the agent's reply to. And `underruns: 10`, the playout hangover a signaled call does not
have, because `EndCall` says exactly when the caller stopped.

Pass `-require-signaling` to the gateway to refuse these outright.

### Reset

Every rehearsal leaves provider state behind. `make demo` resets to `clean` when it
finishes, but a failure demo run by hand does not:

```bash
curl -X POST localhost:8080/chaos -d '{"profile":"clean"}'
```

```bash
curl -s localhost:8080/chaos | python3 -m json.tool
```

## When something goes wrong mid-session

| Symptom | Almost always | Do this |
|---|---|---|
| dashboards empty, calls working | telemetry off | the gateway needs `-dd`; restart it with it |
| `connection refused` from voicectl | gateway not running, or wrong port | `curl localhost:8080/healthz` |
| every call fails at setup | a previous rehearsal left an error rate armed | `curl -s localhost:8080/chaos` and reset to `clean` |
| jitter looks absurdly high on clean | `-pace` below 20 ms | drop the flag; accelerated pacing *is* jitter by definition |
| loss figures look too low | calls are unsignaled | check `signaled` in the call log |
| a beat looks identical to the previous one | `/chaos` applied, client profile not changed | the network side is a client flag, not a gateway setting |
| no reply audio | `-no-reply`, or an unsignaled call | neither can receive audio back |

The last row of that table is the one worth internalizing: **network impairment is the
client's, provider impairment is the gateway's.** Nothing in an RTP stream says how it was
degraded, so the gateway cannot apply or even observe network conditions. `GET /chaos` says
so in its response for exactly this reason.

## If the audience asks for something not scripted

```bash
# more calls, heavier, all three network profiles at once
./bin/voicectl load -calls 50 -concurrency 10 -profile clean,mobile,lossy-wan
```

```bash
# the latency/loss trade-off: the same impaired stream through a shallow then a deep buffer
./bin/voicegw -jbuf-target 2 ...   # discards late packets, conceals more
./bin/voicegw -jbuf-target 10 ...  # discards none, conceals only what the network lost
```

```bash
# a real recognizer and a real voice, if credentials are to hand
./bin/voicegw -real-stt -real-tts -google-project YOUR_PROJECT -dd
```

```bash
# SIP signaling correlated to the same calls
python3 deploy/sip/sip_log_generator.py --call-log /tmp/calls.jsonl --follow
```

A question this project cannot answer honestly: anything about SIP signaling behaviour
itself. The control plane mirrors INVITE/BYE semantics over gRPC and the signaling logs are
reconstructed after the fact from calls that really happened. Say that plainly — the media
path is authentic, the signaling protocol is not, and the reason is that instrumenting the
flow was the goal rather than implementing SIP.
