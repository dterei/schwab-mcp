package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// Test-only tools, registered through addTool unless said otherwise.

// echoIn is the arguments of the echo tool: one of each kind of rule.
type echoIn struct {
	Limit   int    `json:"limit,omitempty"`
	From    string `json:"from,omitempty"`
	Account string `json:"account,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// echoOut is the echo tool's answer.
type echoOut struct {
	envelope
	Limit int    `json:"limit"`
	From  *Date  `json:"from"`
	Note  *Ident `json:"note"`
}

// withEcho registers echo, which answers its arguments.
func withEcho() envOption {
	return withTools(func(s *Server) {
		testTool(s, "echo", map[string]argRule{"limit": {desc: "Rows, 1 to 200.", min: new(1.0), max: new(200.0)}},
			func(_ context.Context, _ *call, in echoIn) (echoOut, error) {
				return echoOut{envelope: envelope{Source: sourceLocal}, Limit: in.Limit, From: datePtr(nytime.Date(in.From))}, nil
			})
	})
}

// pickIn is the arguments of the pick tool.
type pickIn struct {
	Account string `json:"account,omitempty"`
}

// pickOut is the pick tool's answer: the accounts it picked, and, for one,
// its type as Schwab's account endpoint sends it.
type pickOut struct {
	envelope
	Accounts []accountRef `json:"accounts"`
	Keys     []Ident      `json:"keys"`
	Type     *Ident       `json:"type"`
}

// withPick registers pick, which picks the accounts its account argument
// names, and asks Schwab for the one it picks.
func withPick() envOption {
	return withTools(func(s *Server) {
		testTool(s, "pick", nil, func(ctx context.Context, c *call, in pickIn) (pickOut, error) {
			as, err := c.pickAccounts(ctx, in.Account)
			if err != nil {
				return pickOut{}, err
			}
			out := pickOut{envelope: envelope{Source: sourceSchwab}, Accounts: []accountRef{}, Keys: []Ident{}}
			for _, a := range as {
				out.Accounts = append(out.Accounts, c.ref(a))
				out.Keys = append(out.Keys, Ident(a.key()))
			}
			if len(as) == 1 {
				acct, err := askAccount(ctx, c, &as[0], func(cl *schwab.Client, hash schwab.AccountHash) (schwab.Account, error) {
					return cl.Account(ctx, hash)
				})
				if err != nil {
					return pickOut{}, err
				}
				out.Type = identPtr(acct.SecuritiesAccount.Type)
			}
			return out, nil
		})
	})
}

// "arguments": null, {} and absent are no arguments; [] is not an object.
func TestNullArguments(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withEcho())
	for _, args := range []any{json.RawMessage("null"), `{}`, nil, json.RawMessage(" null ")} {
		if r := e.call("echo", args); r.isError {
			t.Errorf("arguments %s: %s", args, r.text)
		}
	}
	for _, args := range []string{`[]`, `"x"`, `3`, `[{"limit":1}]`} {
		r := e.call("echo", args)
		if r.code() != "bad_argument" || !strings.Contains(r.text, "must be a JSON object") {
			t.Errorf("arguments %s: %s", args, r.text)
		}
	}
	// Arguments over 64 KiB are refused before any check of their values,
	// which the error never quotes.
	long := `{"account":"` + strings.Repeat("a", 70<<10) + `"}`
	r := e.call("echo", long)
	if r.code() != "bad_argument" || !strings.Contains(r.text, "must be a JSON object of at most 64 KiB") || strings.Contains(r.text, "aaaa") {
		t.Errorf("arguments of 70 KiB: %.300s", r.text)
	}
}

// A bad argument is named with what it takes, from its schema, and its
// value is never quoted: an out-of-range limit, a bad date, a wrong type
// that holds a full account number, a number that is no whole number, an
// argument the tool does not take, one whose name is no argument's shape,
// a missing required one.
func TestBadArgumentsQuoteNoValue(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withEcho(), withTools(func(s *Server) {
		type idIn struct {
			ID string `json:"id"`
		}
		testTool(s, "needs_id", map[string]argRule{"id": {desc: "An ID.", pattern: idPattern}},
			func(context.Context, *call, idIn) (echoOut, error) {
				return echoOut{envelope: envelope{Source: sourceLocal}}, nil
			})
	}))
	e.plant("12345678")
	for _, tc := range []struct {
		tool, args, want, value string
	}{
		{"echo", `{"limit":500}`, "limit takes a whole number from 1 to 200.", "500"},
		{"echo", `{"limit":2.5}`, "limit takes a whole number from 1 to 200.", "2.5"},
		{"echo", `{"limit":"29470135"}`, "limit takes a whole number from 1 to 200.", "29470135"},
		{"echo", `{"from":"2026-13-45x"}`, "from takes a New York date, YYYY-MM-DD.", "2026-13-45x"},
		{"echo", `{"detail":"verbose 12345678"}`, "detail takes one of compact, full.", "verbose"},
		{"echo", `{"account":""}`, "account takes a string of 1 to 100 characters.", `""`},
		{"echo", `{"nope":1}`, "There is no argument nope. This tool takes account, detail, from, limit.", "1"},
		{"echo", `{"bad name 12345678":1}`, "An argument has a name this tool does not take.", "bad name"},
		{"needs_id", `{}`, "id is required; it takes 1 to 19 digits.", ""},
		{"needs_id", `{"id":"x1004517688099"}`, "id takes 1 to 19 digits.", "x1004517688099"},
	} {
		r := e.call(tc.tool, tc.args)
		if r.code() != "bad_argument" || !strings.Contains(r.text, tc.want) {
			t.Errorf("%s %s: %s, want [bad_argument] %s", tc.tool, tc.args, r.text, tc.want)
		}
		if tc.value != "" && strings.Contains(r.text, tc.value) {
			t.Errorf("%s %s: the error quotes the value %s: %s", tc.tool, tc.args, tc.value, r.text)
		}
	}
	// A whole number written with a fraction or an exponent is a whole
	// number, as the schema's integer is: some clients send every number so.
	for args, limit := range map[string]float64{`{"limit":5.0}`: 5, `{"limit":1e1}`: 10, `{"limit":2.000E0}`: 2, `{"limit":200}`: 200} {
		if r := e.call("echo", args); r.isError || r.value["limit"] != limit {
			t.Errorf("echo %s: %s, want limit %v", args, r.text, limit)
		}
	}
	if got := string(integral([]byte(`{"a":[1.5,1e2,{"b":10000000000000000000000.0}],"c":"1.0e1","d":1004517688099}`))); got !=
		`{"a":[1.5,100,{"b":10000000000000000000000}],"c":"1.0e1","d":1004517688099}` {
		t.Errorf("integral = %s", got)
	}
}

// A panic in a tool's handler, in a goroutine it starts through safeGo, or
// in a resource's handler answers as an error, and the next call works.
func TestPanicsAreErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withEcho(), withTools(func(s *Server) {
		testTool(s, "panics", nil, func(context.Context, *call, struct{}) (echoOut, error) {
			panic("a test's panic")
		})
		testTool(s, "goroutine_panics", nil, func(ctx context.Context, _ *call, _ struct{}) (echoOut, error) {
			return echoOut{}, <-safeGo(ctx, "a test's goroutine", func(context.Context) error { panic("a goroutine's panic") })
		})
		s.addResource(&mcp.Resource{URI: "schwab://test/panics", Name: "panics", MIMEType: "text/plain"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				panic("a resource's panic")
			})
	}))
	for _, tool := range []string{"panics", "goroutine_panics"} {
		r := e.call(tool, `{}`)
		if r.code() != "internal" {
			t.Errorf("%s: %s, want [internal]", tool, r.text)
		}
		if r := e.call("echo", `{}`); r.isError {
			t.Errorf("after %s: %s", tool, r.text)
		}
	}
	_, err := e.readResource("schwab://test/panics")
	if we, ok := errors.AsType[*jsonrpc.Error](err); !ok || we.Code != jsonrpc.CodeInternalError {
		t.Errorf("a panicking resource: %v, want -32603", err)
	}
	if r := e.call("echo", `{}`); r.isError {
		t.Errorf("after the resource: %s", r.text)
	}
	if !strings.Contains(e.logs.String(), "a tool panicked") || !strings.Contains(e.logs.String(), "a request panicked") {
		t.Errorf("the panics were not logged:\n%s", e.logs)
	}
}

// blocker is a test-only tool that takes a slot and blocks until released
// or its context ends, counting how many run at once.
type blocker struct {
	running, most atomic.Int32
	started       chan struct{}
	release       chan struct{}
}

func newBlocker() *blocker {
	return &blocker{started: make(chan struct{}, 100), release: make(chan struct{})}
}

// register registers the blocker as the tool block.
func (b *blocker) register(s *Server) {
	testTool(s, "block", nil, func(ctx context.Context, _ *call, _ struct{}) (echoOut, error) {
		n := b.running.Add(1)
		defer b.running.Add(-1)
		for {
			if m := b.most.Load(); n <= m || b.most.CompareAndSwap(m, n) {
				break
			}
		}
		b.started <- struct{}{}
		select {
		case <-b.release:
			return echoOut{envelope: envelope{Source: sourceLocal}}, nil
		case <-ctx.Done():
			return echoOut{}, ctx.Err()
		}
	})
}

// With 4 slots, 20 calls of a tool that blocks: 4 run at once, no more;
// the others wait, so that one of them takes the slot that a call ends
// during the wait, and those still waiting at the slot wait answer busy,
// as tool errors; the 4 then running end when released.
func TestSlots(t *testing.T) {
	t.Parallel()
	b := newBlocker()
	e := newEnv(t, withTools(b.register), withOptions(func(o *Options) {
		o.MaxCalls, o.SlotWait, o.CallTimeout = 4, 300*time.Millisecond, 5*time.Second
	}))
	results := make(chan result, 20)
	for range 20 {
		go func() {
			res, err := e.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "block", Arguments: json.RawMessage(`{}`)})
			if err != nil {
				t.Error(err)
				results <- result{}
				return
			}
			tc, _ := res.Content[0].(*mcp.TextContent)
			results <- result{text: tc.Text, isError: res.IsError}
		}()
	}
	for range 4 {
		<-b.started
	}
	b.release <- struct{}{} // one call ends, and one that waits takes its slot
	if r := <-results; r.isError {
		t.Fatalf("the call released: %s", r.text)
	}
	<-b.started
	for range 15 {
		r := <-results
		if r.code() != "busy" || !strings.Contains(r.text, "4 calls are already running") {
			t.Fatalf("a call that waited: %s, want [busy]", r.text)
		}
	}
	close(b.release)
	for range 4 {
		if r := <-results; r.isError {
			t.Errorf("a call that ran: %s", r.text)
		}
	}
	if most := b.most.Load(); most != 4 {
		t.Errorf("%d calls ran at once, want 4", most)
	}
}

// A request that Schwab never answers ends at the call's budget: the call
// answers timed_out, within the budget and 200 ms, and its goroutines end.
func TestDeadline(t *testing.T) {
	e := newEnv(t, withPick(), withOptions(func(o *Options) { o.CallTimeout = 500 * time.Millisecond }))
	if r := e.call("pick", `{"account":"135"}`); r.isError {
		t.Fatal(r.text)
	}
	before := runtime.NumGoroutine()
	e.fake.Override(schwabtest.AccountRoute, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	start := time.Now()
	r := e.call("pick", `{"account":"135"}`)
	took := time.Since(start)
	if r.code() != "timed_out" {
		t.Errorf("a request never answered: %s, want [timed_out]", r.text)
	}
	if took > 700*time.Millisecond {
		t.Errorf("the call took %v, over its budget of 500 ms and 200 ms", took)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("%d goroutines after the call, %d before it", n, before)
	}
}

// Ending the root context, as Close does, ends a running call at once.
func TestShutdownCancelsHandlers(t *testing.T) {
	t.Parallel()
	b := newBlocker()
	e := newEnv(t, withTools(b.register), withOptions(func(o *Options) { o.CallTimeout = 10 * time.Second }))
	done := make(chan result, 1)
	go func() {
		res, err := e.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "block", Arguments: json.RawMessage(`{}`)})
		if err != nil {
			done <- result{text: err.Error()}
			return
		}
		tc, _ := res.Content[0].(*mcp.TextContent)
		done <- result{text: tc.Text, isError: res.IsError}
	}()
	<-b.started
	start := time.Now()
	e.srv.cancelRoot()
	select {
	case r := <-done:
		if took := time.Since(start); took > time.Second {
			t.Errorf("the call ended %v after the root context", took)
		}
		if !r.isError {
			t.Errorf("the cancelled call answered %s", r.text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end when the root context did")
	}
}

// Run serves a session until its input ends or its context does, and
// returns nil for either, ending a running call at once; a malformed line
// fails the session, and Run returns its error. Each says why it ended in
// one line of the log at most: the failure's, at error, and nothing for
// the others. Each problem of the config is logged at warn when Run
// starts.
func TestRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		end   func(in io.WriteCloser, cancel context.CancelFunc)
		fails bool
	}{
		{"input ends", func(in io.WriteCloser, _ context.CancelFunc) { in.Close() }, false},
		{"context ends", func(_ io.WriteCloser, cancel context.CancelFunc) { cancel() }, false},
		{"malformed line", func(in io.WriteCloser, _ context.CancelFunc) { _, _ = io.WriteString(in, "this is not JSON\n") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newBlocker()
			e := newEnv(t, withTools(b.register), withOptions(func(o *Options) { o.CallTimeout = 10 * time.Second }),
				withConfig(func(c *config.Config) {
					c.Problems = append(c.Problems, config.Problem{Code: ProblemStrayArgument, Setting: "arguments"})
				}))
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			lines := make(chan string, 10)
			go func() {
				sc := bufio.NewScanner(outR)
				for sc.Scan() {
					lines <- sc.Text()
				}
				close(lines)
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ran := make(chan error, 1)
			go func() { ran <- e.srv.Run(ctx, &mcp.IOTransport{Reader: inR, Writer: outW}) }()
			for _, line := range []string{
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
				`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"block","arguments":{}}}`,
			} {
				if _, err := io.WriteString(inW, line+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			<-b.started
			start := time.Now()
			tc.end(inW, cancel)
			select {
			case err := <-ran:
				if (err != nil) != tc.fails {
					t.Errorf("Run returned %v", err)
				}
				if took := time.Since(start); took > 2*time.Second {
					t.Errorf("Run returned %v after the session ended", took)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return")
			}
			if b.running.Load() != 0 {
				t.Error("the running call did not end")
			}
			var errorLines, warned int
			for _, line := range e.logs.lines() {
				if strings.Contains(line, "level=ERROR") {
					errorLines++
				}
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "the server was started with an argument it does not take") {
					warned++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.fails]; errorLines != want {
				t.Errorf("%d lines at error, want %d:\n%s", errorLines, want, e.logs)
			}
			if warned != 1 {
				t.Errorf("the config's problem was logged %d times, want once:\n%s", warned, e.logs)
			}
			outW.Close()
			for range lines {
			}
		})
	}
}

// fitOut is a test's answer that is a fitter: raw JSON, a flat list and a
// list in each of its rows.
type fitOut struct {
	envelope
	cuts
	Items []Ident         `json:"items"`
	Rows  []fitRow        `json:"rows"`
	Raw   json.RawMessage `json:"raw"`
}

type fitRow struct {
	Fills []Ident `json:"fills"`
}

func (o *fitOut) dropRaw() bool {
	had := o.Raw != nil
	o.Raw = nil
	return had
}

func (o *fitOut) lists() []cuttable {
	ls := []cuttable{{path: "items", value: o.Items, count: func() int { return len(o.Items) }, keep: func(n int) { o.Items = o.Items[:n] }}}
	for i := range o.Rows {
		r := &o.Rows[i]
		ls = append(ls, cuttable{
			path: fmt.Sprintf("rows.%d.fills", i), value: r.Fills,
			count: func() int { return len(r.Fills) }, keep: func(n int) { r.Fills = r.Fills[:n] },
		})
	}
	return ls
}

// fitIn says how big a fitOut to make.
type fitIn struct {
	Items int  `json:"items,omitempty"`
	Fills int  `json:"fills,omitempty"`
	Raw   bool `json:"raw,omitempty"`
	Fixed int  `json:"fixed,omitempty"`
}

// withFit registers fit, which answers a fitOut of the size asked, and
// big, which answers as much and is no fitter.
func withFit() envOption {
	return withTools(func(s *Server) {
		rules := map[string]argRule{
			"items": {desc: "How many items.", min: new(0.0), max: new(10_000.0)},
			"fills": {desc: "How many fills.", min: new(0.0), max: new(10_000.0)},
			"raw":   {desc: "Whether to send raw JSON."},
			"fixed": {desc: "How many bytes of warnings.", min: new(0.0), max: new(100_000.0)},
		}
		build := func(c *call, in fitIn) fitOut {
			out := fitOut{envelope: envelope{Source: sourceLocal}, Items: []Ident{}, Rows: []fitRow{{Fills: []Ident{}}, {Fills: []Ident{}}}}
			for i := range in.Items {
				out.Items = append(out.Items, Ident(fmt.Sprintf("item-%05d-%s", i, strings.Repeat("x", 30))))
			}
			for i := range in.Fills {
				out.Rows[0].Fills = append(out.Rows[0].Fills, Ident(fmt.Sprintf("fill-%05d", i)))
			}
			out.Rows[1].Fills = append(out.Rows[1].Fills, "a short list")
			if in.Raw {
				out.Raw = json.RawMessage(`{"blob":"` + strings.Repeat("r", 5000) + `"}`)
			}
			if in.Fixed > 0 {
				c.warn("partial", "%s", Ident(strings.Repeat("w", in.Fixed)))
			}
			return out
		}
		testTool(s, "fit", rules, func(_ context.Context, c *call, in fitIn) (fitOut, error) { return build(c, in), nil })
		type bigOut struct {
			envelope
			Items []Ident `json:"items"`
		}
		testTool(s, "big", rules, func(_ context.Context, c *call, in fitIn) (bigOut, error) {
			return bigOut{envelope: envelope{Source: sourceLocal}, Items: build(c, in).Items}, nil
		})
	})
}

// An answer over the cap that is a fitter drops its raw JSON first, then
// halves its longest list, keeping its first items, again and again until
// it fits: cut names each list cut, with how many items it shows of how
// many, and the warning list_cut says so. One that is no fitter, or has
// nothing left to cut, is too_big. An answer within the cap is untouched.
var _ = addSizeCases(sizeCase{name: "fitter", opts: []envOption{withFit()}, run: sizeFitter},
	sizeCase{name: "margin", opts: []envOption{withRows()}, run: sizeMargin})

// sizeFitter is TestSizeCap's case of a fitter.
func sizeFitter(t *testing.T, e *env) {
	small := e.call("fit", `{"items":3,"fills":3,"raw":true}`)
	if small.isError || len(small.value["cut"].([]any)) != 0 || slices.Contains(small.warnings(), "list_cut") || small.value["raw"] == nil {
		t.Errorf("a small answer was cut: %s", small.text)
	}
	r := e.call("fit", `{"items":800,"fills":3000,"raw":true}`)
	if r.isError {
		t.Fatal(r.text)
	}
	if w := r.warnings(); len(w) != 2 || w[0] != "raw_omitted" || w[1] != "list_cut" {
		t.Errorf("warnings %v, want raw_omitted, then list_cut", w)
	}
	if r.value["raw"] != nil {
		t.Error("raw was kept")
	}
	var got struct {
		Cut   []cutList `json:"cut"`
		Items []string  `json:"items"`
		Rows  []fitRow  `json:"rows"`
	}
	if err := json.Unmarshal(r.raw, &got); err != nil {
		t.Fatal(err)
	}
	total := map[Ident]int{"items": 800, "rows.0.fills": 3000}
	for _, c := range got.Cut {
		if c.Total != total[c.Path] {
			t.Errorf("cut %s: total %d, want %d", c.Path, c.Total, total[c.Path])
		}
	}
	if len(got.Cut) != 2 || len(got.Items) != shownOf(got.Cut, "items") || len(got.Rows[0].Fills) != shownOf(got.Cut, "rows.0.fills") {
		t.Errorf("cut %+v does not say what the lists show: %d items, %d fills", got.Cut, len(got.Items), len(got.Rows[0].Fills))
	}
	if len(got.Rows[1].Fills) != 1 {
		t.Error("the short list was cut")
	}
	if got.Items[0] != "item-00000-"+strings.Repeat("x", 30) || got.Rows[0].Fills[0] != "fill-00000" {
		t.Error("a cut list does not keep its first items")
	}
	if r := e.call("big", `{"items":800}`); r.code() != "too_big" {
		t.Errorf("an answer over the cap that is no fitter: %s, want [too_big]", r.text)
	}
	if r := e.call("fit", `{"fixed":30000}`); r.code() != "too_big" {
		t.Errorf("an answer whose fixed part is over the cap: %s, want [too_big]", r.text)
	}
}

// shownOf returns how many items cut says the list at path shows.
func shownOf(cs []cutList, path Ident) int {
	for _, c := range cs {
		if c.Path == path {
			return c.Shown
		}
	}
	return -1
}

// pageRows fills a page with rows while the answer, next_cursor counted at
// its longest, stays within the budget, and at most limit; a page holds at
// least one row, however big.
func TestPageRows(t *testing.T) {
	t.Parallel()
	c := &call{s: &Server{o: Options{PageBytes: 1000}}}
	fixed := 100 // with next_cursor null
	for _, tc := range []struct {
		sizes []int
		limit int
		want  int
	}{
		{nil, 10, 0},
		{[]int{5000}, 10, 1},
		{[]int{100, 100, 100, 100, 100}, 10, 3}, // 100 - 4 + 2 + 512 = 610, then 100, 101, 101 fit
		{[]int{100, 100, 100, 100, 100}, 2, 2},
		{[]int{386}, 10, 1},
		{[]int{100, 289}, 10, 2}, // 610 + 100 + 1 + 289 = 1000
		{[]int{100, 290}, 10, 1},
	} {
		if got := c.pageRows(fixed, tc.sizes, tc.limit); got != tc.want {
			t.Errorf("pageRows(%d, %v, %d) = %d, want %d", fixed, tc.sizes, tc.limit, got, tc.want)
		}
	}
}

// rowsOut is the rows tool's answer, a page of rows.
type rowsOut struct {
	envelope
	page
	Rows []Ident `json:"rows"`
}

// withRows registers rows, which answers a page filled to the budget, then
// given three warnings of 300 characters, partial, zeros_as_null and
// value_withheld's own, which come after a page is cut.
func withRows() envOption {
	return withTools(func(s *Server) {
		testTool(s, "rows", nil, func(_ context.Context, c *call, _ struct{}) (rowsOut, error) {
			out := rowsOut{envelope: envelope{Source: sourceLocal}, Rows: []Ident{}}
			fixed, err := encode(out)
			if err != nil {
				return out, err
			}
			var all []Ident
			var sizes []int
			for i := range 1000 {
				row := Ident(fmt.Sprintf("row-%04d-%s", i, strings.Repeat("y", 180)))
				b, _ := encode(row)
				all, sizes = append(all, row), append(sizes, len(b))
			}
			n := c.pageRows(len(fixed), sizes, 1000)
			out.Rows = all[:n]
			out.NextCursor = new(Cursor(strings.Repeat("c", cursorMax)))
			for _, code := range []Ident{"partial", "zeros_as_null"} {
				c.warn(code, "%s", Ident(strings.Repeat("w", 300)))
			}
			c.warn("value_withheld", "%s", Ident(strings.Repeat("v", 300)))
			return out, nil
		})
	})
}

// sizeMargin is TestSizeCap's case of the margin between the page budget
// and the cap: the rows tool's answer stays within the cap.
func sizeMargin(t *testing.T, e *env) {
	r := e.call("rows", `{}`)
	if r.isError {
		t.Fatal(r.text)
	}
	if n := len(r.text); n <= e.srv.o.PageBytes {
		t.Errorf("the answer has %d bytes: the page did not fill the budget of %d", n, e.srv.o.PageBytes)
	}
}

// The text block holds exactly the bytes of the structured content (design
// 8.2), whatever the strings hold: HTML's characters as they are in the
// text, which the SDK escapes in the structured content alone, U+2028 and
// U+2029, quotes, backslashes, control characters and characters beyond
// the basic plane. env.check compares the two on every call; the check
// itself fails when the text is not escaped as the SDK escapes.
func TestTextEqualsStructured(t *testing.T) {
	t.Parallel()
	text := "AT&T <x> > y    \"q\" \\ \t\x01 \U0001F600 é"
	e := newEnv(t, withTools(func(s *Server) {
		testTool(s, "text", nil, func(context.Context, *call, struct{}) (echoOut, error) {
			return echoOut{envelope: envelope{Source: sourceLocal}, Note: new(Ident(text))}, nil
		})
	}))
	r := e.call("text", `{}`)
	if !strings.Contains(r.text, "AT&T <x> > y") {
		t.Errorf("the text escapes HTML's characters: %s", r.text)
	}
	if string(r.raw) == r.text {
		t.Error("the wire's structured content holds & unescaped: the check compares nothing")
	}
	var note struct{ Note string }
	if err := json.Unmarshal(r.raw, &note); err != nil || note.Note != text {
		t.Errorf("the structured content holds %q, want %q", note.Note, text)
	}
}

// The middleware's backstop, the second net, covers what addTool's does
// not: a tool registered with the SDK's own AddTool that returns a known
// number in its text, its structured content and an error result has all
// three replaced, its text still its structured content, and the warning
// value_withheld added; so have a resource's contents, their text and URI,
// and a prompt's description and messages. A protocol error has its
// message replaced, and its data, which resources/read drops: a handler's,
// and the SDK's own for a tool or a prompt that no name matches, which
// quotes the name the client sent. addTool's own net replaces a known
// number in a tool's error before the middleware sees it. Each hit is
// logged, and told to the hook with where it was.
func TestMiddlewareBackstopCatchesALeak(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	hits, told := map[string]int{}, 0
	leak := `{"source":"local","warnings":[],"note":"account 29470135","n":61803398}`
	failure := &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "no account 29470135", Data: json.RawMessage(`{"account":"29470135"}`)}
	e := newEnv(t, withBackstopAllowed(), withOptions(func(o *Options) {
		o.onBackstop = func(where string, n int) {
			mu.Lock()
			hits[where] += n
			told++
			mu.Unlock()
		}
	}), withTools(func(s *Server) {
		s.MCP().AddTool(&mcp.Tool{Name: "leaks", InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				switch string(req.Params.Arguments) {
				case `{"error":true}`:
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "[internal] account 29470135"}}}, nil
				case `{"protocol":true}`:
					return nil, failure
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: leak}}, StructuredContent: json.RawMessage(leak)}, nil
			})
		testTool(s, "fails", nil, func(context.Context, *call, struct{}) (echoOut, error) {
			return echoOut{}, &toolErr{code: codeInternal, msg: "account 29470135"}
		})
		s.addResource(&mcp.Resource{URI: "schwab://test/leaks", Name: "leaks", MIMEType: "text/plain"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
					{URI: "schwab://test/29470135", MIMEType: "text/plain", Text: "account 29470135"},
				}}, nil
			})
		s.addResource(&mcp.Resource{URI: "schwab://test/fails", Name: "fails", MIMEType: "text/plain"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) { return nil, failure })
		s.MCP().AddPrompt(&mcp.Prompt{Name: "leaks"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Description: "for 29470135", Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "review 29470135"}},
			}}, nil
		})
	}))
	e.srv.mask.known.Add(individual, options) // as the directory would have
	ctx := t.Context()
	// hit checks that the hook was told of n values replaced in where, and
	// nowhere else, since the last check.
	hit := func(what, where string, n int) {
		t.Helper()
		mu.Lock()
		got := maps.Clone(hits)
		clear(hits)
		mu.Unlock()
		if !maps.Equal(got, map[string]int{where: n}) {
			t.Errorf("%s: the hook was told of %v, want %d in %s", what, got, n, where)
		}
	}

	res, err := e.cs.CallTool(ctx, &mcp.CallToolParams{Name: "leaks", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	raw := e.structuredOf(text)
	want := `{"source":"local","warnings":[{"code":"value_withheld","message":"` + valueWithheld + `"}],"note":"account ...135","n":null}`
	if text != want {
		t.Errorf("text =\n%s\nwant\n%s", text, want)
	}
	if string(raw) != strings.ReplaceAll(want, "'", `'`) && string(raw) != want {
		t.Errorf("structured content =\n%s\nwant the text", raw)
	}
	hit("a tool's answer", "tools/call", 2)
	res, err = e.cs.CallTool(ctx, &mcp.CallToolParams{Name: "leaks", Arguments: json.RawMessage(`{"error":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "[internal] account ...135" {
		t.Errorf("the error = %q", got)
	}
	hit("a tool's error result", "tools/call", 1)
	res, err = e.cs.CallTool(ctx, callParams("fails"))
	if err != nil || res.Content[0].(*mcp.TextContent).Text != "[internal] account ...135" {
		t.Errorf("an error of addTool's: %v, %v", res, err)
	}
	hit("an error of addTool's", "fails", 1)

	read, err := e.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "schwab://test/leaks"})
	if err != nil || read.Contents[0].Text != "account ...135" || read.Contents[0].URI != "schwab://test/...135" {
		t.Errorf("a resource's contents: %+v, %v", read, err)
	}
	hit("a resource's contents", "resources/read", 2)
	prompt, err := e.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "leaks"})
	if err != nil || prompt.Description != "for ...135" || prompt.Messages[0].Content.(*mcp.TextContent).Text != "review ...135" {
		t.Errorf("a prompt: %+v, %v", prompt, err)
	}
	hit("a prompt", "prompts/get", 2)

	// Protocol errors: a tool's, which keeps its data cleared; a resource
	// read's, which drops it; and the SDK's own.
	for _, tc := range []struct {
		what, where string
		call        func() error
		says, data  string
		n           int
	}{
		{"a tool's protocol error", "tools/call", func() error {
			_, err := e.cs.CallTool(ctx, &mcp.CallToolParams{Name: "leaks", Arguments: json.RawMessage(`{"protocol":true}`)})
			return err
		}, "no account ...135", `{"account":"...135"}`, 2},
		{"a resource's protocol error", "resources/read", func() error {
			_, err := e.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "schwab://test/fails"})
			return err
		}, "no account ...135", "", 1},
		{"an unknown tool", "tools/call", func() error {
			_, err := e.cs.CallTool(ctx, &mcp.CallToolParams{Name: individual})
			return err
		}, `unknown tool "...135"`, "", 1},
		{"an unknown prompt", "prompts/get", func() error {
			_, err := e.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "review " + individual})
			return err
		}, `unknown prompt "review ...135"`, "", 1},
	} {
		we, ok := errors.AsType[*jsonrpc.Error](tc.call())
		if !ok || we.Message != tc.says || string(we.Data) != tc.data {
			t.Errorf("%s: %+v, want %q and data %q", tc.what, we, tc.says, tc.data)
		}
		hit(tc.what, tc.where, tc.n)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(e.logs.String(), "masking backstop replaced"); n != told {
		t.Errorf("%d hits were logged, and the hook was told of %d:\n%s", n, told, e.logs)
	}
	e.assertClean()
}

// addTool's backstop replaces a known number that an answer holds where no
// layer above masked it, and says so with value_withheld; a known number
// written as a number becomes null, which then fails the output schema, so
// that the leak is an internal error rather than an answer.
func TestBackstopInAddTool(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	type countOut struct {
		envelope
		Count int `json:"count"`
	}
	e := newEnv(t, withBackstopAllowed(), withOptions(func(o *Options) {
		o.onBackstop = func(string, int) { hits.Add(1) }
	}), withTools(func(s *Server) {
		testTool(s, "leaks_ident", nil, func(context.Context, *call, struct{}) (echoOut, error) {
			return echoOut{envelope: envelope{Source: sourceLocal}, Note: new(Ident("symbol 29470135"))}, nil
		})
		testTool(s, "leaks_number", nil, func(context.Context, *call, struct{}) (countOut, error) {
			return countOut{envelope: envelope{Source: sourceLocal}, Count: 61803398}, nil
		})
	}))
	e.srv.mask.known.Add(individual, options) // as the directory would have
	r := e.call("leaks_ident", `{}`)
	if r.isError || r.value["note"] != "symbol ...135" || !slices.Contains(r.warnings(), "value_withheld") {
		t.Errorf("an answer that leaks in an Ident: %s", r.text)
	}
	r = e.call("leaks_number", `{}`)
	if r.code() != "internal" || !strings.Contains(r.text, "failed its own schema check") {
		t.Errorf("an answer that leaks a number: %s", r.text)
	}
	if hits.Load() != 2 {
		t.Errorf("the hook was told of %d hits, want 2", hits.Load())
	}
}

// An account number whose digits read as a date leaves every date and time
// whole: the answers' as_of, a row's date and time, and the asked days, of
// the day that the number spells, come out as they are, with no
// value_withheld and no value that the backstop's hook is told of, while
// the number itself is never sent.
func TestDateLikeAccountNumber(t *testing.T) {
	t.Parallel()
	const number = "20261113" // testNow's day
	e := newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		f.AddAccount(schwabtest.Account{Number: number, Details: `{"securitiesAccount":{"type":"CASH","accountNumber":"` + number + `"}}`})
		f.AddRows(schwabtest.HashFor(number), txnJSON(number, 73508265501, ny(2026, time.November, 13, 10, 0), "DIVIDEND_OR_INTEREST",
			"Free Balance Interest Adjustment", 0.01))
	}))
	const asOf = "2026-11-13T16:40:00-05:00"
	if r := e.call("list_accounts", `{}`); r.isError || r.value["as_of"] != asOf || slices.Contains(r.warnings(), "value_withheld") ||
		!strings.Contains(r.text, `"account":"...113"`) {
		t.Errorf("list_accounts: %.600s", r.text)
	}
	r := e.call("get_transactions", `{"source":"schwab","account":"113","from":"2026-11-01","to":"2026-11-13"}`)
	rows, asked := txnRowsOf(r), txnCoverageOf(r, "...113")["asked"]
	if r.isError || r.value["as_of"] != asOf || len(rows) != 1 || rows[0]["date"] != "2026-11-13" ||
		rows[0]["time"] != "2026-11-13T10:00:00-05:00" || fmt.Sprint(asked) != "map[from:2026-11-01 to:2026-11-13]" ||
		slices.Contains(r.warnings(), "value_withheld") {
		t.Errorf("get_transactions: %.900s", r.text)
	}
	if sent := strings.Join(e.sent(), "\n"); strings.Contains(sent, number) {
		t.Errorf("the number was sent or logged whole:\n%s", sent)
	}
}

// withResources registers a fixed resource, a reference page and the
// positions template, each answering its URI, with the guard's pattern;
// and a resource that answers as the echo tool does.
func withResources() envOption {
	return withTools(func(s *Server) {
		text := func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "ok"}}}, nil
		}
		s.addResource(&mcp.Resource{URI: "schwab://test/fixed", Name: "fixed", MIMEType: "text/plain"}, text)
		s.addResource(&mcp.Resource{URI: referencePrefix + "test", Name: "reference", MIMEType: "text/markdown"}, text)
		s.addResource(&mcp.Resource{URI: "schwab://test/echo", Name: "echo", MIMEType: "application/json"},
			func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				b, e := s.toolText(ctx, "echo", map[string]any{"limit": 3})
				if e != nil {
					return nil, resourceError(e)
				}
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: b}}}, nil
			})
		s.addResourceTemplate(&mcp.ResourceTemplate{URITemplate: "schwab://accounts/{account}/positions", Name: "positions", MIMEType: "text/plain"},
			accountURI, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				if strings.Contains(req.Params.URI, "/999/") {
					return nil, resourceError(&toolErr{code: codeAccountNotFound})
				}
				return text(ctx, req)
			})
	})
}

// accountURI is the guard's pattern of the positions template's URIs.
var accountURI = regexp.MustCompile(`^schwab://accounts/[A-Za-z0-9.]{1,80}/positions$`)

// The resource guard (#1426) refuses, before the SDK sees it, a URI that no
// resource serves, without the URI and without data: control characters,
// a space, an escape, non-ASCII, another scheme, a template value outside
// its set; a handler's own refusal of a value that picks no account is the
// same. The session lives on.
func TestResourceGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withEcho(), withResources())
	for _, uri := range []string{
		"schwab://accounts/\u007f/positions", "schwab://accounts/a b/positions", "schwab://accounts/%7F/positions",
		"schwab://accounts/ünïcode/positions", "https://example.com/x", "schwab://test/fixed/", "schwab://accounts/999/positions",
		"schwab://accounts/" + strings.Repeat("a", 81) + "/positions", "",
	} {
		_, err := e.readResource(uri)
		we, ok := errors.AsType[*jsonrpc.Error](err)
		if !ok || we.Code != jsonrpc.CodeInvalidParams || we.Message != "resource not found" || len(we.Data) > 0 {
			t.Errorf("%q: %v, want -32602 resource not found without data", uri, err)
		}
	}
	for _, uri := range []string{"schwab://test/fixed", "schwab://accounts/135/positions", "schwab://accounts/...135/positions"} {
		if r, err := e.readResource(uri); err != nil || r.Contents[0].Text != "ok" {
			t.Errorf("%q after the refusals: %v", uri, err)
		}
	}
	r, err := e.readResource("schwab://test/echo")
	if err != nil || !strings.Contains(r.Contents[0].Text, `"limit":3`) {
		t.Errorf("a resource that answers as a tool: %v %v", r, err)
	}
}

// Every result that carries cache fields is private, with a time to live
// of 0, but a day for a reference page, in a 2026-07-28 session and an
// older one alike (design 11.3).
func TestCacheScopeIsPrivate(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{"", "2025-06-18"} {
		e := newEnv(t, withEcho(), withResources(), withProtocol(protocol))
		ctx := t.Context()
		tools, err := e.cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		resources, err := e.cs.ListResources(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		templates, err := e.cs.ListResourceTemplates(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		prompts, err := e.cs.ListPrompts(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		fixed, err := e.readResource("schwab://test/fixed")
		if err != nil {
			t.Fatal(err)
		}
		reference, err := e.readResource(referencePrefix + "test")
		if err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]mcp.Cacheable{
			"tools/list": tools.Cacheable, "resources/list": resources.Cacheable, "resources/templates/list": templates.Cacheable,
			"prompts/list": prompts.Cacheable, "resources/read": fixed.Cacheable,
		} {
			if c.CacheScope != "private" || c.TTLMs != 0 {
				t.Errorf("%s %s: %+v, want private and 0", protocol, name, c)
			}
		}
		if c := reference.Cacheable; c.CacheScope != "private" || c.TTLMs != 86_400_000 {
			t.Errorf("%s a reference page: %+v, want private and a day", protocol, c)
		}
		if protocol == "" && !slices.ContainsFunc(e.wire.lines(), func(l string) bool {
			return strings.HasPrefix(l, "read: ") && strings.Contains(l, `"instructions"`) && strings.Contains(l, `"cacheScope":"private"`)
		}) {
			t.Error("server/discover's result is not private")
		}
	}
}

// An account argument picks exactly one account (design 6.4): by its hash
// or a prefix of 12 or more of its characters, in any case; by its store
// ref; by the digits its number ends in, with or without ..., dashes or
// spaces; by its nickname as Schwab sent it or as masked. None, or
// several, is an error that names the accounts, each hash whole, and
// echoes the argument with no more of a number than ... and 3 digits, but a
// hash whole. A hash that Schwab no longer takes is looked up again once;
// an account no longer shared is account_not_shared.
func TestAccountSelection(t *testing.T) {
	t.Parallel()
	hash := schwabtest.HashFor(individual)
	e := newEnv(t, withPick())
	e.plant("12345678")
	for _, tc := range []struct{ arg, want string }{
		{"(none)", "...135 ...398 ...783"},
		{hash, "...135"},
		{strings.ToLower(hash[:12]), "...135"},
		{"A2", "...398"},
		{"a3", "...783"},
		{"135", "...135"},
		{"...398", "...398"},
		{"6180-3398", "...398"},
		{"61803398", "...398"},
		{"Individual", "...135"},
		{"options  &  MARGIN", "...398"},
	} {
		args := map[string]string{"account": tc.arg}
		if tc.arg == "(none)" {
			args = nil
		}
		r := e.call("pick", args)
		if r.isError {
			t.Errorf("%q: %s", tc.arg, r.text)
			continue
		}
		var got pickOut
		if err := json.Unmarshal(r.raw, &got); err != nil {
			t.Fatal(err)
		}
		var masked []string
		for _, a := range got.Accounts {
			masked = append(masked, string(a.Account))
		}
		if strings.Join(masked, " ") != tc.want {
			t.Errorf("%q picked %v, want %s", tc.arg, masked, tc.want)
		}
		if len(got.Keys) == 1 && got.Keys[0] != *got.Accounts[0].AccountHash {
			t.Errorf("%q: the key %s is not the hash", tc.arg, got.Keys[0])
		}
	}
	for _, tc := range []struct{ arg, code, echo, not string }{
		{"999", "account_not_found", `"999"`, ""},
		{"12345678", "account_not_found", `"...678"`, "12345678"},
		{"ABCDEF123456", "account_not_found", `"ABCDEF123456"`, ""},
		{"Nobody 29470135", "account_not_found", `"Nobody ...135"`, ""},
	} {
		r := e.call("pick", map[string]string{"account": tc.arg})
		if r.code() != tc.code || !strings.Contains(r.text, tc.echo) {
			t.Errorf("%q: %s, want [%s] echoing %s", tc.arg, r.text, tc.code, tc.echo)
		}
		if !strings.Contains(r.text, hash) || !strings.Contains(r.text, `...398 "Options & Margin" (account_hash `+schwabtest.HashFor(options)+")") {
			t.Errorf("%q: the error does not name the accounts with their hashes whole: %s", tc.arg, r.text)
		}
	}

	// Two accounts of one nickname, and a nickname that holds a number,
	// which a selector may give as masked.
	e2 := newEnv(t, withPick(), withFake(func(f *schwabtest.Server) {
		f.SetUserPreference(`{"accounts":[{"accountNumber":"29470135","nickName":"Joint","primaryAccount":true},` +
			`{"accountNumber":"61803398","nickName":"joint"},{"accountNumber":"40221783","nickName":"Kids 4022-1783"}]}`)
	}))
	r := e2.call("pick", `{"account":"Joint"}`)
	if r.code() != "account_ambiguous" || !strings.Contains(r.text, `"Joint" matches 2 accounts, ...135 and ...398`) {
		t.Errorf("an ambiguous nickname: %s", r.text)
	}
	for _, arg := range []string{"Kids 4022-1783", "Kids ...783"} {
		if r := e2.call("pick", map[string]string{"account": arg}); r.isError || !strings.Contains(r.text, `"account":"...783"`) {
			t.Errorf("%q: %s", arg, r.text)
		}
	}

	// A hash that Schwab no longer takes: the directory reads the hashes
	// again, once, and the call goes on with the new one.
	e.fake.Rehash(hash, schwabtest.HashFor("rehashed"))
	if r := e.call("pick", `{"account":"135"}`); r.isError || !strings.Contains(r.text, `"type":"CASH"`) {
		t.Errorf("after a new hash: %s", r.text)
	}
	e3 := newEnv(t, withPick())
	if r := e3.call("pick", `{"account":"398"}`); r.isError {
		t.Fatal(r.text)
	}
	e3.fake.RemoveAccount(schwabtest.HashFor(options))
	if r := e3.call("pick", `{"account":"398"}`); r.code() != "account_not_shared" {
		t.Errorf("an account no longer shared: %s", r.text)
	}
}

// An account that only the local history knows, which Schwab does not
// share, is none of the accounts of a tool that reads Schwab: an argument
// that names it is account_not_shared, which names it, and the error for
// one that names no account lists only the accounts that the login shares.
// get_transactions, which reads the local history too, lists it apart, by
// its store ref, and reads it there. get_transaction, which reads Schwab
// for a transaction that the local history does not hold, says that the
// login does not share it, or, of one that the local history saw the login
// stop sharing, that Schwab no longer shares it.
func TestStoreOnlyAccount(t *testing.T) {
	t.Parallel()
	path := storeCopy(t)
	gone := schwabtest.SpareAccountNumbers[0]
	exec(t, rawStore(t, path),
		"INSERT INTO account (number, created_from, first_seen_at, last_seen_at) "+
			"VALUES ('12345678', 'user', '2026-01-05T19:11:00.000Z', '2026-01-05T19:11:00.000Z')",
		"INSERT INTO account (number, created_from, first_seen_at, last_seen_at, not_shared_since) "+
			"VALUES ('"+gone+"', 'api', '2026-01-05T19:11:00.000Z', '2026-01-05T19:11:00.000Z', '2026-11-01T03:30:00.000Z')")
	e := newEnv(t, withStore(path), withPick())
	e.plant("12345678", gone)
	const notShared = "The login does not share ...678, which only the local history knows; ask the user to run schwabtx login and tick it."
	for _, tc := range []struct{ tool, arg string }{{"pick", "A4"}, {"pick", "678"}, {"get_balances", "12345678"}, {"get_positions", "a4"}} {
		if r := e.call(tc.tool, map[string]string{"account": tc.arg}); r.code() != "account_not_shared" || !strings.Contains(r.text, notShared) {
			t.Errorf("%s of %q: %s", tc.tool, tc.arg, r.text)
		}
	}
	for _, args := range []string{
		`{"id":"73508261999","account":"A4"}`, `{"id":"T73508261999","account":"678"}`,
		`{"id":"T~20250305T150000.0123456789ab","account":"A4"}`,
	} {
		if r := e.call("get_transaction", args); r.code() != "account_not_shared" || !strings.Contains(r.text, notShared) {
			t.Errorf("get_transaction %s: %s", args, r.text)
		}
	}
	if r := e.call("get_transaction", `{"id":"73508261999","account":"A5"}`); r.code() != "account_not_shared" ||
		!strings.Contains(r.text, "Schwab no longer shares ...265; ask the user to run schwabtx login and tick it.") {
		t.Errorf("get_transaction of an account that the login stopped sharing: %s", r.text)
	}
	if r := e.call("pick", `{"account":"999"}`); r.code() != "account_not_found" || strings.Contains(r.text, "...678") ||
		!strings.Contains(r.text, `The login shares ...135 "Individual" (account_hash `) {
		t.Errorf("an account of none, to a tool that reads Schwab: %s", r.text)
	}
	if r := e.call("get_transactions", `{"account":"999"}`); r.code() != "account_not_found" ||
		!strings.Contains(r.text, `(account_hash `+schwabtest.HashFor(empty)+"). Only the local history knows ...678 (store_ref A4), "+
			"...265 (store_ref A5). Pass one of those.") {
		t.Errorf("an account of none, to get_transactions: %s", r.text)
	}
	if r := e.call("get_transactions", `{"account":"A4","source":"store"}`); r.isError || txnCoverageOf(r, "...678")["read_from"] != "store" {
		t.Errorf("the account that only the store knows, from the store: %.300s", r.text)
	}
	// A login that has ended does not concern a call of accounts that the
	// login does not share: under auto, it checks no login, and says nothing
	// of Schwab, though the server knows the login has ended, and the
	// directory's read of Schwab is warm.
	e = newEnv(t, withStore(path), withLoginAt(testNow.Add(-10*time.Minute)))
	e.plant("12345678", gone)
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	e.fake.EndLogin()
	e.fake.RevokeAccess()
	if r := e.call("get_transactions", `{"account":"135","source":"schwab"}`); r.code() != "login_expired" {
		t.Fatalf("the login that Schwab ended: %.300s", r.text)
	}
	if r := e.call("get_transactions", `{"account":"A4"}`); r.isError || txnCoverageOf(r, "...678")["read_from"] != "store" ||
		slices.Contains(r.warnings(), "api_unavailable") || strings.Contains(r.text, "login_expired") {
		t.Errorf("the account that only the store knows, under auto, the login ended: %.600s", r.text)
	}
}

// When the preferences cannot be read, the nicknames are null and the
// answer says so; when they differ from what Schwab has sent before, the
// answer says where, with Schwab's values masked.
func TestNicknames(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withPick(), withFake(func(f *schwabtest.Server) {
		f.Override(schwabtest.UserPreferenceRoute, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"down"}`, http.StatusServiceUnavailable)
		})
	}))
	r := e.call("pick", `{}`)
	if r.isError || !slices.Contains(r.warnings(), "nicknames_missing") || strings.Contains(r.text, `"nickname":"`) {
		t.Errorf("without the preferences: %s", r.text)
	}
	e = newEnv(t, withPick(), withFake(func(f *schwabtest.Server) {
		f.SetUserPreference(`{"accounts":[{"accountNumber":"29470135","nickName":29470135}]}`)
	}))
	r = e.call("pick", `{}`)
	if r.isError || !slices.Contains(r.warnings(), "problems") {
		t.Errorf("preferences with a problem: %s", r.text)
	}
}

// Before any request, the call checks what costs Schwab nothing (design
// 6.2): a config that cannot be used, the app key and secret, a saved login
// that has not lapsed nor been refused; a login that lapses within 48
// hours is warned of; and every login error asks the user to run schwabtx
// login in their own terminal, never the model.
func TestLoginErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts []envOption
		code string
		want []string
	}{
		{"missing", []envOption{withoutLogin()}, "not_logged_in", []string{
			"There is no Schwab login at ~/.config/schwabtx/token.json (from config.json).",
			"in their own terminal", "do not run it yourself", "run `schwabtx setup` there",
		}},
		{"lapsed", []envOption{withLoginAt(testNow.Add(-8 * 24 * time.Hour))}, "login_expired", []string{
			"logins last 7 days", "do not run it yourself",
		}},
		{"no app", []envOption{withConfig(func(c *config.Config) { c.AppKey = config.Setting{} })}, "not_configured", []string{
			"no Schwab app key and secret", "`schwabtx setup`",
		}},
		{"unreadable config", []envOption{withConfig(func(c *config.Config) {
			c.Problems = append(c.Problems, config.Problem{Code: config.CodeUnreadable, Setting: "config_file", Path: c.File})
		})}, "config_unreadable", []string{"at ~/.config/schwabtx/config.json (it could not be read"}},
		{"stray argument", []envOption{withConfig(func(c *config.Config) {
			c.Problems = append(c.Problems, config.Problem{Code: ProblemStrayArgument, Setting: "arguments"})
		})}, "config_unreadable", []string{"must be quoted in a shell line"}},
		// A path with a space, not quoted: --config names its first part,
		// which does not exist, and the rest is a stray argument, which
		// cmd/schwab-mcp adds after the config's problems.
		{"a path split at its space", []envOption{withConfig(func(c *config.Config) {
			c.Problems = append(c.Problems, config.Problem{Code: config.CodeMissing, Setting: "config_file", Path: filepath.Join(filepath.Dir(c.File), "Application")},
				config.Problem{Code: ProblemStrayArgument, Setting: "arguments"})
		})}, "config_unreadable", []string{"must be quoted in a shell line"}},
		{"plain http", []envOption{withConfig(func(c *config.Config) {
			c.BaseURL = config.Setting{Value: "http://example.com", From: config.FromFile}
		})}, "config_unreadable", []string{"https, or http to this computer only"}},
	} {
		e := newEnv(t, append(tc.opts, withPick())...)
		r := e.call("pick", `{}`)
		if r.code() != tc.code {
			t.Errorf("%s: %s, want [%s]", tc.name, r.text, tc.code)
		}
		for _, w := range tc.want {
			if !strings.Contains(r.text, w) {
				t.Errorf("%s: %s, want it to say %q", tc.name, r.text, w)
			}
		}
		if n := e.fake.APICalls(); n != 0 {
			t.Errorf("%s: %d requests reached the API", tc.name, n)
		}
	}

	e := newEnv(t, withPick(), withLoginAt(testNow.Add(-6*24*time.Hour)))
	r := e.call("pick", `{"account":"135"}`)
	if r.isError || !slices.Contains(r.warnings(), "login_expires_soon") ||
		!strings.Contains(r.text, "lapses in about 24 hours (2026-11-14 16:40 ET)") {
		t.Errorf("a login that lapses within 48 hours: %s", r.text)
	}

	// Schwab refuses the login: the refresh is refused, and the next call
	// knows it without a request.
	e = newEnv(t, withPick(), withLoginAt(testNow.Add(-24*time.Hour)), withFake(func(f *schwabtest.Server) { f.EndLogin() }))
	if r := e.call("pick", `{}`); r.code() != "login_expired" {
		t.Errorf("a refused login: %s", r.text)
	}
	requests := e.fake.TokenRequests() + e.fake.APICalls()
	if r := e.call("pick", `{}`); r.code() != "login_expired" {
		t.Errorf("a refused login, again: %s", r.text)
	}
	if n := e.fake.TokenRequests() + e.fake.APICalls(); n != requests {
		t.Errorf("a login refused before made %d more requests", n-requests)
	}
}

// A call whose request ends while it waits on Schwab, as when the client
// cancels it or closes its input, is timed_out, as after a signal, and the
// log does not say that Schwab could not be reached: the SDK ends a request
// at the input's end with io.EOF as the cause, and an HTTP request to
// Schwab fails with that cause, which is no failure of Schwab's or of the
// network's. The tool here fails as such a request does once its context
// ends.
func TestEndedRequestIsNoSchwabFailure(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	e := newEnv(t, withoutStore(), withTools(func(s *Server) {
		testTool(s, "waits", nil, func(ctx context.Context, _ *call, _ struct{}) (echoOut, error) {
			started <- struct{}{}
			<-ctx.Done()
			return echoOut{}, &requestError{err: fmt.Errorf("GET /trader/v1/accounts: %w", io.EOF)}
		})
	}))
	ctx, cancel := context.WithCancel(t.Context())
	called := make(chan error, 1)
	go func() {
		_, err := e.cs.CallTool(ctx, callParams("waits"))
		called <- err
	}()
	<-started
	cancel()
	if err := <-called; err == nil {
		t.Error("a cancelled call answered")
	}
	deadline := time.Now().Add(time.Minute)
	for !strings.Contains(e.logs.String(), "msg=call tool=waits") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "msg=call tool=waits duration=") || !strings.Contains(logs, "outcome=timed_out") ||
		strings.Contains(logs, "could not be reached") {
		t.Errorf("a call whose request ended:\n%s", logs)
	}
}

// A protocol error that the SDK words before the middleware runs, for
// params it cannot decode, quotes no part of the request: its message is
// the method and the code's words. Any other keeps the SDK's words, with
// the known numbers and the secrets replaced.
func TestProtocolErrorsQuoteNoRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore())
	const unknown = "5550001234"
	e.plant(unknown)
	out := e.runOnce(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"portfolio_review","arguments":{"account":`+unknown+`}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":`+unknown+`,"arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"schwab://accounts/`+individual+`/nothing"}}`,
	)
	for id, want := range map[string]string{
		`"id":2`: `{"code":-32602,"message":"handling 'prompts/get': invalid params: they do not decode as the method's"}`,
		`"id":3`: `{"code":-32602,"message":"handling 'tools/call': invalid params: they do not decode as the method's"}`,
		`"id":4`: `{"code":-32602,"message":"resource not found"}`,
	} {
		if i := slices.IndexFunc(out, func(l string) bool { return strings.Contains(l, id+",") }); i < 0 || !strings.Contains(out[i], `"error":`+want) {
			t.Errorf("the answer of %s: want the error %s, got\n%s", id, want, strings.Join(out, "\n"))
		}
	}
	e.assertClean()

	// guardLine words the errors of a batch too, keeps what is not an
	// error byte for byte, and replaces the known numbers in the SDK's
	// other errors, which a hit of the backstop's logs.
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mask.known.Add(individual)
	for _, tc := range []struct{ in, want string }{
		{
			`{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"\"29470135\" unsupported"}}`,
			`{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"\"...135\" unsupported"}}`,
		},
		{
			`[{"jsonrpc":"2.0","id":8,"error":{"code":-32602,"message":"handling 'prompts/get': invalid params: unmarshaling \"{\\\"account\\\":5550001234}\""}},` +
				`{"jsonrpc":"2.0","id":9,"result":{}}]`,
			`[{"jsonrpc":"2.0","id":8,"error":{"code":-32602,"message":"handling 'prompts/get': invalid params: they do not decode as the method's"}},` +
				`{"jsonrpc":"2.0","id":9,"result":{}}]`,
		},
		{`{"jsonrpc":"2.0","id":10,"result":{"error":"29470135"}}`, `{"jsonrpc":"2.0","id":10,"result":{"error":"29470135"}}`},
	} {
		if got := string(s.guardLine([]byte(tc.in + "\n"))); got != tc.want+"\n" {
			t.Errorf("guardLine(%s) =\n%s\nwant\n%s", tc.in, got, tc.want)
		}
	}
}

// A path in a log line shows as masker.path shows it, or masked by pattern
// as outside text, even before the server has seen the numbers its names
// hold: the start line's config and problems, and the warnings of a token
// file and a store that others can read, which the schwab and history
// packages word with the whole path. Run comes first, and auth_status reads
// the token file and opens the store before the directory has read
// Schwab's accounts.
func TestLogsMaskPaths(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "home")
	token := filepath.Join(home, "Schwab-"+individual, "token-"+options+".json")
	store := filepath.Join(home, "Schwab", empty, "history.db")
	copyFile(t, sharedStore(t), store)
	const rate = "443322110099"
	e := newEnv(t, withHome(home), withTokenFile(token), withStore(store),
		withConfigFile(filepath.Join(home, "Schwab-"+individual, "config.json")),
		withConfig(func(c *config.Config) {
			c.RateFile.Value = filepath.Join(home, ".cache", "rate-"+rate)
			c.Problems = append(c.Problems, config.Problem{Code: config.CodeMadePrivate, Setting: "config_file", Path: c.File},
				config.Problem{Code: config.CodeRelativePath, Setting: "SCHWAB_STORE_FILE", Path: filepath.Join("acct-"+individual, "history.db")})
		}))
	e.plant(rate)
	loose := runtime.GOOS != "windows"
	if loose {
		for _, f := range []string{token, store} {
			if err := os.Chmod(f, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.runOnce()
	if r := e.call("auth_status", `{}`); r.isError {
		t.Fatal(r.text)
	}
	logs := e.logs.String()
	want := []string{
		"config.file=~/Schwab-...135/config.json", "config.token_file.path=~/Schwab-...135/token-...398.json",
		"config.store_file.path=~/Schwab/...783/history.db", "config.rate_file.path=~/.cache/rate-...099",
		"config_file: others could read it; it is now readable only by you (~/Schwab-...135/config.json)",
		"SCHWAB_STORE_FILE: its path is not absolute (history.db)",
	}
	if loose {
		want = append(want, `msg="~/Schwab-...135/token-...398.json could be read by others; it is now readable only by you" component=schwab`,
			`msg="~/Schwab/...783/history.db can be read by others;`)
	}
	for _, w := range want {
		if !strings.Contains(logs, w) {
			t.Errorf("the log lacks %s:\n%s", w, logs)
		}
	}
}

// An address of the API that the server refuses is logged by its host and
// why, in the server's words: schwab.NewAuth's error quotes the whole
// address, which may hold a user and a password.
func TestRefusedAddressIsLoggedByItsHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ base, want string }{
		{"https://jdoe:pass29470135word@127.0.0.1:1", "the API's address cannot be used: it holds a user name\" host=127.0.0.1:1"},
		{"http://example.com/x?user=jdoe", "the API's address cannot be used: it holds a query or a fragment\" host=example.com"},
		{"http://example.com", "the API's address cannot be used: it is plain http to another computer\" host=example.com"},
		{"ftp://jdoe@example.com", "the API's address cannot be used: it is not an https or http address\" host=example.com"},
	} {
		e := newEnv(t, withoutStore(), withPick(), withConfig(func(c *config.Config) {
			c.BaseURL = config.Setting{Value: tc.base, From: config.FromFile}
		}))
		if r := e.call("pick", `{}`); r.code() != "config_unreadable" {
			t.Errorf("%s: %s", tc.base, r.text)
		}
		logs := e.logs.String()
		if !strings.Contains(logs, tc.want) || strings.Contains(logs, "jdoe") || strings.Contains(logs, "pass") {
			t.Errorf("%s: the log lacks %q, or holds the address's user:\n%s", tc.base, tc.want, logs)
		}
	}
	// addressRefusal refuses what NewAuth refuses, and takes what it takes.
	token := filepath.Join(t.TempDir(), "token.json")
	for _, base := range []string{
		"", schwab.DefaultBaseURL, "http://127.0.0.1:8080", "http://localhost", "http://[::1]:1",
		"https://jdoe@example.com", "http://example.com", "https://example.com/#x", "ftp://example.com", "https://",
	} {
		_, err := schwab.NewAuth(schwab.Config{AppKey: "k", AppSecret: "s", BaseURL: base, TokenFile: token})
		if why := addressRefusal(base); (why != "") != (err != nil) {
			t.Errorf("addressRefusal(%q) = %q, and schwab.NewAuth: %v", base, why, err)
		}
	}
}

// A token file's path that schwab.NewAuth cannot follow, a.json being a
// link to b.json and b.json one back to a.json, is config_unreadable that
// names the path, in a tool's error and in auth_status's action alike: not
// the API's address, which here is one that the server may use. Windows
// takes such a path as it is.
func TestTokenPathThatCannotBeFollowed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("schwab.NewAuth takes a path of links that cannot be followed as it is on Windows")
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	for _, link := range [][2]string{{b, a}, {a, b}} {
		if err := os.Symlink(link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}
	e := newEnv(t, withoutStore(), withoutLogin(), withTokenFile(a), withPick())
	const says = "The server cannot follow the token file's path, a.json (from config.json), as when a link on it leads back to itself."
	if r := e.call("pick", `{}`); r.code() != "config_unreadable" || !strings.Contains(r.text, says) || strings.Contains(r.text, "address") {
		t.Errorf("a tool: %s", r.text)
	}
	if r := e.call("auth_status", `{}`); !strings.HasPrefix(fmt.Sprint(r.value["action"]), says) {
		t.Errorf("auth_status: %s", r.text)
	}
	if n := e.fake.APICalls(); n != 0 {
		t.Errorf("%d requests reached the API", n)
	}
}

// Every code has a sentence, but those that each call composes, and no
// sentence is for a code the list lacks.
func TestErrorCodesHaveText(t *testing.T) {
	t.Parallel()
	composed := []errorCode{
		codeNotConfigured, codeConfigUnreadable, codeNotLoggedIn, codeBusy, codeAccountNotFound, codeAccountAmbiguous,
		codeAccountNotShared, codeNotFound, codeBadArgument, codeBadCursor,
	}
	for _, code := range errorCodes {
		if _, ok := errorText[code]; !ok && !slices.Contains(composed, code) {
			t.Errorf("%s has no sentence", code)
		}
	}
	for code := range errorText {
		if !slices.Contains(errorCodes, code) {
			t.Errorf("%s has a sentence and is not listed", code)
		}
	}
}

// toolError words each kind of failure with its code, and gives Schwab's
// own words only as the detail.
func TestToolError(t *testing.T) {
	t.Parallel()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.mask.known.Add(individual)
	for _, tc := range []struct {
		err    error
		code   errorCode
		detail string
	}{
		{context.DeadlineExceeded, codeTimedOut, ""},
		{&requestError{err: schwab.ErrLoginExpired}, codeLoginExpired, ""},
		{&requestError{err: &schwab.Error{Status: 429}}, codeRateLimited, "HTTP 429 from Schwab"},
		{&requestError{err: &schwab.Error{Status: 400, Message: "bad 29470135"}}, codeSchwabRefused, "HTTP 400 from Schwab: bad ...135"},
		{&requestError{err: &schwab.Error{Status: 503}}, codeSchwabUnavailable, "HTTP 503 from Schwab"},
		{&requestError{err: &schwab.Error{Status: 403, Reference: "18.1"}}, codeEdgeRefused, "(reference #18.1)"},
		{&requestError{err: &schwab.Error{Status: 500, Body: "<h1>Whitelabel Error Page</h1>"}}, codeAppNotReady, "HTTP 500"},
		{&requestError{err: &schwab.Error{Status: 502, Code: "protocol.http.TooBigBody"}}, codeTooBig, "TooBigBody"},
		{&requestError{err: errors.New("dial tcp: connection refused")}, codeSchwabUnavailable, ""},
		{fmt.Errorf("wrapped: %w", errStoreRead), codeStoreUnusable, ""},
		{errors.New("a bug"), codeInternal, ""},
	} {
		e := s.toolError("t", tc.err)
		if e.code != tc.code || !strings.Contains(string(e.detail), tc.detail) || (tc.detail == "") != (e.detail == "") {
			t.Errorf("%v: %s, want [%s] with the detail %q", tc.err, e.Error(), tc.code, tc.detail)
		}
	}
}

// The call's reserve is the least of 10 seconds and a quarter of its
// budget; roomForMore says whether more than that is left.
func TestReserve(t *testing.T) {
	t.Parallel()
	for budget, want := range map[time.Duration]time.Duration{45 * time.Second: 10 * time.Second, 8 * time.Second: 2 * time.Second} {
		c := &call{budget: budget, deadline: time.Now().Add(budget)}
		if got := c.reserve(); got != want {
			t.Errorf("a budget of %v keeps back %v, want %v", budget, got, want)
		}
		if !c.roomForMore() {
			t.Errorf("a budget of %v has no room at its start", budget)
		}
		c.deadline = time.Now().Add(want / 2)
		if c.roomForMore() {
			t.Errorf("a budget of %v has room within its reserve", budget)
		}
	}
}

// New builds a server from options with every default, registering every
// tool, and refuses options that cannot work; addTool refuses, at startup,
// a tool whose output does not embed the envelope, a fitter that does not
// embed cuts, an argument without a description, and a schema outside the
// portable subset.
func TestNew(t *testing.T) {
	t.Parallel()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.o.CallTimeout != 45*time.Second || s.o.MaxCalls != 4 || s.o.PageBytes != 20_000 || s.o.MaxBytes != 24_000 {
		t.Errorf("defaults %+v", s.o)
	}
	if err := s.Close(); err != nil {
		t.Error(err)
	}
	for _, o := range []Options{{PageBytes: 24_000}, {PageBytes: 100, MaxBytes: 50}, {CallTimeout: -time.Second}} {
		if _, err := New(o); err == nil {
			t.Errorf("New took %+v", o)
		}
	}
	type bare struct {
		Count int `json:"count"`
	}
	type undescribed struct {
		Mystery string `json:"mystery,omitempty"`
	}
	type mapped struct {
		envelope
		Values map[string]int `json:"values"`
	}
	for name, register := range map[string]func(*Server){
		"an output without the envelope": func(s *Server) {
			testTool(s, "bad", nil, func(context.Context, *call, struct{}) (bare, error) { return bare{}, nil })
		},
		"a fitter without cuts": func(s *Server) {
			testTool(s, "bad", nil, func(context.Context, *call, struct{}) (noCutsFitter, error) { return noCutsFitter{}, nil })
		},
		"an argument without a description": func(s *Server) {
			testTool(s, "bad", nil, func(context.Context, *call, undescribed) (echoOut, error) { return echoOut{}, nil })
		},
		"a map in the output": func(s *Server) {
			testTool(s, "bad", nil, func(context.Context, *call, struct{}) (mapped, error) { return mapped{}, nil })
		},
	} {
		s, err := New(Options{})
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: addTool took it", name)
				}
			}()
			register(s)
		}()
		s.Close()
	}
}

// noCutsFitter is a fitter that does not embed cuts.
type noCutsFitter struct {
	envelope
	Raw json.RawMessage `json:"raw"`
}

func (o *noCutsFitter) dropRaw() bool     { return false }
func (o *noCutsFitter) lists() []cuttable { return nil }

// An answer names one of the four sources; one that names another is a
// bug, answered as internal.
func TestSourceIsOneOfFour(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withTools(func(s *Server) {
		testTool(s, "no_source", nil, func(context.Context, *call, struct{}) (echoOut, error) { return echoOut{}, nil })
		testTool(s, "other_source", nil, func(context.Context, *call, struct{}) (echoOut, error) {
			return echoOut{envelope: envelope{Source: "elsewhere"}}, nil
		})
	}))
	for _, tool := range []string{"no_source", "other_source"} {
		if r := e.call(tool, `{}`); r.code() != "internal" {
			t.Errorf("%s: %s, want [internal]", tool, r.text)
		}
	}
}
