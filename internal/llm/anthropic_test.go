package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The real provider is tested against a local HTTP server rather than the live
// API. The SDK speaks ordinary HTTP, so this exercises the parts that can
// actually be wrong — the request this program builds and the response parsing
// — with no credentials, no network, and no per-run cost. The live path is
// covered separately behind the integration build tag.

// capturedRequest is one request the fake API received, decoded loosely so a
// test can assert on fields without mirroring the whole schema.
type capturedRequest struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    []any          `json:"system"`
	Messages  []wireMessage  `json:"messages"`
	Tools     []wireTool     `json:"tools"`
	Thinking  map[string]any `json:"thinking"`
	Output    struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// fakeAPI serves canned responses and records what it was asked.
type fakeAPI struct {
	t         *testing.T
	responses []string
	// status, when non-zero for a given round, is returned instead of a body.
	status []int

	got   []capturedRequest
	calls int
	srv   *httptest.Server
}

func newFakeAPI(t *testing.T, responses ...string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, responses: responses}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	var req capturedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decoding the request the SDK sent: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.got = append(f.got, req)
	round := f.calls
	f.calls++

	if round < len(f.status) && f.status[round] != 0 {
		// A 400 is used for failure cases because the SDK retries 429s and 5xx,
		// which would make the recorded round count unpredictable.
		w.WriteHeader(f.status[round])
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`))
		return
	}

	if round >= len(f.responses) {
		f.t.Errorf("the fake API was called %d times but only %d responses were queued",
			f.calls, len(f.responses))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(f.responses[round]))
}

// agent builds a provider pointed at the fake API.
func (f *fakeAPI) agent(t *testing.T, cfg AnthropicConfig) *Anthropic {
	t.Helper()
	cfg.APIKey = "test-key-not-a-real-credential"
	cfg.RequestOptions = append(cfg.RequestOptions,
		option.WithBaseURL(f.srv.URL),
		// Retries would make the recorded round count depend on timing.
		option.WithMaxRetries(0),
	)
	a, err := NewAnthropic(cfg)
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	return a
}

// textResponse is a canned reply with no tool call.
func textResponse(text string, in, out int) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant",
		"model":       DefaultAnthropicModel,
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": in, "output_tokens": out},
	})
	return string(b)
}

// toolUseResponse is a canned reply asking for a tool, with a thinking block in
// front of it so the tests cover the echo-back path that must preserve it.
func toolUseResponse(id, name, args string, in, out int) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant",
		"model": DefaultAnthropicModel,
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "", "signature": "sig-abc"},
			map[string]any{"type": "tool_use", "id": id, "name": name,
				"input": json.RawMessage(args)},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": in, "output_tokens": out},
	})
	return string(b)
}

func TestAnthropicSendsTheVoiceAgentRequest(t *testing.T) {
	api := newFakeAPI(t, textResponse("Your balance is fine.", 120, 14))
	a := api.agent(t, AnthropicConfig{})

	def, _ := AccountLookupTool()
	reply, err := a.Reply(context.Background(), Request{
		CallID:     "call-1",
		Transcript: "What is my balance?",
		Tools:      []Tool{def},
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if reply.Text != "Your balance is fine." {
		t.Errorf("Text = %q", reply.Text)
	}
	if reply.InputTokens != 120 || reply.OutputTokens != 14 {
		t.Errorf("tokens = %d/%d, want 120/14", reply.InputTokens, reply.OutputTokens)
	}

	got := api.got[0]
	if got.Model != DefaultAnthropicModel {
		t.Errorf("model = %q, want %q", got.Model, DefaultAnthropicModel)
	}
	if got.MaxTokens != defaultAnthropicMaxTokens {
		t.Errorf("max_tokens = %d, want %d", got.MaxTokens, defaultAnthropicMaxTokens)
	}

	// Adaptive thinking, not a token budget: a budget is rejected outright on
	// this model family, and disabling thinking risks tool calls arriving as
	// plain text.
	if got.Thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v, want type adaptive", got.Thinking)
	}
	if _, budgeted := got.Thinking["budget_tokens"]; budgeted {
		t.Error("the request carries budget_tokens, which this model rejects")
	}
	if got.Output.Effort != defaultAnthropicEffort {
		t.Errorf("effort = %q, want %q", got.Output.Effort, defaultAnthropicEffort)
	}

	if len(got.System) == 0 {
		t.Error("no system prompt was sent; the agent would not know it is on a phone call")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", got.Messages)
	}
	if got.Messages[0].Content[0].Text != "What is my balance?" {
		t.Errorf("transcript = %q", got.Messages[0].Content[0].Text)
	}

	// The tool must arrive with its schema intact, or the model cannot know
	// what argument to supply.
	if len(got.Tools) != 1 {
		t.Fatalf("tools = %+v, want 1", got.Tools)
	}
	tool := got.Tools[0]
	if tool.Name != AccountLookupToolName {
		t.Errorf("tool name = %q", tool.Name)
	}
	if tool.Description == "" {
		t.Error("the tool was sent with no description")
	}
	if _, ok := tool.InputSchema.Properties["account_id"]; !ok {
		t.Errorf("tool schema properties = %v, want account_id", tool.InputSchema.Properties)
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "account_id" {
		t.Errorf("tool schema required = %v, want [account_id]", tool.InputSchema.Required)
	}
}

type wireTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	} `json:"input_schema"`
}

// TestAnthropicToolRoundTrip is the central test: it covers the reason this
// provider has to keep state at all. A tool result is matched to its call by
// ID, so the second request must replay the assistant message that asked for
// the tool — reconstructing the turn without it is rejected by the API.
func TestAnthropicToolRoundTrip(t *testing.T) {
	const toolUseID = "toolu_01xyz"
	api := newFakeAPI(t,
		toolUseResponse(toolUseID, AccountLookupToolName, `{"account_id":"4729"}`, 200, 30),
		textResponse("Your balance is one thousand two hundred eighty four dollars.", 260, 20),
	)
	a := api.agent(t, AnthropicConfig{})

	def, fn := AccountLookupTool()

	first, err := a.Reply(context.Background(), Request{
		CallID:     "call-2",
		Transcript: "What is my balance?",
		Tools:      []Tool{def},
	})
	if err != nil {
		t.Fatalf("first Reply: %v", err)
	}
	if !first.NeedsTools() {
		t.Fatalf("first reply asked for no tools: %+v", first)
	}
	if first.ToolCalls[0].ID != toolUseID {
		t.Errorf("tool call ID = %q, want %q", first.ToolCalls[0].ID, toolUseID)
	}
	if first.ToolCalls[0].Name != AccountLookupToolName {
		t.Errorf("tool call name = %q", first.ToolCalls[0].Name)
	}

	// Run the tool for real, the way the pipeline would.
	content, err := fn(context.Background(), first.ToolCalls[0].Args)
	if err != nil {
		t.Fatalf("running the tool: %v", err)
	}

	second, err := a.Reply(context.Background(), Request{
		CallID:     "call-2",
		Transcript: "What is my balance?",
		Tools:      []Tool{def},
		ToolResults: []ToolResult{{
			ID: first.ToolCalls[0].ID, Name: first.ToolCalls[0].Name, Content: content,
		}},
	})
	if err != nil {
		t.Fatalf("second Reply: %v", err)
	}
	if second.NeedsTools() {
		t.Errorf("the second reply asked for more tools: %+v", second)
	}
	if !strings.Contains(second.Text, "one thousand") {
		t.Errorf("Text = %q", second.Text)
	}

	// The continuation request is where the state matters.
	if len(api.got) != 2 {
		t.Fatalf("the API was called %d times, want 2", len(api.got))
	}
	msgs := api.got[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("continuation sent %d messages, want 3 (question, tool request, result): %+v",
			len(msgs), msgs)
	}

	if msgs[1].Role != "assistant" {
		t.Errorf("messages[1].Role = %q, want assistant", msgs[1].Role)
	}
	var sawToolUse, sawThinking bool
	for _, b := range msgs[1].Content {
		switch b.Type {
		case "tool_use":
			sawToolUse = true
			if b.ID != toolUseID {
				t.Errorf("replayed tool_use ID = %q, want %q", b.ID, toolUseID)
			}
		case "thinking":
			sawThinking = true
		}
	}
	if !sawToolUse {
		t.Error("the assistant's tool request was not replayed; the API would reject the result")
	}
	if !sawThinking {
		t.Error("the thinking block was dropped from the replayed turn, which must be echoed unchanged")
	}

	if msgs[2].Role != "user" {
		t.Errorf("messages[2].Role = %q, want user", msgs[2].Role)
	}
	if got := msgs[2].Content[0]; got.Type != "tool_result" || got.ToolUseID != toolUseID {
		t.Errorf("tool result block = %+v, want tool_result for %q", got, toolUseID)
	}
}

// TestAnthropicFailedToolIsReportedNotDropped covers the degraded-tool path:
// the model is told the lookup failed so it can say so, rather than the result
// being silently omitted and the model left to invent an answer.
func TestAnthropicFailedToolIsReportedNotDropped(t *testing.T) {
	api := newFakeAPI(t, textResponse("I couldn't reach that system just now.", 90, 11))
	a := api.agent(t, AnthropicConfig{})

	_, err := a.Reply(context.Background(), Request{
		CallID:     "call-3",
		Transcript: "What is my balance?",
		ToolResults: []ToolResult{{
			ID:      "toolu_fail",
			Name:    AccountLookupToolName,
			Content: "lookup_account: no account \"9999\"",
			Err:     fmt.Errorf("no such account"),
		}},
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	block := api.got[0].Messages[0].Content[0]
	if block.Type != "tool_result" {
		t.Fatalf("block type = %q, want tool_result", block.Type)
	}
	if !block.IsError {
		t.Error("a failed tool was sent without is_error, so the model would read it as a real answer")
	}
}

// TestAnthropicToolResultWithoutIDIsRejected guards the invariant the API
// enforces: a result with no call to attach to cannot be sent.
func TestAnthropicToolResultWithoutIDIsRejected(t *testing.T) {
	api := newFakeAPI(t)
	a := api.agent(t, AnthropicConfig{})

	_, err := a.Reply(context.Background(), Request{
		CallID:      "call-4",
		Transcript:  "hello",
		ToolResults: []ToolResult{{Name: AccountLookupToolName, Content: "something"}},
	})
	if err == nil {
		t.Fatal("a tool result with no ID was accepted")
	}
	if api.calls != 0 {
		t.Errorf("the API was called %d times; the request should have failed locally", api.calls)
	}
}

// TestAnthropicRefusalSpeaksInsteadOfGoingSilent pins the handling of a
// refusal. It arrives as a successful response with no content, so reading the
// content blindly would hand the caller silence.
func TestAnthropicRefusalSpeaksInsteadOfGoingSilent(t *testing.T) {
	refusal, _ := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant",
		"model":        DefaultAnthropicModel,
		"content":      []any{},
		"stop_reason":  "refusal",
		"stop_details": map[string]any{"type": "refusal", "category": "cyber"},
		"usage":        map[string]any{"input_tokens": 40, "output_tokens": 0},
	})
	api := newFakeAPI(t, string(refusal))
	a := api.agent(t, AnthropicConfig{})

	reply, err := a.Reply(context.Background(), Request{
		CallID: "call-5", Transcript: "something the model declines",
	})
	if err != nil {
		t.Fatalf("a refusal was surfaced as an error, but it is a successful response: %v", err)
	}
	if reply.Text != RefusalReply {
		t.Errorf("Text = %q, want the spoken refusal line", reply.Text)
	}
	if reply.NeedsTools() {
		t.Error("a refusal produced tool calls")
	}
	if reply.InputTokens != 40 {
		t.Errorf("InputTokens = %d, want 40: a refusal still reports what it cost",
			reply.InputTokens)
	}
}

// TestAnthropicRemembersEarlierTurns covers the benefit of holding the history:
// the caller can refer back to what they already said.
func TestAnthropicRemembersEarlierTurns(t *testing.T) {
	api := newFakeAPI(t,
		textResponse("Certainly, which account?", 50, 8),
		textResponse("That one is past due.", 90, 9),
	)
	a := api.agent(t, AnthropicConfig{})

	for _, line := range []string{"I have a question about my account.", "What about the other one?"} {
		if _, err := a.Reply(context.Background(), Request{
			CallID: "call-6", Transcript: line,
		}); err != nil {
			t.Fatalf("Reply(%q): %v", line, err)
		}
	}

	msgs := api.got[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("second turn sent %d messages, want 3 (question, answer, follow-up)", len(msgs))
	}
	if msgs[0].Content[0].Text != "I have a question about my account." {
		t.Errorf("the first turn was not replayed: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("messages[1].Role = %q, want the previous answer", msgs[1].Role)
	}
}

// TestAnthropicSeparatesCalls makes sure one caller never sees another's
// conversation, which keying history by call ID is what prevents.
func TestAnthropicSeparatesCalls(t *testing.T) {
	api := newFakeAPI(t,
		textResponse("Hello there.", 10, 3),
		textResponse("Hello to you too.", 10, 3),
	)
	a := api.agent(t, AnthropicConfig{})

	for _, id := range []string{"call-a", "call-b"} {
		if _, err := a.Reply(context.Background(), Request{
			CallID: id, Transcript: "my secret is " + id,
		}); err != nil {
			t.Fatalf("Reply(%s): %v", id, err)
		}
	}

	second := api.got[1].Messages
	if len(second) != 1 {
		t.Fatalf("the second call sent %d messages, want 1: history leaked between calls", len(second))
	}
	if strings.Contains(second[0].Content[0].Text, "call-a") {
		t.Error("one call's transcript appeared in another call's prompt")
	}
}

// TestAnthropicRollsBackAFailedTurn covers the error path. A turn whose request
// failed must not leave the question in the history, or the next turn would
// replay a question with no answer behind it.
func TestAnthropicRollsBackAFailedTurn(t *testing.T) {
	api := newFakeAPI(t, "", textResponse("Now I can hear you.", 20, 5))
	api.status = []int{http.StatusBadRequest}
	a := api.agent(t, AnthropicConfig{})

	if _, err := a.Reply(context.Background(), Request{
		CallID: "call-7", Transcript: "first attempt",
	}); err == nil {
		t.Fatal("a 400 from the API was not surfaced as an error")
	}

	if _, err := a.Reply(context.Background(), Request{
		CallID: "call-7", Transcript: "second attempt",
	}); err != nil {
		t.Fatalf("the turn after a failure: %v", err)
	}

	msgs := api.got[1].Messages
	if len(msgs) != 1 {
		t.Fatalf("after a failed turn the next request sent %d messages, want 1: %+v", len(msgs), msgs)
	}
	if msgs[0].Content[0].Text != "second attempt" {
		t.Errorf("the failed turn was left in the history: %+v", msgs[0])
	}
}

// TestAnthropicForgetReleasesACall covers the teardown hook.
func TestAnthropicForgetReleasesACall(t *testing.T) {
	api := newFakeAPI(t,
		textResponse("One.", 10, 2),
		textResponse("Two.", 10, 2),
	)
	a := api.agent(t, AnthropicConfig{})

	if _, err := a.Reply(context.Background(), Request{CallID: "call-8", Transcript: "hello"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	a.Forget("call-8")

	if _, err := a.Reply(context.Background(), Request{CallID: "call-8", Transcript: "hello again"}); err != nil {
		t.Fatalf("Reply after Forget: %v", err)
	}
	if n := len(api.got[1].Messages); n != 1 {
		t.Errorf("after Forget the next request sent %d messages, want 1", n)
	}
}

// TestAnthropicInfoCarriesPricing pins that the provider reports what it
// charges, since that is what makes the cost metrics on the span real figures
// rather than zeros.
func TestAnthropicInfoCarriesPricing(t *testing.T) {
	api := newFakeAPI(t)
	a := api.agent(t, AnthropicConfig{})

	info := a.Info()
	if info.Provider != "anthropic" {
		t.Errorf("Provider = %q", info.Provider)
	}
	if info.Pricing.Free() {
		t.Fatalf("the price list for %s is empty, so every cost figure would be zero", info.Model)
	}

	// One thousand input and one thousand output tokens at the published rate.
	in, out := info.Pricing.Cost(1000, 1000)
	wantIn := 1000 * anthropicPricing[DefaultAnthropicModel].InputPerMTok / 1e6
	wantOut := 1000 * anthropicPricing[DefaultAnthropicModel].OutputPerMTok / 1e6
	if in != wantIn || out != wantOut {
		t.Errorf("Cost(1000,1000) = %g/%g, want %g/%g", in, out, wantIn, wantOut)
	}
}

// TestAnthropicUnknownModelHasNoFabricatedPrice covers the deliberate choice to
// report no cost rather than a wrong one.
func TestAnthropicUnknownModelHasNoFabricatedPrice(t *testing.T) {
	api := newFakeAPI(t)
	a := api.agent(t, AnthropicConfig{Model: "claude-not-a-real-model"})

	if !a.Info().Pricing.Free() {
		t.Errorf("an unknown model reported a price: %+v", a.Info().Pricing)
	}
}

func TestAnthropicConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  AnthropicConfig
		ok   bool
	}{
		{"defaults", AnthropicConfig{}, true},
		{"explicit effort", AnthropicConfig{Effort: "xhigh"}, true},
		{"bad effort", AnthropicConfig{Effort: "extreme"}, false},
		{"zero max tokens is defaulted", AnthropicConfig{MaxTokens: 0}, true},
		{"negative max tokens", AnthropicConfig{MaxTokens: -1}, false},
		{"negative history", AnthropicConfig{MaxHistoryTurns: -2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAnthropic(tc.cfg)
			if tc.ok && err != nil {
				t.Errorf("NewAnthropic: unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("NewAnthropic accepted an invalid configuration")
			}
		})
	}
}

// TestTrimHistoryCutsOnTurnBoundaries covers the trimming invariant. A history
// is only a valid request if it starts at a user turn, so trimming by a plain
// message count would eventually produce a request the API rejects.
func TestTrimHistoryCutsOnTurnBoundaries(t *testing.T) {
	user := func(s string) anthropic.MessageParam {
		return anthropic.NewUserMessage(anthropic.NewTextBlock(s))
	}
	assistant := func(s string) anthropic.MessageParam {
		return anthropic.NewAssistantMessage(anthropic.NewTextBlock(s))
	}

	const toolID = "toolu_trim"

	// Four turns. The third used a tool, so it spans four messages: the
	// question, the assistant's tool request, the result, and the answer. That
	// shape is the whole point of the test — the tool result is a *user*
	// message that does not begin a turn.
	msgs := []anthropic.MessageParam{
		user("one"), assistant("a1"),
		user("two"), assistant("a2"),
		user("three"),
		anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(
			toolID, json.RawMessage(`{"account_id":"4729"}`), AccountLookupToolName)),
		anthropic.NewUserMessage(anthropic.NewToolResultBlock(toolID, "the answer", false)),
		assistant("a3"),
		user("four"), assistant("a4"),
	}
	const totalTurns = 4

	for _, keep := range []int{1, 2, 3, 4, 5} {
		got := trimHistory(msgs, keep)

		if len(got) == 0 {
			t.Fatalf("maxTurns=%d: trimming emptied the history", keep)
		}
		if len(got) > len(msgs) {
			t.Errorf("maxTurns=%d: trimming grew the history", keep)
		}

		// The history must open with the caller's own words. An assistant turn
		// or an orphaned tool result at the front is rejected by the API.
		if !isTurnStart(got, 0) {
			t.Errorf("maxTurns=%d: history opens with a message that does not start a turn: %+v",
				keep, got[0])
		}

		turns := 0
		for i := range got {
			if isTurnStart(got, i) {
				turns++
			}
		}
		want := min(keep, totalTurns)
		if turns != want {
			t.Errorf("maxTurns=%d: kept %d turns, want %d", keep, turns, want)
		}

		// No tool result may survive without the request it answers, or the
		// API cannot match them.
		requested := map[string]bool{}
		for _, m := range got {
			for _, b := range m.Content {
				if b.OfToolUse != nil {
					requested[b.OfToolUse.ID] = true
				}
				if b.OfToolResult != nil && !requested[b.OfToolResult.ToolUseID] {
					t.Errorf("maxTurns=%d: a tool result for %q survived without its request",
						keep, b.OfToolResult.ToolUseID)
				}
			}
		}
	}

	// The newest turn is the one being answered, so it must always survive.
	got := trimHistory(msgs, 1)
	if last := got[len(got)-1]; last.Content[0].OfText == nil ||
		last.Content[0].OfText.Text != "a4" {
		t.Errorf("trimming to one turn dropped the newest turn: %+v", got)
	}
}

// TestAnthropicEmptyTranscriptIsRejectedLocally avoids spending a request on a
// turn that cannot mean anything.
func TestAnthropicEmptyTranscriptIsRejectedLocally(t *testing.T) {
	api := newFakeAPI(t)
	a := api.agent(t, AnthropicConfig{})

	if _, err := a.Reply(context.Background(), Request{CallID: "call-9", Transcript: "   "}); err == nil {
		t.Fatal("an empty transcript was accepted")
	}
	if api.calls != 0 {
		t.Errorf("the API was called %d times for an empty transcript", api.calls)
	}
}
