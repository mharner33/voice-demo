package obs

import (
	"context"
	"time"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
	"github.com/DataDog/dd-trace-go/v2/llmobs"
)

// Message is one turn of model input or output. It mirrors llmobs.LLMMessage
// so that nothing outside this package has to import the experimental SDK.
type Message struct {
	Role    string
	Content string
	// ToolCalls are the tools a model asked for in this message. Recording
	// them on the message, as well as on a separate tool span, is what makes
	// the request visible in the LLM span's own output.
	ToolCalls []ToolCall
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID   string
	Name string
	Args []byte
}

// ToolDefinition describes a capability offered to a model. Recording the
// definitions on the LLM span — not just the calls that resulted — is what makes
// a turn where the model declined to use a tool interpretable: without them,
// "no tool call" and "no tool offered" look identical in the trace.
type ToolDefinition struct {
	Name        string
	Description string
	Version     string
	// Schema is the JSON Schema for the tool's arguments.
	Schema []byte
}

// Metrics are the numeric attributes attached to an LLM span.
//
// These are span attributes, not platform metrics. Datadog derives platform
// metrics from the recognized keys (ml_obs.span.llm.input.tokens and friends),
// which is precisely why this project does not also emit token counts over
// DogStatsD — doing so would double count.
type Metrics struct {
	InputTokens  int
	OutputTokens int

	// InputCostUSD and OutputCostUSD are the dollar cost of this call. The SDK
	// has no constant for either: input_cost and output_cost are custom metric
	// keys that Datadog recognizes, so they are spelled literally below.
	//
	// Cost is computed here rather than derived from tokens in a dashboard
	// because only the provider knows its own price list, and a mock provider
	// legitimately costs nothing.
	InputCostUSD  float64
	OutputCostUSD float64

	// TimeToFirstToken is the delay before the first output appeared. The SDK
	// expects seconds, so the conversion happens here rather than at every
	// call site.
	TimeToFirstToken time.Duration

	// BillableCharacters is the right cost proxy for speech services, which
	// bill by audio duration or character count rather than by token.
	BillableCharacters int
}

// toLLMObs converts to the SDK's map form, omitting anything unset so that a
// zero value does not report a misleading zero.
func (m Metrics) toLLMObs() map[string]float64 {
	out := make(map[string]float64, 5)
	if m.InputTokens > 0 {
		out[llmobs.MetricKeyInputTokens] = float64(m.InputTokens)
	}
	if m.OutputTokens > 0 {
		out[llmobs.MetricKeyOutputTokens] = float64(m.OutputTokens)
	}
	if m.InputTokens > 0 || m.OutputTokens > 0 {
		out[llmobs.MetricKeyTotalTokens] = float64(m.InputTokens + m.OutputTokens)
	}
	if m.TimeToFirstToken > 0 {
		out[llmobs.MetricKeyTimeToFirstToken] = m.TimeToFirstToken.Seconds()
	}
	if m.BillableCharacters > 0 {
		out[llmobs.MetricKeyBillableCharacterCount] = float64(m.BillableCharacters)
	}
	if m.InputCostUSD > 0 {
		out[metricKeyInputCost] = m.InputCostUSD
	}
	if m.OutputCostUSD > 0 {
		out[metricKeyOutputCost] = m.OutputCostUSD
	}
	return out
}

// Cost metric keys. These have no SDK constants — Datadog accepts them as
// custom metrics on an LLM span and renders them in the cost views.
const (
	metricKeyInputCost  = "input_cost"
	metricKeyOutputCost = "output_cost"
)

func toToolDefinitions(defs []ToolDefinition) []llmobs.ToolDefinition {
	if len(defs) == 0 {
		return nil
	}
	out := make([]llmobs.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		out = append(out, llmobs.ToolDefinition{
			Name:        d.Name,
			Description: d.Description,
			ToolVersion: d.Version,
			Schema:      d.Schema,
		})
	}
	return out
}

func toLLMMessages(msgs []Message) []llmobs.LLMMessage {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]llmobs.LLMMessage, 0, len(msgs))
	for _, m := range msgs {
		lm := llmobs.LLMMessage{Role: m.Role, Content: m.Content}
		for _, tc := range m.ToolCalls {
			lm.ToolCalls = append(lm.ToolCalls, llmobs.ToolCall{
				ToolID:    tc.ID,
				Name:      tc.Name,
				Arguments: tc.Args,
				Type:      "function",
			})
		}
		out = append(out, lm)
	}
	return out
}

// Span is one unit of work. It may carry an APM span, an Agent Observability
// span, or both. Every method tolerates a nil receiver so the pipeline can be
// instrumented without branching.
type Span struct {
	apm *tracer.Span

	// obs is the LLM Obs span, held through the SDK's common interface.
	obs llmobs.Span
	// textIO and llmIO hold the annotation method appropriate to this span's
	// kind, since the SDK exposes a different one per kind.
	textIO func(input, output string, opts ...llmobs.AnnotateOption)
	llmIO  func(input, output []llmobs.LLMMessage, opts ...llmobs.AnnotateOption)

	tags     map[string]string
	toolDefs []ToolDefinition
}

// StartAPM opens a plain APM span. The transport stages use this: packet loss
// and buffer depth belong in APM, where the rest of the service's latency
// lives, not in the Agent Observability views.
func (t *Tracer) StartAPM(ctx context.Context, name string, tags map[string]string) (*Span, context.Context) {
	if t == nil || !t.cfg.Enabled {
		return nil, ctx
	}
	opts := make([]tracer.StartSpanOption, 0, len(tags)+1)
	opts = append(opts, tracer.ServiceName(t.cfg.Service))
	for k, v := range tags {
		opts = append(opts, tracer.Tag(k, v))
	}
	span, ctx := tracer.StartSpanFromContext(ctx, name, opts...)
	return &Span{apm: span, tags: tags}, ctx
}

// StartAPMAt opens an APM span with an explicit start time.
//
// The transport stages need this. Packet ingest and jitter buffering run
// continuously for the whole call rather than as a nested call stack, so their
// spans can only be created once the call ends, backdated to cover it.
func (t *Tracer) StartAPMAt(ctx context.Context, name string, start time.Time, tags map[string]string) (*Span, context.Context) {
	if t == nil || !t.cfg.Enabled {
		return nil, ctx
	}
	opts := make([]tracer.StartSpanOption, 0, len(tags)+2)
	opts = append(opts, tracer.ServiceName(t.cfg.Service), tracer.StartTime(start))
	for k, v := range tags {
		opts = append(opts, tracer.Tag(k, v))
	}
	span, ctx := tracer.StartSpanFromContext(ctx, name, opts...)
	return &Span{apm: span, tags: tags}, ctx
}

// StartLLMAt opens an LLM span with an explicit start time.
//
// Speech recognition needs this: an utterance is only known to have ended when
// its final transcript arrives, so its span is created at that moment and
// backdated to when the utterance began. Without the backdate every recognition
// span would show a duration of nearly zero.
func (t *Tracer) StartLLMAt(ctx context.Context, name, model, provider string, start time.Time) (*Span, context.Context) {
	if !t.LLMObsEnabled() {
		return nil, ctx
	}
	s, ctx := llmobs.StartLLMSpan(ctx, name,
		llmobs.WithModelName(model),
		llmobs.WithModelProvider(provider),
		llmobs.WithStartTime(start),
	)
	return &Span{obs: s, llmIO: s.AnnotateLLMIO}, ctx
}

// StartWorkflow opens the call-level Agent Observability workflow span.
//
// The session ID is set here and nowhere else: the SDK propagates it down the
// context to every child span, which is what groups a whole call in the LLM
// Obs session view.
func (t *Tracer) StartWorkflow(ctx context.Context, name, sessionID string) (*Span, context.Context) {
	if !t.LLMObsEnabled() {
		return nil, ctx
	}
	s, ctx := llmobs.StartWorkflowSpan(ctx, name, llmobs.WithSessionID(sessionID))
	return &Span{obs: s, textIO: s.AnnotateTextIO}, ctx
}

// StartAgent opens an agent span. Its children are attributed to it
// automatically, which is what produces the agent view's tool breakdown.
func (t *Tracer) StartAgent(ctx context.Context, name string) (*Span, context.Context) {
	if !t.LLMObsEnabled() {
		return nil, ctx
	}
	s, ctx := llmobs.StartAgentSpan(ctx, name)
	return &Span{obs: s, textIO: s.AnnotateTextIO}, ctx
}

// StartTool opens a tool span.
func (t *Tracer) StartTool(ctx context.Context, name string) (*Span, context.Context) {
	if !t.LLMObsEnabled() {
		return nil, ctx
	}
	s, ctx := llmobs.StartToolSpan(ctx, name)
	return &Span{obs: s, textIO: s.AnnotateTextIO}, ctx
}

// StartLLM opens an LLM span for a model call.
//
// Speech recognition and synthesis are modeled as LLM spans even though they
// are not language models: they are the inference calls on this path, and the
// LLM span is the only kind that carries a model name, a provider, and
// per-call metrics. Their cost is reported as billable characters rather than
// tokens, which is how speech services actually bill.
func (t *Tracer) StartLLM(ctx context.Context, name, model, provider string) (*Span, context.Context) {
	if !t.LLMObsEnabled() {
		return nil, ctx
	}
	s, ctx := llmobs.StartLLMSpan(ctx, name,
		llmobs.WithModelName(model),
		llmobs.WithModelProvider(provider),
	)
	return &Span{obs: s, llmIO: s.AnnotateLLMIO}, ctx
}

// TextIO annotates a workflow, agent, tool, or task span.
func (s *Span) TextIO(input, output string, metadata map[string]any) {
	if s == nil || s.textIO == nil {
		return
	}
	s.textIO(input, output, s.annotateOpts(metadata, Metrics{})...)
}

// LLMIO annotates an LLM span with its messages, metrics, and metadata.
func (s *Span) LLMIO(input, output []Message, m Metrics, metadata map[string]any) {
	if s == nil || s.llmIO == nil {
		return
	}
	s.llmIO(toLLMMessages(input), toLLMMessages(output), s.annotateOpts(metadata, m)...)
}

func (s *Span) annotateOpts(metadata map[string]any, m Metrics) []llmobs.AnnotateOption {
	var opts []llmobs.AnnotateOption
	if len(metadata) > 0 {
		opts = append(opts, llmobs.WithAnnotatedMetadata(metadata))
	}
	if metrics := m.toLLMObs(); len(metrics) > 0 {
		opts = append(opts, llmobs.WithAnnotatedMetrics(metrics))
	}
	if len(s.tags) > 0 {
		opts = append(opts, llmobs.WithAnnotatedTags(s.tags))
	}
	if defs := toToolDefinitions(s.toolDefs); len(defs) > 0 {
		opts = append(opts, llmobs.WithAnnotatedToolDefinitions(defs))
	}
	return opts
}

// SetToolDefinitions records the tools offered on this span. They are applied
// on the next annotation, which is why this is called before LLMIO rather than
// after.
func (s *Span) SetToolDefinitions(defs []ToolDefinition) {
	if s == nil {
		return
	}
	s.toolDefs = defs
}

// SetTags attaches tags that will be applied on the next annotation, and to
// the APM span immediately.
func (s *Span) SetTags(tags map[string]string) {
	if s == nil {
		return
	}
	if s.tags == nil {
		s.tags = make(map[string]string, len(tags))
	}
	for k, v := range tags {
		s.tags[k] = v
		if s.apm != nil {
			s.apm.SetTag(k, v)
		}
	}
}

// SetAPMTag sets one tag on the APM span only, for numeric values that do not
// belong in the string-keyed LLM Obs tag set.
func (s *Span) SetAPMTag(key string, value any) {
	if s == nil || s.apm == nil {
		return
	}
	s.apm.SetTag(key, value)
}

// Finish closes the span, marking it errored when err is non-nil. Passing nil
// is safe, which is what allows the single-defer pattern at call sites:
//
//	span, ctx := t.StartLLM(ctx, ...)
//	defer func() { span.Finish(err) }()
//
// The nil guard below is not redundant, despite the SDK documentation stating
// that llmobs.WithError(nil) is a no-op. In dd-trace-go v2.11.1 it is not:
// WithError calls errortrace.WrapN, which correctly returns a nil
// *TracerError, but assigning that typed nil to the config's error-typed field
// leaves the interface non-nil. Finish then calls Error() on it and
// dereferences the nil inner error, panicking. The documented single-defer
// pattern therefore crashes on every *successful* span.
//
// The APM side has no such problem — tracer.WithError assigns the error
// directly, so a nil stays nil — but both are guarded here for symmetry.
//
// This is exactly why every SDK call in this project lives in one package: the
// workaround is three lines in one place rather than a trap at every call site.
func (s *Span) Finish(err error) {
	if s == nil {
		return
	}
	if s.obs != nil {
		if err != nil {
			s.obs.Finish(llmobs.WithError(err))
		} else {
			s.obs.Finish()
		}
	}
	if s.apm != nil {
		if err != nil {
			s.apm.Finish(tracer.WithError(err))
		} else {
			s.apm.Finish()
		}
	}
}

// APMTraceID returns the APM trace this span belongs to, as a hex string. It
// is what links an Agent Observability span back to the APM trace, and what
// the call log records so a log line can be pivoted to its trace.
func (s *Span) APMTraceID() string {
	if s == nil {
		return ""
	}
	if s.obs != nil {
		return s.obs.APMTraceID()
	}
	if s.apm != nil {
		return s.apm.Context().TraceID()
	}
	return ""
}

// APMSpanID returns the APM span ID, for log correlation.
func (s *Span) APMSpanID() uint64 {
	if s == nil || s.apm == nil {
		return 0
	}
	return s.apm.Context().SpanID()
}
