package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// AccountLookupToolName is the tool the demo agent calls. A single realistic
// tool is enough to make the Agent Observability trace show an agent span with
// a nested tool span, which is the shape worth demonstrating.
const AccountLookupToolName = "lookup_account"

// AccountLookupArgs are the tool's arguments.
type AccountLookupArgs struct {
	AccountID string `json:"account_id"`
}

// account is a fixture record. Keeping the data in the tool keeps the demo
// self-contained: no database to stand up, and the same account always returns
// the same answer.
type account struct {
	ID      string
	Holder  string
	Balance string
	Status  string
}

var accounts = map[string]account{
	"4729": {ID: "4729", Holder: "Dana Okafor", Balance: "$1,284.52", Status: "in good standing"},
	"8815": {ID: "8815", Holder: "Sam Ellery", Balance: "$42.10", Status: "past due"},
}

// AccountLookupTool returns the tool definition and its implementation, ready
// to register.
func AccountLookupTool() (Tool, ToolFunc) {
	def := Tool{
		Name:        AccountLookupToolName,
		Description: "Look up a customer account by its identifier, returning the holder, balance, and status.",
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "account_id": {
      "type": "string",
      "description": "The customer's account identifier, usually four digits."
    }
  },
  "required": ["account_id"]
}`),
	}

	fn := func(ctx context.Context, args json.RawMessage) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var a AccountLookupArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("lookup_account: bad arguments: %w", err)
		}
		if a.AccountID == "" {
			return "", fmt.Errorf("lookup_account: account_id is required")
		}
		acct, ok := accounts[a.AccountID]
		if !ok {
			return "", fmt.Errorf("lookup_account: no account %q", a.AccountID)
		}
		return fmt.Sprintf("Account %s belongs to %s, the balance is %s, and it is %s.",
			acct.ID, acct.Holder, acct.Balance, acct.Status), nil
	}

	return def, fn
}

// DefaultRegistry returns a registry with the demo's tools registered.
func DefaultRegistry() (*Registry, error) {
	r := NewRegistry()
	if err := r.Register(AccountLookupTool()); err != nil {
		return nil, err
	}
	return r, nil
}
