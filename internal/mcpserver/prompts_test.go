package mcpserver

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// wholeReview is portfolio_review's text with neither argument.
const wholeReview = `Review my Schwab portfolio.

1. Call list_accounts, then get_balances and get_positions.
2. Call get_transactions for the last 90 days with categories deposit, withdrawal, dividend, interest and fee, and read its totals.
3. Report: the value and cash of each account and in total; the largest positions and their share of the portfolio (portfolio_weight); open profit or loss by position; cash as a share of value; and the last 90 days' deposits, withdrawals, dividends, interest and fees.
4. Point out concentration (any position over 10% of the portfolio), idle cash, and anything the tools' warnings say is missing or partial.

Account numbers are masked; refer to accounts by nickname and their last three digits. Say which figures were null rather than estimating them.`

// getPrompt gets portfolio_review with args, and returns its one message's
// text. What the server sent is left for the test to check with
// assertClean once its calls are done: a check after each would scan the
// whole session again each time.
func (e *env) getPrompt(args map[string]string) (string, error) {
	e.t.Helper()
	p, err := e.cs.GetPrompt(e.t.Context(), &mcp.GetPromptParams{Name: "portfolio_review", Arguments: args})
	if err != nil {
		return "", err
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != "user" {
		e.t.Fatalf("portfolio_review's messages: %+v", p.Messages)
	}
	return p.Messages[0].Content.(*mcp.TextContent).Text, nil
}

// portfolio_review is listed with its two arguments, neither required.
// It asks, in the server's own words, for a review of every account or of
// one, with a focus or none: each argument trimmed and cut to its first 64
// characters, the focus in any case, and the account echoed as an account
// argument's error echoes it, a hash or a prefix of one whole and any
// other run of more than 3 digits as ... and its last 3. It makes no
// request. An argument of another shape, an unknown focus or an argument
// it does not take is -32602, which says what it takes and quotes nothing;
// the session lives on. TestProtocolVersions gets the prompt in each
// protocol version.
func TestPortfolioReview(t *testing.T) {
	t.Parallel()
	hash := schwabtest.HashFor(individual)
	e := newEnv(t, withoutStore())
	before := len(e.fake.Requests())
	listed, err := e.cs.ListPrompts(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Prompts) != 1 || listed.Prompts[0].Name != "portfolio_review" || len(listed.Prompts[0].Arguments) != 2 ||
		slices.ContainsFunc(listed.Prompts[0].Arguments, func(a *mcp.PromptArgument) bool { return a.Required }) {
		t.Errorf("prompts/list: %+v", listed.Prompts)
	}
	if text, err := e.getPrompt(nil); err != nil || text != wholeReview {
		t.Errorf("with no arguments: %v\n%s", err, text)
	}
	text, err := e.getPrompt(map[string]string{"account": "  29470135 ", "focus": "Risk"})
	if err != nil || !strings.HasPrefix(text, "Review my Schwab portfolio for account ...135, focusing on risk.\n\n"+
		"1. Call list_accounts, then get_balances and get_positions for that account.\n2. ") {
		t.Errorf("for one account, with a focus: %v\n%s", err, text)
	}
	for account, shown := range map[string]string{
		hash: hash, strings.ToLower(hash[:12]): strings.ToLower(hash[:12]), "...135": "...135", "135": "135", "A2": "A2",
		strings.Repeat("7", 63) + "12345": "...771", strings.Repeat("z", 64) + " and more": strings.Repeat("z", 64),
	} {
		if text, err := e.getPrompt(map[string]string{"account": account}); err != nil ||
			!strings.HasPrefix(text, "Review my Schwab portfolio for account "+shown+".\n\n") {
			t.Errorf("account %.20s: %v\n%.80s", account, err, text)
		}
	}
	const takesAccount = "account takes one account's last 3 or more digits, its A ref or its account_hash, " +
		"in letters, digits, dots, dashes and underscores; every account when empty."
	const takesFocus = "focus takes one word: allocation, risk, income, activity or cash."
	for _, tc := range []struct {
		args map[string]string
		want string
	}{
		{map[string]string{"account": "Joint 2947-0135"}, takesAccount},
		{map[string]string{"account": "29470135/x"}, takesAccount},
		{map[string]string{"focus": "growth"}, takesFocus},
		{map[string]string{"focus": "risk!"}, takesFocus},
		{map[string]string{"account": "135", "user": "29470135"}, "portfolio_review takes the arguments account and focus."},
	} {
		_, err := e.getPrompt(tc.args)
		if we, ok := errors.AsType[*jsonrpc.Error](err); !ok || we.Code != jsonrpc.CodeInvalidParams || we.Message != tc.want || len(we.Data) > 0 {
			t.Errorf("%v: %v, want -32602 %q", slices.Sorted(maps.Keys(tc.args)), err, tc.want)
		}
	}
	if text, err := e.getPrompt(map[string]string{"focus": "income"}); err != nil || !strings.HasPrefix(text, "Review my Schwab portfolio, focusing on income.\n") {
		t.Errorf("after the refusals: %v", err)
	}
	if n := len(e.fake.Requests()) - before; n != 0 {
		t.Errorf("the prompt made %d requests", n)
	}
	e.assertClean()
}
