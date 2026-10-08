package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ToolPlaceholder is replaced with the tool's output when building a reply that
// follows a tool call.
const ToolPlaceholder = "{{tool}}"

// DefaultCharsPerToken approximates tokenization closely enough for a demo's
// cost figures to be the right order of magnitude. Real tokenizers average
// about four characters per token on English prose.
const DefaultCharsPerToken = 4

// Rule maps something the caller said to how the agent responds.
type Rule struct {
	// Contains is matched case-insensitively against the transcript. An empty
	// string matches anything, which makes a catch-all rule possible.
	Contains string

	// Tool is the tool to call before replying. Ignored when the tool is not
	// registered, so a rule cannot wedge a call by naming a missing tool.
	Tool string
	// ToolArgs is the argument payload for that call.
	ToolArgs json.RawMessage

	// Reply is spoken when no tool is involved.
	Reply string
	// ReplyAfterTool is spoken once the tool has run. Any occurrence of
	// ToolPlaceholder is replaced with the tool's output.
	ReplyAfterTool string
}

// DefaultRules respond to DefaultPhrases from the stt package, so the mock
// pipeline produces a coherent conversation end to end.
var DefaultRules = []Rule{
	{
		Contains:       "balance",
		Tool:           AccountLookupToolName,
		ToolArgs:       json.RawMessage(`{"account_id":"4729"}`),
		ReplyAfterTool: "I've pulled up your account. " + ToolPlaceholder,
	},
	{
		Contains: "account number",
		Reply:    "Thank you, I've verified that account number.",
	},
	{
		Contains:       "dispute",
		Tool:           AccountLookupToolName,
		ToolArgs:       json.RawMessage(`{"account_id":"4729"}`),
		ReplyAfterTool: "I can start a dispute for you. " + ToolPlaceholder,
	},
	{
		Contains: "transfer",
		Reply:    "Of course, I'm transferring you to the billing team now.",
	},
	{
		Contains: "thank you",
		Reply:    "You're welcome. Thanks for calling, and have a good day.",
	},
}

// DefaultFallback is the reply when no rule matches.
const DefaultFallback = "I'm sorry, I didn't catch that. Could you say it again?"

// MockConfig parameterizes the mock agent.
type MockConfig struct {
	// Rules are matched in order; the first match wins. Empty means
	// DefaultRules.
	Rules []Rule
	// Fallback is the reply when nothing matches.
	Fallback string
	// CharsPerToken approximates tokenization for the reported token counts.
	CharsPerToken int
	// Registry, when set, is consulted so that a rule naming an unregistered
	// tool degrades to a plain reply instead of requesting a tool that cannot
	// run.
	Registry *Registry
}

func (c *MockConfig) applyDefaults() {
	if len(c.Rules) == 0 {
		c.Rules = DefaultRules
	}
	if c.Fallback == "" {
		c.Fallback = DefaultFallback
	}
	if c.CharsPerToken == 0 {
		c.CharsPerToken = DefaultCharsPerToken
	}
}

// Validate rejects configurations that cannot produce a reply.
func (c MockConfig) Validate() error {
	if c.CharsPerToken < 1 {
		return fmt.Errorf("llm: CharsPerToken = %d, want >= 1", c.CharsPerToken)
	}
	for i, r := range c.Rules {
		if r.Tool == "" && r.Reply == "" {
			return fmt.Errorf("llm: Rules[%d] has neither a tool nor a reply", i)
		}
		if r.Tool != "" && r.ReplyAfterTool == "" {
			return fmt.Errorf("llm: Rules[%d] calls tool %q but has no ReplyAfterTool",
				i, r.Tool)
		}
	}
	return nil
}

// Mock is a deterministic Agent. The same transcript always produces the same
// reply, the same tool calls, and the same token counts, which is what lets the
// pipeline test assert exact output.
type Mock struct {
	cfg MockConfig
}

// NewMock creates a mock agent. The zero MockConfig is valid.
func NewMock(cfg MockConfig) (*Mock, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Mock{cfg: cfg}, nil
}

// Info identifies the mock.
func (m *Mock) Info() Info { return Info{Provider: "mock", Model: "scripted"} }

// Reply produces the agent's response. When a matching rule names an available
// tool and no results have come back yet, it asks for the tool; on the second
// call, with results in hand, it produces the spoken reply.
func (m *Mock) Reply(ctx context.Context, req Request) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}

	rule, matched := m.match(req.Transcript)

	var r Reply
	switch {
	case len(req.ToolResults) > 0:
		r.Text = m.renderAfterTool(rule, matched, req.ToolResults)

	case matched && rule.Tool != "" && m.toolAvailable(rule.Tool, req.Tools):
		r.ToolCalls = []ToolCall{{
			ID:   fmt.Sprintf("call_%s_%s", rule.Tool, shortID(req.Transcript)),
			Name: rule.Tool,
			Args: rule.ToolArgs,
		}}

	case matched && rule.Reply != "":
		r.Text = rule.Reply

	case matched && rule.ReplyAfterTool != "":
		// The rule wanted a tool that is not available, so fall back to its
		// post-tool text with the placeholder removed.
		r.Text = strings.TrimSpace(strings.ReplaceAll(rule.ReplyAfterTool, ToolPlaceholder, ""))

	default:
		r.Text = m.cfg.Fallback
	}

	r.InputTokens = m.countTokens(req.Transcript) + m.toolResultTokens(req.ToolResults)
	r.OutputTokens = m.countTokens(r.Text)
	for _, tc := range r.ToolCalls {
		// A requested tool call costs output tokens even with no prose.
		r.OutputTokens += m.countTokens(tc.Name) + m.countTokens(string(tc.Args))
	}
	return r, nil
}

func (m *Mock) match(transcript string) (Rule, bool) {
	lower := strings.ToLower(transcript)
	for _, r := range m.cfg.Rules {
		if r.Contains == "" || strings.Contains(lower, strings.ToLower(r.Contains)) {
			return r, true
		}
	}
	return Rule{}, false
}

// toolAvailable reports whether a tool is both offered this turn and backed by
// a real implementation.
func (m *Mock) toolAvailable(name string, offered []Tool) bool {
	if m.cfg.Registry != nil && !m.cfg.Registry.Has(name) {
		return false
	}
	for _, t := range offered {
		if t.Name == name {
			return true
		}
	}
	return false
}

func (m *Mock) renderAfterTool(rule Rule, matched bool, results []ToolResult) string {
	var parts []string
	for _, res := range results {
		if res.Err != nil {
			// Surface the failure rather than inventing an answer, which is the
			// behavior worth demonstrating when a tool is degraded.
			return fmt.Sprintf("I wasn't able to look that up just now: %s", res.Content)
		}
		parts = append(parts, res.Content)
	}
	content := strings.Join(parts, " ")

	tmpl := rule.ReplyAfterTool
	if !matched || tmpl == "" {
		tmpl = ToolPlaceholder
	}
	return strings.TrimSpace(strings.ReplaceAll(tmpl, ToolPlaceholder, content))
}

func (m *Mock) countTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / m.cfg.CharsPerToken
	if n < 1 {
		n = 1
	}
	return n
}

func (m *Mock) toolResultTokens(results []ToolResult) int {
	var n int
	for _, r := range results {
		n += m.countTokens(r.Content)
	}
	return n
}

// shortID derives a stable identifier from a string, so tool-call IDs are
// reproducible across runs.
func shortID(s string) string {
	const fnvOffset, fnvPrime = 2166136261, 16777619
	h := uint32(fnvOffset)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= fnvPrime
	}
	return fmt.Sprintf("%08x", h)
}
