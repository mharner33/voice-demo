package obs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// The call log is the third leg of the demo, alongside traces and metrics: one
// JSON line per call carrying the network, buffer, and AI figures together with
// the trace IDs that pivot to the matching trace.
//
// The dd.trace_id and dd.span_id attribute names are Datadog's reserved keys
// for log-trace correlation. The trace ID is the 128-bit hex form the SDK
// reports; the span ID is decimal, which is what the agent expects.

// TurnRecord is one agent turn in the call log.
type TurnRecord struct {
	Index        int      `json:"index"`
	Transcript   string   `json:"transcript"`
	Confidence   float64  `json:"confidence"`
	Reply        string   `json:"reply,omitempty"`
	ToolCalls    []string `json:"tool_calls,omitempty"`
	ToolRounds   int      `json:"tool_rounds,omitempty"`
	InputTokens  int      `json:"input_tokens,omitempty"`
	OutputTokens int      `json:"output_tokens,omitempty"`
	FinalMs      int64    `json:"final_ms"`
	LLMMs        int64    `json:"llm_ms"`
	TTSFirstMs   int64    `json:"tts_first_byte_ms"`
	TTSAudioMs   int64    `json:"tts_audio_ms"`
	Error        string   `json:"error,omitempty"`
}

// CallRecord is one line of the call log.
type CallRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Event     string    `json:"event"`
	CallID    string    `json:"call_id"`
	SSRC      string    `json:"ssrc,omitempty"`
	Codec     string    `json:"codec,omitempty"`
	// Profile is the gateway's provider-impairment profile. The client's
	// network profile is not knowable from the stream; see CallTags.
	Profile string `json:"provider_profile,omitempty"`

	// Network, as measured from what arrived.
	PacketsRx   uint64  `json:"packets_rx"`
	PacketsLost uint64  `json:"packets_lost"`
	LossPct     float64 `json:"loss_pct"`
	Reordered   uint64  `json:"reordered"`
	Duplicated  uint64  `json:"duplicated"`
	JitterMs    float64 `json:"jitter_ms"`
	MOS         float64 `json:"mos"`
	DurationMs  int64   `json:"duration_ms"`

	// Jitter buffer.
	Played        uint64  `json:"frames_played"`
	Concealed     uint64  `json:"frames_concealed"`
	ConcealPct    float64 `json:"conceal_pct"`
	LateDrops     uint64  `json:"late_drops"`
	Evicted       uint64  `json:"evicted"`
	Underruns     uint64  `json:"underruns"`
	TargetDepthMs float64 `json:"target_depth_ms"`

	// AI pipeline.
	Turns          []TurnRecord `json:"turns,omitempty"`
	TurnCount      int          `json:"turn_count"`
	ToolCalls      int          `json:"tool_calls"`
	PipelineErrors int          `json:"pipeline_errors"`
	InputTokens    int          `json:"input_tokens"`
	OutputTokens   int          `json:"output_tokens"`
	FirstPartialMs int64        `json:"first_partial_ms,omitempty"`
	ConcealedInPct float64      `json:"concealed_in_pct"`
	FramesOut      int          `json:"frames_out"`
	DroppedFrames  uint64       `json:"pipeline_dropped"`

	// Providers, so a log search can separate mock runs from real ones.
	STTProvider string `json:"stt_provider,omitempty"`
	LLMProvider string `json:"llm_provider,omitempty"`
	TTSProvider string `json:"tts_provider,omitempty"`

	// Error is set when the call itself failed, as opposed to one turn.
	Error string `json:"error,omitempty"`

	// Datadog log-trace correlation.
	TraceID string `json:"dd.trace_id,omitempty"`
	SpanID  string `json:"dd.span_id,omitempty"`
	Service string `json:"service,omitempty"`
	Env     string `json:"env,omitempty"`
}

// CallLog writes call records as JSON lines. A nil *CallLog discards writes, so
// the call site needs no branch.
type CallLog struct {
	mu     sync.Mutex
	enc    *json.Encoder
	closer io.Closer
}

// NewCallLog opens a call log. An empty path or "-" writes to stdout, which is
// what the container does so the agent picks the lines up from the container
// log stream rather than needing a mounted file.
func NewCallLog(path string) (*CallLog, error) {
	if path == "" || path == "-" {
		return &CallLog{enc: json.NewEncoder(os.Stdout)}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("obs: opening the call log %s: %w", path, err)
	}
	return &CallLog{enc: json.NewEncoder(f), closer: f}, nil
}

// NewCallLogTo writes to an arbitrary destination, for tests.
func NewCallLogTo(w io.Writer) *CallLog {
	return &CallLog{enc: json.NewEncoder(w)}
}

// Write appends one record.
func (l *CallLog) Write(rec CallRecord) error {
	if l == nil || l.enc == nil {
		return nil
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	if rec.Event == "" {
		rec.Event = "call.end"
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.enc.Encode(rec); err != nil {
		return fmt.Errorf("obs: writing the call log: %w", err)
	}
	return nil
}

// Close releases the underlying file, if any.
func (l *CallLog) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closer.Close()
}

// Correlate fills in the trace and service attributes that let a log line be
// pivoted to its trace. It takes the span rather than raw IDs so that the
// correlation can never be filled in with the wrong format.
func (r *CallRecord) Correlate(span *Span, t *Tracer) {
	if span != nil {
		r.TraceID = span.APMTraceID()
		if id := span.APMSpanID(); id != 0 {
			r.SpanID = strconv.FormatUint(id, 10)
		}
	}
	if t != nil {
		r.Service = t.cfg.Service
		r.Env = t.cfg.Env
	}
}
