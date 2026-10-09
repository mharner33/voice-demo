#!/usr/bin/env bash
#
# The scripted demo: four beats, each one flag apart, with a dashboard on
# screen throughout.
#
# The beats are ordered so that each one answers a question the previous one
# raises. Clean establishes what healthy looks like — without it, every later
# number is unanchored. Mobile shows that light impairment is visible at all.
# Lossy-wan is the beat that makes the demo's central claim: packet loss
# reaches the AI layer and degrades transcript confidence. Provider-degraded
# then separates the two causes, because an audience that has just watched the
# network hurt the agent will reasonably ask how you would tell a sick network
# from a sick provider. The answer is on screen: the network panels stay clean
# while the span durations blow out.
#
# The gateway is never restarted between beats. That is what the /chaos
# endpoint is for: a restart would clear every live graph and the call list,
# and take the audience's attention with it.
#
# Usage:
#   deploy/demo.sh                      # against a gateway already running
#   CALLS=20 CONCURRENCY=5 deploy/demo.sh
#   BEAT_PAUSE=30 deploy/demo.sh        # longer pauses to talk over
#
set -euo pipefail

CONTROL=${CONTROL:-127.0.0.1:50051}
HTTP=${HTTP:-127.0.0.1:8080}
VOICECTL=${VOICECTL:-./bin/voicectl}

# Twelve calls at four concurrent is enough to make a dashboard move without
# a laptop struggling to run the gateway, the agent and the load at once.
CALLS=${CALLS:-12}
CONCURRENCY=${CONCURRENCY:-4}

# The pause between beats is where the talking happens. It also matters
# technically: Datadog's metric rollup means a beat shorter than a minute can
# be hard to separate on a dashboard, so a real customer session should raise
# this rather than lower it.
BEAT_PAUSE=${BEAT_PAUSE:-20}

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
dim() { printf '\033[2m%s\033[0m\n' "$*"; }

rule() { printf '\n%s\n' "────────────────────────────────────────────────────────────────"; }

die() {
  printf '\033[31m%s\033[0m\n' "$*" >&2
  exit 1
}

require() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

require curl

[[ -x "$VOICECTL" ]] || die "$VOICECTL not found; run 'make build' first"

# A gateway that is not up is the overwhelmingly common way this script fails,
# and the error has to say what to do about it rather than surfacing as a
# connection refused from the first call.
if ! curl -fsS "http://${HTTP}/healthz" >/dev/null 2>&1; then
  die "no gateway answering on http://${HTTP}/healthz

Start one first:
  ./bin/voicegw -addr 127.0.0.1:5004 -grpc-addr ${CONTROL} -dd

The demo needs the /chaos endpoint to change provider conditions without a
restart; a restart between beats would clear every live graph."
fi

# Provider impairment is server-side state that outlives a run, so a previous
# demo left halfway through provider-degraded would silently poison this one's
# baseline.
chaos() {
  curl -fsS -X POST "http://${HTTP}/chaos" -d "$1" >/dev/null \
    || die "could not set provider impairment: $1"
}

beat() {
  local name=$1 profile=$2 provider=$3 watch=$4

  rule
  bold "BEAT: ${name}"
  dim "  client network profile: ${profile}"
  dim "  gateway provider profile: ${provider}"
  echo
  bold "  Watch:"
  printf '%s\n' "$watch" | sed 's/^/    /'
  echo

  chaos "{\"profile\":\"${provider}\"}"

  "$VOICECTL" load \
    -control "$CONTROL" \
    -calls "$CALLS" \
    -concurrency "$CONCURRENCY" \
    -profile "$profile" \
    -quiet

  if [[ ${BEAT_PAUSE} -gt 0 ]]; then
    echo
    dim "  pausing ${BEAT_PAUSE}s — this is the beat's window on the dashboard"
    sleep "$BEAT_PAUSE"
  fi
}

rule
bold "voice-demo — scripted demo"
dim "  control plane: ${CONTROL}"
dim "  chaos endpoint: http://${HTTP}/chaos"
dim "  ${CALLS} calls per beat, ${CONCURRENCY} concurrent, ${BEAT_PAUSE}s between beats"
echo
dim "  Open before starting:"
dim "    1. the voice-demo dashboard (make dashboard uploads it)"
dim "    2. APM trace list for service:voicegw, grouped by resource"
dim "    3. Agent Observability → sessions, which is the call-level AI view"

beat "1 — clean" clean clean \
"• the baseline: loss 0%, jitter near zero, MOS above 4.3
• transcript confidence at its ceiling (0.95), audio quality 'clean'
• note the shape of a healthy trace: rtp.ingest, jitter_buffer, then the AI spans"

beat "2 — mobile" mobile clean \
"• loss around 1%, jitter around 30ms: visible, not yet painful
• MOS drops but stays usable; the jitter buffer absorbs the variance
• concealed audio appears for the first time — the buffer inventing frames"

beat "3 — lossy-wan" lossy-wan clean \
"• this is the point of the demo: 5% bursty loss, 80ms jitter
• conceal rate climbs, and transcript confidence follows it down
• the AI quality metric moves because the *network* moved — nothing about
  the model changed between beat 1 and beat 3
• open one call's session: the concealed fraction is on the workflow span,
  and the Evaluations view shows audio_quality as 'degraded' or 'poor'"

beat "4 — provider-degraded" clean provider-degraded \
"• the control: a clean network with sick providers
• network panels go back to beat-1 numbers — loss 0%, MOS above 4.3
• but stt.transcribe and agent.reply span durations blow out, and some STT
  calls fail outright
• this is how you tell a sick network from a sick provider, and it is why
  transport lives in APM while the AI layer lives in Agent Observability"

# Left clean, so the next run's baseline is a baseline. A demo that ended in a
# degraded state would make the next one's beat 1 inexplicable.
chaos '{"profile":"clean"}'

rule
bold "done — provider impairment reset to clean"
echo
dim "To correlate SIP signaling with these calls:"
dim "  python3 deploy/sip/sip_log_generator.py --call-log <the gateway's -call-log> --follow"
dim "Then search Datadog logs for a call_id: the INVITE, the BYE with its loss"
dim "figures, and the trace carrying the transcript all share it."
