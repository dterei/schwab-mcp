package lookup

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// ExpiryWarningWindow is how long before the login lapses the tools start
// to warn. Two days, so that a snapshot scheduled once a day warns at least
// a day ahead, whatever the time of day of the login.
const ExpiryWarningWindow = 48 * time.Hour

// ExpiresIn returns how long a login that lapses at expiresAt has left at
// now, to the nearest hour, and whether it is due for renewal: it lapses
// within ExpiryWarningWindow and has not lapsed yet. A zero expiresAt, a
// time that is not known, is never due.
func ExpiresIn(expiresAt, now time.Time) (left time.Duration, soon bool) {
	if expiresAt.IsZero() {
		return 0, false
	}
	d := expiresAt.Sub(now)
	return d.Round(time.Hour), d > 0 && d <= ExpiryWarningWindow
}

// Kind is what an error from the login or the API means for what to do
// about it.
type Kind int

// The kinds that Classify tells apart.
const (
	// Other is any error of no other kind.
	Other Kind = iota
	// NotLoggedIn is no saved login (schwab.ErrNotLoggedIn).
	NotLoggedIn
	// LoginExpired is a saved login that Schwab refused: its 7 days are up,
	// or a later login replaced it (schwab.ErrLoginExpired).
	LoginExpired
	// AppRejected is Schwab refusing the app key or secret
	// (schwab.ErrAppRejected).
	AppRejected
	// RateLimited is Schwab limiting the requests of the app key, which
	// every program that uses it shares (schwab.ErrRateLimited).
	RateLimited
	// AppNotReady is Schwab's Whitelabel error page, with which it answers
	// until the app says Ready For Use on developer.schwab.com, and for an
	// account type that its API does not serve.
	AppNotReady
	// EdgeRefused is a request that Schwab's edge network turned away:
	// Akamai's 403 page, with a reference number.
	EdgeRefused
	// TokenRefused is Schwab refusing an access token that it had just
	// issued (schwab.ErrTokenRefused), as it does when the app lacks the
	// API product that the request needs.
	TokenRefused
	// InvalidAccount is Schwab refusing the account in the request's path
	// (schwab.ErrInvalidAccount).
	InvalidAccount
	// TooBig is an answer larger than Schwab's gateway passes on
	// (schwab.ErrResponseTooBig).
	TooBig
)

var kindNames = [...]string{
	Other: "Other", NotLoggedIn: "NotLoggedIn", LoginExpired: "LoginExpired", AppRejected: "AppRejected",
	RateLimited: "RateLimited", AppNotReady: "AppNotReady", EdgeRefused: "EdgeRefused",
	TokenRefused: "TokenRefused", InvalidAccount: "InvalidAccount", TooBig: "TooBig",
}

// String returns k's name, such as LoginExpired.
func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
	return kindNames[k]
}

// Classify returns err's kind. An error of two kinds is the first of them
// in the order of the constants, Other aside: a refused login that Schwab
// answered with a 403 page of its edge network is LoginExpired.
func Classify(err error) Kind {
	se, fromSchwab := errors.AsType[*schwab.Error](err)
	switch {
	case errors.Is(err, schwab.ErrNotLoggedIn):
		return NotLoggedIn
	case errors.Is(err, schwab.ErrLoginExpired):
		return LoginExpired
	case errors.Is(err, schwab.ErrAppRejected):
		return AppRejected
	case errors.Is(err, schwab.ErrRateLimited):
		return RateLimited
	case fromSchwab && se.Status == http.StatusInternalServerError && strings.Contains(se.Body, "Whitelabel Error Page"):
		return AppNotReady
	case fromSchwab && se.Status == http.StatusForbidden && se.Reference != "":
		return EdgeRefused
	case errors.Is(err, schwab.ErrTokenRefused):
		return TokenRefused
	case errors.Is(err, schwab.ErrInvalidAccount):
		return InvalidAccount
	case errors.Is(err, schwab.ErrResponseTooBig):
		return TooBig
	}
	return Other
}
