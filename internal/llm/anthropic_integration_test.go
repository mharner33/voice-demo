//go:build integration

package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests call the real API. They are behind the integration build tag
// because they cost money and need credentials; `make test` never runs them.
// Run with:
//
//	make test-integration
//
// The offline tests in anthropic_test.go cover the request this program builds
// and the response parsing. What can only be checked here is whether a real
// model, given this system prompt and this tool, actually behaves the way the
// voice pipeline needs it to.

// skipWithoutCredentials skips unless the environment can authenticate. The SDK
// resolves credentials itself, so an unset ANTHROPIC_API_KEY does not mean
// there are none — a logged-in CLI profile also works — but an unset key is the
// only signal available here without making a request.
func skipWithoutCredentials(t *testing.T) {
	t.Helper()
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		t.Skip("no ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN; skipping the live model test")
	}
}

func liveAgent(t *testing.T) *Anthropic {
	t.Helper()
	a, err := NewAnthropic(AnthropicConfig{})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	return a
}

// TestLiveAgentRepliesBriefly checks the property the voice channel actually
// depends on. A reply is read aloud, so length is latency: a paragraph that
// reads fine on screen is fifteen seconds of a caller unable to interrupt.
func TestLiveAgentRepliesBriefly(t *testing.T) {
	skipWithoutCredentials(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a := liveAgent(t)
	start := time.Now()
	reply, err := a.Reply(ctx, Request{
		CallID:     "live-brief",
		Transcript: "hi, I'd like to know when my next payment is due",
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	t.Logf("in %v, %d/%d tokens: %q", time.Since(start).Round(time.Millisecond),
		reply.InputTokens, reply.OutputTokens, reply.Text)

	if reply.Text == "" {
		t.Fatal("the model produced no text to speak")
	}
	if reply.InputTokens == 0 || reply.OutputTokens == 0 {
		t.Errorf("tokens = %d/%d; cost metrics would be empty",
			reply.InputTokens, reply.OutputTokens)
	}

	// Roughly three sentences' worth. Generous enough not to be flaky, tight
	// enough to catch a reply that has stopped being phone-shaped.
	if words := len(strings.Fields(reply.Text)); words > 60 {
		t.Errorf("the reply is %d words, which is a long time to listen to: %q",
			words, reply.Text)
	}

	// Markdown is audible as punctuation noise once a synthesizer reads it.
	for _, bad := range []string{"**", "##", "- ", "\n\n", "•"} {
		if strings.Contains(reply.Text, bad) {
			t.Errorf("the reply contains %q, which does not survive being read aloud: %q",
				bad, reply.Text)
		}
	}
}

// TestLiveAgentCallsTheTool is the test that matters for the trace: a real
// model, offered the account tool, has to request it rather than inventing a
// balance. The span tree the demo shows depends on it.
func TestLiveAgentCallsTheTool(t *testing.T) {
	skipWithoutCredentials(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	a := liveAgent(t)
	def, fn := AccountLookupTool()

	first, err := a.Reply(ctx, Request{
		CallID:     "live-tool",
		Transcript: "this is account four seven two nine, what is my balance?",
		Tools:      []Tool{def},
	})
	if err != nil {
		t.Fatalf("first Reply: %v", err)
	}

	if !first.NeedsTools() {
		t.Fatalf("the model answered without looking the account up, so the trace would "+
			"show no tool span: %q", first.Text)
	}
	call := first.ToolCalls[0]
	if call.Name != AccountLookupToolName {
		t.Fatalf("the model called %q, want %q", call.Name, AccountLookupToolName)
	}
	t.Logf("tool call: %s(%s)", call.Name, call.Args)

	content, err := fn(ctx, call.Args)
	if err != nil {
		t.Fatalf("the model supplied arguments the tool rejected (%s): %v", call.Args, err)
	}

	second, err := a.Reply(ctx, Request{
		CallID:      "live-tool",
		Transcript:  "this is account four seven two nine, what is my balance?",
		Tools:       []Tool{def},
		ToolResults: []ToolResult{{ID: call.ID, Name: call.Name, Content: content}},
	})
	if err != nil {
		t.Fatalf("continuation Reply: %v", err)
	}
	t.Logf("spoken reply: %q", second.Text)

	if second.Text == "" {
		t.Fatal("the continuation produced no text to speak")
	}
	// The figure the tool returned has to reach the caller in some form. The
	// tool says "$1,284.52"; spoken back it may be worded, so only the dollars
	// are checked.
	if !strings.Contains(second.Text, "1,284") && !strings.Contains(second.Text, "1284") &&
		!strings.Contains(strings.ToLower(second.Text), "twelve hundred") &&
		!strings.Contains(strings.ToLower(second.Text), "one thousand") {
		t.Errorf("the reply does not appear to report the balance the tool returned: %q",
			second.Text)
	}
}

// TestLiveAgentHandlesAGarbledTranscript covers the case packet loss actually
// produces. The recognizer hands over damaged text, and the agent should ask
// for a repeat rather than confidently answering the wrong question.
func TestLiveAgentHandlesAGarbledTranscript(t *testing.T) {
	skipWithoutCredentials(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a := liveAgent(t)
	def, _ := AccountLookupTool()

	reply, err := a.Reply(ctx, Request{
		CallID:     "live-garbled",
		Transcript: "my ac— —ance on the —— four seven —— is that ——",
		Tools:      []Tool{def},
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	t.Logf("reply to a garbled transcript: %q (tools requested: %d)",
		reply.Text, len(reply.ToolCalls))

	if reply.Text == "" && !reply.NeedsTools() {
		t.Error("the agent neither spoke nor acted on a damaged transcript")
	}
}

// TestLiveAgentReportsRealCost closes the loop on the cost metrics: the price
// list has to produce a non-zero figure from real token counts.
func TestLiveAgentReportsRealCost(t *testing.T) {
	skipWithoutCredentials(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a := liveAgent(t)
	reply, err := a.Reply(ctx, Request{
		CallID: "live-cost", Transcript: "thank you, goodbye",
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	in, out := a.Info().Pricing.Cost(reply.InputTokens, reply.OutputTokens)
	t.Logf("%d/%d tokens cost $%.6f in, $%.6f out", reply.InputTokens, reply.OutputTokens, in, out)
	if in <= 0 || out <= 0 {
		t.Errorf("cost = $%g/$%g from %d/%d tokens, want both above zero",
			in, out, reply.InputTokens, reply.OutputTokens)
	}
}
