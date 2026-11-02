package schwab

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// correlationHeader is the response header that carries the ID by which
// Schwab's Trader API team can trace a request. Schwab spells it
// Schwab-Client-CorrelID too, which Go's header names treat as the same.
const correlationHeader = "Schwab-Client-CorrelId"

// tooBigCode is the gateway's error code for an answer larger than it
// passes on, such as an option chain without bounds. The same request fails
// the same way every time.
const tooBigCode = "protocol.http.TooBigBody"

var (
	// ErrRateLimited means that Schwab answered 429, or that the rate limit
	// that this package keeps for Schwab's would let the request go only
	// after the caller's deadline. Check for it with errors.Is.
	ErrRateLimited = errors.New("rate limited")
	// ErrResponseTooBig means that the answer was larger than Schwab's
	// gateway passes on, so the request has to ask for less, as with a
	// narrower option chain. Check for it with errors.Is.
	ErrResponseTooBig = errors.New("the answer was too big")
)

// Error is an error response from Schwab.
type Error struct {
	Status int // the HTTP status code
	// CorrelID is the response's correlation ID, for a support request to
	// Schwab, when it has one.
	CorrelID string
	// Reference is the reference number on a page from Schwab's edge
	// network rather than its API, such as Akamai's "Access Denied", which
	// carries no correlation ID.
	Reference string
	// Code is the error code of an OAuth error response, such as
	// invalid_grant, or of a fault from Schwab's gateway, such as
	// protocol.http.TooBigBody. Detail is the code of another OAuth error
	// that Schwab sometimes nests in the description.
	Code, Detail string
	// Message and Errors are what Schwab's other error responses say: the
	// Accounts and Trading API's message and errors, the Market Data API's
	// errors (each one's detail, or else its title, with the parameter or
	// header at fault), or the gateway's fault.
	Message string
	Errors  []string
	// RetryAfter is how long the response's Retry-After header asked to
	// wait, or zero.
	RetryAfter time.Duration
	// Tries is how many times the request was sent, when it was tried more
	// than once.
	Tries int
	// Header is the response's header, without its cookies.
	Header http.Header
	// Body is the response body as sent. An OAuth error's can quote
	// Schwab's internal errors, with a digest of the token, so it is not
	// shown.
	Body string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d from Schwab", e.Status)
	if e.Tries > 1 {
		fmt.Fprintf(&b, " after %d tries", e.Tries)
	}
	switch said := e.says(); {
	case said != "" && e.Code != "":
		fmt.Fprintf(&b, ": %s (%s)", said, e.Code)
	case said != "":
		b.WriteString(": " + said)
	case e.Detail != "":
		fmt.Fprintf(&b, ": %s (%s)", e.Code, e.Detail)
	case e.Code != "":
		b.WriteString(": " + e.Code)
	}
	if e.CorrelID != "" {
		fmt.Fprintf(&b, " (correlation ID %s)", e.CorrelID)
	}
	if e.Reference != "" {
		fmt.Fprintf(&b, " (reference #%s)", e.Reference)
	}
	return b.String()
}

// Is makes a 429 match ErrRateLimited, and the gateway's refusal of a large
// answer ErrResponseTooBig.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrRateLimited:
		return e.Status == http.StatusTooManyRequests
	case ErrResponseTooBig:
		return e.Code == tooBigCode
	}
	return false
}

// says returns what the body says, short: the message and errors, else what
// an HTML page is about, else the start of the body. An OAuth error says
// nothing here, since its description may quote a digest of the token.
func (e *Error) says() string {
	parts := make([]string, 0, 1+len(e.Errors))
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	for _, s := range e.Errors {
		if s != "" && s != e.Message {
			parts = append(parts, s)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	if e.Code != "" {
		return ""
	}
	body := strings.TrimSpace(e.Body)
	if strings.HasPrefix(body, "<") {
		if about := htmlAbout(body); about != "" {
			return "an HTML page, " + about
		}
		return "an HTML page"
	}
	return clip(body, 500)
}

var (
	htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	htmlH1    = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	htmlTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	// htmlNoise is what an HTML page's text holds that is not its content.
	htmlNoise = regexp.MustCompile(`(?is)<(head|script|style)[^>]*>.*?</(head|script|style)>`)
	// edgeRef is the reference number on a page from Akamai, Schwab's edge
	// network.
	edgeRef = regexp.MustCompile(`Reference #([0-9A-Za-z.]+)`)
)

// htmlAbout returns what an HTML page is about: its title, else its first
// heading, else the start of its text.
func htmlAbout(page string) string {
	for _, re := range []*regexp.Regexp{htmlTitle, htmlH1} {
		if m := re.FindStringSubmatch(page); m != nil {
			if s := htmlText(m[1]); s != "" {
				return s
			}
		}
	}
	return clip(htmlText(htmlNoise.ReplaceAllString(page, " ")), 200)
}

// htmlText returns the text of an HTML fragment, its spaces collapsed.
func htmlText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(htmlTag.ReplaceAllString(s, " "))), " ")
}

// clip returns s, cut to at most n bytes with "..." after it if it was
// longer.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// newError makes the error for an answer that is not a success, reading
// whichever of Schwab's error formats its body is in.
func newError(status int, header http.Header, body []byte) *Error {
	e := &Error{Status: status, CorrelID: header.Get(correlationHeader), Header: header.Clone(), Body: string(body)}
	e.Header.Del("Set-Cookie")
	if d, ok := retryAfter(header, time.Now()); ok {
		e.RetryAfter = d
	}
	var v struct {
		Message string          `json:"message"`
		Errors  json.RawMessage `json:"errors"`
		Fault   *struct {
			FaultString string `json:"faultstring"`
			Detail      struct {
				ErrorCode string `json:"errorcode"`
			} `json:"detail"`
		} `json:"fault"`
	}
	if json.Unmarshal(body, &v) == nil {
		e.Message = v.Message
		e.Errors = errorList(v.Errors)
		if v.Fault != nil {
			if e.Message == "" {
				e.Message = v.Fault.FaultString
			}
			e.Code = v.Fault.Detail.ErrorCode
		}
	}
	if e.Code == "" {
		var desc string
		e.Code, desc = oauthError(e.Body)
		if e.Code != "" {
			e.Detail, _ = oauthError(desc)
		}
	}
	if trimmed := strings.TrimSpace(e.Body); strings.HasPrefix(trimmed, "<") {
		if m := edgeRef.FindStringSubmatch(html.UnescapeString(trimmed)); m != nil {
			e.Reference = m[1]
		}
	}
	return e
}

// errorList reads the errors of an error body: strings, as the Accounts and
// Trading API sends them, or objects, as the Market Data API does, whose
// status may be a number or a string. An object's source, the parameter or
// header at fault, is kept with it.
func errorList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var strs []string
	if json.Unmarshal(raw, &strs) == nil {
		return strs
	}
	var objects []struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Source struct {
			Parameter string `json:"parameter"`
			Header    string `json:"header"`
			Pointer   string `json:"pointer"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &objects) != nil {
		return nil
	}
	var list []string
	for _, o := range objects {
		s := o.Detail
		if s == "" {
			s = o.Title
		}
		if s == "" {
			continue
		}
		switch src := o.Source; {
		case src.Parameter != "":
			s += " (parameter " + src.Parameter + ")"
		case src.Header != "":
			s += " (header " + src.Header + ")"
		case src.Pointer != "":
			s += " (at " + src.Pointer + ")"
		}
		list = append(list, s)
	}
	return list
}

// oauthError reads an OAuth error, {"error": ..., "error_description": ...},
// from s, which may have text before it, such as "400 Bad Request: ", and
// may hold it as a JSON string, as Schwab does when it nests one in another's
// description. It returns empty strings when there is none.
func oauthError(s string) (code, description string) {
	var e struct {
		Code        any    `json:"error"`
		Description string `json:"error_description"`
	}
	if i := strings.IndexByte(s, '{'); i >= 0 && json.NewDecoder(strings.NewReader(s[i:])).Decode(&e) == nil {
		code, _ := e.Code.(string)
		return code, e.Description
	}
	if i := strings.IndexByte(s, '"'); i >= 0 {
		var inner string
		if json.NewDecoder(strings.NewReader(s[i:])).Decode(&inner) == nil && inner != s {
			return oauthError(inner)
		}
	}
	return "", ""
}
