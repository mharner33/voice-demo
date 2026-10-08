package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCallLogWritesJSONLines(t *testing.T) {
	var buf bytes.Buffer
	log := NewCallLogTo(&buf)

	for i := 0; i < 3; i++ {
		if err := log.Write(CallRecord{CallID: "c-1", PacketsRx: uint64(i)}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		if rec["call_id"] != "c-1" {
			t.Errorf("line %d call_id = %v", i, rec["call_id"])
		}
	}
}

// TestCallLogUsesDatadogCorrelationKeys pins the attribute names. Datadog's
// log-trace correlation looks for dd.trace_id and dd.span_id specifically;
// anything else and a log line cannot be pivoted to its trace, which is the
// whole reason the call log carries them.
func TestCallLogUsesDatadogCorrelationKeys(t *testing.T) {
	var buf bytes.Buffer
	log := NewCallLogTo(&buf)

	rec := CallRecord{CallID: "c-1"}
	rec.TraceID = "6ac8045200000000791aa3a27f123347"
	rec.SpanID = "8726467146295685959"
	rec.Service = "voicegw"
	rec.Env = "demo"

	if err := log.Write(rec); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]string{
		"dd.trace_id": "6ac8045200000000791aa3a27f123347",
		"dd.span_id":  "8726467146295685959",
		"service":     "voicegw",
		"env":         "demo",
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %q", key, got[key], want)
		}
	}

	// The IDs must be strings. Large trace IDs lose precision if a JSON parser
	// reads them as float64, which would silently break correlation.
	raw := buf.String()
	if !strings.Contains(raw, `"dd.trace_id":"6ac8`) {
		t.Errorf("dd.trace_id was not emitted as a string: %s", raw)
	}
	if !strings.Contains(raw, `"dd.span_id":"8726`) {
		t.Errorf("dd.span_id was not emitted as a string: %s", raw)
	}
}

func TestCallLogDefaultsTimestampAndEvent(t *testing.T) {
	var buf bytes.Buffer
	log := NewCallLogTo(&buf)

	before := time.Now().Add(-time.Second)
	if err := log.Write(CallRecord{CallID: "c-1"}); err != nil {
		t.Fatal(err)
	}

	var got CallRecord
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Event != "call.end" {
		t.Errorf("Event = %q, want the default call.end", got.Event)
	}
	if got.Timestamp.Before(before) {
		t.Errorf("Timestamp = %v, want a recent time", got.Timestamp)
	}

	// An explicit event is preserved.
	buf.Reset()
	if err := log.Write(CallRecord{CallID: "c-1", Event: "call.failed"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Event != "call.failed" {
		t.Errorf("Event = %q, want call.failed", got.Event)
	}
}

// TestCallLogCarriesAllThreeLayers checks that one line holds the network, the
// buffer, and the AI figures together. That is the point of the call log: a
// single record that lets someone see a degraded transcript next to the packet
// loss that caused it.
func TestCallLogCarriesAllThreeLayers(t *testing.T) {
	var buf bytes.Buffer
	log := NewCallLogTo(&buf)

	err := log.Write(CallRecord{
		CallID:         "c-076eb2a3",
		Codec:          "PCMU",
		Profile:        "provider-degraded",
		PacketsRx:      284,
		PacketsLost:    16,
		LossPct:        5.33,
		JitterMs:       29.8,
		MOS:            2.06,
		Played:         310,
		Concealed:      26,
		ConcealPct:     8.39,
		LateDrops:      4,
		TurnCount:      3,
		ToolCalls:      2,
		InputTokens:    96,
		OutputTokens:   86,
		ConcealedInPct: 5.0,
		Turns: []TurnRecord{{
			Index:      0,
			Transcript: "hello I'm calling about my account balance",
			Confidence: 0.87,
			Reply:      "I've pulled up your account.",
			ToolCalls:  []string{"lookup_account"},
			ToolRounds: 2,
			LLMMs:      1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{
		// Network.
		"packets_rx", "packets_lost", "loss_pct", "jitter_ms", "mos",
		// Buffer.
		"frames_played", "frames_concealed", "conceal_pct", "late_drops",
		// AI.
		"turn_count", "tool_calls", "input_tokens", "output_tokens",
		"concealed_in_pct", "turns",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("the call log is missing %q", key)
		}
	}

	turns, ok := got["turns"].([]any)
	if !ok || len(turns) != 1 {
		t.Fatalf("turns = %v, want one entry", got["turns"])
	}
	turn := turns[0].(map[string]any)
	if turn["transcript"] == "" {
		t.Error("the turn carries no transcript")
	}
	if turn["confidence"] != 0.87 {
		t.Errorf("turn confidence = %v, want 0.87", turn["confidence"])
	}
}

func TestCallLogCorrelate(t *testing.T) {
	tt, obsT := startSDK(t)
	_ = tt

	span, _ := obsT.StartAPM(context.Background(), "voice.call", nil)
	defer span.Finish(nil)

	var rec CallRecord
	rec.Correlate(span, obsT)

	if rec.TraceID == "" {
		t.Error("Correlate left the trace ID empty")
	}
	if rec.SpanID == "" {
		t.Error("Correlate left the span ID empty")
	}
	if rec.Service != "voicegw" {
		t.Errorf("Service = %q, want voicegw", rec.Service)
	}
	if rec.Env != "test" {
		t.Errorf("Env = %q, want test", rec.Env)
	}

	// A nil span must not panic, and must leave the fields empty rather than
	// writing a bogus correlation.
	var empty CallRecord
	empty.Correlate(nil, nil)
	if empty.TraceID != "" || empty.SpanID != "" {
		t.Error("Correlate with a nil span invented IDs")
	}
}

func TestNilCallLogDiscards(t *testing.T) {
	var log *CallLog
	if err := log.Write(CallRecord{CallID: "c-1"}); err != nil {
		t.Errorf("Write on a nil CallLog = %v, want nil", err)
	}
	if err := log.Close(); err != nil {
		t.Errorf("Close on a nil CallLog = %v, want nil", err)
	}
}

func TestCallLogToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.jsonl")

	log, err := NewCallLog(path)
	if err != nil {
		t.Fatalf("NewCallLog: %v", err)
	}
	if err := log.Write(CallRecord{CallID: "c-1"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening must append rather than truncate, so a restart does not
	// discard a run's history.
	log2, err := NewCallLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log2.Write(CallRecord{CallID: "c-2"}); err != nil {
		t.Fatal(err)
	}
	log2.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines after reopening, want 2 — the log was truncated", len(lines))
	}
	if !strings.Contains(lines[0], "c-1") || !strings.Contains(lines[1], "c-2") {
		t.Errorf("records are not in append order: %v", lines)
	}
}

func TestCallLogRejectsUnwritablePath(t *testing.T) {
	if _, err := NewCallLog(filepath.Join(t.TempDir(), "nope", "calls.jsonl")); err == nil {
		t.Error("NewCallLog accepted a path in a nonexistent directory")
	}
}

// TestCallLogIsConcurrencySafe matters because many calls end at once under
// load. Meaningful under -race.
func TestCallLogIsConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	log := NewCallLogTo(&buf)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			log.Write(CallRecord{CallID: "c-1", PacketsRx: uint64(i)})
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	// Interleaved writes would produce unparseable lines.
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d was corrupted by concurrent writes: %v", i, err)
		}
	}
}
