package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The receiving middleware (design 5.7) wraps everything the SDK runs for
// a request on the request's goroutine: it recovers a panic, refuses a
// resource URI the server does not serve before the SDK sees it, cuts
// every handler's context by the root context, words protocol errors
// without the secrets and known numbers, and runs the backstop over every
// text and structured content that leaves: the second net, behind
// addTool's own.

// middleware is the server's receiving middleware.
func (s *Server) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("a request panicked", "method", method, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				res, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal error in the server; it is still running"}
			}
		}()
		if r, ok := req.(*mcp.ReadResourceRequest); ok && (r.Params == nil || !s.servesURI(r.Params.URI)) {
			// #1426: the SDK's own not-found error builds its data with %q,
			// which is not JSON for some URIs, and ends the session; and the
			// URI is the client's, which is never echoed.
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "resource not found"}
		}
		ctx, cancel := s.rootContext(ctx)
		defer cancel()
		res, err = next(ctx, method, req)
		if err != nil {
			err = s.sanitize(method, err)
		}
		s.backstopResult(method, res)
		return res, err
	}
}

// servesURI reports whether uri is one of the resources the server serves:
// a fixed resource's URI exactly, or a match of a template's pattern.
func (s *Server) servesURI(uri string) bool {
	return slices.Contains(s.fixedURIs, uri) || slices.ContainsFunc(s.uriPatterns, func(re *regexp.Regexp) bool {
		return re.MatchString(uri)
	})
}

// sanitize returns err, a protocol error, as a client may see it: a
// *jsonrpc.Error keeps its code, its message loses the secrets and the
// known numbers, and its data is dropped for resources/read and wherever
// it is not JSON, and kept elsewhere, since a client of 2026-07-28 needs
// the versions that -32022's data lists. Any other error becomes one of
// the same text.
func (s *Server) sanitize(method string, err error) error {
	clean := func(m string) string { return s.mask.known.Text(s.mask.hide(m)) }
	if we, ok := errors.AsType[*jsonrpc.Error](err); ok {
		out := &jsonrpc.Error{Code: we.Code, Message: clean(we.Message)}
		if method != "resources/read" && len(we.Data) > 0 && json.Valid(we.Data) {
			if data, n, berr := s.mask.backstop(we.Data); berr == nil {
				if n > 0 {
					s.backstopHit(method, n)
				}
				out.Data = data
			}
		}
		if out.Message != we.Message {
			s.backstopHit(method, 1)
		}
		return out
	}
	msg := clean(err.Error())
	if msg != err.Error() {
		s.backstopHit(method, 1)
	}
	return errors.New(msg)
}

// backstopResult runs the backstop over res, a result that leaves: every
// text block of a tool, resource or prompt result, each resource content's
// text and URI, and a tool result's structured content when it is raw JSON,
// the only form the server makes. A tool result whose text was its
// structured content stays so, and a structured content with warnings
// gains value_withheld.
func (s *Server) backstopResult(method string, res mcp.Result) {
	if s.mask.off {
		return
	}
	hits := 0
	text := func(t string) string {
		if hidden := s.mask.known.Text(s.mask.hide(t)); hidden != t {
			hits++
			return hidden
		}
		return t
	}
	switch r := res.(type) {
	case *mcp.CallToolResult:
		if r == nil {
			return
		}
		raw, ok := r.StructuredContent.(json.RawMessage)
		if ok {
			if masked, n, err := s.mask.backstop(raw); err == nil && n > 0 {
				hits += n
				w := warning{Code: "value_withheld", Message: s.mask.compose(valueWithheld)}
				if withWarning, err := withWarning(masked, w); err == nil {
					masked = withWarning
				}
				for _, c := range r.Content {
					if tc, ok := c.(*mcp.TextContent); ok && tc.Text == string(raw) {
						tc.Text = string(masked)
					}
				}
				r.StructuredContent = json.RawMessage(masked)
			}
		}
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				tc.Text = text(tc.Text)
			}
		}
	case *mcp.ReadResourceResult:
		if r == nil {
			return
		}
		for _, c := range r.Contents {
			if c != nil {
				c.Text, c.URI = text(c.Text), text(c.URI)
			}
		}
	case *mcp.GetPromptResult:
		if r == nil {
			return
		}
		r.Description = text(r.Description)
		for _, m := range r.Messages {
			if tc, ok := m.Content.(*mcp.TextContent); ok {
				tc.Text = text(tc.Text)
			}
		}
	}
	if hits > 0 {
		s.backstopHit(method, hits)
	}
}

// withWarning returns raw, an object, with w added to its warnings, when it
// has a list of them; its other members stay byte for byte.
func withWarning(raw []byte, add warning) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return raw, errors.New("not an object")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for first := true; dec.More(); first = false {
		tok, err := dec.Token()
		if err != nil {
			return raw, err
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return raw, err
		}
		if key == "warnings" {
			var ws []json.RawMessage
			if json.Unmarshal(value, &ws) == nil {
				w, err := encode(add)
				if err != nil {
					return raw, err
				}
				if value, err = encode(append(ws, w)); err != nil {
					return raw, err
				}
			}
		}
		k, err := encode(key)
		if err != nil {
			return raw, err
		}
		if !first {
			out.WriteByte(',')
		}
		out.Write(k)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
