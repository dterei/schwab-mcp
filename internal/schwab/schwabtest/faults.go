package schwabtest

import (
	"io"
	"net/http"
	"slices"
)

// Answer returns a handler that answers with status, the headers given as
// pairs of a name and a value, and body. It adds no header of its own.
func Answer(status int, body string, headers ...string) http.HandlerFunc {
	if len(headers)%2 != 0 {
		panic("schwabtest: Answer takes headers in pairs of a name and a value")
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Add(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// Canned answers, for Script, Queue, Override, OverridePath and
// Failure.Handler. Each that sends a body sends a file of testdata/errors,
// with the status and headers of the file's entry in testdata/index.json.
// Having no fake to count with, the JSON ones carry a fixed correlation
// ID, corr- and the status, spelled as Schwab spells it on an error; the
// pages carry none. Each says whether Schwab has been seen sending it.
var (
	// Cut closes the connection without an answer, as a failure of the
	// network would, even after part of an answer is written. Over HTTP/2,
	// whose connections a handler cannot take over, it resets the stream
	// instead, by panicking with http.ErrAbortHandler. It never reads its
	// request, so Cut(w, nil) will do.
	Cut http.HandlerFunc = func(w http.ResponseWriter, _ *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}
	// Hang answers nothing until the request's context ends: until the
	// client gives up or hangs up, as it does when the context it sent the
	// request with is canceled. t.Context() is canceled just before the end
	// of the test closes the fake; closing the fake does not end a hang but
	// waits for it, so a hang that nothing ends holds the test up. Hang
	// reads the request's body first: until the body is read, the server
	// does not watch the connection, and would not see the client hang up.
	Hang http.HandlerFunc = func(_ http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		<-r.Context().Done()
	}

	// TooMany is Schwab's rate limit: 429 with a body of white space,
	// Schwab-Client-CorrelID corr-429, X-Request-Id req-429 and no
	// Retry-After. Seen live.
	TooMany = canned("errors/rate_limit_429.txt")
	// InvalidAccount is 400 {"message":"Invalid account number"}, Schwab's
	// answer to an account number in place of a hash. Seen live.
	InvalidAccount = canned("errors/invalid_account_400.json")
	// InvalidAccountList is InvalidAccount's message in a list of one. One
	// report.
	InvalidAccountList = canned("errors/invalid_account_list_400.json")
	// NotAuthorized is 401 "Client not authorized", in the Market Data
	// API's shape, with the fixed UUID 00000000-0000-4000-8000-000000000000
	// as its errors[].id: Schwab's answer to a token it does not take, and
	// to a product the app lacks. Seen live.
	NotAuthorized = canned("errors/not_authorized_401.json")
	// InvalidAPIKey is 401 with the gateway's fault oauth.v2.InvalidApiKey.
	// It is Apigee's documented fault, never seen from Schwab.
	InvalidAPIKey = canned("errors/invalid_api_key_401.json")
	// AccessDenied is 403 with the "Access Denied" page of Schwab's edge
	// network, Akamai, whose text is in character references: a Reference
	// #, AkamaiReference, and no correlation ID. Seen live.
	AccessDenied = canned("errors/akamai_403.html")
	// Whitelabel is 500 with Spring's Whitelabel Error Page, as for an app
	// that is not yet Ready For Use. Reported.
	Whitelabel = canned("errors/whitelabel_500.html")
	// TooBigBody is 502 with the gateway's fault protocol.http.TooBigBody,
	// for an answer too large to pass on. Seen live, from Market Data.
	TooBigBody = canned("errors/too_big_body_502.json")
	// Unavailable is 503 with the gateway's default fault for a service
	// that is down. Inferred from Apigee's documentation.
	Unavailable = canned("errors/unavailable_503.json")
	// GatewayTimeout is 504 with the gateway's default fault for a timeout.
	// Inferred from Apigee's documentation.
	GatewayTimeout = canned("errors/gateway_timeout_504.json")
)

// The token endpoint's refusals of a refresh token, for SetRefusal and
// Failure.Body, as community clients have logged them.
var (
	// Refusal2026 is the refusal of a refresh token that lapsed or was
	// revoked, as logged in 2026: unsupported_token_type, with
	// invalid_grant nested in its description. The fake's refusal unless
	// SetRefusal sets another.
	Refusal2026 = string(mustFixture("errors/refusal_2026_400.json"))
	// Refusal2024 is the refusal of a refresh token that a later login
	// replaced, as logged in 2024: unsupported_token_type, with
	// refresh_token_authentication_error nested in its description as a
	// quoted string, its token digests written abc.
	Refusal2024 = string(mustFixture("errors/refusal_2024_400.json"))
	// RefusalInvalidClient is the refusal as schwab-py's documentation of
	// 2024 gives it: invalid_client, "refresh token invalid". No log shows
	// it.
	RefusalInvalidClient = string(mustFixture("errors/refusal_invalid_client_400.json"))
)

// cannedID is the errors[].id of NotAuthorized, which the fake's own 401
// replaces with one from its counter.
const cannedID = "00000000-0000-4000-8000-000000000000"

// notAuthorizedBody is NotAuthorized's body.
var notAuthorizedBody = string(mustFixture("errors/not_authorized_401.json"))

// canned returns a handler that sends file as its index entry says.
func canned(file string) http.HandlerFunc {
	for _, e := range Index() {
		if e.File == file {
			return replay(e.Response.Status, e.Response.Headers, mustFixture(file))
		}
	}
	panic("schwabtest: testdata/index.json has no entry for " + file)
}

// replay returns a handler that sends status, headers, each name as it is
// given, and body.
func replay(status int, headers map[string][]string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		for k, v := range headers {
			h[k] = slices.Clone(v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}
