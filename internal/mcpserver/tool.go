package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A handler computes a tool's output. It never sees raw arguments, never
// encodes output, and reports failure with an error that toolError words.
type handler[In, Out any] func(ctx context.Context, c *call, in In) (Out, error)

// toolDef is what a tool is, beside its handler and its types.
type toolDef struct {
	name, title, description string
	// annotations are the tool's hints, every one set (design 10.0). The
	// zero value is a read-only tool's: read-only, not destructive,
	// idempotent, and of a closed world, titled title.
	annotations mcp.ToolAnnotations
	// slot says that each call takes one of the call slots: every tool but
	// auth_status, which reads only local files.
	slot bool
	// args are the rules of the tool's arguments, applied over argRules.
	args map[string]argRule
}

// call is what a handler works with, made for each call.
type call struct {
	s        *Server
	tool     string
	mask     *masker
	accounts *directory
	store    *storeHandle
	// now is the server's clock at the call's start, which the answer is as
	// of; budget is the call's budget and deadline when it ends, by the
	// system clock.
	now      time.Time
	budget   time.Duration
	deadline time.Time
	// warnings are what the answer will say, in the order they came.
	warnings []warning
	// cl is the client, once client has checked the login.
	cl *schwab.Client
}

// warn adds the warning code to the answer, saying what format and args
// compose (masker.compose). A warning of a code already given is added
// again only with another message.
func (c *call) warn(code Ident, format string, args ...any) {
	w := warning{Code: code, Message: c.mask.compose(format, args...)}
	if !slices.Contains(c.warnings, w) {
		c.warnings = append(c.warnings, w)
	}
}

// maxArguments is the most bytes of arguments a call may send.
const maxArguments = 64 << 10

// knownArgument is the shape of an argument's name that an error may echo.
var knownArgument = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)

// A tool is a registered tool: its handler, and its schemas, resolved once.
type tool[In, Out any] struct {
	s     *Server
	def   toolDef
	h     handler[In, Out]
	in    *jsonschema.Schema
	inRes *jsonschema.Resolved
	out   *jsonschema.Resolved
}

// A toolInfo is what the tests need of a registered tool.
type toolInfo struct {
	name          string
	in, out       reflect.Type
	inSch, outSch *jsonschema.Schema
}

// addTool registers a tool: its schemas are built from In and Out
// (schema.go) and held to the portable subset, and each call is checked,
// run and encoded as design 4.2 says. Out must embed envelope, and embed
// cuts when it is a fitter. A bad tool panics, at startup: TestNew covers
// every tool.
func addTool[In, Out any](s *Server, def toolDef, h handler[In, Out]) {
	sch, err := schemasOf[In, Out](def)
	if err != nil {
		panic(err.Error())
	}
	in, out, outType := sch.in, sch.out, reflect.TypeFor[Out]()
	t := &tool[In, Out]{s: s, def: def, h: h, in: in, inRes: sch.inRes, out: sch.outRes}
	ann := def.annotations
	if ann == (mcp.ToolAnnotations{}) {
		ann = mcp.ToolAnnotations{
			ReadOnlyHint: true, DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false),
			Title: def.title,
		}
	}
	s.tools = append(s.tools, toolInfo{name: def.name, in: reflect.TypeFor[In](), out: outType, inSch: in, outSch: out})
	s.runners[def.name] = t.call
	s.mcp.AddTool(&mcp.Tool{
		Name: def.name, Title: def.title, Description: def.description,
		InputSchema: in, OutputSchema: out, Annotations: &ann,
	}, t.serve)
}

// serve answers one call of the tool: its answer's bytes as the one text
// block and as the structured content, or its error.
func (t *tool[In, Out]) serve(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	b, e := t.call(ctx, req.Params.Arguments)
	if e != nil {
		return e.result(), nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: json.RawMessage(b),
	}, nil
}

// call runs one call of the tool, in design 4.2's order, inside a recover:
// a panic is an internal error, and the server lives on. It returns the
// answer's bytes, or the error that the answer is instead, whose text has
// passed the backstop.
func (t *tool[In, Out]) call(ctx context.Context, raw json.RawMessage) (b []byte, e *toolErr) {
	s := t.s
	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("a tool panicked", "tool", t.def.name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			b, e = nil, s.errorFor(codeInternal)
		}
		outcome := "ok"
		if e != nil {
			outcome = string(e.code)
			if clean := s.mask.cleanErr(e); clean != e {
				s.backstopHit(t.def.name, 1)
				e = clean
			}
		}
		s.log.Info("call", "tool", t.def.name, "duration", time.Since(start).Round(time.Millisecond),
			"outcome", outcome, "bytes", len(b))
	}()
	defer s.begin()()
	c := &call{
		s: s, tool: t.def.name, mask: s.mask, accounts: s.accounts, store: s.store,
		now: s.o.Now(), budget: s.o.CallTimeout, deadline: start.Add(s.o.CallTimeout),
	}
	out, err := t.run(ctx, c, raw)
	if err == nil {
		b, err = t.answer(c, &out)
	}
	if err != nil {
		if _, worded := errors.AsType[*toolErr](err); !worded && ctx.Err() != nil {
			// The request ended: the client cancelled it or closed its input,
			// or a signal ended the server. The SDK ends a request at the
			// input's end with io.EOF as the cause, which an HTTP request to
			// Schwab then fails with; that is the call's end, not Schwab's.
			return nil, s.errorFor(codeTimedOut)
		}
		return nil, s.toolError(t.def.name, err)
	}
	return b, nil
}

// run checks and decodes the arguments, takes a slot and the budget, and
// runs the handler: steps 1 to 5.
func (t *tool[In, Out]) run(ctx context.Context, c *call, raw json.RawMessage) (Out, error) {
	var zero Out
	in, err := t.arguments(c, raw)
	if err != nil {
		return zero, err
	}
	ctx, cancel := context.WithDeadline(ctx, c.deadline)
	defer cancel()
	if t.def.slot {
		release, err := t.s.takeSlot(ctx)
		if err != nil {
			return zero, err
		}
		defer release()
	}
	return t.h(ctx, c, in)
}

// arguments checks raw, the call's arguments, against the input schema and
// decodes them into In: steps 1 to 3. Its errors name the argument and
// what it takes, and never quote what the model sent.
func (t *tool[In, Out]) arguments(c *call, raw json.RawMessage) (In, error) {
	var in In
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		raw = json.RawMessage("{}")
	}
	notObject := c.failf(codeBadArgument, "The arguments must be a JSON object of at most 64 KiB.")
	if len(raw) > maxArguments {
		return in, notObject
	}
	// Steps 1 and 2 decode into any, numbers as float64, as jsonschema-go
	// checks them: it would take a json.Number for a string.
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return in, notObject
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return in, notObject
	}
	for _, name := range slices.Sorted(maps.Keys(obj)) {
		if _, ok := t.in.Properties[name]; !ok {
			return in, t.unknownArgument(c, name)
		}
	}
	for _, name := range t.in.Required {
		if _, ok := obj[name]; !ok {
			return in, c.failf(codeBadArgument, "%s is required; it takes %s.", Ident(name), takes(t.in.Properties[name]))
		}
	}
	if err := t.inRes.Validate(obj); err != nil {
		if m := propertyAt.FindStringSubmatch(err.Error()); m != nil {
			if p, ok := t.in.Properties[m[1]]; ok {
				return in, c.failf(codeBadArgument, "%s takes %s.", Ident(m[1]), takes(p))
			}
		}
		return in, c.failf(codeBadArgument, "The arguments do not match the tool's input schema.")
	}
	// Step 3, the one decode with UseNumber, into In, of the arguments with
	// each integral number written as an integer: the schema check takes
	// 5.0 and 1e1 as integers, as JSON Schema does, and some clients send
	// every number with a fraction.
	dec := json.NewDecoder(bytes.NewReader(integral(raw)))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			if p, ok := t.in.Properties[te.Field]; ok {
				return in, c.failf(codeBadArgument, "%s takes %s.", Ident(te.Field), takes(p))
			}
		}
		return in, c.failf(codeBadArgument, "The arguments do not match the tool's input schema.")
	}
	return in, nil
}

// integral returns raw, JSON that decodes, with each number that is an
// integer but written with a fraction or an exponent, such as 5.0 or 1e1,
// written as the integer, exactly, however large; raw itself when it holds
// none.
func integral(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	changed := false
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		case json.Number:
			if !strings.ContainsAny(string(x), ".eE") {
				return x
			}
			f, _, err := big.ParseFloat(string(x), 10, 1024, big.ToNearestEven)
			if err == nil && f.IsInt() {
				n, _ := f.Int(nil)
				changed = true
				return json.Number(n.String())
			}
		}
		return v
	}
	v = walk(v)
	if !changed {
		return raw
	}
	b, err := encode(v)
	if err != nil {
		return raw
	}
	return b
}

// unknownArgument is the error for an argument that the tool does not
// take, named only when its name has the shape of one.
func (t *tool[In, Out]) unknownArgument(c *call, name string) error {
	names := slices.Sorted(maps.Keys(t.in.Properties))
	takesNone := "This tool takes no arguments."
	if len(names) > 0 {
		takesNone = "This tool takes " + strings.Join(names, ", ") + "."
	}
	if knownArgument.MatchString(name) {
		return c.failf(codeBadArgument, "There is no argument %s. "+takesNone, Ident(name))
	}
	return c.failf(codeBadArgument, "An argument has a name this tool does not take. "+takesNone)
}

// answer encodes out, the handler's output: the warnings and the cuts set,
// through the backstop, checked against the output schema, and fitted to
// the cap: steps 6 to 8.
func (t *tool[In, Out]) answer(c *call, out *Out) ([]byte, error) {
	env := any(out).(enveloped).env()
	if cs, ok := any(out).(cutter); ok {
		cs.cutRecord().Cut = []cutList{}
	}
	if env.AsOf == "" {
		env.AsOf = instantOf(c.now)
	}
	if !slices.Contains(sources, env.Source) {
		return nil, fmt.Errorf("%s answered from the source %q, which is none of the server's", t.def.name, env.Source)
	}
	b, err := t.encodeOut(c, out, env)
	if err != nil {
		return nil, err
	}
	if len(b) <= t.s.o.MaxBytes {
		return b, nil
	}
	f, ok := any(out).(fitter)
	if !ok {
		return nil, t.s.errorFor(codeTooBig)
	}
	if f.dropRaw() {
		c.warn("raw_omitted", "Schwab's JSON was left out to stay under the size cap.")
		if b, err = t.encodeOut(c, out, env); err != nil {
			return nil, err
		}
	}
	record := any(out).(cutter).cutRecord()
	for len(b) > t.s.o.MaxBytes {
		longest, size := cuttable{}, -1
		for _, l := range f.lists() {
			if l.count() == 0 {
				continue
			}
			lb, err := encode(l.value)
			if err != nil {
				return nil, err
			}
			if len(lb) > size {
				longest, size = l, len(lb)
			}
		}
		if size < 0 {
			return nil, t.s.errorFor(codeTooBig)
		}
		total := longest.count()
		longest.keep(total / 2)
		i := slices.IndexFunc(record.Cut, func(cl cutList) bool { return cl.Path == Ident(longest.path) })
		if i < 0 {
			record.Cut = append(record.Cut, cutList{Path: Ident(longest.path), Shown: total / 2, Total: total})
		} else {
			record.Cut[i].Shown = total / 2
		}
		hint := ""
		if h, ok := any(out).(interface{ cutHint() string }); ok {
			hint = " " + h.cutHint()
		}
		c.warn("list_cut", "Lists were cut to stay under the size cap; cut names each, with how many of its items it shows of how many."+hint)
		if b, err = t.encodeOut(c, out, env); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// encodeOut encodes out with the call's warnings, through the backstop,
// and checks the bytes against the output schema: steps 6 and 7. A value
// that the backstop replaced adds the warning value_withheld.
func (t *tool[In, Out]) encodeOut(c *call, out *Out, env *envelope) ([]byte, error) {
	b, n, err := t.encodeOnce(c, out, env)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		t.s.backstopHit(t.def.name, n)
		c.warn("value_withheld", valueWithheld)
		if b, _, err = t.encodeOnce(c, out, env); err != nil {
			return nil, err
		}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("decoding %s's own answer: %w", t.def.name, err)
	}
	if err := t.out.Validate(v); err != nil {
		t.s.log.Error("an answer failed its own schema check", "tool", t.def.name, "check", c.mask.known.Text(c.mask.hide(err.Error())))
		return nil, c.failf(codeInternal, "The server's answer failed its own schema check, a bug; it was not sent.")
	}
	return b, nil
}

// encodeOnce encodes out with the call's warnings and runs the backstop
// over it, returning how many values it replaced.
func (t *tool[In, Out]) encodeOnce(c *call, out *Out, env *envelope) ([]byte, int, error) {
	env.Warnings = append([]warning{}, c.warnings...)
	b, err := encode(out)
	if err != nil {
		return nil, 0, fmt.Errorf("encoding %s's answer: %w", t.def.name, err)
	}
	return c.mask.backstop(b)
}

// valueWithheld is the warning value_withheld's sentence.
const valueWithheld = "The server withheld a value of this answer that it should not show; this is a bug of the server's."

// backstopHit says that the backstop replaced n values in tool's answer: a
// bug in the layers above, logged with no value, and the tests' hook.
func (s *Server) backstopHit(tool string, n int) {
	s.log.Error(fmt.Sprintf("masking backstop replaced %d values in %s", n, tool))
	if s.o.onBackstop != nil {
		s.o.onBackstop(tool, n)
	}
}

// cursorMax is the most bytes that a cursor takes; a page's budget counts
// next_cursor at it.
const cursorMax = 512

// pageRows returns how many rows a page holds, of rows whose encodings
// take sizes bytes: at most limit, and while the answer, fixed bytes with
// no rows and next_cursor null, stays within the page budget with the rows
// and next_cursor at its longest; at least one when there is one.
func (c *call) pageRows(fixed int, sizes []int, limit int) int {
	total := fixed - len("null") + len(`""`) + cursorMax
	n := 0
	for n < len(sizes) && n < limit {
		add := sizes[n]
		if n > 0 {
			add++ // the comma
		}
		if n > 0 && total+add > c.s.o.PageBytes {
			break
		}
		total += add
		n++
	}
	return n
}
