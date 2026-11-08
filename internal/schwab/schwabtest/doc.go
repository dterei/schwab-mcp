// Package schwabtest is a fake of Charles Schwab's Trader API for tests: a
// server that imitates Schwab's token endpoint, its authorize page and the
// account endpoints behind them, with Schwab's rules for tokens, and the
// fixtures it serves, which are answers in the shape Schwab sends them,
// with an index that says what request each file answers and where it
// came from.
//
// It does not import package schwab, so that package's own tests can use
// it as well as everyone else's. It is for tests only. go build would
// compile it into a program without complaint, so a lint rule in
// .golangci.yml refuses an import of it from any file but a test's.
//
// # Using it
//
// New starts a fake over HTTP and NewTLS one over HTTPS, which speaks
// HTTP/2. A client logs in to URL with AppKey, AppSecret, CallbackURL and
// Code, or through the authorize page, as a browser would. LoadFixtures
// gives the fake the fixtures' three accounts, and AddAccount and AddRow
// give it others. Script, Queue, Override and OverridePath make the API
// answer as a test needs, the canned answers, such as TooMany and
// AccessDenied, being Schwab's own; FailTokens does the same for the token
// endpoint. Requests, APICalls, TokenRequests and Grants say what the fake
// was sent.
//
// # What it imitates
//
// The token endpoint takes the app's key and secret by Basic auth and a
// form. A login ends every earlier login's refresh tokens, since Schwab
// allows one login at a time. A refresh gives a new access token and the
// same refresh token, leaves the older access tokens working, and does not
// extend the login's 7 days. An access token lapses expires_in seconds
// after it is issued, 1800 unless SetExpiresIn says otherwise. A refresh
// token that no longer works is refused with Refusal2026, every time. The
// tokens hold ".", "-", "_", "=" and "@", as Schwab's do; see AccessToken.
//
// The authorize page sends the browser to CallbackURL with a code, which
// works once within 30 seconds, a session ID, and the state it was sent.
//
// The API answers a request only with a working access token. Without
// one it answers Schwab's 401, NotAuthorized's body with an ID of its own.
// Each answer the fake builds itself carries a correlation ID and a
// request ID, UUIDs from a counter (00000000-0000-4000-8000-000000000001,
// and so on), the first spelled as Schwab's spec spells it:
// Schwab-Client-CorrelId on a success and Schwab-Client-CorrelID on an
// error.
//
// The account endpoints answer from the fake's accounts, each with its
// rows of transactions, as the bytes a test or a fixture gave: a list is
// its elements as given, joined, and nothing is encoded again, so a
// client meets Schwab's escapes, numbers and repeated keys as they are.
// The accounts are listed in the order they were added; GET /accounts
// sends them without positions unless fields=positions asks for them; an
// account number or hash the fake does not know gets Schwab's 400
// Invalid account number. A window of transactions gets the account's
// rows in it, [] past its history, as TransactionRules says: by default
// at most MaxTransactions, the newest kept, grouped by type in the spec's
// order and newest first within each type. GET /userPreference sends what
// SetUserPreference set, or one built from the accounts. A path under
// /trader/v1/ or /marketdata/v1/ that no Route serves and nothing else
// answers gets a 404 that says it is the fake's.
//
// # How it reads a row
//
// A Row's JSON is any JSON object. Its activityId, if it is an integer or
// a string holding one, is the row's id, which transactions/{id} finds it
// by; a row with any other activityId, or none, has no id, and is served
// in lists only, after the rows with ids at the same time. Its time is
// Row.Time, or else its JSON's, in Schwab's form (2026-08-04T15:22:41+0000)
// or RFC 3339's, with or without a fraction; a row without one is refused,
// since windows need it. Its type is Row.Type, or else its JSON's, if a
// string. Its tradeDate, its legs' symbols and the ~SYM that ends its
// description are read once, leniently, for FilterOnTradeDate and symbol=.
//
// # How it answers an API request
//
//  1. OnAPI's hook sees the request, once it is routed.
//  2. The request is counted, in APICalls; AllRequests counts every
//     request of any kind.
//  3. Without a working access token it gets the 401, and nothing below
//     happens.
//  4. It is recorded, in Requests.
//  5. The answer is the first of: the next Script entry; the route's next
//     Queue entry; the route's Override, or, for a path that no Route
//     serves, OverridePath's handler for it; the answer LoadRecording
//     loaded for exactly this method, path and query; the fake's own. A
//     nil Script or Queue entry is taken from its list but answers
//     nothing, so the request goes on down the list.
//
// Steps 2 to 5 are one step under the fake's lock, and every handler runs
// after the lock is released, so a handler may call the Server's methods.
// So may OnAPI's and OnToken's hooks, which run without the lock. OnGrant's
// hook, the clock given to SetNow and TransactionRules.MaxWindow run under
// the lock, and must not. A method that is given bad input fails the test
// with Errorf, since it may be called on one of the server's goroutines,
// and changes nothing; only New, NewTLS, Fixture, LoadFixtures and
// LoadRecording, which a test calls on its own goroutine, use Fatalf.
//
// # What is a guess, and where the fake departs from Schwab
//
//   - Code works any number of times and never lapses, so that tests can
//     log in again and again; Schwab's codes, and the authorize page's,
//     work once within 30 seconds.
//   - A wrong app key or secret gets invalid_client with 401, a status
//     never recorded.
//   - The token endpoint's answers are application/json and carry no
//     correlation headers; whether Schwab's do is not known.
//   - Every body that is a guess says so in its own text, such as the
//     token endpoint's answer to a bad code, the authorize page's errors,
//     the 400s for a query the fake refuses and the 404 for an unknown id,
//     so that no output can pass one off as Schwab's.
//   - A stale hash gets Invalid account number, as an account number does;
//     that is inferred, not seen.
//   - How the cap meets a request for several types, whether a window's
//     bounds are inclusive, and whether it takes rows by time or by
//     tradeDate are not known. The defaults are guesses, each settable in
//     TransactionRules, as is the longest window, one calendar year.
//   - Query dates are taken in the forms seen taken, RFC 3339 instants
//     with Z or an offset, and no others; a date alone gets the wording
//     Schwab was reported to send.
package schwabtest
