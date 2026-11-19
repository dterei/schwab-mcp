package mcpserver

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// portfolio_review (design 11.5) is the server's prompt: one user message,
// in the server's own words, that asks for a review of the portfolio and
// says which tools to call for it. One command-line client splits a
// prompt's arguments at white space, so both arguments are single words,
// and a nickname, which may hold a space, is not offered for the account.

// focuses are the words that portfolio_review's focus takes.
var focuses = []string{"allocation", "risk", "income", "activity", "cash"}

// promptWord is the shape of an argument of portfolio_review, trimmed and
// cut: letters, digits, dots, dashes and underscores, or nothing.
var promptWord = regexp.MustCompile(`^[A-Za-z0-9._-]*$`)

// maxPromptArgument is how many characters of an argument portfolio_review
// reads.
const maxPromptArgument = 64

// registerPrompts registers portfolio_review.
func (s *Server) registerPrompts() {
	s.mcp.AddPrompt(&mcp.Prompt{
		Name: "portfolio_review", Title: "Review my Schwab portfolio",
		Description: "A review of the user's Schwab accounts: their values and cash, the largest positions and their " +
			"share of the portfolio, open profit or loss, and the last 90 days' deposits, withdrawals, dividends, interest " +
			"and fees, with the tools to call for each.",
		Arguments: []*mcp.PromptArgument{
			{Name: "account", Description: "One account: the last 3 or more digits of its number, its A ref or its hash; every account when empty."},
			{Name: "focus", Description: "One word: allocation, risk, income, activity or cash."},
		},
	}, s.portfolioReview)
}

// portfolioReview answers portfolio_review. An argument that it does not
// take, or does not take as given, is -32602, which says what it takes and
// never quotes the value. It makes no request: the account is not looked
// up, only echoed, as an error echoes an account argument (call.shown): a
// hash, or a prefix of one, whole, and anything else with no more of a
// number than ... and its last 3 digits.
func (s *Server) portfolioReview(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	args := req.Params.Arguments
	for name := range args {
		if name != "account" && name != "focus" {
			return nil, invalidPrompt("portfolio_review takes the arguments account and focus.")
		}
	}
	account, ok := promptArgument(args["account"])
	if !ok {
		return nil, invalidPrompt("account takes one account's last 3 or more digits, its A ref or its account_hash, " +
			"in letters, digits, dots, dashes and underscores; every account when empty.")
	}
	focus, ok := promptArgument(args["focus"])
	focus = strings.ToLower(focus)
	if !ok || focus != "" && !slices.Contains(focuses, focus) {
		return nil, invalidPrompt("focus takes one word: allocation, risk, income, activity or cash.")
	}
	m := s.mask
	forAccount, thatAccount, focusing := m.words(""), m.words(""), m.words("")
	if account != "" {
		forAccount = m.compose(" for account %s", (&call{s: s, mask: m}).shown(account))
		thatAccount = m.words(" for that account")
	}
	if focus != "" {
		focusing = m.compose(", focusing on %s", Ident(focus))
	}
	text := m.compose("Review my Schwab portfolio%s%s.\n\n"+
		"1. Call list_accounts, then get_balances and get_positions%s.\n"+
		"2. Call get_transactions for the last 90 days with categories deposit, withdrawal, dividend, interest and fee, "+
		"and read its totals.\n"+
		"3. Report: the value and cash of each account and in total; the largest positions and their share of the "+
		"portfolio (portfolio_weight); open profit or loss by position; cash as a share of value; and the last 90 days' "+
		"deposits, withdrawals, dividends, interest and fees.\n"+
		"4. Point out concentration (any position over 10%% of the portfolio), idle cash, and anything the tools' "+
		"warnings say is missing or partial.\n\n"+
		"Account numbers are masked; refer to accounts by nickname and their last three digits. "+
		"Say which figures were null rather than estimating them.", forAccount, focusing, thatAccount)
	return &mcp.GetPromptResult{
		Description: "A review of the user's Schwab portfolio.",
		Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: string(text)}}},
	}, nil
}

// promptArgument returns v, an argument of portfolio_review, trimmed and
// cut to its first 64 characters, and whether it has the shape of one.
func promptArgument(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if r := []rune(v); len(r) > maxPromptArgument {
		v = string(r[:maxPromptArgument])
	}
	return v, promptWord.MatchString(v)
}

// invalidPrompt returns the error of a prompt's argument that it does not
// take: -32602, with msg, which says what it takes.
func invalidPrompt(msg string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: msg}
}
