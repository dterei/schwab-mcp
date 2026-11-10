package schwab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// A testAnswer is an answer for recordAnswers to keep: the request, and the
// answer's status, headers and body.
type testAnswer struct {
	path, query string
	status      int
	header      http.Header
	body        string
	cut         bool // the body breaks off, as when the connection breaks part way through it
}

// newGetRequest returns a GET request for u, as the client sends one.
func newGetRequest(t *testing.T, u string) (*http.Request, error) {
	t.Helper()
	return http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodGet, u, nil)
}

// answerOf returns the fixture file as the answer to the request that its
// index entry names, with the status and headers the entry gives.
func answerOf(t *testing.T, file string) testAnswer {
	t.Helper()
	for _, e := range schwabtest.Index() {
		if e.File == file {
			return testAnswer{
				path: e.Request.Path, query: e.Request.Query, status: e.Response.Status,
				header: http.Header(e.Response.Headers).Clone(), body: string(readTestdata(t, file)),
			}
		}
	}
	t.Fatalf("no index entry names %s", file)
	return testAnswer{}
}

// apiFixtures returns F: the indexed files made by hand whose request is a
// GET of an API path, as answers, in the index's order. The token
// endpoint's bodies are never recorded, so they are not among them, and a
// recording, once committed, holds the fakes that a planting is to stand
// apart from.
func apiFixtures(t *testing.T) []testAnswer {
	t.Helper()
	var f []testAnswer
	for _, e := range schwabtest.Index() {
		if e.Source != "recorded" && e.Request.Method == http.MethodGet && underAPI(e.Request.Path) {
			f = append(f, answerOf(t, e.File))
		}
	}
	return f
}

// A planter puts other values in place of the fixtures' identifiers, each
// fixture value always by the same planted value, so that a recording built
// from the fixtures holds identifiers that no fake is: other account numbers,
// hashes, ids with their ranks and numbers of digits kept, nicknames,
// masked numbers of the same form ending in their planted numbers' digits,
// and customer, correlation and errors' IDs. The values come from a seed,
// so that two planters with two seeds plant two recordings independently.
type planter struct {
	t       *testing.T
	rng     *rand.Rand
	planted map[identifier]map[string]string // each fixture value, by its kind, and what is planted in its place
	used    map[string]bool                  // the values planted so far, which no other may be
	text    string                           // every fixture's text, which no planted value may be found in
}

// newPlanter returns a planter of seed for answers, the fixtures to plant.
func newPlanter(t *testing.T, seed uint64, answers []testAnswer) *planter {
	t.Helper()
	p := &planter{t: t, rng: rand.New(rand.NewPCG(seed, seed^0x5eed)), planted: map[identifier]map[string]string{}, used: map[string]bool{}}
	var all strings.Builder
	for _, a := range answers {
		all.WriteString(a.body + "\n" + a.path + "?" + a.query + "\n")
		for name, values := range a.header {
			all.WriteString(name + ": " + strings.Join(values, ",") + "\n")
		}
	}
	p.text = all.String()
	return p
}

// draw returns a value of n characters from alphabet, the first from first,
// that no other planted value is and that the fixtures' text does not hold.
func (p *planter) draw(n int, first, alphabet string) string {
	for range 1000 {
		b := []byte{first[p.rng.IntN(len(first))]}
		for len(b) < n {
			b = append(b, alphabet[p.rng.IntN(len(alphabet))])
		}
		if v := string(b); !p.used[v] && !strings.Contains(p.text, v) && !holdsFakeAccount(v) {
			p.used[v] = true
			return v
		}
	}
	p.t.Fatal("no value left to plant")
	return ""
}

// The alphabets of planted values: no 0 or 1, so that a planted value can
// be told from a fake, whose counters start at 1 and 0.
const (
	plantDigits = "23456789"
	plantHex    = "23456789ABCDEF"
)

// value returns what is planted in place of orig, a fixture's value of kind
// id, the same each time. Ids are planted by plantIDs, which keeps their
// order.
func (p *planter) value(id identifier, orig string) string {
	if p.planted[id] == nil {
		p.planted[id] = map[string]string{}
	}
	if v, ok := p.planted[id][orig]; ok {
		return v
	}
	var v string
	switch id {
	case idAccountNumber:
		v = p.draw(8, plantDigits, plantDigits)
	case idHash:
		v = p.draw(64, plantHex, plantHex)
	case idCustomer:
		v = strings.ToLower(p.draw(64, plantHex, plantHex))
	case idCorrelation, idError, idTrace:
		u := strings.ToLower(p.draw(32, plantHex, plantHex))
		v = u[:8] + "-" + u[8:12] + "-" + u[12:16] + "-" + u[16:20] + "-" + u[20:]
	case idNickname:
		v = "Pl" + strings.ToLower(p.draw(6, "BCDFGHJKLMNPRSTVWZ", "AEIOUBCDFGHJKLMNPRSTVWZ"))
	default:
		p.t.Fatalf("no value to plant for %s", id)
	}
	p.planted[id][orig] = v
	return v
}

// plantIDs fills in the planted ids of kind id in place of reals, the
// fixtures' ids of that kind: as many digits each, in the same order.
func (p *planter) plantIDs(id identifier, reals []string) {
	reals = slices.Clone(reals)
	slices.SortFunc(reals, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	reals = slices.Compact(reals)
	byLen := map[int][]string{}
	for _, r := range reals {
		byLen[len(r)] = append(byLen[len(r)], r)
	}
	if p.planted[id] == nil {
		p.planted[id] = map[string]string{}
	}
	for n, group := range byLen {
		var fakes []string
		for range group {
			fakes = append(fakes, p.draw(n, plantDigits, plantDigits))
		}
		slices.Sort(fakes)
		for i, r := range group {
			p.planted[id][r] = fakes[i]
		}
	}
}

// values returns every value that p planted, of every kind, and of the
// masked numbers, the planted numbers' digits that they end in.
func (p *planter) values() []string {
	var vs []string
	for _, m := range p.planted {
		for _, v := range m {
			vs = append(vs, v)
		}
	}
	return vs
}

// plant returns answers, fixtures, with every identifier that the scrub
// policy maps replaced by its planted value: in each JSON body, each value
// at a path that a table maps, as a string for a string and a number for a
// number; in each path, the account and the id of a transaction; and each
// correlation header's value.
func (p *planter) plant(answers []testAnswer) []testAnswer {
	p.t.Helper()
	pages := errorPages(p.t)
	ids := map[identifier][]string{}
	type found struct {
		a    int
		sc   jsonScalar
		r    rule
		orig string
	}
	var mapped []found
	for i, a := range answers {
		var e schwabtest.Entry
		e.Request.Method, e.Request.Path, e.Response.Status = http.MethodGet, a.path, a.status
		kind, ok := policyKindOf(pages, e, []byte(a.body))
		if !ok || schwabtest.ExtensionFor([]byte(a.body)) != ".json" {
			continue
		}
		body := []byte(a.body)
		walkJSON(body, func(sc jsonScalar) {
			if r, ok := policyTables[kind].rule(sc.path); ok && r.class == classMap {
				orig := idOf(body[sc.start:sc.end])
				mapped = append(mapped, found{i, sc, r, orig})
				ids[r.id] = append(ids[r.id], orig)
			}
		}, nil)
		if m := byIDPath.FindStringSubmatch(a.path); m != nil && m[1] == "transactions" {
			ids[idActivity] = append(ids[idActivity], m[2])
		}
	}
	for _, id := range []identifier{idActivity, idOrder, idPosition} {
		p.plantIDs(id, ids[id])
	}
	out := slices.Clone(answers)
	splices := make([][]splice, len(answers))
	for _, f := range mapped {
		body := []byte(answers[f.a].body)
		raw := body[f.sc.start:f.sc.end]
		var v string
		switch f.r.id {
		case idDisplay:
			number, ok := besideAccount(body, f.sc.in)
			if !ok {
				p.t.Fatalf("a masked number with no account number beside it")
			}
			planted := p.value(idAccountNumber, number)
			tail := trailingDigits.FindString(f.orig)
			v = f.orig[:len(f.orig)-len(tail)] + planted[len(planted)-len(tail):]
		default:
			v = p.value(f.r.id, f.orig)
		}
		with := []byte(v)
		if raw[0] == '"' {
			with = jsonText(v)
		}
		splices[f.a] = append(splices[f.a], splice{start: f.sc.start, end: f.sc.end, with: with})
	}
	for i, a := range answers {
		o := &out[i]
		o.body = string(applySplices([]byte(a.body), splices[i]))
		if rest, ok := strings.CutPrefix(a.path, "/trader/v1/accounts/"); ok {
			parts := strings.Split(rest, "/")
			switch {
			case parts[0] == "accountNumbers":
			case len(parts[0]) == 64:
				parts[0] = p.value(idHash, parts[0])
			default:
				parts[0] = p.value(idAccountNumber, parts[0])
			}
			if len(parts) == 3 && parts[1] == "transactions" {
				parts[2] = p.planted[idActivity][parts[2]]
			}
			o.path = "/trader/v1/accounts/" + strings.Join(parts, "/")
		}
		o.header = a.header.Clone()
		for name, values := range o.header {
			if isCorrelationHeader(name) {
				for j, v := range values {
					o.header[name][j] = p.value(idTrace, v)
				}
			}
		}
	}
	return out
}

// plantF returns F planted with seed, and the planter, which knows each
// value it planted.
func plantF(t *testing.T, seed uint64) ([]testAnswer, *planter) {
	t.Helper()
	f := apiFixtures(t)
	p := newPlanter(t, seed, f)
	return p.plant(f), p
}

// scrubAnswers keeps answers as a live run's recording does, reads that
// recording back as the scrubber does, and scrubs it with cfg, its home
// folder a fake one if cfg has none.
func scrubAnswers(t *testing.T, answers []testAnswer, cfg scrubConfig) (*scrubOutput, error) {
	t.Helper()
	recording, err := readRecording(recordAnswers(t, answers))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.home == "" {
		cfg.home = "/home/zq9user"
	}
	if cfg.version == "" {
		cfg.version = "c0ffee"
	}
	if cfg.amounts.mode == "" {
		cfg.amounts = amountsPolicy{mode: "refuse"}
	}
	return scrubRecording(t, recording, cfg)
}

// leaksOf returns each of values that out holds anywhere, its files and
// their entries, entries.json, review.txt and the shapes, as written and
// decoded, as gate (v) searches; values of fewer than 6 characters as whole
// tokens.
func leaksOf(out *scrubOutput, values []string) []string {
	set := newIdentSet()
	for _, v := range values {
		set.add(idPathAccount, v)
	}
	var texts []leakText
	for _, f := range out.files {
		texts = append(texts, textsOf(f.body)...)
		texts = append(texts, entryTexts(f.entry)...)
	}
	texts = append(texts, textsOf(out.entriesJSON())...)
	for _, s := range append([]string{out.review}, mapValues(out.shapes)...) {
		for _, form := range unescapedForms(s) {
			texts = append(texts, leakText{where: "text", text: form})
		}
	}
	var found []string
	for _, t := range texts {
		for _, m := range set.find(t.text, t.number) {
			if !slices.Contains(found, m.v.orig) {
				found = append(found, m.v.orig)
			}
		}
	}
	return found
}

// mapValues returns the values of m, in no order.
func mapValues(m map[string]string) []string {
	var vs []string
	for _, v := range m {
		vs = append(vs, v)
	}
	return vs
}

// fileNamed returns the output's file made from the answer called name.
func fileNamed(t *testing.T, o *scrubOutput, name string) *scrubbedFile {
	t.Helper()
	for _, f := range o.files {
		if f.from.raw.name == name {
			return f
		}
	}
	t.Fatalf("answer %s was not written: %s", name, o.review)
	return nil
}

// Two recordings planted from F, each with values of its own, scrub to the
// same bytes under keep, the files and their entries alike, since a fake
// follows a value's place and never the value; and what they scrub to
// differs from F only at the values that the policy maps, F holding the
// fixtures' own fakes. Under refuse too, the two scrub alike. The output
// holds no planted value, and with the hand-made fixtures it passes the
// value hygiene tests and checkAnswer.
func TestScrubberTwoPlantings(t *testing.T) {
	f := apiFixtures(t)
	one, p1 := plantF(t, 1)
	two, p2 := plantF(t, 2)
	if slices.Equal(p1.values(), p2.values()) || len(p1.values()) < 40 {
		t.Fatalf("the plantings are not independent, or plant too little: %d values", len(p1.values()))
	}
	for _, policy := range []amountsPolicy{{mode: "keep"}, {mode: "refuse"}} {
		out1, err := scrubAnswers(t, one, scrubConfig{amounts: policy})
		if err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
		out2, err := scrubAnswers(t, two, scrubConfig{amounts: policy})
		if err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
		if !bytes.Equal(out1.entriesJSON(), out2.entriesJSON()) || len(out1.files) != len(out2.files) {
			t.Errorf("%s: the entries differ:\n%s\n%s", policy, out1.entriesJSON(), out2.entriesJSON())
		}
		for i := range min(len(out1.files), len(out2.files)) {
			if !bytes.Equal(out1.files[i].body, out2.files[i].body) {
				t.Errorf("%s: %s differs between the plantings", policy, out1.files[i].entry.File)
			}
		}
		if out1.review != out2.review {
			t.Errorf("%s: review.txt differs between the plantings:\n%s\n%s", policy, out1.review, out2.review)
		}
		for _, leak := range slices.Concat(leaksOf(out1, p1.values()), leaksOf(out2, p2.values())) {
			t.Errorf("%s: the output holds a planted value of %d characters", policy, len(leak))
		}
		if policy.mode != "keep" {
			continue
		}
		if len(out1.files) != len(f) {
			t.Fatalf("keep wrote %d files of F's %d:\n%s", len(out1.files), len(f), out1.review)
		}
		for i, file := range out1.files {
			differsOnlyWhereMapped(t, f[i], file)
		}
		files := indexedFiles(t)
		for _, file := range out1.files {
			files = append(files, indexedFile{entry: file.entry, body: file.body})
			if k, ok := kindByName(file.from.kind); ok {
				for _, finding := range checkAnswer(k.name, file.body) {
					if finding.fails && !strings.Contains(finding.String(), "repeated key, reported only") {
						t.Errorf("%s: checkAnswer finds %s", file.entry.File, finding)
					}
				}
			}
		}
		for _, problem := range checkValues(t, files) {
			t.Errorf("with the hand-made fixtures: %s", problem)
		}
		problems, _ := standAloneProblems(t, files)
		for _, problem := range problems {
			t.Errorf("with the hand-made fixtures: %s", problem)
		}
		pages := errorPages(t)
		for _, file := range out1.files {
			if kind, ok := policyKindOf(pages, file.entry, file.body); !ok || kind != file.from.kind {
				t.Errorf("%s is of kind %q, %v, once committed; want %q", file.entry.File, kind, ok, file.from.kind)
			}
		}
	}
}

// differsOnlyWhereMapped checks that file, what the scrubber wrote of the
// planted answer from want, an answer of F, differs from want only at the
// values that the policy maps: in its body, its path and its correlation
// headers.
func differsOnlyWhereMapped(t *testing.T, want testAnswer, file *scrubbedFile) {
	t.Helper()
	name := file.entry.File
	before, after := []byte(want.body), file.body
	if file.from.ext == ".json" {
		bs, as := scalarsOf(before), scalarsOf(after)
		if len(bs) != len(as) || !bytes.Equal(skeleton(before, bs), skeleton(after, as)) {
			t.Errorf("%s: its keys, punctuation or white space differ from F's", name)
			return
		}
		for i, sc := range as {
			if bytes.Equal(before[bs[i].start:bs[i].end], after[sc.start:sc.end]) {
				continue
			}
			if r, ok := file.from.table.rule(sc.path); !ok || r.class != classMap {
				t.Errorf("%s: %s differs from F's, and the policy does not map it", name, pathName(sc.path))
			}
		}
	} else if !bytes.Equal(before, after) {
		t.Errorf("%s differs from F's", name)
	}
	wantParts, gotParts := strings.Split(want.path, "/"), strings.Split(file.entry.Request.Path, "/")
	if len(wantParts) != len(gotParts) || file.entry.Request.Query != want.query {
		t.Errorf("%s: the request %s?%s, want %s?%s", name, file.entry.Request.Path, file.entry.Request.Query, want.path, want.query)
	}
	for i := range min(len(wantParts), len(gotParts)) {
		byID := i == 6 && wantParts[5] == "transactions"
		if wantParts[i] != gotParts[i] && !byID {
			t.Errorf("%s: part %d of the path differs from F's", name, i)
		}
	}
	for header, values := range want.header {
		got := file.entry.Response.Headers[header]
		if isCorrelationHeader(header) {
			for _, v := range got {
				if !counterUUID.MatchString(v) {
					t.Errorf("%s: %s is not a UUID of the counter", name, header)
				}
			}
		} else if !slices.Equal(got, values) {
			t.Errorf("%s: %s is %q, want F's %q", name, header, got, values)
		}
	}
}

// recordAnswers keeps answers as -live.record keeps a live run's, through
// the recorder itself, in a raw folder of its own, which it returns. Each
// answer has its status and headers, Content-Type and Content-Length among
// them, its body as given, and a time sent of testNow.
func recordAnswers(t *testing.T, answers []testAnswer) string {
	t.Helper()
	rec, err := newRecorder(t.TempDir(), "c0ffee")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range answers {
		u := DefaultBaseURL + a.path
		if a.query != "" {
			u += "?" + a.query
		}
		req, err := newGetRequest(t, u)
		if err != nil {
			t.Fatal(err)
		}
		resp := stubAnswer(req, a.status, a.body)
		resp.Header = a.header.Clone()
		if resp.Header == nil {
			resp.Header = map[string][]string{}
		}
		resp.Header.Set("Content-Length", strconv.Itoa(len(a.body)))
		if a.cut {
			resp.Body = &cutBody{r: strings.NewReader(a.body)}
		}
		rec.record(req, testNow, resp)
		_ = resp.Body.Close()
	}
	if err := rec.failure(); err != nil || rec.n != len(answers) {
		t.Fatalf("recorded %d answers of %d: %v", rec.n, len(answers), err)
	}
	return rec.dir
}

// The scrubber's front door refuses, before it writes anything, a run in
// CI, a run without an out folder or with one that is there already or is
// inside the module or a git work tree, a policy for amounts it does not
// know, and a folder that holds no recording or a broken one; and it writes
// a recording that it takes to a folder of its own, readable only by the
// user: the candidate fixtures alone under recorded/<date>/, and
// entries.json, review.txt and shapes/ beside them.
func TestScrubFolders(t *testing.T) {
	module := plantModule(t)
	home := filepath.Join(t.TempDir(), "zq9user")
	dir := t.TempDir()
	planted, _ := plantF(t, 1)
	limited := planted[9] // a 429
	limited.query = "symbol=SPY&periodType=year"
	raw := recordAnswers(t, []testAnswer{planted[0], planted[3], limited}) // account numbers, transactions and a 429
	run := func(raw, out, amounts string, env ...string) (string, error) {
		return scrubFolders(t, scrubRun{
			raw: raw, out: out, amounts: amounts, module: module, home: home, version: func() string { return "c0ffee" },
			getenv: func(name string) string {
				for i := 0; i+1 < len(env); i += 2 {
					if env[i] == name {
						return env[i+1]
					}
				}
				return ""
			},
		})
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "001.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownKey := t.TempDir()
	for name, b := range map[string]string{"001.json": `{"request":{"method":"GET","path":"/x"},"response":{"status":200},"extra":1}`, "001.body": "[]"} {
		if err := os.WriteFile(filepath.Join(unknownKey, name), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, raw, out, amounts string
		env                     []string
		want                    string
	}{
		{"in CI", raw, filepath.Join(dir, "a"), "refuse", []string{"CI", "true"}, "never runs in CI"},
		{"in GitHub's", raw, filepath.Join(dir, "a"), "refuse", []string{"GITHUB_ACTIONS", "true"}, "never runs in CI"},
		{"no out folder", raw, "", "refuse", nil, "-scrub.out is required"},
		{"amounts unknown", raw, filepath.Join(dir, "a"), "scale:1", nil, "-scrub.amounts takes"},
		{"out inside the module", raw, filepath.Join(module, "out"), "refuse", nil, "inside the module"},
		{"out inside a work tree", raw, filepath.Join(repo, "out"), "refuse", nil, "inside a git work tree"},
		{"out there already", raw, dir, "refuse", nil, "exists; -scrub.out names a folder that the scrubber makes"},
		{"no recording", t.TempDir(), filepath.Join(dir, "a"), "refuse", nil, "holds no answer of a recording"},
		{"half an answer", broken, filepath.Join(dir, "a"), "refuse", nil, "not both"},
		{"a key the scrubber does not know", unknownKey, filepath.Join(dir, "a"), "refuse", nil, "cannot be read whole"},
	} {
		if _, err := run(tc.raw, tc.out, tc.amounts, tc.env...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error that says %q", tc.name, err, tc.want)
		}
		if _, err := os.Lstat(filepath.Join(dir, "a")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: the out folder was made: %v", tc.name, err)
		}
	}

	out := filepath.Join(dir, "deep", "out")
	summary, err := run(raw, out, "refuse")
	if err != nil {
		t.Fatal(err)
	}
	if want := "wrote 2 candidate fixtures"; !strings.Contains(summary, want) || !strings.Contains(summary, "1 answer left out") {
		t.Errorf("summary %q", summary)
	}
	var files []string
	err = filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, p)
		files = append(files, filepath.ToSlash(rel))
		if want := os.FileMode(0o600); d.IsDir() {
			want = 0o700
			if info, err := d.Info(); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != want) {
				t.Errorf("%s: mode %v, %v; want %v", rel, info.Mode(), err, want)
			}
		} else if info, err := d.Info(); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != want) {
			t.Errorf("%s: mode %v, %v; want %v", rel, info.Mode(), err, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	day := testNow.UTC().Format("2006-01-02")
	if want := []string{
		".", "entries.json", "recorded", "recorded/" + day, "recorded/" + day + "/001.json", "recorded/" + day + "/003.txt",
		"review.txt", "shapes", "shapes/002.txt",
	}; !slices.Equal(files, want) {
		t.Errorf("the out folder holds %q, want %q", files, want)
	}
	b, err := os.ReadFile(filepath.Join(out, "entries.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var entries []schwabtest.Entry
	if err := dec.Decode(&entries); err != nil || len(entries) != 2 {
		t.Fatalf("entries.json: %d entries, %v", len(entries), err)
	}
	if e := entries[0]; e.File != "recorded/"+day+"/001.json" || e.Source != "recorded" || e.Recorded != day ||
		e.Scrub != "c0ffee; amounts refuse" || e.Request.Path != "/trader/v1/accounts/accountNumbers" ||
		!slices.Contains(e.Response.OtherHeaders, "Content-Length") || e.Response.Headers["Content-Type"][0] != "application/json" {
		t.Errorf("the first entry: %+v", e)
	}
	if !bytes.HasSuffix(b, []byte("\n")) || bytes.Contains(b, []byte(`\u0026`)) || !bytes.Contains(b, []byte(`"query": "symbol=SPY&periodType=year"`)) {
		t.Errorf("entries.json is not in index.json's form: %s", b)
	}
	if shape, err := os.ReadFile(filepath.Join(out, "shapes", "002.txt")); err != nil ||
		!strings.Contains(string(shape), "14 records") || !strings.Contains(string(shape), "\nnetAmount: a number with ") {
		t.Errorf("shapes/002.txt: %v\n%s", err, shape)
	}
	if again, err := run(raw, out, "refuse"); err == nil || !strings.Contains(err.Error(), "-scrub.out names a folder that the scrubber makes") {
		t.Errorf("a second run into the same folder: %q, %v", again, err)
	}
}
