package schwab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// correlationHeader is the response header that carries the ID by which
// Schwab's Trader API team can trace a request.
const correlationHeader = "Schwab-Client-CorrelId"

// Error is an error response from Schwab.
type Error struct {
	Status int // the HTTP status code
	// CorrelID is the response's correlation ID, for a support request to
	// Schwab, when it has one.
	CorrelID string
	// Code is the error code of an OAuth error response, such as
	// invalid_grant. Detail is the code of another OAuth error that Schwab
	// sometimes nests in the description.
	Code, Detail string
	// Message and Errors are what Schwab's other error responses say: the
	// Accounts and Trading API's message and errors, the Market Data API's
	// errors (each one's detail, or else its title), or the gateway's fault.
	Message string
	Errors  []string
	// Body is the response body as sent. An OAuth error's can quote
	// Schwab's internal errors, with a digest of the token, so it is not
	// shown.
	Body string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d from Schwab", e.Status)
	switch said := e.says(); {
	case e.Detail != "":
		fmt.Fprintf(&b, ": %s (%s)", e.Code, e.Detail)
	case e.Code != "":
		fmt.Fprintf(&b, ": %s", e.Code)
	case said != "":
		b.WriteString(": " + said)
	}
	if e.CorrelID != "" {
		fmt.Fprintf(&b, " (correlation ID %s)", e.CorrelID)
	}
	return b.String()
}

// says returns what the body says, short: the message and errors, else the
// title of an HTML page, else the start of the body.
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
	body := strings.TrimSpace(e.Body)
	if strings.HasPrefix(body, "<") {
		if m := htmlTitle.FindStringSubmatch(body); m != nil {
			return "an HTML page, " + strings.Join(strings.Fields(m[1]), " ")
		}
		return "an HTML page"
	}
	if len(body) > 500 {
		body = body[:500] + "..."
	}
	return body
}

var htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// newError makes the error for an answer that is not a success, reading
// whichever of Schwab's error formats its body is in.
func newError(status int, header http.Header, body []byte) *Error {
	e := &Error{Status: status, CorrelID: header.Get(correlationHeader), Body: string(body)}
	var v struct {
		Message string          `json:"message"`
		Errors  json.RawMessage `json:"errors"`
		Fault   *struct {
			FaultString string `json:"faultstring"`
		} `json:"fault"`
	}
	if json.Unmarshal(body, &v) == nil {
		e.Message = v.Message
		if v.Fault != nil && e.Message == "" {
			e.Message = v.Fault.FaultString
		}
		e.Errors = errorList(v.Errors)
	}
	var desc string
	e.Code, desc = oauthError(e.Body)
	if e.Code != "" {
		e.Detail, _ = oauthError(desc)
	}
	return e
}

// errorList reads the errors of an error body: strings, as the Accounts and
// Trading API sends them, or objects, as the Market Data API does, whose
// status may be a number or a string.
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
	}
	if json.Unmarshal(raw, &objects) != nil {
		return nil
	}
	var list []string
	for _, o := range objects {
		if o.Detail != "" {
			list = append(list, o.Detail)
		} else if o.Title != "" {
			list = append(list, o.Title)
		}
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
