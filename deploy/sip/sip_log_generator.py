#!/usr/bin/env python3
"""Emit SIP signaling logs for calls the gateway has already handled.

This is the adapted form of the standalone SIP log generator from the
rum-install repo. That one invented random calls to exercise Datadog's
multiline log aggregation; this one reads voicegw's call log and writes the
signaling that *those* calls would have produced, carrying the same call_id in
the SIP Call-ID header.

That single shared identifier is the whole point. It gives the demo signaling,
media and AI in one log view — a search for a call_id finds the INVITE, the
BYE, the per-call media record with its loss and jitter, and the trace that
holds the transcript and the agent's reply — without this project having to
implement SIP, which decision 3 in docs/plan.md deliberately leaves out of
scope.

What it is not: a SIP stack, or evidence that one works. The signaling is
derived after the fact from a call that really happened over the gRPC control
plane. The timings, the parties and the outcome are real; the protocol text
around them is a faithful reconstruction.

Usage:
    # one pass over an existing call log
    ./sip_log_generator.py --call-log /var/log/voicegw/calls.jsonl

    # follow the gateway's log as a demo runs, which is the normal mode
    voicegw -call-log calls.jsonl &
    ./sip_log_generator.py --call-log calls.jsonl --follow

    # or pipe the gateway's stdout straight in
    voicegw | ./sip_log_generator.py --call-log - --out /tmp/sip_signaling.log

Standard library only: this repo adds no dependency it does not need, and the
original's use of faker bought nothing that mattered here.
"""

import argparse
import json
import os
import random
import sys
import time
from datetime import datetime, timedelta, timezone

# The SIP proxy and media gateway this reconstruction speaks as. They are
# cosmetic — but stable, because a demo that showed a different proxy IP on
# every call would invite questions about the wrong thing.
PROXY_IP = "10.20.0.11"
PROXY_PORT = 5060
GATEWAY_IP = "10.20.0.40"
GATEWAY_PORT = 5060
USER_AGENT = "voice-demo-gw/0.1 (reconstructed from the gRPC control plane)"
TRANSPORT = "UDP"

ALLOW = "INVITE,ACK,BYE,CANCEL,OPTIONS,PRACK,UPDATE,INFO"

# G.711 u-law at 20 ms, which is what the media path actually carries.
SDP_PTIME_MS = 20


def ts(dt):
    """Format a timestamp the way the original generator did, so an existing
    Datadog multiline rule keyed on [Time:...] still applies."""
    return dt.strftime("%m-%d@%H:%M:%S.") + f"{dt.microsecond // 1000:03d}"


def sip_uri(number, host):
    """A phone number as a SIP URI. The gateway's From/To are E.164-ish
    already, which is what a real deployment would carry."""
    if not number:
        return f"sip:anonymous@{host}"
    return f"sip:{number.lstrip('+')}@{host}"


def tag(rng, width=10):
    return "".join(rng.choice("0123456789abcdef") for _ in range(width))


def branch(rng):
    # RFC 3261 requires the magic cookie prefix on every Via branch.
    return "z9hG4bK" + tag(rng, 16)


def sdp(call, host, port, session_id):
    """The offer or answer. PCMU and PCMA are the two codecs this demo
    supports, and the call log says which one was negotiated, so the
    reconstruction advertises the real one rather than a plausible list."""
    codec = call.get("codec", "PCMU")
    payload = 8 if codec == "PCMA" else 0
    return [
        "v=0",
        f"o=- {session_id} {session_id} IN IP4 {host}",
        "s=voice-demo",
        f"c=IN IP4 {host}",
        "t=0 0",
        f"m=audio {port} RTP/AVP {payload} 101",
        f"a=rtpmap:{payload} {codec}/8000",
        "a=rtpmap:101 telephone-event/8000",
        f"a=ptime:{SDP_PTIME_MS}",
        "a=sendrecv",
    ]


def headers(call, rng, method, cseq, from_tag, to_tag, caller_host, body=None):
    """One SIP message's headers, in the order a real stack emits them, ending
    with Via — which is the marker the original generator's multiline rule uses
    to find the end of a record."""
    call_id = f"{call['call_id']}@{caller_host}"
    lines = [
        f"{method} {sip_uri(call.get('to'), GATEWAY_IP)} SIP/2.0"
        if not method.startswith("SIP/2.0")
        else method,
        f"Call-ID: {call_id}",
        f"CSeq: {cseq} {method.split()[0] if not method.startswith('SIP/2.0') else 'INVITE'}",
        f"From: <{sip_uri(call.get('from'), caller_host)}>;tag={from_tag}",
        f"To: <{sip_uri(call.get('to'), GATEWAY_IP)}>"
        + (f";tag={to_tag}" if to_tag else ""),
        f"Contact: <sip:{GATEWAY_IP}:{GATEWAY_PORT};transport={TRANSPORT.lower()}>",
        f"User-Agent: {USER_AGENT}",
        f"Allow: {ALLOW}",
        f"Max-Forwards: {rng.randint(60, 70)}",
    ]
    if body:
        lines.append("Content-Type: application/sdp")
        lines.append(f"Content-Length: {sum(len(b) + 2 for b in body)}")
    else:
        lines.append("Content-Length: 0")
    lines.append(
        f"Via: SIP/2.0/{TRANSPORT} {PROXY_IP}:{PROXY_PORT};branch={branch(rng)};received={PROXY_IP}"
    )
    return lines


def record(call, dt, direction, first_line, rng, cseq, from_tag, to_tag,
           caller_host, body=None, note=None):
    """One complete log record: a timestamp, the message, and the internal
    state lines a gateway writes around it."""
    out = [f"[Time:{ts(dt)}]"]
    out.extend(
        headers(call, rng, first_line, cseq, from_tag, to_tag, caller_host, body)
    )
    if body:
        out.extend(body)

    arrow = "Incoming" if direction == "in" else "Outgoing"
    out.append(
        f"<157> ---- {arrow} SIP Message {'from' if direction == 'in' else 'to'} "
        f"{PROXY_IP}:{PROXY_PORT} ---- call_id={call['call_id']} [Time:{ts(dt)}]"
    )
    if note:
        out.append(f"<157> {note} call_id={call['call_id']} [Time:{ts(dt)}]")
    return out


def dialog(call):
    """The records for one call: setup, the media window, and teardown.

    Every timestamp is derived from the call's own timing — its start, its
    measured duration — so the signaling timeline lines up with the media
    timeline on a Datadog log graph rather than merely coexisting with it.
    """
    # Seeded per call, so re-running over the same log produces identical
    # output. A demo that showed different tags for the same call on a second
    # pass would be unexplainable.
    rng = random.Random(call["call_id"])

    start = parse_time(call.get("timestamp"))
    duration_ms = int(call.get("duration_ms") or 0)
    # The call log is written at teardown, so its timestamp is the *end*.
    start = start - timedelta(milliseconds=duration_ms)

    from_tag = tag(rng)
    to_tag = tag(rng)
    caller_host = f"198.51.100.{rng.randint(10, 200)}"
    session_id = rng.randint(10**9, 10**10 - 1)
    caller_rtp = rng.randrange(20000, 40000, 2)

    failed = call.get("event") == "call.failed"
    unsignaled = not call.get("signaled", True)

    records = []

    # INVITE, with the caller's offer.
    records.append(
        record(call, start, "in", "INVITE", rng, 1, from_tag, None, caller_host,
               body=sdp(call, caller_host, caller_rtp, session_id),
               note=f"network_profile={call.get('network_profile') or 'unknown'}")
    )
    records.append(
        record(call, start + timedelta(milliseconds=8), "out", "SIP/2.0 100 Trying",
               rng, 1, from_tag, to_tag, caller_host)
    )
    records.append(
        record(call, start + timedelta(milliseconds=22), "out", "SIP/2.0 180 Ringing",
               rng, 1, from_tag, to_tag, caller_host)
    )

    if unsignaled:
        # An unsignaled call never set up through the control plane, so there
        # is no dialog to reconstruct past this point. Saying so is more useful
        # than inventing a 200 OK that never happened.
        records.append(
            record(call, start + timedelta(milliseconds=30), "out",
                   "SIP/2.0 481 Call/Transaction Does Not Exist",
                   rng, 1, from_tag, to_tag, caller_host,
                   note="media arrived for an SSRC that never signaled; "
                        "no trailing-loss accounting is possible")
        )
        return records

    answer_at = start + timedelta(milliseconds=40)
    records.append(
        record(call, answer_at, "out", "SIP/2.0 200 OK", rng, 1, from_tag, to_tag,
               caller_host,
               body=sdp(call, GATEWAY_IP, 5004, session_id))
    )
    records.append(
        record(call, answer_at + timedelta(milliseconds=12), "in", "ACK", rng, 1,
               from_tag, to_tag, caller_host,
               note="media path established")
    )

    # Teardown. The BYE comes from whichever side ended the call, which the
    # gateway's end_reason records: "bye" is the caller hanging up, anything
    # else is the gateway giving up on a stream that went quiet.
    end_at = start + timedelta(milliseconds=duration_ms)
    reason = call.get("end_reason") or "bye"
    caller_hung_up = reason in ("bye", "normal")

    status = "200 OK"
    bye_note = (
        f"packets_rx={call.get('packets_rx', 0)} "
        f"packets_lost={call.get('packets_lost', 0)} "
        f"loss_pct={call.get('loss_pct', 0):.2f} "
        f"mos={call.get('mos', 0):.2f} end_reason={reason}"
    )
    if failed:
        bye_note += f" error={call.get('error', 'unknown')}"

    records.append(
        record(call, end_at, "in" if caller_hung_up else "out", "BYE", rng, 2,
               from_tag, to_tag, caller_host, note=bye_note)
    )
    records.append(
        record(call, end_at + timedelta(milliseconds=6),
               "out" if caller_hung_up else "in", f"SIP/2.0 {status}", rng, 2,
               from_tag, to_tag, caller_host)
    )
    return records


def parse_time(value):
    """Parse the call log's RFC 3339 timestamp, falling back to now.

    A record with no usable timestamp still gets signaling, placed at the
    current time: dropping the call entirely would make the signaling view
    quietly incomplete, which is worse than a slightly misplaced dialog.
    """
    if not value:
        return datetime.now(timezone.utc)
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return datetime.now(timezone.utc)


def emit(records, out):
    for lines in records:
        for line in lines:
            out.write(line + "\n")
        out.flush()


def is_call_record(obj):
    """Only finished calls produce signaling. The gateway also writes live
    progress lines, and a dialog per progress report would multiply every
    call's signaling by however often the reporter ran."""
    return isinstance(obj, dict) and obj.get("call_id") and obj.get("event") in (
        "call.end",
        "call.failed",
    )


def read_lines(path, follow, poll=0.25):
    """Yield lines from the call log, optionally following it.

    Following re-opens the file if it is truncated or rotated, because a demo
    run usually starts the gateway and this script at the same moment and the
    file may not exist yet.
    """
    if path == "-":
        for line in sys.stdin:
            yield line
        return

    while not os.path.exists(path):
        if not follow:
            raise FileNotFoundError(path)
        time.sleep(poll)

    with open(path, "r") as f:
        while True:
            line = f.readline()
            if line:
                yield line
                continue
            if not follow:
                return
            # A truncated file means the gateway restarted; start over rather
            # than waiting at an offset past the end of the new file.
            if f.tell() > os.path.getsize(path):
                f.seek(0)
            time.sleep(poll)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--call-log", default="-",
                    help='voicegw call log to read; "-" is stdin')
    ap.add_argument("--out", default="/tmp/sip_signaling.log",
                    help='where to write the SIP records; "-" is stdout')
    ap.add_argument("--follow", action="store_true",
                    help="keep reading as the gateway appends, for a live demo")
    ap.add_argument("--echo-calls", action="store_true",
                    help="also copy the call-log lines through to stdout, "
                         "for use in a pipeline")
    args = ap.parse_args()

    out = sys.stdout if args.out == "-" else open(args.out, "a", buffering=1)
    calls = 0

    try:
        for line in read_lines(args.call_log, args.follow):
            if args.echo_calls:
                sys.stdout.write(line)
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                # The gateway's stdout carries its own log lines too; anything
                # that is not JSON is not a call record.
                continue
            if not is_call_record(obj):
                continue

            emit(dialog(obj), out)
            calls += 1
            if out is not sys.stdout:
                print(f"signaling written for call {obj['call_id']} "
                      f"({calls} total)", file=sys.stderr)
    except KeyboardInterrupt:
        pass
    finally:
        if out is not sys.stdout:
            out.close()

    print(f"{calls} calls reconstructed", file=sys.stderr)


if __name__ == "__main__":
    main()
