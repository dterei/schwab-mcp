package schwab

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// reportHeading opens report.txt.
const reportHeading = "schwabtx live report: what Schwab's API answered, as key paths, kinds, counts and statuses.\n" +
	"It holds no account number, hash, id, balance or description; read it before you share it.\n\n"

// w8Mark marks a line of the report that reveals W-8 withholding rows,
// which show the owner's nonresident tax status: the owner decides on each
// before the report is shared.
const w8Mark = "[W-8] "

// A liveReport is a live run's report.txt, written section by section as
// the run goes, so that a run that stops keeps what it has. Each line is
// scrubbed and self-checked before it is printed or written, and each
// section once more before it is written. At the end each section is
// scrubbed and checked again, with all that the run saw by then, and the
// whole report written over the file.
type liveReport struct {
	file  string
	scrub *liveScrubber
	show  func(line string) // prints a line, as -v does
	// clean scrubs a line or a section; a test replaces it to show that the
	// self-check catches what the scrubber missed.
	clean func(string) string
	// written holds each section as it was written, with said's marks, w8
	// how many lines were marked W-8, and problems why the report fails the
	// run.
	written  []string
	w8       int
	problems []string
}

// A reportSection is a part of the report, written whole: a title, such as
// an endpoint's template, and its lines.
type reportSection struct {
	title string
	lines []reportLine
}

// A reportLine is a line of the report, and whether it reveals W-8 rows.
// Its text is the run's own words, which may name Schwab's keys, headers
// and wording, with any of Schwab's own words in it marked by said, such
// as a message that is not on its wording: the scrubber hides the
// owner's nicknames and symbols only there, and every identifier and path
// anywhere.
type reportLine struct {
	text string
	w8   bool
}

// add adds a line to s.
func (s *reportSection) add(text string) {
	s.lines = append(s.lines, reportLine{text: text})
}

// addf adds a line to s, as fmt.Sprintf formats it.
func (s *reportSection) addf(format string, args ...any) {
	s.add(fmt.Sprintf(format, args...))
}

// addW8 adds a line to s that reveals W-8 rows.
func (s *reportSection) addW8(text string) {
	s.lines = append(s.lines, reportLine{text: text, w8: true})
}

// newLiveReport makes file, a report that is not there yet, readable only
// by the user. Its lines are scrubbed by scrub and printed by show.
func newLiveReport(file string, scrub *liveScrubber, show func(string)) (*liveReport, error) {
	if err := createPrivate(file, []byte(reportHeading)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s holds a report already, which is never written over", file)
		}
		return nil, err
	}
	return &liveReport{file: file, scrub: scrub, show: show, clean: scrub.scrub}, nil
}

// leftOutLine stands in the report for a line that the self-check left
// out.
const leftOutLine = "(a line left out: it held something that the run saw)"

// write scrubs and checks s, adds it to the end of the report, and prints
// its lines, without said's marks. A line, or the whole section, that the
// self-check finds something in is left out, without a word of it shown,
// a note standing in its place, and fails the run.
func (rp *liveReport) write(s *reportSection) error {
	title := rp.clean(s.title)
	if rp.scrub.holds(title) {
		title = "a section"
		rp.problems = append(rp.problems, "the title of a section held something that the run saw, so it was left out")
	}
	title = unmark(title)
	var b strings.Builder
	b.WriteString("## " + title + "\n")
	shown := []string{"## " + title}
	w8 := 0
	for i, l := range s.lines {
		text := rp.clean(l.text)
		switch {
		case rp.scrub.holds(text):
			rp.problems = append(rp.problems, fmt.Sprintf("line %d of %s held something that the run saw, so it was left out", i+1, title))
			text = leftOutLine
		case l.w8:
			text = w8Mark + text
			w8++
		}
		b.WriteString(text + "\n")
		shown = append(shown, unmark(text))
	}
	b.WriteString("\n")
	section := b.String()
	if rp.scrub.holds(section) {
		section = "## " + title + "\n(left out: it held something that the run saw)\n\n"
		shown = []string{"## " + title, "(left out)"}
		rp.problems = append(rp.problems, title+" held something that the run saw, so it was left out")
		w8 = 0
	}
	rp.written = append(rp.written, section)
	rp.w8 += w8
	f, err := os.OpenFile(rp.file, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(unmark(section))
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	for _, line := range shown {
		rp.show(line)
	}
	return nil
}

// recheck scrubs and checks each section written so far again, with all
// that the run saw by now: one that holds something the scrubber learned
// only after it was written is left out, a note standing in its place, and
// its lines marked W-8 are no longer counted.
func (rp *liveReport) recheck() {
	for i, section := range rp.written {
		if rp.scrub.holds(rp.clean(section)) {
			rp.written[i] = "## (a section left out: it held something that the run saw)\n\n"
			rp.w8 -= strings.Count(section, "\n"+w8Mark)
			rp.problems = append(rp.problems, "a section held something that the run saw only later, so it was left out")
		}
	}
}

// finish writes the report again, whole, over the file, each section
// scrubbed and checked again, as recheck does.
func (rp *liveReport) finish() error {
	rp.recheck()
	var b strings.Builder
	b.WriteString(reportHeading)
	for _, section := range rp.written {
		b.WriteString(unmark(rp.clean(section)))
	}
	return replacePrivate(rp.file, []byte(b.String()))
}

// stateVersion is the format of state.json.
const stateVersion = 1

// stateKeySize is how many random bytes the state's key holds.
const stateKeySize = 32

// A stateStore is a live run's state.json, what the probes compare across
// runs, with its key, state.key, beside it. Every identifier it holds is an
// HMAC-SHA256 digest under the key, 32 random bytes made on the first run,
// which keeps the digests comparable across logins and apps: an unkeyed
// digest of an 8-digit account number falls to trying every number in
// about two seconds. The two files together still give the numbers back as
// fast, so they are as private as the token file, never to be shared.
type stateStore struct {
	file   string
	key    []byte
	probes map[string]json.RawMessage // each probe's state, by its name
}

// stateJSON is state.json as it is written.
type stateJSON struct {
	Version int                        `json:"version"`
	Probes  map[string]json.RawMessage `json:"probes"`
}

// openState reads file, the state, and keyFile, its key, making the key on
// the first run. Either file made readable by others is made private
// again, with a warning. A state without its key cannot be compared, and is
// refused rather than started again, as is a key or a state that cannot be
// read.
func openState(file, keyFile string, warn func(string)) (*stateStore, error) {
	for _, dir := range []string{filepath.Dir(file), filepath.Dir(keyFile)} {
		if err := makePrivateDir(dir); err != nil {
			return nil, err
		}
	}
	key, err := readStateKey(file, keyFile)
	if err != nil {
		return nil, err
	}
	s := &stateStore{file: file, key: key, probes: map[string]json.RawMessage{}}
	b, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, makePrivate(keyFile, warn)
	case err != nil:
		return nil, err
	}
	var st stateJSON
	if err := json.Unmarshal(b, &st); err != nil || st.Version < 1 {
		return nil, fmt.Errorf("%s is not a live state; move it away to start afresh", file)
	}
	if st.Version > stateVersion {
		return nil, fmt.Errorf("%s is from a newer version of the live mode (format %d, where this one reads %d)", file, st.Version, stateVersion)
	}
	if st.Probes != nil {
		s.probes = st.Probes
	}
	return s, errors.Join(makePrivate(file, warn), makePrivate(keyFile, warn))
}

// readStateKey returns the key in keyFile, making it, readable only by the
// user, if neither it nor file, the state, is there yet.
func readStateKey(file, keyFile string) ([]byte, error) {
	b, err := os.ReadFile(keyFile)
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(file); err == nil {
			return nil, fmt.Errorf("%s is there without its key, %s, so what it holds cannot be compared; move it away to start afresh", file, keyFile)
		}
		key := make([]byte, stateKeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		switch err := createPrivate(keyFile, []byte(hex.EncodeToString(key)+"\n")); {
		case err == nil:
			return key, nil
		case !errors.Is(err, fs.ErrExist):
			return nil, err
		}
		b, err = os.ReadFile(keyFile) // made by another run meanwhile
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != stateKeySize {
		return nil, fmt.Errorf("%s is not a key of the live state; move it and the state away to start afresh", keyFile)
	}
	return key, nil
}

// digest returns the keyed digest of value for purpose, such as "account
// number": the first 16 bytes of the HMAC-SHA256, under the state's key, of
// the purpose and the value, in hex. The purpose keeps one value's digests
// for two uses apart.
func (s *stateStore) digest(purpose, value string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// get reads the state of probe into v, reporting whether there is one.
func (s *stateStore) get(probe string, v any) (bool, error) {
	raw, ok := s.probes[probe]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

// put sets the state of probe to v, for save to write.
func (s *stateStore) put(probe string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.probes[probe] = b
	return nil
}

// save writes the state over its file, readable only by the user.
func (s *stateStore) save() error {
	b, err := json.MarshalIndent(stateJSON{Version: stateVersion, Probes: s.probes}, "", "  ")
	if err != nil {
		return err
	}
	return replacePrivate(s.file, append(b, '\n'))
}

// testReport returns a report in a temporary folder whose lines s scrubs,
// and the lines it prints.
func testReport(t *testing.T, s *liveScrubber) (*liveReport, *[]string) {
	t.Helper()
	var shown []string
	rp, err := newLiveReport(filepath.Join(t.TempDir(), "report.txt"), s, func(line string) { shown = append(shown, line) })
	if err != nil {
		t.Fatal(err)
	}
	return rp, &shown
}

// readFile returns the text of path, failing the test if it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The report is written section by section, each scrubbed, and its lines
// printed as written; a line marked W-8 says so. At the end it is written
// again with what the scrubber learned since, and it is readable only by
// the user. A report that is there already is never written over.
func TestLiveReport(t *testing.T) {
	s := newLiveScrubber()
	s.addAccount(1, "29470135", "HASH1234")
	rp, shown := testReport(t, s)
	first := &reportSection{title: "accounts/{account 1}"}
	first.add("200, application/json, 1 record")
	first.addf("an error: HTTP %d from Schwab: Invalid account number 29470135", 400)
	first.addW8("a W-8 Withholding row, in row 73508261946")
	if err := rp.write(first); err != nil {
		t.Fatal(err)
	}
	want := reportHeading + "## accounts/{account 1}\n200, application/json, 1 record\n" +
		"an error: HTTP 400 from Schwab: Invalid account number {account 1}\n" +
		"[W-8] a W-8 Withholding row, in row 73508261946\n\n"
	if got := readFile(t, rp.file); got != want {
		t.Errorf("after one section:\n%s\nwant\n%s", got, want)
	}
	if want := []string{
		"## accounts/{account 1}", "200, application/json, 1 record",
		"an error: HTTP 400 from Schwab: Invalid account number {account 1}", "[W-8] a W-8 Withholding row, in row 73508261946",
	}; !slices.Equal(*shown, want) {
		t.Errorf("printed %q, want %q", *shown, want)
	}

	s.addIdent("activity id", "73508261946")
	if err := rp.write(&reportSection{title: "guard", lines: []reportLine{{text: "1 request"}}}); err != nil {
		t.Fatal(err)
	}
	if err := rp.finish(); err != nil {
		t.Fatal(err)
	}
	want = strings.Replace(want, "row 73508261946", "row {activity id 1}", 1) + "## guard\n1 request\n\n"
	if got := readFile(t, rp.file); got != want || rp.w8 != 1 || len(rp.problems) != 0 {
		t.Errorf("finished:\n%s\nwant\n%s\n(%d W-8 lines, problems %q)", got, want, rp.w8, rp.problems)
	}
	if _, err := newLiveReport(rp.file, s, func(string) {}); err == nil {
		t.Error("a second report over the first")
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(rp.file); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the report: %v, %v; want mode 0600", info.Mode(), err)
		}
	}
}

// A line, a title or a section that still holds something the run saw,
// because the scrubber missed it, is left out, without a word of it printed
// or written, a note standing in its place, and fails the run, as is a
// value split over two lines, which only the whole section shows; so is a
// section that holds something the scrubber learned only after it was
// written, whose lines marked W-8 are then no longer counted. A nickname in
// Schwab's words is something the run saw, while in the run's own words it
// is a word that spells one, which is printed as it is, and said's marks are
// never printed.
func TestLiveReportSelfCheck(t *testing.T) {
	s := newLiveScrubber()
	s.addAccount(1, "29470135", "HASH1234")
	s.addName("Zq Fund")
	rp, shown := testReport(t, s)
	rp.clean = func(text string) string { return text } // a scrubber that misses everything
	sec := &reportSection{title: "accounts/HASH1234"}
	sec.add("fine, as the Zq Fund words go")
	sec.add("account 29470135")
	sec.add("Schwab said: " + said("no Zq Fund here"))
	if err := rp.write(sec); err != nil {
		t.Fatal(err)
	}
	if err := rp.write(&reportSection{title: "ok", lines: []reportLine{{text: "line, " + said("as Schwab says")}}}); err != nil {
		t.Fatal(err)
	}
	s.addIdent("streamer correl id", "zq9 first\nzq9 second")
	if err := rp.write(&reportSection{title: "split", lines: []reportLine{{text: "zq9 first"}, {text: "zq9 second"}}}); err != nil {
		t.Fatal(err)
	}
	s.addIdent("order id", "1004517638295")
	rp.written = append(rp.written, "## late\n"+w8Mark+"order 1004517638295\n\n")
	rp.w8++
	if err := rp.finish(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, rp.file) + strings.Join(*shown, "\n")
	for _, v := range []string{"29470135", "HASH1234", "1004517638295", "zq9 first\nzq9 second", "no Zq Fund", "\x02", "\x03"} {
		if strings.Contains(got, v) {
			t.Errorf("the report or the lines printed hold %q:\n%s", v, got)
		}
	}
	if !strings.Contains(got, "## a section\nfine, as the Zq Fund words go\n"+leftOutLine+"\n"+leftOutLine+"\n\n") ||
		!strings.Contains(got, "## a section\nfine, as the Zq Fund words go\n"+leftOutLine+"\n"+leftOutLine+"\n## ok\n") ||
		!strings.Contains(got, "## ok\nline, as Schwab says\n") || !strings.Contains(got, "## split\n(left out: it held something that the run saw)\n") ||
		!strings.Contains(got, "## (a section left out: it held something that the run saw)\n") || len(rp.problems) != 5 || rp.w8 != 0 {
		t.Errorf("the report:\n%s\nproblems %q, %d W-8 lines", got, rp.problems, rp.w8)
	}
}

// The state's key is 32 random bytes, made on the first run, readable only
// by the user, and read on every later run, so that a digest stays the same
// from run to run; another key gives other digests, and so does another
// purpose. A digest is the HMAC-SHA256 of the purpose and the value, cut to
// 16 bytes, and the state's file holds no value it was given to digest. A
// state or a key that others can read is made private again.
func TestStateDigests(t *testing.T) {
	dir := t.TempDir()
	file, keyFile := filepath.Join(dir, "live", "state.json"), filepath.Join(dir, "live", "state.key")
	st, err := openState(file, keyFile, func(msg string) { t.Error(msg) })
	if err != nil {
		t.Fatal(err)
	}
	key := readFile(t, keyFile)
	if len(key) != 2*stateKeySize+1 || strings.Trim(key, "0123456789abcdef\n") != "" {
		t.Errorf("the key file holds %q", key)
	}
	d := st.digest("account number", "29470135")
	mac := hmac.New(sha256.New, st.key)
	mac.Write([]byte("account number\x0029470135"))
	if want := hex.EncodeToString(mac.Sum(nil)[:16]); d != want {
		t.Errorf("digest = %s, want %s", d, want)
	}
	if d == st.digest("hash", "29470135") || d == st.digest("account number", "29470136") {
		t.Error("digests of another purpose or value are the same")
	}
	if err := st.put("Q10", map[string]string{d: "same"}); err != nil {
		t.Fatal(err)
	}
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	again, err := openState(file, keyFile, func(msg string) { t.Error(msg) })
	if err != nil {
		t.Fatal(err)
	}
	var q10 map[string]string
	if ok, err := again.get("Q10", &q10); !ok || err != nil || q10[d] != "same" || again.digest("account number", "29470135") != d {
		t.Errorf("the state read again: %v, %v, %v", q10, ok, err)
	}
	if ok, err := again.get("Q12", &q10); ok || err != nil {
		t.Errorf("a probe without state: %v, %v", ok, err)
	}
	if readFile(t, keyFile) != key || strings.Contains(readFile(t, file), "29470135") {
		t.Errorf("the key changed, or the state holds the value:\n%s", readFile(t, file))
	}
	otherFile := filepath.Join(dir, "other", "state.json") // never saved
	other, err := openState(otherFile, filepath.Join(dir, "other", "state.key"), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if other.digest("account number", "29470135") == d {
		t.Error("another key gives the same digest")
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, path := range []string{file, keyFile, filepath.Dir(file)} {
		want := os.FileMode(0o600)
		if path == filepath.Dir(file) {
			want = 0o700
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; want mode %v", path, info.Mode(), err, want)
		}
	}
	// A file that others can read, as one copied in by hand, is made
	// private again on a later run, with a warning that names it: the state
	// readable by its group, its key by everyone, and a key by everyone
	// before there is a state.
	for _, tc := range []struct {
		state, path string
		mode        os.FileMode
	}{
		{file, file, 0o640},
		{file, keyFile, 0o644},
		{otherFile, filepath.Join(dir, "other", "state.key"), 0o644},
	} {
		if err := os.Chmod(tc.path, tc.mode); err != nil {
			t.Fatal(err)
		}
		var warned []string
		_, err := openState(tc.state, filepath.Join(filepath.Dir(tc.state), "state.key"), func(msg string) { warned = append(warned, msg) })
		if err != nil || len(warned) != 1 || !strings.HasPrefix(warned[0], tc.path+" ") {
			t.Errorf("%s at mode %v: %v, warned %q", tc.path, tc.mode, err, warned)
		}
		if info, err := os.Stat(tc.path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s made private: %v, %v", tc.path, info.Mode(), err)
		}
	}
}

// A state without its key, a key that is not one, and a state that cannot
// be read are refused, never started afresh over; nor is a key file that
// is there already ever written over.
func TestStateRefusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	goodKey := strings.Repeat("ab", stateKeySize) + "\n"
	for _, tc := range []struct {
		name, state, key, want string
	}{
		{"no key", `{"version":1,"probes":{}}`, "", "is there without its key"},
		{"a short key", "", "abcd\n", "is not a key of the live state"},
		{"a key that is not hex", "", strings.Repeat("zz", stateKeySize), "is not a key of the live state"},
		{"a state that is not JSON", "{", goodKey, "is not a live state"},
		{"a state of no version", "{}", goodKey, "is not a live state"},
		{"a newer state", `{"version":2}`, goodKey, "is from a newer version of the live mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, keyFile := filepath.Join(dir, tc.name, "state.json"), filepath.Join(dir, tc.name, "state.key")
			if tc.state != "" {
				write(filepath.Join(tc.name, "state.json"), tc.state)
			}
			if tc.key != "" {
				write(filepath.Join(tc.name, "state.key"), tc.key)
			}
			_, err := openState(file, keyFile, func(string) {})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("openState: %v, want %q", err, tc.want)
			}
			if tc.key != "" && readFile(t, keyFile) != tc.key {
				t.Error("the key file was written over")
			}
		})
	}
	keyFile := write(filepath.Join("kept", "state.key"), goodKey)
	st, err := openState(filepath.Join(dir, "kept", "state.json"), keyFile, func(string) {})
	if err != nil || hex.EncodeToString(st.key) != strings.TrimSpace(goodKey) || readFile(t, keyFile) != goodKey {
		t.Errorf("a key file that is there: %v", err)
	}
}
