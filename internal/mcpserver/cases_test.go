package mcpserver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The tests that run every tool's cases. Each tool's test file adds its
// own, in a package-level var (var _ = addToolCases(...)), so that the
// shared tests need no edit when a tool lands:
//
//   - toolCases, for TestToolsEmptyAndFull (design 15.2): at least an
//     empty and a full case per tool, each compared with its golden answer;
//   - leakPlantings and leakCalls, for TestNoAccountNumberLeaks (15.3): what
//     a tool's world plants in the fake, and the calls that must leak none;
//   - sizeCases, for TestSizeCap (15.7).

// A toolCase is one case of TestToolsEmptyAndFull: a call of tool with
// args, a JSON object, in a world that opts set up, whose answer, compact,
// testdata/golden/<tool>_<name>.json holds (go test -update rewrites it),
// and which check, when not nil, checks further. Every tool has an "empty"
// case and a "full" case.
type toolCase struct {
	tool, name string
	opts       []envOption
	args       string
	check      func(t *testing.T, e *env, r result)
}

// toolCases are the cases of TestToolsEmptyAndFull.
var toolCases []toolCase

// addToolCases adds cs to toolCases.
func addToolCases(cs ...toolCase) bool {
	toolCases = append(toolCases, cs...)
	return true
}

// The protocol versions each case runs in: 2026-07-28, the newest, through
// server/discover, and 2025-06-18 through initialize.
var caseProtocols = []string{"", "2025-06-18"}

// Each tool answers each of its cases, in a 2026-07-28 session and in a
// 2025-06-18 one alike, as its golden says: every figure's change shows in
// review. env.call checks each answer against its output schema, its text
// against its structured content, its size, and every message for leaks.
func TestToolsEmptyAndFull(t *testing.T) {
	t.Parallel()
	cases := slices.Clone(toolCases)
	slices.SortFunc(cases, func(a, b toolCase) int { return strings.Compare(a.tool+"_"+a.name, b.tool+"_"+b.name) })
	for _, tc := range cases {
		for _, protocol := range caseProtocols {
			name := tc.tool + "_" + tc.name + "/" + protocol
			if protocol == "" {
				name = tc.tool + "_" + tc.name + "/newest"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t, append(slices.Clone(tc.opts), withProtocol(protocol))...)
				r := e.call(tc.tool, tc.args)
				golden := filepath.Join("testdata", "golden", tc.tool+"_"+tc.name+".json")
				if *update && protocol != "" {
					return // the newest session writes it
				}
				checkGolden(t, golden, e.goldenText(r))
				if tc.check != nil {
					tc.check(t, e, r)
				}
			})
		}
	}
}

// goldenText returns r as its golden holds it: an answer indented, an error
// as its text, each with the fake's address, which changes from run to run,
// written 127.0.0.1:PORT.
func (e *env) goldenText(r result) []byte {
	e.t.Helper()
	text := []byte(r.text)
	if !r.isError {
		var b bytes.Buffer
		if err := json.Indent(&b, text, "", "  "); err != nil {
			e.t.Fatal(err)
		}
		text = b.Bytes()
	}
	text = bytes.ReplaceAll(text, []byte(e.fake.Addr()), []byte("127.0.0.1:PORT"))
	return append(text, '\n')
}

// Every tool that the server serves has an empty and a full case, so that
// the output check has run on every schema both ways; and every golden is
// a case's.
func TestEveryToolCovered(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for name := range e.tools {
		for _, want := range []string{"empty", "full"} {
			if !slices.ContainsFunc(toolCases, func(tc toolCase) bool { return tc.tool == name && tc.name == want }) {
				t.Errorf("%s has no %s case", name, want)
			}
		}
	}
	files, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !slices.ContainsFunc(toolCases, func(tc toolCase) bool { return filepath.Base(f) == tc.tool+"_"+tc.name+".json" }) {
			t.Errorf("%s is the golden of no case", f)
		}
	}
}

// toolTitles are the tools' titles, as design 10.0's table gives them.
var toolTitles = map[string]string{
	"auth_status":      "Schwab login status",
	"list_accounts":    "Schwab accounts",
	"get_balances":     "Schwab balances",
	"get_positions":    "Schwab positions",
	"get_transactions": "Schwab transactions",
	"get_transaction":  "One Schwab transaction",
	"get_orders":       "Schwab orders",
	"get_order":        "One Schwab order",
}

// Every tool has design 10.0's title, as its own and in its annotations,
// and declares every hint as 10.0 has it: read-only, not destructive and
// idempotent, and a closed world (decision 25), since the spec's defaults
// are destructive and an open world, and an editor client asks the user to
// review each result of a tool of an open world. Each is read from
// tools/list as the wire carried it, in a 2026-07-28 session and in a
// 2025-06-18 one.
func TestToolAnnotations(t *testing.T) {
	t.Parallel()
	for _, protocol := range caseProtocols {
		e := newEnv(t, withProtocol(protocol), withoutStore())
		var tools []struct {
			Name        string         `json:"name"`
			Title       string         `json:"title"`
			Annotations map[string]any `json:"annotations"`
		}
		for _, line := range e.wire.lines() {
			msg, ok := strings.CutPrefix(line, "read: ")
			if !ok {
				continue
			}
			var m struct {
				Result *struct {
					Tools json.RawMessage `json:"tools"`
				} `json:"result"`
			}
			if json.Unmarshal([]byte(msg), &m) == nil && m.Result != nil && m.Result.Tools != nil {
				if err := json.Unmarshal(m.Result.Tools, &tools); err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(tools) != len(e.tools) {
			t.Fatalf("%q: tools/list's answer lists %d tools, want %d", protocol, len(tools), len(e.tools))
		}
		for _, tool := range tools {
			title, ok := toolTitles[tool.Name]
			if !ok {
				t.Errorf("%s has no title in toolTitles", tool.Name)
				continue
			}
			got, err := json.Marshal(tool.Annotations)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"destructiveHint":false,"idempotentHint":true,"openWorldHint":false,"readOnlyHint":true,"title":"` + title + `"}`
			if tool.Title != title || string(got) != want {
				t.Errorf("%q: %s has the title %q and the annotations %s, want %q and %s", protocol, tool.Name, tool.Title, got, title, want)
			}
		}
	}
}

// A leakWorld is TestNoAccountNumberLeaks' world, which each tool's
// plantings add to. The core's planting runs first, its file's name being
// first, and sets store.
type leakWorld struct {
	// home is the server's home folder, whose name holds a planted number.
	home string
	// store is the store that the server reads, a copy of the shared store
	// under home, which a planting may add to.
	store string
	// opts set up the env, after the world's own.
	opts []envOption
	// base returns the options that the world's sides take too, for a world
	// whose home folder is home: the world's paths, under home, and its
	// accounts, but not what a tool's planting makes Schwab answer. The
	// core's planting sets it, and puts its options for the world's own
	// home in opts.
	base func(t *testing.T, home string) []envOption
	// planted are the numbers planted beyond the fixtures'.
	planted []string
	// masked are texts that the answers must hold, so that a test that
	// strips everything cannot pass by accident.
	masked []string
	// forbidden are texts beyond the numbers and the secrets that nothing
	// sent or logged may hold, such as the names of a planted user.
	forbidden []string
	// checks run once the calls are done, for what a tool's own leak test
	// must look at in the env, such as the digests of its cursors.
	checks []func(t *testing.T, e *env)
	// sides are worlds beside this one, made from it, whose calls answer
	// the error codes that its own cannot.
	sides []leakSide
}

// A leakSide is a world beside TestNoAccountNumberLeaks' own, made from its
// base, under a home folder of its own, with opts after it, for error codes
// that its own world cannot give: a server without its app key or a login,
// a config with problems, a store that cannot be read, Schwab refusing in
// each of its ways. Its calls are made in turn, or run makes them, for
// what calls in turn cannot do, and returns their results.
type leakSide struct {
	name string
	// opts, when not nil, returns the side's options; home is its home
	// folder, which holds a planted number.
	opts  func(home string) []envOption
	calls []leakCall
	run   func(t *testing.T, e *env) []result
}

// leakCodes are the error codes that TestNoAccountNumberLeaks' calls must
// answer, or quote in a warning, in its worlds: every code but internal,
// which only a bug of the server's gives, and token_refused, which
// schwab.Client.Get never returns, since it answers a token that Schwab
// refuses as soon as it was issued with Schwab's own 401, a schwab_refused.
var leakCodes = slices.DeleteFunc(slices.Clone(errorCodes), func(c errorCode) bool {
	return c == codeInternal || c == codeTokenRefused
})

// codeMention is an error code in a result's text: the error's own, or one
// that a warning quotes.
var codeMention = regexp.MustCompile(`\[([a-z_]+)\] `)

// plant adds numbers to those that must never appear.
func (w *leakWorld) plant(numbers ...string) { w.planted = append(w.planted, numbers...) }

// leakPlantings set up TestNoAccountNumberLeaks' world: the fake's, the
// store's and the paths', each tool's own.
var leakPlantings []func(t *testing.T, w *leakWorld)

// A leakCall is one call of TestNoAccountNumberLeaks: a tool and its
// arguments, a JSON object.
type leakCall struct {
	tool, args string
}

// leakCalls are TestNoAccountNumberLeaks' calls.
var leakCalls []leakCall

// addLeaks adds a tool's plantings and calls.
func addLeaks(plantings []func(t *testing.T, w *leakWorld), calls ...leakCall) bool {
	leakPlantings = append(leakPlantings, plantings...)
	leakCalls = append(leakCalls, calls...)
	return true
}

// No full account number reaches the model or the log (design 15.3): in a
// world that plants numbers where Schwab, the store and the paths put
// them, every leak call of every tool answers, and nothing the server sends
// or logs holds a planted number, plainly or as mask.Text reads one, a
// secret, or a text a planting forbids; the masked forms are there; each
// planting's own checks hold; and the fake granted no authorization code
// while the server ran. With the backstop off, the same holds: each layer
// above holds on its own. And with it off, the worlds beside it (leakSide)
// are held to the same, each at once, and the calls of all of them answer,
// or quote in a warning, every code of leakCodes. They run once, with the
// backstop off, where each layer above must hold on its own: on, the hook
// would fail on a value the backstop found, and the scan finds the rest,
// so a second run would find nothing more.
func TestNoAccountNumberLeaks(t *testing.T) {
	t.Parallel()
	for _, off := range []bool{false, true} {
		name := "backstop on"
		if off {
			name = "backstop off"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := &leakWorld{home: filepath.Join(t.TempDir(), "home-"+empty)}
			if err := os.MkdirAll(w.home, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, p := range leakPlantings {
				p(t, w)
			}
			opts := append([]envOption{withHome(w.home)}, w.opts...)
			if off {
				opts = append(opts, withOptions(func(o *Options) { o.backstopOff = true }))
			}
			e := newEnv(t, opts...)
			e.plant(w.planted...)
			grants, _ := e.fake.Grants()
			codes := map[string]bool{}
			note := func(rs ...result) {
				for _, r := range rs {
					for _, m := range codeMention.FindAllStringSubmatch(r.text, -1) {
						codes[m[1]] = true
					}
				}
			}
			for _, c := range leakCalls {
				note(e.call(c.tool, c.args))
			}
			e.assertClean()
			sent := strings.Join(e.sent(), "\n")
			for _, m := range w.masked {
				if !strings.Contains(sent, m) {
					t.Errorf("no answer holds %q", m)
				}
			}
			for _, f := range w.forbidden {
				if strings.Contains(sent, f) {
					t.Errorf("an answer or a log line holds %q", f)
				}
			}
			for _, check := range w.checks {
				check(t, e)
			}
			if now, _ := e.fake.Grants(); now != grants {
				t.Errorf("the fake granted %d authorization codes while the server ran", now-grants)
			}
			if strings.Contains(sent, schwabtest.AppSecret) {
				t.Error("the app secret was sent")
			}
			if !off {
				return
			}
			var mu sync.Mutex
			t.Run("sides", func(t *testing.T) {
				for _, side := range w.sides {
					t.Run(side.name, func(t *testing.T) {
						t.Parallel()
						home := filepath.Join(t.TempDir(), "home-"+empty)
						opts := append([]envOption{withHome(home), withOptions(func(o *Options) { o.backstopOff = true })}, w.base(t, home)...)
						if side.opts != nil {
							opts = append(opts, side.opts(home)...)
						}
						e := newEnv(t, opts...)
						e.plant(w.planted...)
						var rs []result
						if side.run != nil {
							rs = side.run(t, e)
						}
						for _, c := range side.calls {
							rs = append(rs, e.call(c.tool, c.args))
						}
						e.assertClean()
						sent := strings.Join(e.sent(), "\n")
						for _, f := range w.forbidden {
							if strings.Contains(sent, f) {
								t.Errorf("an answer or a log line holds %q", f)
							}
						}
						mu.Lock()
						note(rs...)
						mu.Unlock()
					})
				}
			})
			for _, code := range leakCodes {
				if !codes[string(code)] {
					t.Errorf("no call of the leak worlds answered %s or quoted it in a warning", code)
				}
			}
		})
	}
}

// A sizeCase is one case of TestSizeCap: a world, and what to call in it
// and check.
type sizeCase struct {
	name string
	opts []envOption
	run  func(t *testing.T, e *env)
}

// sizeCases are TestSizeCap's cases.
var sizeCases []sizeCase

// addSizeCases adds cs to sizeCases.
func addSizeCases(cs ...sizeCase) bool {
	sizeCases = append(sizeCases, cs...)
	return true
}

// Every answer fits the cap (design 8.3): each tool's size cases, and the
// core's own of a fitter, of an answer that is no fitter, and of the
// margin between the page budget and the cap.
func TestSizeCap(t *testing.T) {
	t.Parallel()
	for _, sc := range sizeCases {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, newEnv(t, sc.opts...))
		})
	}
}
