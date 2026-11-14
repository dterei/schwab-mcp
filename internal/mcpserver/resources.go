package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The account resources (design 11.1 to 11.3): schwab://accounts, which is
// list_accounts' answer, and the template of one account's positions,
// get_positions' first page for it. Each runs its tool as a call of it
// would run, slot and budget included, so that its text is the tool's
// answer, masked as the tool masks it, which the middleware's backstop
// then passes over again. Nothing a model needs exists only as a resource,
// since some clients show resources only when the person attaches them.

// accountsURI is the URI of the accounts resource.
const accountsURI = "schwab://accounts"

// positionsURI matches the URIs of the positions template: an account of 1
// to 80 letters, digits and dots, which no URI needs to escape. The
// middleware refuses any other URI before the SDK sees it (#1426).
var positionsURI = regexp.MustCompile(`^schwab://accounts/([A-Za-z0-9.]{1,80})/positions$`)

// lastDigits is an account's last 3 or more digits, with or without ...,
// as a positions URI may give them.
var lastDigits = regexp.MustCompile(`^(?:\.\.\.)?[0-9]{3,}$`)

// registerAccountResources registers the account resources.
func (s *Server) registerAccountResources() {
	s.addResource(&mcp.Resource{
		URI: accountsURI, Name: "accounts", Title: "Schwab accounts", MIMEType: "application/json",
		Description: "The accounts this Schwab login shares, with their values and cash and the totals, as list_accounts answers.",
	}, s.readAccounts)
	s.addResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "schwab://accounts/{account}/positions", Name: "positions", Title: "Positions of one Schwab account",
		MIMEType: "application/json",
		Description: "The first page of one account's positions, as get_positions answers it: account is the account's " +
			"account_hash or the first 12 or more of its characters, its store ref (A1), or the last 3 or more digits of its " +
			"number. get_positions with next_cursor gives the rest.",
	}, positionsURI, s.readPositions)
}

// readAccounts reads schwab://accounts.
func (s *Server) readAccounts(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	text, e := s.toolText(ctx, "list_accounts", struct{}{})
	if e != nil {
		return nil, resourceError(e)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: accountsURI, MIMEType: "application/json", Text: text}}}, nil
}

// readPositions reads the positions of the one account that the URI's
// account picks: an account hash or a prefix of one, a store ref, or the
// last digits of a number; not a nickname, which may hold a space, and
// which the template does not offer. Its content's URI is the account's
// canonical one, of its hash, never the URI the client asked for, which
// may hold the account's number: left empty, the SDK would copy that.
func (s *Server) readPositions(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	m := positionsURI.FindStringSubmatch(req.Params.URI)
	if m == nil || !hashLike(m[1]) && !storeRef.MatchString(m[1]) && !lastDigits.MatchString(m[1]) {
		return nil, resourceError(&toolErr{code: codeAccountNotFound})
	}
	text, e := s.toolText(ctx, "get_positions", positionsIn{Account: m[1]})
	if e != nil {
		return nil, resourceError(e)
	}
	var answer struct {
		Accounts []struct {
			AccountHash *string `json:"account_hash"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(text), &answer); err != nil || len(answer.Accounts) != 1 || answer.Accounts[0].AccountHash == nil {
		s.log.Error("a positions resource's answer names no one account with a hash", "accounts", len(answer.Accounts))
		return nil, resourceError(s.errorFor(codeInternal))
	}
	uri := "schwab://accounts/" + *answer.Accounts[0].AccountHash + "/positions"
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: text}}}, nil
}
