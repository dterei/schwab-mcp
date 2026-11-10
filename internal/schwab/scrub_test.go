package schwab

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The scrubber's flags. A run with a flag is never served from the test
// cache.
var (
	scrubRaw     = flag.String("scrub", "", "make candidate fixtures of a live recording, the raw folder that -live.record kept (never in CI)")
	scrubOut     = flag.String("scrub.out", "", "the folder for the scrubber's output, which it makes, and which must not exist yet; required with -scrub")
	scrubAmounts = flag.String("scrub.amounts", "refuse", "what the scrubber does with money: refuse, keep or scale:K")
)

// TestScrub makes candidate fixtures of a live run's recording, never in
// CI:
//
//	go test ./internal/schwab -run '^TestScrub$' -scrub=<live out>/raw -scrub.out=DIR -v
//
// It reads the raw folder that -live.record kept, replaces every
// identifier with a fake, consistently across the recording, keeps or
// replaces every other value as the scrub policy says, and checks what it
// made by its gates before it writes anything to DIR: recorded/<date>/
// with the candidate fixtures, entries.json with their index entries,
// review.txt, which says what it did and holds no value of the
// recording, and shapes/. -scrub.amounts=refuse, the default, writes no
// answer that holds money, only its shape; keep keeps every amount, and
// scale:K multiplies each by K, which hides nothing. DIR may not be inside
// the module or a git work tree, nor exist yet.
//
// Without -scrub it is skipped; with it, it fails where CI or
// GITHUB_ACTIONS is set, so that no CI run fails for want of the flag, nor
// makes fixtures. The owner reads review.txt, copies recorded/<date>/ into
// schwabtest/testdata/recorded/<date>/ and adds entries.json's entries to
// index.json there; see docs/live.md.
func TestScrub(t *testing.T) {
	if *scrubRaw == "" {
		t.Skip("-scrub makes candidate fixtures of a live recording")
	}
	if err := scrubRefusesCI(os.Getenv); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("no home folder: ", err)
	}
	// What the test prints shows the home folder as ~ and hides the user
	// name, as the live mode's does.
	shown := newLiveScrubber()
	shown.setHome(home)
	module, err := moduleRoot(".")
	if err != nil {
		t.Fatal(shown.scrub("the module: " + err.Error()))
	}
	summary, err := scrubFolders(t, scrubRun{
		raw: *scrubRaw, out: *scrubOut, amounts: *scrubAmounts, getenv: os.Getenv, module: module, home: home,
		version: func() string { return gitCommit(t.Context(), module) },
	})
	if err != nil {
		t.Fatal(shown.scrub(err.Error()))
	}
	t.Log(shown.scrub(summary))
}

// A scrubRun is what TestScrub takes from its flags and its machine, for
// scrubFolders.
type scrubRun struct {
	raw, out, amounts string              // the flags -scrub, -scrub.out and -scrub.amounts
	getenv            func(string) string // reads the environment
	module, home      string              // the module's root and the home folder
	version           func() string       // the scrubber's version: the commit, and whether the tree is modified
}

// scrubRefusesCI returns why the scrubber must not run where getenv reads
// the environment, or nil: CI or GITHUB_ACTIONS set means a CI job, which
// holds no recording, and must never make fixtures.
func scrubRefusesCI(getenv func(string) string) error {
	for _, name := range []string{"CI", "GITHUB_ACTIONS"} {
		if getenv(name) != "" {
			return fmt.Errorf("-scrub never runs in CI, and %s is set", name)
		}
	}
	return nil
}

// scrubFolders scrubs the recording in r.raw into r.out, a folder it makes,
// and returns what to tell the owner. It refuses before it reads anything
// in CI, without -scrub.out, with a policy for amounts that it does not
// know, and for an out folder that exists or is inside the module or a git
// work tree, where the fixtures could be committed unread. It writes
// nothing unless the gates take the whole output.
func scrubFolders(t testing.TB, r scrubRun) (string, error) {
	t.Helper()
	if err := scrubRefusesCI(r.getenv); err != nil {
		return "", err
	}
	amounts, err := parseAmounts(r.amounts)
	if err != nil {
		return "", err
	}
	if r.out == "" {
		return "", errors.New("-scrub.out is required: the folder for the candidate fixtures, which the scrubber makes")
	}
	out, err := filepath.Abs(r.out)
	if err != nil {
		return "", err
	}
	if err := refuseInside(out, r.module); err != nil {
		return "", fmt.Errorf("%w; name another folder with -scrub.out", err)
	}
	if _, err := os.Lstat(out); err == nil {
		return "", fmt.Errorf("%s exists; -scrub.out names a folder that the scrubber makes", out)
	}
	raw, err := filepath.Abs(r.raw)
	if err != nil {
		return "", err
	}
	answers, err := readRecording(raw)
	if err != nil {
		return "", fmt.Errorf("reading the recording in %s: %w", raw, err)
	}
	o, err := scrubRecording(t, answers, scrubConfig{
		amounts: amounts, version: r.version(), home: r.home, paths: []string{raw, out, r.module},
	})
	if err != nil {
		return "", fmt.Errorf("the scrubber wrote nothing: %w", err)
	}
	if err := writeScrubOutput(out, o); err != nil {
		return "", err
	}
	left := len(answers) - len(o.files)
	return fmt.Sprintf("wrote %s to %s, with entries.json and review.txt beside them; %s left out, as review.txt says. "+
		"Read review.txt before you commit anything.",
		plural(len(o.files), "candidate fixture"), filepath.Join(out, "recorded", o.date), plural(left, "answer")), nil
}

// writeScrubOutput writes o to out, a folder that it makes, every folder
// and file readable only by the user, as a recording is: recorded/<date>/
// with the candidate fixtures and nothing else, so that the folder the owner
// copies holds only files with index entries; and beside it entries.json,
// review.txt and shapes/.
func writeScrubOutput(out string, o *scrubOutput) error {
	if err := makePrivateDir(filepath.Dir(out)); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	day := filepath.Join(out, "recorded", o.date)
	for _, dir := range []string{filepath.Join(out, "recorded"), day, filepath.Join(out, "shapes")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	for _, f := range o.files {
		if err := createPrivate(filepath.Join(day, filepath.Base(f.entry.File)), f.body); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(o.shapes)) {
		if err := createPrivate(filepath.Join(out, "shapes", name+".txt"), []byte(o.shapes[name])); err != nil {
			return err
		}
	}
	if err := createPrivate(filepath.Join(out, "entries.json"), o.entriesJSON()); err != nil {
		return err
	}
	return createPrivate(filepath.Join(out, "review.txt"), []byte(o.review))
}

// The scrubber refuses to run where CI or GITHUB_ACTIONS is set, and so in
// CI itself, where both are.
func TestScrubRefusesCI(t *testing.T) {
	for _, tc := range []struct {
		vars []string
		want string
	}{
		{nil, ""},
		{[]string{"SCHWAB_BASE_URL", "http://127.0.0.1"}, ""},
		{[]string{"CI", "true"}, "-scrub never runs in CI, and CI is set"},
		{[]string{"GITHUB_ACTIONS", "true"}, "-scrub never runs in CI, and GITHUB_ACTIONS is set"},
	} {
		getenv := func(name string) string {
			for i := 0; i+1 < len(tc.vars); i += 2 {
				if tc.vars[i] == name {
					return tc.vars[i+1]
				}
			}
			return ""
		}
		if err := scrubRefusesCI(getenv); fmt.Sprint(err) != tc.want && (err != nil || tc.want != "") {
			t.Errorf("scrubRefusesCI(%q) = %v, want %q", tc.vars, err, tc.want)
		}
	}
	if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
		if scrubRefusesCI(os.Getenv) == nil {
			t.Error("scrubRefusesCI takes this CI job")
		}
	}
}

// -scrub.amounts takes refuse, keep or scale:K with K a whole number of 2
// or more, written as such.
func TestParseAmounts(t *testing.T) {
	for in, want := range map[string]string{"refuse": "refuse", "keep": "keep", "scale:2": "scale:2", "scale:20": "scale:20"} {
		if p, err := parseAmounts(in); err != nil || p.String() != want {
			t.Errorf("parseAmounts(%q) = %v, %v", in, p, err)
		}
	}
	for _, in := range []string{"", "scale", "scale:1", "scale:0", "scale:-3", "scale:x", "scale:07", "scale:2.5", "scale: 2", "Keep", "drop"} {
		if p, err := parseAmounts(in); err == nil {
			t.Errorf("parseAmounts(%q) = %v", in, p)
		}
	}
}
