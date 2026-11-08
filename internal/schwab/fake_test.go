package schwab

import (
	"net/http"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The app and the login that the fake takes.
const (
	testKey    = schwabtest.AppKey
	testSecret = schwabtest.AppSecret
	testCode   = schwabtest.Code
)

// fakeOAuth is the fake Schwab of package schwabtest: its token endpoint,
// and the API behind it, which answers only a working access token. As
// Schwab does, a login retires the refresh tokens of earlier ones, while a
// refresh keeps the refresh token. It can also rotate refresh tokens and
// retire older access tokens, which Schwab is not known to do, to check that
// the coordination would cope.
type fakeOAuth = schwabtest.Server

// newFakeOAuth starts a fake over HTTP, which the end of the test closes.
func newFakeOAuth(t *testing.T) *fakeOAuth { return schwabtest.New(t) }

// newFakeOAuthH2 is newFakeOAuth over HTTPS with HTTP/2, which a client
// gets from f.Client().
func newFakeOAuthH2(t *testing.T) *fakeOAuth { return schwabtest.NewTLS(t) }

// answer returns a script entry that answers with status, headers in pairs,
// and body.
var answer = schwabtest.Answer

// tooMany is a script entry that answers as Schwab does when it rate
// limits: 429, a body of white space, and no Retry-After.
var tooMany = schwabtest.TooMany

// cut closes the connection without an answer, as a network failure would,
// even after part of an answer is written.
func cut(w http.ResponseWriter) { schwabtest.Cut(w, nil) }

// lastToken returns the access token of the last API request that carried a
// working one, or "" before the first.
func lastToken(f *fakeOAuth) string {
	rs := f.Requests()
	if len(rs) == 0 {
		return ""
	}
	return rs[len(rs)-1].Token
}
