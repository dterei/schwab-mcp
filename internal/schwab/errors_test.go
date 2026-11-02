package schwab

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// akamaiPage is the page Schwab's edge network, Akamai, sends when it
// refuses a request, as community clients have logged it: its text is
// written with character references.
const akamaiPage = `<HTML><HEAD>
<TITLE>Access Denied</TITLE>
</HEAD><BODY>
<H1>Access Denied</H1>

You don't have permission to access "http&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;v1&#47;oauth&#47;token" on this server.<P>
Reference&#32;&#35;18&#46;9efa3f17&#46;1792539138&#46;32dec4a
<P>https&#58;&#47;&#47;errors&#46;edgesuite&#46;net&#47;18&#46;9efa3f17&#46;1792539138&#46;32dec4a</P>
</BODY>
</HTML>
`

// whitelabelPage is Spring's page for an error without a page of its own,
// which Schwab sends for an app that is not yet Ready For Use.
const whitelabelPage = `<html><body><h1>Whitelabel Error Page</h1><p>This application has no configured error view, so you are seeing this as a fallback.</p><div id='created'>Mon Nov 02 10:01:02 EST 2026</div><div>There was an unexpected error (type=Internal Server Error, status=500).</div></body></html>`

func TestErrorFormats(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		header  []string
		correl  string
		message string
		errors  []string
		code    string
		ref     string
		want    string
	}{
		{
			name: "Accounts and Trading", status: 400, header: []string{"Schwab-Client-CorrelId", "c-1"}, correl: "c-1",
			body:    `{"message":"Invalid date range","errors":["startDate must be before endDate"]}`,
			message: "Invalid date range", errors: []string{"startDate must be before endDate"},
			want: "HTTP 400 from Schwab: Invalid date range; startDate must be before endDate (correlation ID c-1)",
		},
		{
			name: "Accounts and Trading, a message alone, with the header spelled CorrelID", status: 400,
			header: []string{"Schwab-Client-CorrelID", "c-2"}, correl: "c-2",
			body:    `{"message":"Invalid account number"}`,
			message: "Invalid account number",
			want:    "HTTP 400 from Schwab: Invalid account number (correlation ID c-2)",
		},
		{
			name: "Accounts and Trading, a 401 in the Market Data format", status: 401,
			body:   `{"errors":[{"id":"1a2345b6-0000-0000-0000-000000000000","status":401,"title":"Unauthorized","detail":"Client not authorized"}]}`,
			errors: []string{"Client not authorized"},
			want:   "HTTP 401 from Schwab: Client not authorized",
		},
		{
			name: "Market Data, with the status as a string and the header at fault", status: 400,
			body:   `{"errors":[{"id":"6e0c","status":"400","title":"Bad Request","detail":"Missing header","source":{"header":"Authorization"}}]}`,
			errors: []string{"Missing header (header Authorization)"},
			want:   "HTTP 400 from Schwab: Missing header (header Authorization)",
		},
		{
			name: "Market Data, with the parameter at fault", status: 400,
			body:   `{"errors":[{"id":"9","status":"400","title":"Bad Request","detail":"Search combination should not exceed 500.","source":{"parameter":"symbols"}}]}`,
			errors: []string{"Search combination should not exceed 500. (parameter symbols)"},
			want:   "HTTP 400 from Schwab: Search combination should not exceed 500. (parameter symbols)",
		},
		{
			name: "Market Data, with the attributes at fault, as its spec gives them", status: 400,
			body:   `{"errors":[{"id":"a208a739-0000","status":"400","title":"Bad Request","detail":"Search combination should have min of 1.","source":{"pointer":["/data/attributes/symbols","/data/attributes/cusips","/data/attributes/ssids"]}}]}`,
			errors: []string{"Search combination should have min of 1. (at /data/attributes/symbols, /data/attributes/cusips, /data/attributes/ssids)"},
			want:   "HTTP 400 from Schwab: Search combination should have min of 1. (at /data/attributes/symbols, /data/attributes/cusips, /data/attributes/ssids)",
		},
		{
			name: "Market Data, with an entry in another form among the others", status: 400,
			body:   `{"errors":[{"detail":"Bad field","source":{"parameter":"fields"}},42,{"title":"Unauthorized","source":{"header":"Authorization"}}]}`,
			errors: []string{"Bad field (parameter fields)", "Unauthorized (header Authorization)"},
			want:   "HTTP 400 from Schwab: Bad field (parameter fields); Unauthorized (header Authorization)",
		},
		{
			name: "Market Data, with the status as a number and only a title", status: 404,
			body:   `{"errors":[{"id":"7a1","status":404,"title":"Not Found"}]}`,
			errors: []string{"Not Found"},
			want:   "HTTP 404 from Schwab: Not Found",
		},
		{
			name: "the gateway", status: 502,
			body:    `{"fault":{"faultstring":"Body buffer overflow","detail":{"errorcode":"protocol.http.TooBigBody"}}}`,
			message: "Body buffer overflow", code: tooBigCode,
			want: "HTTP 502 from Schwab: Body buffer overflow (protocol.http.TooBigBody)",
		},
		{
			name: "Schwab's rate limit", status: 429, body: " \n",
			header: []string{"Schwab-Client-CorrelId", "c-3"}, correl: "c-3",
			want: "HTTP 429 from Schwab (correlation ID c-3)",
		},
		{
			name: "Akamai's edge page", status: 403, body: akamaiPage,
			ref:  "18.9efa3f17.1792539138.32dec4a",
			want: "HTTP 403 from Schwab: an HTML page, Access Denied (reference #18.9efa3f17.1792539138.32dec4a)",
		},
		{
			name: "an HTML page with a heading but no title", status: 500, body: whitelabelPage,
			want: "HTTP 500 from Schwab: an HTML page, Whitelabel Error Page",
		},
		{
			name: "an HTML page with neither", status: 502,
			body: "<html><head><style>p{}</style></head><body><p>Bad&nbsp;gateway</p>\n<p>try later</p></body></html>",
			want: "HTTP 502 from Schwab: an HTML page, Bad gateway try later",
		},
		{
			name: "plain text", status: 500, body: "  oops  ",
			want: "HTTP 500 from Schwab: oops",
		},
		{
			name: "no body", status: 503,
			want: "HTTP 503 from Schwab",
		},
		{
			name: "a message and the same error", status: 400,
			body:    `{"message":"Bad request","errors":["Bad request"]}`,
			message: "Bad request", errors: []string{"Bad request"},
			want: "HTTP 400 from Schwab: Bad request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			for i := 0; i+1 < len(tc.header); i += 2 {
				header.Set(tc.header[i], tc.header[i+1])
			}
			e := newError(tc.status, header, []byte(tc.body))
			if e.Message != tc.message || !slices.Equal(e.Errors, tc.errors) || e.CorrelID != tc.correl ||
				e.Code != tc.code || e.Reference != tc.ref || e.Body != tc.body {
				t.Errorf("read %#v", e)
			}
			if got := e.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}

	long := newError(http.StatusInternalServerError, http.Header{}, []byte(strings.Repeat("x", 600)))
	if got := long.Error(); !strings.HasSuffix(got, strings.Repeat("x", 500)+"...") {
		t.Errorf("a long body: %q, want its first 500 bytes", got)
	}
	// Cut where a character begins, so that the text stays UTF-8.
	accents := newError(http.StatusInternalServerError, http.Header{}, []byte("x"+strings.Repeat("é", 300)))
	if got := accents.Error(); !utf8.ValidString(got) || !strings.HasSuffix(got, "é...") {
		t.Errorf("a long body of accents: %q, want it cut before a character", got)
	}
	// A large HTML page is read for what it is about only near its start.
	page := "<html><body><p>Something went wrong</p>" + strings.Repeat("<p>filler</p>", 1<<20) + "<title>Late</title></body></html>"
	if got := newError(http.StatusBadGateway, http.Header{}, []byte(page)).Error(); !strings.HasPrefix(got, "HTTP 502 from Schwab: an HTML page, Something went wrong filler") {
		t.Errorf("a large page: %.100q", got)
	}
	if e := newError(http.StatusForbidden, http.Header{}, []byte("<html>Reference #18.abc.</html>")); e.Reference != "18.abc" {
		t.Errorf("a reference at the end of a sentence: %q", e.Reference)
	}
}

func TestErrorIs(t *testing.T) {
	for _, tc := range []struct {
		e               *Error
		limited, tooBig bool
	}{
		{newError(http.StatusTooManyRequests, http.Header{}, nil), true, false},
		{newError(http.StatusBadGateway, http.Header{}, []byte(`{"fault":{"faultstring":"x","detail":{"errorcode":"protocol.http.TooBigBody"}}}`)), false, true},
		{newError(http.StatusBadGateway, http.Header{}, []byte(`{"fault":{"faultstring":"x","detail":{"errorcode":"other"}}}`)), false, false},
		{newError(http.StatusServiceUnavailable, http.Header{}, nil), false, false},
	} {
		if got := errors.Is(tc.e, ErrRateLimited); got != tc.limited {
			t.Errorf("%v: Is(ErrRateLimited) = %v", tc.e, got)
		}
		if got := errors.Is(tc.e, ErrResponseTooBig); got != tc.tooBig {
			t.Errorf("%v: Is(ErrResponseTooBig) = %v", tc.e, got)
		}
	}
}

// The error keeps the response's header, without its cookies, and what
// Retry-After asked.
func TestErrorKeepsTheHeader(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "12")
	header.Set("Set-Cookie", "session=secret")
	header.Set("X-Request-Id", "r-1")
	e := newError(http.StatusTooManyRequests, header, nil)
	if e.RetryAfter != 12*time.Second || e.Header.Get("X-Request-Id") != "r-1" || e.Header.Get("Set-Cookie") != "" {
		t.Errorf("RetryAfter %v, header %v", e.RetryAfter, e.Header)
	}
	if header.Get("Set-Cookie") == "" {
		t.Error("the response's own header lost its cookie")
	}
}
