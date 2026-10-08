// Package llm defines the agent seam that turns a transcript into a spoken
// reply, and provides a deterministic mock implementation.
//
// The interface is shaped around tool calling rather than plain completion,
// because tool calls are what make the Agent Observability views worth looking
// at: a trace showing an agent span with a nested tool span and token costs
// tells a story that two bare completion spans do not. Phase 6 swaps the mock
// for a real model; nothing above this interface changes.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is a capability offered to the model.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema for the tool's arguments.
	Schema json.RawMessage
}

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	// ID correlates a call with its result across a turn.
	ID   string
	Name string
	Args json.RawMessage
}

// ToolResult is what a tool returned, fed back to the model.
type ToolResult struct {
	ID      string
	Name    string
	Content string
	// Err is set when the tool failed. The model is told about the failure
	// rather than the turn being abandoned, since a well-behaved agent should
	// recover or explain.
	Err error
}

// Request is one turn of the conversation.
type Request struct {
	// Transcript is what the caller said.
	Transcript string
	// Tools are the capabilities available this turn.
	Tools []Tool
	// ToolResults carries results from a previous round of tool calls. When it
	// is non-empty the model is being asked to continue a turn, not start one.
	ToolResults []ToolResult
	// CallID identifies the call, for providers that support request metadata.
	CallID string
}

// Reply is the model's response.
type Reply struct {
	// Text is the reply to speak. It may be empty when the model only wants
	// tools run.
	Text string
	// ToolCalls are the tools the model wants executed before it continues.
	ToolCalls []ToolCall
	// Token counts, for cost tracking on the LLM Observability span.
	InputTokens  int
	OutputTokens int
}

// NeedsTools reports whether this reply is asking for tool execution rather
// than concluding the turn.
func (r Reply) NeedsTools() bool { return len(r.ToolCalls) > 0 }

// TotalTokens is the sum reported on the span.
func (r Reply) TotalTokens() int { return r.InputTokens + r.OutputTokens }

// Info identifies the implementation, for span annotation.
type Info struct {
	Provider string // "mock", "anthropic"
	Model    string // "scripted", "claude-..."
}

// Agent turns a transcript into a reply, possibly by way of tool calls.
type Agent interface {
	Reply(ctx context.Context, req Request) (Reply, error)
	Info() Info
}

// ToolFunc implements a tool.
type ToolFunc func(ctx context.Context, args json.RawMessage) (string, error)

// Registry holds the tools available to an agent and runs them.
type Registry struct {
	defs  []Tool
	funcs map[string]ToolFunc
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{funcs: make(map[string]ToolFunc)}
}

// Register adds a tool. Registering a duplicate name is an error rather than a
// silent overwrite, since a shadowed tool would be very hard to notice in a
// trace.
func (r *Registry) Register(t Tool, fn ToolFunc) error {
	if t.Name == "" {
		return fmt.Errorf("llm: tool name is empty")
	}
	if fn == nil {
		return fmt.Errorf("llm: tool %q has no implementation", t.Name)
	}
	if _, dup := r.funcs[t.Name]; dup {
		return fmt.Errorf("llm: tool %q is already registered", t.Name)
	}
	r.defs = append(r.defs, t)
	r.funcs[t.Name] = fn
	return nil
}

// Tools returns the definitions to offer the model.
func (r *Registry) Tools() []Tool {
	if r == nil {
		return nil
	}
	out := make([]Tool, len(r.defs))
	copy(out, r.defs)
	return out
}

// Run executes one tool call. An unknown tool or a failing tool yields a
// ToolResult carrying the error rather than an error return, so the agent loop
// can hand the failure back to the model.
func (r *Registry) Run(ctx context.Context, call ToolCall) ToolResult {
	res := ToolResult{ID: call.ID, Name: call.Name}

	fn, ok := r.funcs[call.Name]
	if !ok {
		res.Err = fmt.Errorf("llm: no such tool %q", call.Name)
		res.Content = res.Err.Error()
		return res
	}
	content, err := fn(ctx, call.Args)
	if err != nil {
		res.Err = err
		res.Content = err.Error()
		return res
	}
	res.Content = content
	return res
}

// Has reports whether a tool is registered.
func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.funcs[name]
	return ok
}
