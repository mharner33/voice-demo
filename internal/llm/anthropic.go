package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultAnthropicModel is the model the demo calls.
const DefaultAnthropicModel = string(anthropic.ModelClaudeOpus5)

// DefaultSystemPrompt shapes the agent for a voice channel.
//
// The length instruction is the load-bearing part. Every reply here is spoken
// aloud by a synthesizer at roughly 150 words per minute, so a paragraph that
// reads fine on screen is fifteen seconds of a caller waiting without being
// able to interrupt. Text length is latency on this channel.
const DefaultSystemPrompt = `You are a voice agent for a bank's customer service line. You are speaking to the caller on the telephone, and your replies are read aloud.

Keep every reply to one or two short sentences. Speak in plain words a listener can follow the first time: no lists, no headings, no markdown, no symbols, and no figures the ear cannot parse. Write numbers the way you would say them.

The transcript you receive comes from automatic speech recognition over a telephone line, so it may be garbled or may have dropped words. If you cannot tell what the caller asked, say so briefly and ask them to repeat it rather than guessing.

When you need account details, call the lookup_account tool rather than inventing them. Never state a balance, a name, or an account status that a tool has not given you.`

// Voice replies are short, but adaptive thinking shares the output budget with
// the spoken text on this model family, so the ceiling has to leave room for
// reasoning that is never read aloud. Sized for headroom rather than economy:
// hitting the cap truncates the reply mid-sentence, which the caller hears.
const defaultAnthropicMaxTokens = 4096

// defaultAnthropicEffort keeps reasoning shallow.
//
// Low effort is the right default for a phone call: the task is a short
// customer-service exchange, and the caller is sitting in silence for every
// token spent thinking. It is also why thinking is left *on* — see
// AnthropicConfig.Effort.
const defaultAnthropicEffort = string(anthropic.OutputConfigEffortLow)

// defaultHistoryTurns bounds how much of the conversation is replayed. A call
// that ran long would otherwise grow its own prompt without limit.
const defaultHistoryTurns = 8

// conversationTTL is how long an inactive call's history is kept. The provider
// cannot see call teardown, so abandoned conversations are reaped on age.
const conversationTTL = 30 * time.Minute

// RefusalReply is spoken when the model declines to answer.
//
// A refusal arrives as a successful response with no content, so without this
// the caller would hear silence — the one failure mode a phone line must never
// have. Saying something and handing off is strictly better than a dead line.
const RefusalReply = "I'm not able to help with that on this line. Let me pass you to a colleague."

// anthropicPricing is the published price list in USD per million tokens.
// Keyed by model so that changing the model does not silently keep the old
// model's cost figures.
var anthropicPricing = map[string]Pricing{
	string(anthropic.ModelClaudeOpus5):    {InputPerMTok: 5, OutputPerMTok: 25},
	string(anthropic.ModelClaudeOpus4_8):  {InputPerMTok: 5, OutputPerMTok: 25},
	string(anthropic.ModelClaudeSonnet5):  {InputPerMTok: 3, OutputPerMTok: 15},
	string(anthropic.ModelClaudeHaiku4_5): {InputPerMTok: 1, OutputPerMTok: 5},
}

// AnthropicConfig parameterizes the real agent.
type AnthropicConfig struct {
	// APIKey authenticates to the API. When empty the SDK resolves credentials
	// from the environment itself, which is the preferred path: it means a key
	// never has to be passed through this program's flags or logs.
	APIKey string

	// Model defaults to DefaultAnthropicModel.
	Model string

	// System defaults to DefaultSystemPrompt.
	System string

	// MaxTokens bounds the response, thinking included.
	MaxTokens int

	// Effort controls reasoning depth: low, medium, high, xhigh, or max.
	//
	// Thinking itself is deliberately left enabled at low effort rather than
	// disabled. Disabling it on this model family has a failure mode that would
	// be invisible here and ruinous: the model occasionally writes a tool call
	// into its spoken text instead of emitting a real tool call, so the turn
	// "succeeds", the lookup never runs, and the caller is read a balance that
	// was never fetched. Low effort buys most of the latency saving without
	// that risk.
	Effort string

	// MaxHistoryTurns bounds the replayed conversation.
	MaxHistoryTurns int

	// Pricing overrides the built-in price list for the chosen model.
	Pricing Pricing

	// RequestOptions are passed to the client, for tests that need to point it
	// at a local server.
	RequestOptions []option.RequestOption
}

func (c *AnthropicConfig) applyDefaults() {
	if c.Model == "" {
		c.Model = DefaultAnthropicModel
	}
	if c.System == "" {
		c.System = DefaultSystemPrompt
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = defaultAnthropicMaxTokens
	}
	if c.Effort == "" {
		c.Effort = defaultAnthropicEffort
	}
	if c.MaxHistoryTurns == 0 {
		c.MaxHistoryTurns = defaultHistoryTurns
	}
	if c.Pricing.Free() {
		// An unknown model leaves pricing at zero rather than guessing. A
		// missing cost figure is recoverable; a wrong one quietly misinforms
		// every cost dashboard downstream.
		c.Pricing = anthropicPricing[c.Model]
	}
}

// Validate rejects a configuration that cannot work.
func (c AnthropicConfig) Validate() error {
	if c.MaxTokens < 1 {
		return fmt.Errorf("llm: MaxTokens = %d, want >= 1", c.MaxTokens)
	}
	if c.MaxHistoryTurns < 1 {
		return fmt.Errorf("llm: MaxHistoryTurns = %d, want >= 1", c.MaxHistoryTurns)
	}
	switch c.Effort {
	case string(anthropic.OutputConfigEffortLow),
		string(anthropic.OutputConfigEffortMedium),
		string(anthropic.OutputConfigEffortHigh),
		string(anthropic.OutputConfigEffortXhigh),
		string(anthropic.OutputConfigEffortMax):
	default:
		return fmt.Errorf("llm: Effort = %q, want low, medium, high, xhigh, or max", c.Effort)
	}
	return nil
}

// conversation is one call's message history.
type conversation struct {
	messages []anthropic.MessageParam
	lastUsed time.Time
}

// Anthropic is an Agent backed by the Claude API.
//
// It is stateful where the Agent interface is not. The interface takes one
// transcript at a time, but the API is given the whole conversation on every
// request, and a turn that calls a tool must replay the assistant message that
// requested it — tool results are matched to their calls by ID. So history is
// kept here, keyed by call ID. The side benefit is a coherent agent: the caller
// can say "what about the other one" and be understood.
type Anthropic struct {
	cfg    AnthropicConfig
	client anthropic.Client

	mu    sync.Mutex
	calls map[string]*conversation
}

// NewAnthropic creates an agent backed by the real API.
func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	opts := make([]option.RequestOption, 0, len(cfg.RequestOptions)+1)
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	opts = append(opts, cfg.RequestOptions...)

	return &Anthropic{
		cfg:    cfg,
		client: anthropic.NewClient(opts...),
		calls:  make(map[string]*conversation),
	}, nil
}

// Info identifies the provider, including its price list so the pipeline can
// report what the call cost.
func (a *Anthropic) Info() Info {
	return Info{Provider: "anthropic", Model: a.cfg.Model, Pricing: a.cfg.Pricing}
}

// Forget drops a call's history. The gateway calls it at teardown so a finished
// call's context is released immediately rather than waiting to age out.
func (a *Anthropic) Forget(callID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.calls, callID)
}

// Reply sends one turn to the model.
func (a *Anthropic) Reply(ctx context.Context, req Request) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}

	// Appending the caller's turn and reading back the history is done under
	// the lock; the API call is not, since it takes hundreds of milliseconds
	// and turns within one call are already serialized by the pipeline.
	history, mark, err := a.appendInbound(req)
	if err != nil {
		return Reply{}, err
	}

	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.cfg.Model),
		MaxTokens: int64(a.cfg.MaxTokens),
		System:    []anthropic.TextBlockParam{{Text: a.cfg.System}},
		Messages:  history,
		Tools:     toAnthropicTools(req.Tools),
		// Adaptive thinking is the only supported mode on this model family;
		// depth is controlled by effort, not by a token budget.
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(a.cfg.Effort),
		},
	})
	if err != nil {
		// Roll the inbound turn back out of the history. Leaving it would mean
		// the next turn replayed a question the model never saw an answer to.
		a.rollback(req.CallID, mark)
		return Reply{}, fmt.Errorf("llm: anthropic: %w", err)
	}

	reply := Reply{
		InputTokens:  int(msg.Usage.InputTokens),
		OutputTokens: int(msg.Usage.OutputTokens),
	}

	// The stop reason is checked before the content is read. A refusal is a
	// successful HTTP response whose content is empty or partial, so reading it
	// blindly would hand the caller silence.
	if msg.StopReason == anthropic.StopReasonRefusal {
		a.rollback(req.CallID, mark)
		reply.Text = RefusalReply
		return reply, nil
	}

	var text strings.Builder
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteString(" ")
			}
			text.WriteString(block.Text)
		case "tool_use":
			reply.ToolCalls = append(reply.ToolCalls, ToolCall{
				ID:   block.ID,
				Name: block.Name,
				Args: json.RawMessage(block.Input),
			})
		}
	}
	reply.Text = strings.TrimSpace(text.String())

	// The response is echoed back verbatim via ToParam, which preserves
	// thinking blocks and tool_use IDs unchanged. Reconstructing the assistant
	// turn by hand would drop them, and the API rejects tool results whose
	// calls it cannot find.
	a.commit(req.CallID, msg.ToParam(), reply.NeedsTools())

	return reply, nil
}

// appendInbound adds the caller's side of this round to the conversation and
// returns the history to send, plus the length to roll back to on failure.
func (a *Anthropic) appendInbound(req Request) ([]anthropic.MessageParam, int, error) {
	var inbound anthropic.MessageParam

	if len(req.ToolResults) > 0 {
		// Every result is returned in a single user message. Splitting them
		// across messages trains the model out of requesting parallel tool
		// calls, and a failed tool is reported as an error result rather than
		// dropped, so the model can explain itself instead of stalling.
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(req.ToolResults))
		for _, tr := range req.ToolResults {
			if tr.ID == "" {
				return nil, 0, fmt.Errorf(
					"llm: anthropic: tool result for %q has no call ID to match", tr.Name)
			}
			blocks = append(blocks, anthropic.NewToolResultBlock(
				tr.ID, tr.Content, tr.Err != nil))
		}
		inbound = anthropic.NewUserMessage(blocks...)
	} else {
		transcript := strings.TrimSpace(req.Transcript)
		if transcript == "" {
			return nil, 0, fmt.Errorf("llm: anthropic: empty transcript")
		}
		inbound = anthropic.NewUserMessage(anthropic.NewTextBlock(transcript))
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.reapLocked()

	c := a.calls[req.CallID]
	if c == nil {
		c = &conversation{}
		a.calls[req.CallID] = c
	}
	mark := len(c.messages)
	c.messages = append(c.messages, inbound)
	c.lastUsed = time.Now()

	// The slice is copied because the API call runs without the lock.
	out := make([]anthropic.MessageParam, len(c.messages))
	copy(out, c.messages)
	return out, mark, nil
}

// commit records the model's turn. History is trimmed only once the turn has
// concluded, so a tool call and its result are never split apart by trimming.
func (a *Anthropic) commit(callID string, msg anthropic.MessageParam, midTurn bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	c := a.calls[callID]
	if c == nil {
		return
	}
	c.messages = append(c.messages, msg)
	c.lastUsed = time.Now()
	if !midTurn {
		c.messages = trimHistory(c.messages, a.cfg.MaxHistoryTurns)
	}
}

// rollback discards everything added at or after mark.
func (a *Anthropic) rollback(callID string, mark int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if c := a.calls[callID]; c != nil && mark <= len(c.messages) {
		c.messages = c.messages[:mark]
	}
}

// reapLocked drops conversations that have gone quiet.
func (a *Anthropic) reapLocked() {
	cutoff := time.Now().Add(-conversationTTL)
	for id, c := range a.calls {
		if c.lastUsed.Before(cutoff) {
			delete(a.calls, id)
		}
	}
}

// isTurnStart reports whether msgs[i] begins a new conversational turn.
//
// A turn starts at a user message that carries what the caller said. The
// subtlety is that a tool result is *also* a user message, and it is not
// preceded by another user message — the sequence is question, assistant
// tool request, tool result — so position alone cannot distinguish the two.
// The content can: a continuation consists of tool_result blocks.
func isTurnStart(msgs []anthropic.MessageParam, i int) bool {
	if i < 0 || i >= len(msgs) || msgs[i].Role != anthropic.MessageParamRoleUser {
		return false
	}
	return !isToolResultMessage(msgs[i])
}

// isToolResultMessage reports whether a message carries only tool results.
func isToolResultMessage(m anthropic.MessageParam) bool {
	if len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.OfToolResult == nil {
			return false
		}
	}
	return true
}

// trimHistory keeps the most recent turns, where a turn is what the caller said
// plus everything that followed it.
//
// It cuts on a turn boundary rather than at a fixed message count because a
// history is only a valid request if it begins with the caller's own words.
// Cutting by count would eventually open a request with an assistant turn, or
// with tool results whose request had been dropped, and the API rejects both.
func trimHistory(msgs []anthropic.MessageParam, maxTurns int) []anthropic.MessageParam {
	// Find the start of each turn, newest first, stopping one past the limit so
	// we know whether trimming is needed at all.
	var starts []int
	for i := len(msgs) - 1; i >= 0; i-- {
		if !isTurnStart(msgs, i) {
			continue
		}
		starts = append(starts, i)
		if len(starts) > maxTurns {
			break
		}
	}
	if len(starts) <= maxTurns {
		return msgs
	}
	// starts is newest-first, so the maxTurns-th entry is the oldest turn to
	// keep. Slicing at the last entry instead would keep one turn too many.
	return msgs[starts[maxTurns-1]:]
}

// toAnthropicTools converts the registry's definitions to the API's form.
func toAnthropicTools(tools []Tool) []anthropic.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := anthropic.ToolInputSchemaParam{}
		if len(t.Schema) > 0 {
			// The schema is authored as JSON Schema, which is what the API
			// wants; it is decoded only to hand the SDK the properties and
			// required list as separate fields.
			var parsed struct {
				Properties json.RawMessage `json:"properties"`
				Required   []string        `json:"required"`
			}
			if err := json.Unmarshal(t.Schema, &parsed); err == nil {
				if len(parsed.Properties) > 0 {
					schema.Properties = parsed.Properties
				}
				schema.Required = parsed.Required
			}
		}

		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: schema,
			},
		})
	}
	return out
}
