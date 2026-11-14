package mcpserver

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// positionsOfURI is the URI of the positions of the account that account
// picks.
func positionsOfURI(account string) string { return "schwab://accounts/" + account + "/positions" }

// read reads the resource uri, as readResource does but for the check of
// what the server sent, which the test makes with assertClean once its
// reads are done: a check after each would scan the whole session again
// each time.
func (e *env) read(uri string) (*mcp.ReadResourceResult, error) {
	e.t.Helper()
	return e.cs.ReadResource(e.t.Context(), &mcp.ReadResourceParams{URI: uri})
}

// The accounts resource is list_accounts' answer, and the positions
// template's is get_positions' first page for the one account its URI
// picks, by its hash, a prefix of it in any case, its store ref, or its
// last digits with or without ..., under the account's canonical URI, of
// its hash: never the URI asked for, which may hold the number. Listing
// the resources and the templates makes no request. A read is private,
// with a time to live of 0.
func TestAccountResources(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	before := len(e.fake.Requests())
	resources, err := e.cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	templates, err := e.cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(resources.Resources, func(r *mcp.Resource) bool {
		return r.URI == accountsURI && r.MIMEType == "application/json" && r.Description != ""
	}) {
		t.Error("resources/list lacks schwab://accounts")
	}
	if !slices.ContainsFunc(templates.ResourceTemplates, func(r *mcp.ResourceTemplate) bool {
		return r.URITemplate == "schwab://accounts/{account}/positions" && r.MIMEType == "application/json" && r.Description != ""
	}) {
		t.Error("resources/templates/list lacks the positions template")
	}
	if n := len(e.fake.Requests()) - before; n != 0 {
		t.Errorf("listing the resources made %d requests", n)
	}

	r, err := e.read(accountsURI)
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Contents[0]; len(r.Contents) != 1 || c.URI != accountsURI || c.MIMEType != "application/json" ||
		c.Text != e.call("list_accounts", `{}`).text {
		t.Errorf("schwab://accounts: %+v", r.Contents)
	}
	if r.CacheScope != "private" || r.TTLMs != 0 {
		t.Errorf("schwab://accounts: %+v, want private and 0", r.Cacheable)
	}

	hash := schwabtest.HashFor(individual)
	want := e.call("get_positions", `{"account":"135"}`).text
	for _, account := range []string{"135", "...135", individual, hash, strings.ToLower(hash[:12]), "A1"} {
		r, err := e.read(positionsOfURI(account))
		if err != nil {
			t.Errorf("%.10s: %v", account, err)
			continue
		}
		if c := r.Contents[0]; len(r.Contents) != 1 || c.URI != positionsOfURI(hash) || c.MIMEType != "application/json" || c.Text != want {
			t.Errorf("%.10s: %s, %.300s", account, c.URI, c.Text)
		}
	}
	e.assertClean()
}

// A positions URI whose account is not one of the forms the template
// offers, a nickname among them, or that picks no account or several, is
// -32602 "resource not found", without the URI and without data; a
// failure upstream, of Schwab or of the login, is -32603 with the tool's
// error. The session lives on.
func TestPositionsResourceRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		f.AddAccount(schwabtest.Account{Number: "12340398", Details: bareAccount("12340398")})
	}))
	e.plant("12340398")
	for _, account := range []string{"Individual", "12", "398", "999", "A9", "0123456789AB"} {
		_, err := e.read(positionsOfURI(account))
		if we, ok := errors.AsType[*jsonrpc.Error](err); !ok || we.Code != jsonrpc.CodeInvalidParams || we.Message != "resource not found" || len(we.Data) > 0 {
			t.Errorf("%s: %v, want -32602 resource not found without data", account, err)
		}
	}
	loggedOut := newEnv(t, withoutStore(), withoutLogin())
	e.fake.Override(schwabtest.AccountsRoute, schwabtest.Unavailable)
	for _, uri := range []string{accountsURI, positionsOfURI("135")} {
		if _, err := e.read(uri); !upstreamError(err, "schwab_unavailable") {
			t.Errorf("%s with Schwab down: %v, want -32603 with the tool's error", uri, err)
		}
		if _, err := loggedOut.read(uri); !upstreamError(err, "not_logged_in") {
			t.Errorf("%s with no login: %v, want -32603 with the tool's error", uri, err)
		}
	}
	e.fake.Override(schwabtest.AccountsRoute, nil)
	if r, err := e.read(positionsOfURI("...135")); err != nil || r.Contents[0].URI != positionsOfURI(schwabtest.HashFor(individual)) {
		t.Errorf("a read after the refusals: %v", err)
	}
	e.assertClean()
	loggedOut.assertClean()
}

// upstreamError reports whether err is a resource read's -32603 with the
// error of code that the tool answered, and no data.
func upstreamError(err error, code string) bool {
	we, ok := errors.AsType[*jsonrpc.Error](err)
	return ok && we.Code == jsonrpc.CodeInternalError && strings.HasPrefix(we.Message, "["+code+"] ") && len(we.Data) == 0
}

// In each protocol version the SDK speaks, a client lists the tools, calls
// one, reads a resource and gets the prompt.
func TestProtocolVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, withoutStore(), withProtocol(version))
			if got := e.cs.InitializeResult().ProtocolVersion; got != version || e.tools["get_positions"] == nil {
				t.Fatalf("protocol %s, %d tools", got, len(e.tools))
			}
			if r := e.call("list_accounts", `{}`); r.isError {
				t.Error(r.text)
			}
			if r, err := e.read(positionsOfURI("398")); err != nil || r.Contents[0].URI != positionsOfURI(schwabtest.HashFor(options)) {
				t.Errorf("the positions of ...398: %v", err)
			}
			p, err := e.cs.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "portfolio_review", Arguments: map[string]string{"focus": "cash"}})
			if err != nil || len(p.Messages) != 1 || !strings.HasPrefix(p.Messages[0].Content.(*mcp.TextContent).Text, "Review my Schwab portfolio, focusing on cash.") {
				t.Errorf("the prompt: %v", err)
			}
			e.assertClean()
		})
	}
}
