# User guide

`voicegw` is the gateway. `voicectl` is the caller. Build both with `make build`; the binaries land in `./bin`.

The gateway listens for RTP, runs a jitter buffer, then STT → LLM → TTS, and sends the spoken reply back. The client sets the call up over gRPC, sends G.711 audio, and optionally impairs the stream on the way out. Mocks are the default for all three AI stages, and telemetry is off unless you turn it on, so a local run needs no credentials and no Datadog agent.

```bash
make build

./bin/voicegw -addr 127.0.0.1:5004 -grpc-addr 127.0.0.1:50051

./bin/voicectl send -duration 5s -save-reply reply.wav
```

`voicegw -h`, `voicectl send -h`, and `voicectl load -h` print the same flags this page documents. Go's flag parser accepts both `-flag value` and `-flag=value`. A bare bool flag turns the option on; pass `-flag=false` to force it off. Durations are Go duration strings: `500ms`, `5s`, `1m`.

SIGINT and SIGTERM stop either process. `voicectl load` exits non-zero when any call fails.

## voicegw

One process, three listeners:

| Listener | Default | Flag |
|---|---|---|
| RTP (UDP) | `:5004` | `-addr` |
| gRPC control plane | `:50051` | `-grpc-addr` |
| HTTP (`/chaos`, `/healthz`) | `:8080` | `-http-addr` |

`-http-addr ""` disables the HTTP listener. A bind failure on that port is logged and the gateway keeps serving calls.

### Media and call lifetime

| Flag | Default | What it does |
|---|---|---|
| `-addr` | `:5004` | UDP address that receives RTP. |
| `-grpc-addr` | `:50051` | gRPC address for `StartCall` / `EndCall`. |
| `-report-interval` | `5s` | How often live per-call stats are printed. |
| `-idle-timeout` | `3s` | Silence after which a stream that never got `EndCall` is treated as ended. |
| `-require-signaling` | `false` | Drop RTP from an SSRC that never called `StartCall`. |
| `-call-log` | `-` (`VOICE_CALL_LOG`) | Call-log path. `-` is stdout. |

Without `-require-signaling`, RTP from an unknown SSRC still opens a call. That call is tagged `signaled=false`: trailing packet loss cannot be reconstructed, and there is no address to send reply audio to.

### Jitter buffer

Depths are in 20 ms frames.

| Flag | Default | What it does |
|---|---|---|
| `-jbuf-target` | `3` (60 ms) | Prebuffer depth before playout starts. |
| `-jbuf-max` | `25` (500 ms) | Hard cap. Must be at least the target. |
| `-jbuf-adaptive` | `false` | Deepen the buffer when packets arrive late. |
| `-conceal` | `repeat` | What fills a missing frame: `repeat`, `silence`, or `noise`. |

A shallow target plays sooner and drops packets that arrive after playout has moved on. A deeper target waits those packets out and conceals only what the network actually lost, plus the playout hangover.

### Provider faults

These wrap the STT, LLM, and TTS clients. They apply whether the stage is a mock or a real API. A negative value means "leave the profile's setting alone," so `0` is a real override (including `-stt-fail-after 0`, which disarms the mid-call dropout).

| Flag | Default | What it does |
|---|---|---|
| `-provider-profile` | `clean` | Named scenario. See [Profiles](#profiles). |
| `-stt-latency` | unset | Extra delay before each recognition stream opens. |
| `-stt-error-rate` | unset | Fraction of recognition streams that fail at open, `0`–`1`. |
| `-llm-latency` | unset | Extra delay before each agent turn. |
| `-llm-error-rate` | unset | Fraction of agent turns that fail, `0`–`1`. |
| `-tts-latency` | unset | Extra delay before each synthesis. |
| `-stt-fail-after` | unset | Kill the recognition stream after this much audio has been consumed. `0` disables it. |
| `-fault-seed` | `1` | RNG seed for the error-rate injectors. LLM uses `seed+1`, TTS uses `seed+2`. |

`-stt-error-rate` fails a stream as it opens. `-stt-fail-after` fails a stream that was already working, mid-utterance. Latency is applied before an injected error, so a failing call still costs the timeout.

The same knobs are live on `POST /chaos` after startup. Network loss and jitter are not: those are applied by `voicectl` on the way out.

### Which AI implementation

Each stage is independent. The mock stays in place until its own flag is set. API keys are environment variables, never flags.

| Flag | Default | Env override | What it does |
|---|---|---|---|
| `-real-stt` | `false` | `VOICE_REAL_STT` | Google Speech-to-Text v2. |
| `-real-tts` | `false` | `VOICE_REAL_TTS` | Google Cloud Text-to-Speech. |
| `-google-project` | empty | `GOOGLE_CLOUD_PROJECT` | Project billed for both Google APIs. Required when either is real. |
| `-google-location` | `global` | `GOOGLE_CLOUD_LOCATION` | STT location. A non-`global` value needs that region's endpoint. |
| `-stt-model` | `telephony` | `VOICE_STT_MODEL` | `telephony`, `telephony_short`, `long`, or `short`. |
| `-stt-language` | `en-US` | `VOICE_STT_LANGUAGE` | BCP-47 tag for recognition. |
| `-tts-voice` | `en-US-Neural2-C` | `VOICE_TTS_VOICE` | Google voice name. |
| `-tts-language` | `en-US` | `VOICE_TTS_LANGUAGE` | BCP-47 tag for synthesis. Must match the voice. |
| `-real-llm` | `false` | `VOICE_REAL_LLM` | Anthropic Messages API. |
| `-llm-model` | SDK default (`claude-opus-5` when `VOICE_LLM_MODEL` is set as in `.env.example`) | `VOICE_LLM_MODEL` | Model id for `-real-llm`. |
| `-llm-effort` | `low` | `VOICE_LLM_EFFORT` | `low`, `medium`, `high`, `xhigh`, or `max`. |

Google credentials come from Application Default Credentials (`gcloud auth application-default login`, or `GOOGLE_APPLICATION_CREDENTIALS` pointing at a service-account JSON). The Anthropic key is `ANTHROPIC_API_KEY`.

Startup logs one line each for `stt`, `agent`, and `tts`, naming the implementation that is actually serving.

### Datadog

Off by default. `-dd` turns traces, DogStatsD metrics, and the correlated call log shipping on. With `-dd` and `-dd-llmobs` (itself on by default), the process refuses to start if it cannot reach an agent that supports LLM Observability.

| Flag | Default | Env override | What it does |
|---|---|---|---|
| `-dd` | `false` | `VOICE_DD_ENABLED` | Ship traces, metrics, and logs. |
| `-dd-llmobs` | `true` | `DD_LLMOBS_ENABLED` | Agent Observability spans. Only consulted when `-dd` is on. |
| `-dd-agentless` | `false` | `DD_LLMOBS_AGENTLESS_ENABLED` | Submit LLM Obs directly. Needs `DD_API_KEY` in this process. |
| `-dd-service` | `voicegw` | `DD_SERVICE` | Service name. |
| `-dd-env` | `demo` | `DD_ENV` | Environment tag. |
| `-dd-version` | `0.1.0` | `DD_VERSION` | Version tag. |
| `-dd-ml-app` | `voice-demo` | `DD_LLMOBS_ML_APP` | ML app name in Agent Observability. |
| `-dd-agent-host` | `localhost` | `DD_AGENT_HOST` | Agent hostname. Compose uses `datadog-agent`. |
| `-dd-trace-port` | `8126` | `DD_TRACE_AGENT_PORT` | APM intake. |
| `-dd-statsd-port` | `8125` | `DD_DOGSTATSD_PORT` | DogStatsD. |

`make up` starts the gateway and a Datadog agent together, with `-dd` already on the container command line. A hand-started `./bin/voicegw` does not print a trace id until you pass `-dd`.

## voicectl

```text
voicectl send [flags]      one call
voicectl load [flags]      many calls
voicectl fixtures [flags]  write the built-in synthetic WAVs
voicectl profiles          list impairment profiles
```

### send

Places one call. With signaling (the default) the client asks the control plane for the media address, so `-to` is optional. Reply audio is received on an ephemeral UDP port and declared at setup.

| Flag | Default | What it does |
|---|---|---|
| `-control` | `127.0.0.1:50051` | Gateway gRPC address. |
| `-to` | empty | Gateway RTP `host:port`. Taken from `StartCall` when empty. With `-no-signaling` and still empty, falls back to `127.0.0.1:5004`. |
| `-no-signaling` | `false` | Send RTP only. Trailing loss is invisible to the gateway, and no reply audio comes back. |
| `-from` | `+15551234567` | Caller id, the stand-in for a SIP From. |
| `-to-number` | `+18005550100` | Callee id, the stand-in for a SIP To. |
| `-codec` | `PCMU` | `PCMU` or `PCMA`. |
| `-file` | empty | WAV to send. Any sample rate is resampled to 8 kHz. When set, `-tone` and `-duration` are ignored. |
| `-tone` | `440` | Hz of the synthetic tone used when `-file` is empty. |
| `-duration` | `5s` | Length of that tone. |
| `-pace` | `20ms` | Gap between frames. Below 20 ms the call finishes faster and the gateway's jitter number is inflated. |
| `-profile` | `clean` | Network impairment profile. |
| `-seed` | current time, ns | Impairment RNG. Pin it to replay a run. |
| `-save-reply` | empty | Write the agent's reply to this WAV. |
| `-reply-port` | `0` | Local port for reply RTP. `0` lets the kernel pick. |
| `-no-reply` | `false` | Do not listen for reply audio. |

Network knobs overlay the profile. A negative value means "not set," so `0` really does turn that impairment off.

| Flag | Default | Range |
|---|---|---|
| `-loss-pct` | unset | `0`–`100` |
| `-jitter-ms` | unset | `≥ 0` |
| `-latency-ms` | unset | `≥ 0` |
| `-reorder-pct` | unset | `0`–`100` |
| `-dup-pct` | unset | `0`–`100` |

`lossy-wan`'s burst-loss model is part of the profile. The per-knob flags change the percentages and the constant delay; they do not switch burst mode on or off.

### load

Same call path as `send`, many times. Profiles and fixtures cycle across calls. Caller numbers come from a small pool.

| Flag | Default | What it does |
|---|---|---|
| `-control` | `127.0.0.1:50051` | Gateway gRPC address. |
| `-to` | empty | RTP address. Derived from the control plane when empty. |
| `-calls` | `50` | How many calls to place. `0` with `-duration` runs until the timer fires. |
| `-duration` | `0` | Stop after this long. `0` means place exactly `-calls` calls. Both set means "up to N calls, or until the time is up." |
| `-concurrency` | `10` | Calls in flight. |
| `-stagger` | `100ms` | Delay between each worker's first call. |
| `-profile` | `clean` | One profile, or a comma-separated list cycled across calls. |
| `-fixtures` | empty | Directory of WAVs. Empty uses the built-in synthetic set. |
| `-codec` | `PCMU` | `PCMU` or `PCMA`. |
| `-pace` | `20ms` | Frame interval. Leave it at 20 ms if you will read the jitter figures. |
| `-seed` | `1` | Replay seed for the whole run. |
| `-quiet` | `false` | Suppress the per-call line. |
| `-verbose` | `false` | Also print each turn's transcript and reply. |

The same network overrides as `send` apply, and they apply to every profile in the list. `-profile clean,mobile -loss-pct 20` keeps the two profiles' jitter different and sets both to 20% loss.

A per-call line looks like:

```text
   3  c-abc123   lossy-wan          balance-check       sent=225  lost=11  =11  loss= 4.89% jitter=  31.2ms mos=2.84 turns=3
```

The `=` between the two loss counts means the gateway's measurement matches the packets the client dropped. `≠` means the gateway's figure is a lower bound (the final sequence number never arrived).

### fixtures

```bash
voicectl fixtures -dir testdata/fixtures
```

`-dir` defaults to `testdata/fixtures`. Writes the five built-in files and prints the `voicectl send -file` invocation for the first one.

| File | Length |
|---|---|
| `short-query.wav` | 2.5 s |
| `balance-check.wav` | 4.5 s |
| `dispute-charge.wav` | 6.5 s |
| `transfer-request.wav` | 8.5 s |
| `long-conversation.wav` | 11 s |

Under the mock recognizer, length is what changes the conversation: an utterance finalizes every two seconds of audio consumed, so 2.5 s is two turns and 11 s is six. Pitch differs so you can tell the files apart by ear. With `-real-stt`, put real speech in a directory and pass it as `-fixtures` or `-file`.

### profiles

`voicectl profiles` prints each named profile and its network settings. No flags.

## Profiles

One name sets a whole scenario. Client profiles (`voicectl -profile`) change the RTP stream. Gateway profiles (`-provider-profile`, or `POST /chaos`) change the AI stages. A profile only fills in its own side; the other side stays clean.

| Name | Network | Provider |
|---|---|---|
| `clean` | none | none |
| `mobile` | 1% loss, 30 ms jitter, 20 ms latency | none |
| `lossy-wan` | 5% burst loss (mean burst 4), 80 ms jitter, 60 ms latency, 2% reorder | none |
| `provider-degraded` | none | STT +2000 ms, STT errors 10%, LLM +3000 ms |
| `stt-dropout` | none | recognition stream killed after 3000 ms of audio |

`provider-degraded` and `stt-dropout` on the client do not impair the network. `mobile` and `lossy-wan` posted to `/chaos` do not impair the providers. Put each name on the side that owns it.

## HTTP

Default base is `http://127.0.0.1:8080`.

### GET /healthz

```bash
curl -sS localhost:8080/healthz
```

```json
{
  "status": "ok",
  "live_calls": 0,
  "provider_profile": "clean"
}
```

### GET /chaos

Current provider settings, plus how many calls each stage has seen and how many of those it failed.

```bash
curl -sS localhost:8080/chaos
```

```json
{
  "provider_profile": "clean",
  "stt": { "latency_ms": 0, "error_rate": 0, "calls": 0, "errors": 0 },
  "llm": { "latency_ms": 0, "error_rate": 0, "calls": 0, "errors": 0 },
  "tts": { "latency_ms": 0, "error_rate": 0, "calls": 0, "errors": 0 },
  "note": "network impairment is applied by the client (voicectl -profile), not here: the gateway cannot degrade a stream it only receives"
}
```

`fail_after_ms` appears on `stt` once that fault has been armed.

### POST /chaos

`POST` and `PUT` both apply. A named `profile` replaces the provider settings wholesale, which is what makes `{"profile":"clean"}` actually clear the previous beat. Individual fields then refine that result. Omit a field to leave it alone. A body with only knobs, and no `profile`, is tagged `custom` so metrics are not labeled with a profile whose numbers no longer match.

| JSON field | Maps to |
|---|---|
| `profile` | a name from the table above |
| `stt_latency_ms` | `-stt-latency` |
| `stt_error_rate` | `-stt-error-rate` (`0`–`1`) |
| `llm_latency_ms` | `-llm-latency` |
| `llm_error_rate` | `-llm-error-rate` (`0`–`1`) |
| `tts_latency_ms` | `-tts-latency` |
| `stt_fail_after_ms` | `-stt-fail-after` (`0` disarms) |

The response is the same document as `GET`. A bad value is `400` with `{"error":"..."}` and nothing is changed. Any other method is `405`.

## Examples

### One clean call, listen to the reply

```bash
./bin/voicegw -addr 127.0.0.1:5004 -grpc-addr 127.0.0.1:50051 -idle-timeout 1s
./bin/voicectl send -duration 5s -save-reply reply.wav
```

Five seconds of 440 Hz, PCMU, no impairment. The summary should show `loss reconciles exactly`, `turns=3`, `tools=2`, and about 4 s of reply audio. The mock recognizer's confidence on a clean call is 0.95. `reply.wav` is the thing you can play.

`-idle-timeout 1s` just ends an abandoned stream sooner when you are iterating. Leave the default `3s` if you care about the idle-reap behavior.

### Send a file, name the parties, pin the codec

```bash
./bin/voicectl fixtures -dir testdata/fixtures
./bin/voicectl send \
  -file testdata/fixtures/balance-check.wav \
  -from +15557654321 -to-number +18005550199 \
  -codec PCMA \
  -save-reply reply.wav
```

### Reproducible network damage

```bash
./bin/voicectl send -duration 5s -profile lossy-wan -seed 7
./bin/voicectl send -duration 5s -profile clean -loss-pct 12 -jitter-ms 40 -reorder-pct 3 -seed 42
./bin/voicectl send -duration 5s -profile lossy-wan -loss-pct 0 -seed 7
```

The third line keeps `lossy-wan`'s jitter, latency, and reordering, and turns loss off. Same `-seed` plus the same profile and audio replays the same drops.

### Shallow buffer versus deep buffer

Same stream both times (`-seed 7`, `lossy-wan`, 4 s). Restart the gateway between them; buffer depth is a startup flag.

```bash
./bin/voicegw -addr 127.0.0.1:5004 -jbuf-target 2 -idle-timeout 1s
./bin/voicectl send -duration 4s -profile lossy-wan -seed 7
```

```bash
./bin/voicegw -addr 127.0.0.1:5004 -jbuf-target 10 -idle-timeout 1s
./bin/voicectl send -duration 4s -profile lossy-wan -seed 7
```

Target 2 (40 ms) late-drops packets that had already arrived. Target 10 (200 ms) conceals the network's losses and the playout hangover, and late-drops nothing. `-jbuf-adaptive` lets the buffer grow toward `-jbuf-max` on its own instead of you picking the depth.

### Concealment you can hear

```bash
./bin/voicegw -addr 127.0.0.1:5004 -conceal silence
./bin/voicectl send -duration 5s -profile lossy-wan -seed 7 -save-reply reply.wav
```

`repeat` (the default) fills a hole with the last good frame. `silence` writes zeros. `noise` writes comfort noise. The reply path's own listener always conceals with silence; this flag is the gateway's inbound buffer.

### Unsignaled media

```bash
./bin/voicectl send -to 127.0.0.1:5004 -no-signaling -duration 5s -loss-pct 10 -seed 1
```

The gateway still prints a call, with `signaled=false`, no reply, and a loss figure that can miss a trailing gap. To refuse that traffic:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -require-signaling
```

### Provider latency at startup

```bash
./bin/voicegw -addr 127.0.0.1:5004 \
  -provider-profile provider-degraded \
  -llm-error-rate 0 \
  -fault-seed 1
./bin/voicectl send -duration 5s
```

`provider-degraded` is +2 s on STT and +3 s on the LLM, with a 10% chance the recognizer fails at open. The extra `-llm-error-rate 0` leaves the profile's latency in place and clears an error rate (the profile's LLM error rate is already 0; the flag is how you would override one that was not).

### Change the beat without restarting

Leave the gateway up. The dashboard, the call list, and the metric streams stay put.

```bash
curl -sS -X POST localhost:8080/chaos -d '{"profile":"provider-degraded"}'
./bin/voicectl send -duration 5s

curl -sS -X POST localhost:8080/chaos -d '{"llm_latency_ms":1500}'
./bin/voicectl send -duration 5s

curl -sS -X POST localhost:8080/chaos -d '{"profile":"clean"}'
```

The middle request is tagged `provider_profile: "custom"` because the knobs no longer match a named profile. `{"profile":"clean"}` clears every provider knob, including `stt_fail_after_ms`.

### Recognizer dies mid-call

```bash
curl -sS -X POST localhost:8080/chaos -d '{"profile":"stt-dropout"}'
./bin/voicectl send -duration 8s
curl -sS -X POST localhost:8080/chaos -d '{"profile":"clean"}'
```

Eight seconds would normally be four turns. The stream dies after 3 s of audio, so you get two, with loss still at 0. The call log carries `stt_error` and the reason. Equivalent at startup: `-provider-profile stt-dropout`, or `-stt-fail-after 3s` on top of any other profile.

### Agent that fails every turn, TTS that is just slow

```bash
curl -sS -X POST localhost:8080/chaos -d '{"llm_error_rate":1,"tts_latency_ms":800}'
./bin/voicectl send -duration 5s
curl -sS localhost:8080/chaos
```

`llm.errors` increments once per failed turn. `tts.latency_ms` is 800. Transport numbers stay at the client's profile, which is still `clean` here.

### A load run, then a timed one

```bash
./bin/voicectl load -calls 50 -concurrency 10 -profile clean,mobile,lossy-wan -seed 1
```

Fifty calls, ten at a time, profiles round-robin, synthetic fixtures round-robin. The report's first line to read is `loss reconciles on all N calls`. Then the per-profile table (loss, jitter, MOS, conceal, turns).

```bash
./bin/voicectl load -calls 0 -duration 30s -concurrency 4 -profile mobile -quiet
./bin/voicectl load -calls 20 -duration 15s -concurrency 4 -profile lossy-wan -verbose
```

The first runs until 30 s. The second stops at 20 calls or 15 s, whichever comes first, and prints every transcript.

### Real speech instead of the synthetic set

```bash
./bin/voicectl load \
  -calls 12 -concurrency 4 \
  -fixtures testdata/fixtures \
  -profile clean,lossy-wan \
  -seed 1
```

Any directory of `.wav` files works. Names come from the filenames.

### Real providers, one stage at a time

Mock STT with a real voice, so you can listen without spending on recognition:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -grpc-addr 127.0.0.1:50051 \
  -real-tts -google-project "$GOOGLE_CLOUD_PROJECT"
./bin/voicectl send -duration 5s -save-reply reply.wav
```

Real recognition, mock tone on the way back, so a test can still check the reply by frequency:

```bash
./bin/voicegw -addr 127.0.0.1:5004 \
  -real-stt -google-project "$GOOGLE_CLOUD_PROJECT" \
  -stt-model telephony -stt-language en-US
./bin/voicectl send -file testdata/fixtures/balance-check.wav
```

All three real, with Datadog:

```bash
export ANTHROPIC_API_KEY=...
export GOOGLE_CLOUD_PROJECT=...
gcloud auth application-default login
gcloud services enable speech.googleapis.com texttospeech.googleapis.com --project "$GOOGLE_CLOUD_PROJECT"

./bin/voicegw -addr 127.0.0.1:5004 -grpc-addr 127.0.0.1:50051 \
  -real-stt -real-tts -real-llm \
  -llm-model claude-opus-5 -llm-effort low \
  -dd -dd-agent-host localhost
```

`-llm-effort medium` (or `high`, `xhigh`, `max`) spends more of the caller's silence on reasoning. `low` is the right default for a phone call.

### Telemetry on, by hand or by compose

Hand-started, agent already on localhost:

```bash
./bin/voicegw -addr 127.0.0.1:5004 -grpc-addr 127.0.0.1:50051 \
  -dd -dd-service voicegw -dd-env demo -dd-ml-app voice-demo \
  -call-log /tmp/calls.jsonl
./bin/voicectl send -duration 5s
```

The client prints `trace: <id>` only when the gateway was started with `-dd`. Open that id in APM.

Compose, which builds the image, starts the agent, and passes `-dd`:

```bash
cp .env.example .env   # set DD_API_KEY
make up
make agent-status
./bin/voicectl send -duration 5s -save-reply /tmp/reply.wav
```

Agentless LLM Obs, no local agent, still needs `-dd` and `DD_API_KEY` in the gateway's environment:

```bash
DD_API_KEY=... ./bin/voicegw -dd -dd-agentless -dd-llmobs
```

### The four-beat demo

Gateway already up, `/healthz` answering:

```bash
make demo
CALLS=20 CONCURRENCY=5 BEAT_PAUSE=60 make demo
```

`make demo` runs `deploy/demo.sh`. Defaults are 12 calls per beat, 4 concurrent, 20 s between beats. The beats are:

| Beat | `voicectl -profile` | `POST /chaos` |
|---|---|---|
| 1 clean | `clean` | `clean` |
| 2 mobile | `mobile` | `clean` |
| 3 lossy-wan | `lossy-wan` | `clean` |
| 4 provider-degraded | `clean` | `provider-degraded` |

The script posts `{"profile":"clean"}` again on the way out. `BEAT_PAUSE` of at least 60 s is what you want in front of a dashboard; Datadog's rollup smears anything shorter together. `CONTROL` and `HTTP` override the addresses (`127.0.0.1:50051` and `127.0.0.1:8080`).

What each beat is supposed to show, and what to do when a number is wrong, is in [demo-runbook.md](demo-runbook.md).

### SIP log lines that share the call id

SIP is not implemented. This reconstructs the INVITE and BYE those calls would have produced, with the real `call_id` in `Call-ID`, so a log search hits signaling, the media record, and the trace together.

```bash
./bin/voicegw -addr 127.0.0.1:5004 -call-log /tmp/calls.jsonl -dd

python3 deploy/sip/sip_log_generator.py \
  --call-log /tmp/calls.jsonl \
  --out /tmp/sip_signaling.log \
  --follow
```

| Flag | Default | What it does |
|---|---|---|
| `--call-log` | `-` (stdin) | The gateway's `-call-log` file. |
| `--out` | `/tmp/sip_signaling.log` | SIP text. `-` is stdout. Opened append. |
| `--follow` | off | Keep reading as the gateway appends. |
| `--echo-calls` | off | Also copy each call-log line to stdout. |

`make sip-logs CALL_LOG=/tmp/calls.jsonl` is the same script with `--follow` and `--out` defaulting to `/tmp/sip_signaling.log`.

## make

| Target | What it does |
|---|---|
| `make build` | `./bin/voicegw` and `./bin/voicectl`. |
| `make test` | Race-detector tests. No cloud, no credentials. |
| `make test-integration` | Tests that call the real providers. Costs money. |
| `make up` / `make down` / `make logs` | Podman compose stack: gateway + Datadog agent. |
| `make agent-status` | APM and DogStatsD sections of `agent status`. |
| `make demo` | `deploy/demo.sh`. See above. |
| `make load` | 50 calls, profiles `clean,mobile,lossy-wan`. |
| `make fixtures` | `voicectl fixtures -dir testdata/fixtures`. |
| `make dashboard` | Upload `deploy/datadog/dashboard.json`. Needs `DD_API_KEY` and `DD_APP_KEY`. |
| `make monitors` | Upload `deploy/datadog/monitors/*.json`. Same keys. |
| `make sip-logs CALL_LOG=...` | Follow the call log and write SIP text. |

`DEMO_CONTROL` (default `127.0.0.1:50051`) and `DEMO_HTTP` (default `127.0.0.1:8080`) are what `make demo` and `make load` dial.
