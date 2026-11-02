package schwab

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestErrorFormats(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		correl  string
		message string
		errors  []string
		want    string
	}{
		{
			name: "Accounts and Trading", status: 400, correl: "c-1",
			body:    `{"message":"Invalid date range","errors":["startDate must be before endDate"]}`,
			message: "Invalid date range", errors: []string{"startDate must be before endDate"},
			want: "HTTP 400 from Schwab: Invalid date range; startDate must be before endDate (correlation ID c-1)",
		},
		{
			name: "Market Data, with the status as a string", status: 400,
			body:   `{"errors":[{"id":"6e0c","status":"400","title":"Bad Request","detail":"Missing header","source":{"header":"Authorization"}}]}`,
			errors: []string{"Missing header"},
			want:   "HTTP 400 from Schwab: Missing header",
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
			message: "Body buffer overflow",
			want:    "HTTP 502 from Schwab: Body buffer overflow",
		},
		{
			name: "an HTML page", status: 403,
			body: "<html><head><title>Access\n Denied</title></head><body>Reference #18.1</body></html>",
			want: "HTTP 403 from Schwab: an HTML page, Access Denied",
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
			if tc.correl != "" {
				header.Set("Schwab-Client-CorrelId", tc.correl)
			}
			e := newError(tc.status, header, []byte(tc.body))
			if e.Message != tc.message || !slices.Equal(e.Errors, tc.errors) || e.CorrelID != tc.correl || e.Body != tc.body {
				t.Errorf("read %+v", e)
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
}
