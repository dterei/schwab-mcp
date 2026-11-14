package mcpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// An errorCode says what a failed call means for what to do about it. The
// codes are part of the contract: docs/mcp.md lists them, and a model can
// act on them, while their sentences can change.
type errorCode string

// The codes of a tool error (design 6.3). market_data_not_enabled comes
// with the market tools.
const (
	codeNotConfigured     errorCode = "not_configured"
	codeConfigUnreadable  errorCode = "config_unreadable"
	codeNotLoggedIn       errorCode = "not_logged_in"
	codeLoginExpired      errorCode = "login_expired"
	codeAppRejected       errorCode = "app_rejected"
	codeAppNotReady       errorCode = "app_not_ready"
	codeTokenRefused      errorCode = "token_refused"
	codeRateLimited       errorCode = "rate_limited"
	codeEdgeRefused       errorCode = "edge_refused"
	codeTimedOut          errorCode = "timed_out"
	codeBusy              errorCode = "busy"
	codeSchwabUnavailable errorCode = "schwab_unavailable"
	codeSchwabRefused     errorCode = "schwab_refused"
	codeTooBig            errorCode = "too_big"
	codeAccountNotFound   errorCode = "account_not_found"
	codeAccountAmbiguous  errorCode = "account_ambiguous"
	codeAccountNotShared  errorCode = "account_not_shared"
	codeNotFound          errorCode = "not_found"
	codeBadArgument       errorCode = "bad_argument"
	codeBadCursor         errorCode = "bad_cursor"
	codeNoStore           errorCode = "no_store"
	codeStoreNewer        errorCode = "store_newer"
	codeStoreNeedsUpdate  errorCode = "store_needs_update"
	codeStoreBusy         errorCode = "store_busy"
	codeStoreUnusable     errorCode = "store_unusable"
	codeInternal          errorCode = "internal"
)

// errorCodes are every code, in the order of design 6.3's table, for the
// tests that hold docs/mcp.md to them.
var errorCodes = []errorCode{
	codeNotConfigured, codeConfigUnreadable, codeNotLoggedIn, codeLoginExpired, codeAppRejected,
	codeAppNotReady, codeTokenRefused, codeRateLimited, codeEdgeRefused, codeTimedOut, codeBusy,
	codeSchwabUnavailable, codeSchwabRefused, codeTooBig, codeAccountNotFound, codeAccountAmbiguous,
	codeAccountNotShared, codeNotFound, codeBadArgument, codeBadCursor, codeNoStore, codeStoreNewer,
	codeStoreNeedsUpdate, codeStoreBusy, codeStoreUnusable, codeInternal,
}

// What each code says when nothing more particular is known: the sentence,
// and what to do.
var errorText = map[errorCode]string{
	codeLoginExpired: "The Schwab login has ended: logins last 7 days, and a login elsewhere ends the one before. " +
		"Ask the user to run `schwabtx login` in their own terminal, then call again; do not run it yourself, it needs their browser.",
	codeAppRejected: "Schwab refused the app key or secret. Ask the user to check them, run `schwabtx setup` again, " +
		"and see that the app says Ready For Use on developer.schwab.com.",
	codeAppNotReady: "Schwab answers this way until the app says Ready For Use on developer.schwab.com, " +
		"and for an account type its API does not serve.",
	codeTokenRefused: "Schwab refused a token it had just issued.",
	codeRateLimited: "Schwab is limiting requests from this app key, which every program using it shares. " +
		"Try again in a minute.",
	codeEdgeRefused:       "Schwab's network turned the request away; that usually passes within hours.",
	codeTimedOut:          "Schwab did not answer in time. Try again, or ask for less.",
	codeSchwabUnavailable: "Schwab could not be reached or did not answer properly. Try again later.",
	codeSchwabRefused:     "Schwab refused the request.",
	codeTooBig:            "The answer is too big. Ask for less: one account, or a shorter period.",
	codeNoStore:           "There is no local history store yet; `schwabtx import` makes one.",
	codeStoreNewer: "The store was written by a newer schwabtx. " +
		"Ask the user to update schwab-mcp to the same version and restart it.",
	codeStoreNeedsUpdate: "The store was last written by an older schwabtx. Ask the user to run `schwabtx status` once " +
		"in their own terminal, which brings it up to date; this server never changes the store.",
	codeStoreBusy: "Another schwabtx process kept the store locked. Try again shortly.",
	codeStoreUnusable: "The store cannot be read (damaged, on a network drive, or not a schwabtx store). " +
		"`schwabtx status` in a terminal says what to do.",
	codeInternal: "The server hit a bug while answering; it is still running.",
}

// A toolErr is a failure that the server has worded: its code, its
// sentence and what to do, composed from masked pieces, and a detail, or
// "". A handler returns one for what it finds itself, such as an account
// that no account matches; toolError words every other error.
type toolErr struct {
	code   errorCode
	msg    FreeText
	detail FreeText
}

func (e *toolErr) Error() string {
	s := "[" + string(e.code) + "] " + string(e.msg)
	if e.detail != "" {
		s += " Detail: " + string(e.detail)
	}
	return s
}

// errorFor returns the failure of code, saying what errorText says of it.
func (s *Server) errorFor(code errorCode) *toolErr {
	return &toolErr{code: code, msg: s.mask.words(errorText[code])}
}

// failf returns the failure of code, saying what format and args compose
// (masker.compose).
func (c *call) failf(code errorCode, format string, args ...any) error {
	return &toolErr{code: code, msg: c.mask.compose(format, args...)}
}

// toolError words err, a handler's failure, as its code says, logs what
// the model is not shown, and returns the code. Schwab's own words go
// into the detail through masker.schwabDetail, never through err.Error().
func (s *Server) toolError(tool string, err error) *toolErr {
	if e, ok := errors.AsType[*toolErr](err); ok {
		return e
	}
	if code, ok := storeCode(err); ok {
		s.log.Warn("the store could not be read", "tool", tool, "code", string(code), "error", err)
		return s.errorFor(code)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return s.errorFor(codeTimedOut)
	}
	se, fromSchwab := errors.AsType[*schwab.Error](err)
	var detail FreeText
	if fromSchwab {
		detail = s.mask.schwabDetail(se)
	}
	code := codeInternal
	switch lookup.Classify(err) {
	case lookup.NotLoggedIn:
		return s.notLoggedIn()
	case lookup.LoginExpired:
		code = codeLoginExpired
	case lookup.AppRejected:
		code = codeAppRejected
	case lookup.RateLimited:
		code = codeRateLimited
	case lookup.AppNotReady:
		code = codeAppNotReady
	case lookup.EdgeRefused:
		code = codeEdgeRefused
	case lookup.TokenRefused:
		code = codeTokenRefused
	case lookup.TooBig:
		code = codeTooBig
	case lookup.InvalidAccount, lookup.Other:
		switch {
		case fromSchwab && se.Status >= http.StatusInternalServerError:
			code = codeSchwabUnavailable
		case fromSchwab:
			code = codeSchwabRefused
		case s.schwabFailure(err):
			// A failure of the network, after the client's tries: its
			// cause is logged, and the model is told that Schwab could not
			// be reached.
			s.log.Warn("Schwab could not be reached", "tool", tool, "error", err)
			code = codeSchwabUnavailable
		default:
			s.log.Error("a tool failed", "tool", tool, "error", err)
		}
	}
	e := s.errorFor(code)
	e.detail = detail
	return e
}

// schwabFailure reports whether err came from a request to Schwab, which
// the client wraps as the request's method and path, rather than from the
// server's own code.
func (s *Server) schwabFailure(err error) bool {
	_, ok := errors.AsType[*requestError](err)
	return ok
}

// A requestError is a failure of a request to Schwab that is not an answer
// of Schwab's, such as a network failure, as call.schwab marks it.
type requestError struct{ err error }

func (e *requestError) Error() string { return e.err.Error() }
func (e *requestError) Unwrap() error { return e.err }

// storeCode returns the code of err, an error of the store, and whether it
// is one: a missing store, one newer or older than this build, one locked,
// or any other error of a read.
func storeCode(err error) (errorCode, bool) {
	switch {
	case errors.Is(err, history.ErrNoStore):
		return codeNoStore, true
	case errors.Is(err, history.ErrNewer):
		return codeStoreNewer, true
	case errors.Is(err, history.ErrNeedsUpdate):
		return codeStoreNeedsUpdate, true
	case errors.Is(err, history.ErrBusy):
		return codeStoreBusy, true
	case errors.Is(err, errStoreRead):
		return codeStoreUnusable, true
	}
	return "", false
}

// result returns e as a tool's error result: one text block, the code in
// brackets, the sentence and the detail, and no structured content, so that
// a client that checks structured content has nothing to reject.
func (e *toolErr) result() *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: e.Error()}}}
}
