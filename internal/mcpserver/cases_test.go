package mcpserver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
}

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
// above holds on its own.
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
			for _, c := range leakCalls {
				e.call(c.tool, c.args)
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
