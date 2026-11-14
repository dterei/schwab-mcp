package lookup

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Each kind of error from the login or the API is told apart, wrapped or
// not, and an error of two kinds is the first of them: the order in which
// schwabtx gave its hints.
func TestClassify(t *testing.T) {
	whitelabel := &schwab.Error{
		Status: http.StatusInternalServerError,
		Body:   "<html><body><h1>Whitelabel Error Page</h1><p>This application has no explicit mapping for /error</p></body></html>",
	}
	edge := &schwab.Error{
		Status:    http.StatusForbidden,
		Body:      "<HTML><HEAD><TITLE>Access Denied</TITLE></HEAD><BODY>Reference #18.2b3c4d5e.1700000000.abcdef</BODY></HTML>",
		Reference: "18.2b3c4d5e.1700000000.abcdef",
	}
	tooBig := &schwab.Error{Status: http.StatusInternalServerError, Code: "protocol.http.TooBigBody"}
	for _, tc := range []struct {
		name string
		err  error
		want Kind
	}{
		{"no login", fmt.Errorf("reading the token file: %w", schwab.ErrNotLoggedIn), NotLoggedIn},
		{"a refused login", fmt.Errorf("%w: %w", schwab.ErrLoginExpired, &schwab.Error{Status: http.StatusBadRequest}), LoginExpired},
		{"a refused app", fmt.Errorf("%w: %w", schwab.ErrAppRejected, &schwab.Error{Status: http.StatusUnauthorized}), AppRejected},
		{"a 429", &schwab.Error{Status: http.StatusTooManyRequests}, RateLimited},
		{"a wait past the deadline", fmt.Errorf("waiting for the rate limit: %w", schwab.ErrRateLimited), RateLimited},
		{"the Whitelabel page", whitelabel, AppNotReady},
		{"the Whitelabel page, wrapped", fmt.Errorf("fetching accounts: %w", whitelabel), AppNotReady},
		{"another 500 page", &schwab.Error{Status: http.StatusInternalServerError, Body: "<html>Internal Server Error</html>"}, Other},
		{"Whitelabel words on a 502", &schwab.Error{Status: http.StatusBadGateway, Body: whitelabel.Body}, Other},
		{"the edge's 403", edge, EdgeRefused},
		{"a 403 without a reference", &schwab.Error{Status: http.StatusForbidden, Body: "{}"}, Other},
		{"the edge's 503", &schwab.Error{Status: http.StatusServiceUnavailable, Reference: edge.Reference}, Other},
		{"a refused token", fmt.Errorf("%w: %w", schwab.ErrTokenRefused, &schwab.Error{Status: http.StatusUnauthorized}), TokenRefused},
		{"an invalid account", &schwab.Error{Status: http.StatusBadRequest, Message: "Invalid account number"}, InvalidAccount},
		{"an answer too big", tooBig, TooBig},
		{"another 400", &schwab.Error{Status: http.StatusBadRequest, Message: "Bad request"}, Other},
		{"a network failure", &url.Error{Op: "Get", URL: "https://api.schwabapi.com/trader/v1/accounts", Err: errors.New("connection refused")}, Other},
		{"no error", nil, Other},

		{"a refused login on the edge's page", fmt.Errorf("%w: %w", schwab.ErrLoginExpired, edge), LoginExpired},
		{"a refused token on the Whitelabel page", fmt.Errorf("%w: %w", schwab.ErrTokenRefused, whitelabel), AppNotReady},
		{"an invalid account too big", &schwab.Error{Status: http.StatusBadRequest, Message: "Invalid account number", Code: tooBig.Code}, InvalidAccount},
		{"too big and rate limited", &schwab.Error{Status: http.StatusTooManyRequests, Code: tooBig.Code}, RateLimited},
	} {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// A login is due for renewal from 48 hours before it lapses, not after;
// the time left is to the nearest hour, outside the window too.
func TestExpiresIn(t *testing.T) {
	now := time.Date(2026, 11, 13, 16, 40, 0, 0, time.UTC)
	for _, tc := range []struct {
		left time.Duration // until the login lapses
		want time.Duration
		soon bool
	}{
		{88*time.Hour + 10*time.Minute, 88 * time.Hour, false},
		{49 * time.Hour, 49 * time.Hour, false},
		{48*time.Hour + time.Nanosecond, 48 * time.Hour, false},
		{48 * time.Hour, 48 * time.Hour, true},
		{36*time.Hour + 20*time.Minute, 36 * time.Hour, true},
		{90 * time.Minute, 2 * time.Hour, true},
		{time.Hour, time.Hour, true},
		{20 * time.Minute, 0, true},
		{time.Nanosecond, 0, true},
		{0, 0, false},
		{-time.Hour, -time.Hour, false},
	} {
		if left, soon := ExpiresIn(now.Add(tc.left), now); left != tc.want || soon != tc.soon {
			t.Errorf("%v left: ExpiresIn = %v, %t; want %v, %t", tc.left, left, soon, tc.want, tc.soon)
		}
	}
	if left, soon := ExpiresIn(time.Time{}, now); left != 0 || soon {
		t.Errorf("no time known: ExpiresIn = %v, %t; want 0, false", left, soon)
	}
}
