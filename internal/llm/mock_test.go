package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/faults"
	"github.com/mharner33/voice-demo/internal/stt"
)

func newTestMock(t *testing.T, cfg MockConfig) *Mock {
	t.Helper()
	m, err := NewMock(cfg)
	if err != nil {
		t.Fatalf("NewMock(%+v): %v", cfg, err)
	}
	return m
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	return r
}

func TestMockRepliesWithoutTools(t *testing.T) {
	m := newTestMock(t, MockConfig{
		Rules: []Rule{{Contains: "transfer", Reply: "Transferring you now."}},
	})

	got, err := m.Reply(context.Background(), Request{Transcript: "can you transfer me please"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Transferring you now." {
		t.Errorf("Text = %q", got.Text)
	}
	if got.NeedsTools() {
		t.Errorf("NeedsTools() = true, want false; got %d calls", len(got.ToolCalls))
	}
	if got.InputTokens == 0 || got.OutputTokens == 0 {
		t.Errorf("tokens = %d in / %d out, want both non-zero",
			got.InputTokens, got.OutputTokens)
	}
	if got.TotalTokens() != got.InputTokens+got.OutputTokens {
		t.Error("TotalTokens does not equal the sum of its parts")
	}
}

// TestMockRequestsToolThenReplies covers the two-step turn that produces the
// agent span shape worth demonstrating: the model asks for a tool, the tool
// runs, and the model speaks using the result.
func TestMockRequestsToolThenReplies(t *testing.T) {
	reg := newTestRegistry(t)
	m := newTestMock(t, MockConfig{Registry: reg})

	const transcript = "I'm calling about my account balance"

	// Round one: the model should ask for the lookup rather than answering.
	first, err := m.Reply(context.Background(), Request{
		Transcript: transcript,
		Tools:      reg.Tools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.NeedsTools() {
		t.Fatalf("first round did not request a tool; Text = %q", first.Text)
	}
	if len(first.ToolCalls) != 1 {
		t.Fatalf("requested %d tools, want 1", len(first.ToolCalls))
	}
	call := first.ToolCalls[0]
	if call.Name != AccountLookupToolName {
		t.Errorf("tool = %q, want %q", call.Name, AccountLookupToolName)
	}
	if call.ID == "" {
		t.Error("tool call has no ID")
	}

	// Run the tool for real.
	res := reg.Run(context.Background(), call)
	if res.Err != nil {
		t.Fatalf("tool failed: %v", res.Err)
	}

	// Round two: the model speaks, incorporating the tool output.
	second, err := m.Reply(context.Background(), Request{
		Transcript:  transcript,
		Tools:       reg.Tools(),
		ToolResults: []ToolResult{res},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.NeedsTools() {
		t.Error("the second round asked for more tools")
	}
	if second.Text == "" {
		t.Fatal("the second round produced no text")
	}
	if !strings.Contains(second.Text, "Dana Okafor") {
		t.Errorf("reply %q does not incorporate the tool result", second.Text)
	}
	if strings.Contains(second.Text, ToolPlaceholder) {
		t.Errorf("reply %q still contains the unsubstituted placeholder", second.Text)
	}
	// The tool output is part of the model's input on the second round.
	if second.InputTokens <= m.countTokens(transcript) {
		t.Errorf("InputTokens = %d, want more than the transcript alone", second.InputTokens)
	}
}

// TestMockDoesNotRequestUnavailableTool guards against a wedged turn: a rule
// naming a tool that is not registered must degrade to a plain reply rather
// than asking for something that can never run.
func TestMockDoesNotRequestUnavailableTool(t *testing.T) {
	// Registry deliberately empty.
	m := newTestMock(t, MockConfig{
		Registry: NewRegistry(),
		Rules: []Rule{{
			Contains:       "balance",
			Tool:           AccountLookupToolName,
			ReplyAfterTool: "Your balance is " + ToolPlaceholder,
		}},
	})

	got, err := m.Reply(context.Background(), Request{Transcript: "what is my balance"})
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsTools() {
		t.Error("the model asked for a tool that is not registered")
	}
	if got.Text == "" {
		t.Error("no fallback text was produced")
	}
	if strings.Contains(got.Text, ToolPlaceholder) {
		t.Errorf("reply %q leaked the placeholder", got.Text)
	}
}

// TestMockDoesNotRequestToolNotOffered covers the same hazard from the other
// side: the tool exists, but was not offered this turn.
func TestMockDoesNotRequestToolNotOffered(t *testing.T) {
	reg := newTestRegistry(t)
	m := newTestMock(t, MockConfig{Registry: reg})

	got, err := m.Reply(context.Background(), Request{
		Transcript: "I'm calling about my account balance",
		Tools:      nil, // nothing offered
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsTools() {
		t.Error("the model asked for a tool that was not offered this turn")
	}
	if got.Text == "" {
		t.Error("no text was produced")
	}
}

// TestMockSurfacesToolFailure checks that a failed tool produces an honest
// reply rather than a confident invention, which is the behavior worth showing
// when a dependency is degraded.
func TestMockSurfacesToolFailure(t *testing.T) {
	m := newTestMock(t, MockConfig{})

	got, err := m.Reply(context.Background(), Request{
		Transcript: "I'm calling about my account balance",
		ToolResults: []ToolResult{{
			Name:    AccountLookupToolName,
			Content: "lookup_account: no account \"9999\"",
			Err:     errors.New("no account"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text == "" {
		t.Fatal("no reply after a tool failure")
	}
	if !strings.Contains(strings.ToLower(got.Text), "wasn't able") {
		t.Errorf("reply %q does not acknowledge the failure", got.Text)
	}
	if strings.Contains(got.Text, "Dana Okafor") {
		t.Error("the reply invented account data after a failed lookup")
	}
}

func TestMockFallbackOnNoMatch(t *testing.T) {
	m := newTestMock(t, MockConfig{
		Rules:    []Rule{{Contains: "balance", Reply: "balance reply"}},
		Fallback: "didn't catch that",
	})

	got, err := m.Reply(context.Background(), Request{Transcript: "completely unrelated words"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "didn't catch that" {
		t.Errorf("Text = %q, want the fallback", got.Text)
	}
}

func TestMockRulesMatchInOrderAndCaseInsensitively(t *testing.T) {
	m := newTestMock(t, MockConfig{
		Rules: []Rule{
			{Contains: "ACCOUNT", Reply: "first"},
			{Contains: "balance", Reply: "second"},
		},
	})

	// Both substrings are present; the earlier rule wins.
	got, err := m.Reply(context.Background(), Request{Transcript: "my account balance please"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "first" {
		t.Errorf("Text = %q, want the first matching rule", got.Text)
	}
}

func TestMockCatchAllRule(t *testing.T) {
	m := newTestMock(t, MockConfig{
		Rules: []Rule{{Contains: "", Reply: "always this"}},
	})
	got, err := m.Reply(context.Background(), Request{Transcript: "anything at all"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "always this" {
		t.Errorf("Text = %q, want the catch-all reply", got.Text)
	}
}

func TestMockIsDeterministic(t *testing.T) {
	reg := newTestRegistry(t)
	req := Request{Transcript: "I'm calling about my account balance", Tools: reg.Tools()}

	a, err := newTestMock(t, MockConfig{Registry: reg}).Reply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newTestMock(t, MockConfig{Registry: reg}).Reply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if a.Text != b.Text || a.InputTokens != b.InputTokens || a.OutputTokens != b.OutputTokens {
		t.Error("two runs produced different replies or token counts")
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		t.Fatalf("tool call counts differ: %d vs %d", len(a.ToolCalls), len(b.ToolCalls))
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID {
			t.Errorf("tool call %d IDs differ: %q vs %q",
				i, a.ToolCalls[i].ID, b.ToolCalls[i].ID)
		}
	}
}

func TestMockHonorsContextCancellation(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Reply(ctx, Request{Transcript: "hello"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Reply = %v, want context.Canceled", err)
	}
}

func TestMockInfo(t *testing.T) {
	info := newTestMock(t, MockConfig{}).Info()
	if info.Provider != "mock" {
		t.Errorf("Provider = %q, want mock", info.Provider)
	}
	if info.Model == "" {
		t.Error("Model is empty")
	}
}

func TestMockConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MockConfig
		wantErr bool
	}{
		{"zero value takes defaults", MockConfig{}, false},
		{"reply only", MockConfig{Rules: []Rule{{Contains: "x", Reply: "y"}}}, false},
		{"zero chars per token takes default", MockConfig{CharsPerToken: 0}, false},
		{"negative chars per token", MockConfig{CharsPerToken: -1}, true},
		{"rule with neither tool nor reply", MockConfig{Rules: []Rule{{Contains: "x"}}}, true},
		{"tool without post-tool reply", MockConfig{
			Rules: []Rule{{Contains: "x", Tool: "t"}},
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewMock(tc.cfg); (err != nil) != tc.wantErr {
				t.Errorf("NewMock() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestDefaultRulesCoverDefaultPhrases keeps the mock conversation coherent: every
// phrase the mock transcriber produces should get a real reply rather than the
// fallback, or the demo's transcript would read as a broken conversation.
func TestDefaultRulesCoverDefaultPhrases(t *testing.T) {
	// Read from stt directly rather than copied, so that changing a phrase
	// there without adding a matching rule here fails this test. A duplicated
	// list would go stale silently. This is a test-only dependency: the llm
	// package itself knows nothing about stt.
	phrases := stt.DefaultPhrases

	reg := newTestRegistry(t)
	m := newTestMock(t, MockConfig{Registry: reg})

	for _, p := range phrases {
		reply, err := m.Reply(context.Background(), Request{Transcript: p, Tools: reg.Tools()})
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		if reply.Text == DefaultFallback {
			t.Errorf("%q fell through to the fallback reply", p)
		}
		if reply.Text == "" && !reply.NeedsTools() {
			t.Errorf("%q produced neither text nor a tool call", p)
		}
	}
}

// --- registry ---

func TestRegistryRunsTool(t *testing.T) {
	reg := newTestRegistry(t)

	if !reg.Has(AccountLookupToolName) {
		t.Fatalf("%s is not registered", AccountLookupToolName)
	}
	if got := len(reg.Tools()); got != 1 {
		t.Errorf("Tools() returned %d definitions, want 1", got)
	}

	res := reg.Run(context.Background(), ToolCall{
		ID:   "c1",
		Name: AccountLookupToolName,
		Args: json.RawMessage(`{"account_id":"4729"}`),
	})
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if res.ID != "c1" || res.Name != AccountLookupToolName {
		t.Errorf("result identity = %q/%q, want c1/%s", res.ID, res.Name, AccountLookupToolName)
	}
	for _, want := range []string{"4729", "Dana Okafor", "$1,284.52", "good standing"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content %q is missing %q", res.Content, want)
		}
	}
}

// TestRegistryReturnsFailuresAsResults checks that tool errors come back as
// results rather than error returns, so the agent loop can hand the failure to
// the model instead of abandoning the turn.
func TestRegistryReturnsFailuresAsResults(t *testing.T) {
	reg := newTestRegistry(t)

	tests := []struct {
		name string
		call ToolCall
	}{
		{"unknown tool", ToolCall{ID: "a", Name: "no_such_tool"}},
		{"unknown account", ToolCall{
			ID: "b", Name: AccountLookupToolName,
			Args: json.RawMessage(`{"account_id":"0000"}`),
		}},
		{"malformed args", ToolCall{
			ID: "c", Name: AccountLookupToolName,
			Args: json.RawMessage(`not json`),
		}},
		{"missing account id", ToolCall{
			ID: "d", Name: AccountLookupToolName,
			Args: json.RawMessage(`{}`),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := reg.Run(context.Background(), tc.call)
			if res.Err == nil {
				t.Fatal("expected an error in the result")
			}
			if res.Content == "" {
				t.Error("a failed result carries no content for the model to read")
			}
			if res.ID != tc.call.ID {
				t.Errorf("result ID = %q, want %q", res.ID, tc.call.ID)
			}
		})
	}
}

func TestRegistryRejectsDuplicateAndInvalid(t *testing.T) {
	reg := NewRegistry()
	def, fn := AccountLookupTool()

	if err := reg.Register(def, fn); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(def, fn); err == nil {
		t.Error("Register allowed a duplicate name, silently shadowing a tool")
	}
	if err := reg.Register(Tool{Name: ""}, fn); err == nil {
		t.Error("Register allowed an empty tool name")
	}
	if err := reg.Register(Tool{Name: "x"}, nil); err == nil {
		t.Error("Register allowed a tool with no implementation")
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	var reg *Registry
	if got := reg.Tools(); got != nil {
		t.Errorf("Tools() on a nil registry = %v, want nil", got)
	}
	if reg.Has("anything") {
		t.Error("Has() on a nil registry returned true")
	}
}

func TestRegistryToolsIsACopy(t *testing.T) {
	reg := newTestRegistry(t)
	tools := reg.Tools()
	if len(tools) == 0 {
		t.Fatal("no tools")
	}
	tools[0].Name = "mutated"

	if reg.Tools()[0].Name == "mutated" {
		t.Error("Tools() exposed the registry's own slice to mutation")
	}
}

func TestAccountLookupToolSchemaIsValidJSON(t *testing.T) {
	def, _ := AccountLookupTool()
	var schema map[string]any
	if err := json.Unmarshal(def.Schema, &schema); err != nil {
		t.Fatalf("the tool schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	if def.Description == "" {
		t.Error("the tool has no description for the model to read")
	}
}

func TestAccountLookupHonorsContext(t *testing.T) {
	_, fn := AccountLookupTool()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := fn(ctx, json.RawMessage(`{"account_id":"4729"}`)); !errors.Is(err, context.Canceled) {
		t.Errorf("tool = %v, want context.Canceled", err)
	}
}

// --- fault decorator ---

func TestWithFaultsCleanReturnsOriginal(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	got, err := WithFaults(m, faults.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got != Agent(m) {
		t.Error("a clean config did not return the original agent unwrapped")
	}
}

func TestWithFaultsInjectsError(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	faulty, err := WithFaults(m, faults.Config{ErrorRate: 1.0})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := faulty.Reply(context.Background(), Request{Transcript: "hello"}); !errors.Is(err, faults.ErrInjected) {
		t.Errorf("Reply = %v, want ErrInjected", err)
	}
	if faulty.Info() != m.Info() {
		t.Error("the wrapper changed Info()")
	}
}

func TestWithFaultsAppliesLatencyPerCall(t *testing.T) {
	m := newTestMock(t, MockConfig{})
	faulty, err := WithFaults(m, faults.Config{ExtraLatency: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := faulty.Reply(context.Background(), Request{Transcript: "hello"}); err != nil {
			t.Fatal(err)
		}
	}
	// Two round trips pay the latency twice, which is what a tool-calling turn
	// against a slow model actually costs.
	if elapsed := time.Since(start); elapsed < 35*time.Millisecond {
		t.Errorf("two calls took %v, want at least ~40ms", elapsed)
	}
}
