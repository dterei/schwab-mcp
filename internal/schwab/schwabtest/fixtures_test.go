package schwabtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// counterID is a UUID of the form the fake's counter gives.
var counterID = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9]{12}$`)

// isCorrelationHeader reports whether name is one of Schwab's correlation
// headers, in any spelling.
func isCorrelationHeader(name string) bool {
	return strings.EqualFold(name, "Schwab-Client-CorrelId") || strings.EqualFold(name, "X-Request-Id")
}

// readAll returns every file of fsys by path, and complains of every name
// that starts with "." or "_", folders' included.
func readAll(fsys fs.FS, where string, complain func(string, ...any)) map[string][]byte {
	got := map[string][]byte{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
			complain("%s: %s starts with %q, which go:embed leaves out without all:", where, p, d.Name()[:1])
		}
		if !d.IsDir() {
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			got[p] = b
		}
		return nil
	})
	if err != nil {
		complain("%s: %v", where, err)
	}
	return got
}

// holds says what a file holds, by the extension ExtensionFor gives it.
var holds = map[string]string{".json": "JSON", ".html": "an HTML page", ".txt": "neither JSON nor a page"}

// checkTestdata returns what is wrong with a testdata folder, given as it
// is on disk, as it is embedded, and its index: the rules that
// testdata/README.md lists.
func checkTestdata(disk, embedded fs.FS, index []Entry) []string {
	var problems []string
	complain := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	onDisk := readAll(disk, "on disk", complain)
	inBinary := readAll(embedded, "embedded", complain)
	for _, p := range slices.Sorted(maps.Keys(onDisk)) {
		if b, ok := inBinary[p]; !ok {
			complain("%s is on disk but not embedded", p)
		} else if !bytes.Equal(b, onDisk[p]) {
			complain("%s differs between disk and the embedded files", p)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(inBinary)) {
		if _, ok := onDisk[p]; !ok {
			complain("%s is embedded but not on disk", p)
		}
	}

	entries := map[string]int{}
	for i, e := range index {
		where := fmt.Sprintf("index entry %d (%s)", i, e.File)
		entries[e.File]++
		b, ok := inBinary[e.File]
		switch {
		case e.File == "":
			complain("%s has no file", where)
		case !ok:
			complain("%s names a file that is not there", where)
		case bytes.Count(b, []byte("\r")) != e.CR:
			complain("%s says cr %d, but the file holds %d", where, e.CR, bytes.Count(b, []byte("\r")))
		}
		if ext := ExtensionFor(b); ok && path.Ext(e.File) != ext {
			complain("%s holds %s, but its name does not end in %s", where, holds[ext], ext)
		}
		if e.Source == "" {
			complain("%s has no source", where)
		}
		if e.Request.Method == "" || !strings.HasPrefix(e.Request.Path, "/") {
			complain("%s has no request: %q %q", where, e.Request.Method, e.Request.Path)
		}
		if s := e.Response.Status; s < 100 || s > 599 {
			complain("%s has status %d", where, s)
		}
		if e.Source == "recorded" {
			if _, err := time.Parse(time.DateOnly, e.Recorded); err != nil {
				complain("%s is a recording without a date: %q", where, e.Recorded)
			} else if !strings.HasPrefix(e.File, "recorded/"+e.Recorded+"/") {
				complain("%s is a recording of %s outside recorded/%s/", where, e.Recorded, e.Recorded)
			}
			if e.Scrub == "" {
				complain("%s is a recording that does not say how it was scrubbed", where)
			}
		} else {
			if strings.HasPrefix(e.File, "recorded/") {
				complain("%s is under recorded/, but its source is %q", where, e.Source)
			}
			if e.Recorded != "" || e.Scrub != "" {
				complain("%s has a date or a scrub policy, but its source is %q", where, e.Source)
			}
		}
		for name, values := range e.Response.Headers {
			switch {
			case strings.EqualFold(name, "Content-Length"):
				complain("%s keeps Content-Length's value, which goes stale once a value is scrubbed", where)
			case !KeptHeader(name):
				complain("%s keeps the value of %s, which KeptHeader does not keep", where, name)
			}
			if len(values) == 0 {
				complain("%s has %s without a value", where, name)
			}
			if !isCorrelationHeader(name) {
				continue
			}
			// An empty value, as Schwab may send one, holds nothing.
			for _, v := range values {
				canned := v == "corr-"+strconv.Itoa(e.Response.Status) || v == "req-429" && e.Response.Status == 429
				if v != "" && !counterID.MatchString(v) && !canned {
					complain("%s has %s %q, which is no fake's", where, name, v)
				}
			}
		}
		seen := map[string]bool{}
		for _, name := range e.Response.OtherHeaders {
			if KeptHeader(name) && !strings.EqualFold(name, "Content-Length") {
				complain("%s keeps only the name of %s, whose value KeptHeader keeps", where, name)
			}
			for kept := range e.Response.Headers {
				if strings.EqualFold(name, kept) {
					complain("%s has %s in both headers and otherHeaders", where, name)
				}
			}
			if seen[strings.ToLower(name)] {
				complain("%s names %s twice in otherHeaders", where, name)
			}
			seen[strings.ToLower(name)] = true
		}
	}
	for _, p := range slices.Sorted(maps.Keys(inBinary)) {
		if p == "README.md" || p == "index.json" {
			continue
		}
		if n := entries[p]; n != 1 {
			complain("%s has %d index entries, want 1", p, n)
		}
	}
	return problems
}

// embeddedTestdata returns the embedded testdata folder.
func embeddedTestdata(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(files, "testdata")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// The files under testdata keep the rules that testdata/README.md lists.
// The test runs in the package's folder, so it sees the files on disk as
// git has them, and can tell a file that go:embed left out.
func TestTestdataHygiene(t *testing.T) {
	for _, p := range checkTestdata(os.DirFS("testdata"), embeddedTestdata(t), Index()) {
		t.Error(p)
	}
}

// Each rule of checkTestdata refuses the case it is for.
func TestTestdataHygieneCatches(t *testing.T) {
	good := func() (fstest.MapFS, []Entry) {
		fsys := fstest.MapFS{
			"README.md":                    {Data: []byte("# Fixtures\n")},
			"index.json":                   {Data: []byte("[]\n")},
			"a.json":                       {Data: []byte(`{"a":1}`)},
			"errors/b.html":                {Data: []byte("<html>\r\n</html>\r\n")},
			"recorded/2026-11-08/001.json": {Data: []byte(`[]`)},
		}
		var a, b, r Entry
		a.File, a.Source = "a.json", "hand-made, modeled on nothing"
		a.Request.Method, a.Request.Path = "GET", "/trader/v1/test"
		a.Response.Status = 200
		a.Response.Headers = map[string][]string{
			"Content-Type":           {"application/json"},
			"Schwab-Client-Correlid": {"00000000-0000-4000-8000-000000000007"},
			"X-Request-Id":           {"00000000-0000-4000-8000-000000000008"},
		}
		a.Response.OtherHeaders = []string{"Content-Length", "ETag"}
		b.File, b.Source, b.CR = "errors/b.html", "rebuilt from a page", 2
		b.Request.Method, b.Request.Path = "GET", "/trader/v1/accounts"
		b.Response.Status = 429
		b.Response.Headers = map[string][]string{"Schwab-Client-CorrelID": {"corr-429"}, "X-Request-Id": {"req-429"}}
		r.File, r.Source, r.Recorded, r.Scrub = "recorded/2026-11-08/001.json", "recorded", "2026-11-08", "abc123 keep"
		r.Request.Method, r.Request.Path = "GET", "/trader/v1/accounts"
		r.Response.Status = 200
		// A correlation ID that Schwab sent empty holds nothing.
		r.Response.Headers = map[string][]string{"Schwab-Client-Correlid": {""}}
		return fsys, []Entry{a, b, r}
	}
	if fsys, index := good(); len(checkTestdata(fsys, fsys, index)) > 0 {
		t.Fatalf("the good folder: %q", checkTestdata(fsys, fsys, index))
	}
	for _, tc := range []struct {
		name   string
		change func(disk, embedded fstest.MapFS, index []Entry) []Entry
		want   string
	}{
		{"a file go:embed left out", func(disk, _ fstest.MapFS, index []Entry) []Entry {
			disk["c.json"] = &fstest.MapFile{Data: []byte("{}")}
			return index
		}, "c.json is on disk but not embedded"},
		{"a file embedded but not on disk", func(_, embedded fstest.MapFS, index []Entry) []Entry {
			embedded["c.json"] = &fstest.MapFile{Data: []byte("{}")}
			return index
		}, "c.json is embedded but not on disk"},
		{"a file whose bytes differ", func(_, embedded fstest.MapFS, index []Entry) []Entry {
			embedded["a.json"] = &fstest.MapFile{Data: []byte(`{"a":2}`)}
			return index
		}, "a.json differs"},
		{"a dot file", func(disk, embedded fstest.MapFS, index []Entry) []Entry {
			disk[".DS_Store"] = &fstest.MapFile{Data: []byte("x")}
			embedded[".DS_Store"] = disk[".DS_Store"]
			return index
		}, `.DS_Store starts with "."`},
		{"a raw copy", func(disk, _ fstest.MapFS, index []Entry) []Entry {
			disk["errors/_raw.json"] = &fstest.MapFile{Data: []byte("x")}
			return index
		}, `errors/_raw.json starts with "_"`},
		{"a folder that go:embed leaves out", func(disk, _ fstest.MapFS, index []Entry) []Entry {
			disk["_old/a.json"] = &fstest.MapFile{Data: []byte("x")}
			return index
		}, `_old starts with "_"`},
		{"a file without an entry", func(_, _ fstest.MapFS, index []Entry) []Entry {
			return index[1:]
		}, "a.json has 0 index entries"},
		{"a file with two entries", func(_, _ fstest.MapFS, index []Entry) []Entry {
			return append(index, index[0])
		}, "a.json has 2 index entries"},
		{"an entry without a file", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].File = "gone.json"
			return index
		}, "names a file that is not there"},
		{"an entry without a source", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Source = ""
			return index
		}, "has no source"},
		{"an entry without a request", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Request.Method = ""
			return index
		}, "has no request"},
		{"a request path that does not start with /", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Request.Path = "trader/v1/test"
			return index
		}, `has no request: "GET" "trader/v1/test"`},
		{"an entry without a status", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.Status = 0
			return index
		}, "has status 0"},
		{"a status past 599", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.Status = 600
			return index
		}, "has status 600"},
		{"JSON named otherwise", func(disk, embedded fstest.MapFS, index []Entry) []Entry {
			for _, fsys := range []fstest.MapFS{disk, embedded} {
				fsys["a.txt"] = fsys["a.json"]
				delete(fsys, "a.json")
			}
			index[0].File = "a.txt"
			return index
		}, "(a.txt) holds JSON, but its name does not end in .json"},
		{"a page named otherwise", func(disk, embedded fstest.MapFS, index []Entry) []Entry {
			for _, fsys := range []fstest.MapFS{disk, embedded} {
				fsys["errors/b.txt"] = fsys["errors/b.html"]
				delete(fsys, "errors/b.html")
			}
			index[1].File = "errors/b.txt"
			return index
		}, "(errors/b.txt) holds an HTML page, but its name does not end in .html"},
		{"a 429's white space named by its Content-Type", func(disk, embedded fstest.MapFS, index []Entry) []Entry {
			disk["recorded/2026-11-08/002.json"] = &fstest.MapFile{Data: []byte(" \n")}
			embedded["recorded/2026-11-08/002.json"] = disk["recorded/2026-11-08/002.json"]
			e := index[2]
			e.File, e.Response.Status = "recorded/2026-11-08/002.json", 429
			e.Response.Headers = map[string][]string{"Content-Type": {"application/json"}}
			return append(index, e)
		}, "(recorded/2026-11-08/002.json) holds neither JSON nor a page, but its name does not end in .txt"},
		{"a count of carriage returns that is wrong", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[1].CR = 0
			return index
		}, "says cr 0, but the file holds 2"},
		{"a recording without a date", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[2].Recorded = ""
			return index
		}, "a recording without a date"},
		{"a recording in another day's folder", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[2].Recorded = "2026-11-09"
			return index
		}, "outside recorded/2026-11-09/"},
		{"a recording without its scrub policy", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[2].Scrub = ""
			return index
		}, "does not say how it was scrubbed"},
		{"a hand-made file among the recordings", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[2].Source = "hand-made"
			return index
		}, `is under recorded/, but its source is "hand-made"`},
		{"a hand-made file with a date", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Recorded = "2026-11-08"
			return index
		}, `has a date or a scrub policy, but its source is "hand-made, modeled on nothing"`},
		{"a rebuilt file with a scrub policy", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[1].Scrub = "abc123 keep"
			return index
		}, `has a date or a scrub policy, but its source is "rebuilt from a page"`},
		{"a header whose value is not kept", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.Headers["ETag"] = []string{`"a1b2"`}
			return index
		}, "keeps the value of ETag"},
		{"Content-Length with its value", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.Headers["Content-Length"] = []string{"7"}
			return index
		}, "keeps Content-Length's value"},
		{"a kept header without a value", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.Headers["Date"] = nil
			return index
		}, "has Date without a value"},
		{"a kept header named only", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.OtherHeaders = append(index[0].Response.OtherHeaders, "date")
			return index
		}, "keeps only the name of date"},
		{"a header in both lists", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[1].Response.OtherHeaders = []string{"x-request-id"}
			return index
		}, "has x-request-id in both"},
		{"a header named twice", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.OtherHeaders = append(index[0].Response.OtherHeaders, "etag")
			return index
		}, "names etag twice"},
		{"a real correlation ID", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[0].Response.Headers["Schwab-Client-Correlid"] = []string{"8f14e45f-ceea-467a-9b5c-1d2a3b4c5d6e"}
			return index
		}, `Schwab-Client-Correlid "8f14e45f-ceea-467a-9b5c-1d2a3b4c5d6e", which is no fake's`},
		{"a real request ID", func(_, _ fstest.MapFS, index []Entry) []Entry {
			delete(index[0].Response.Headers, "X-Request-Id")
			index[0].Response.Headers["x-request-id"] = []string{"8f14e45f-ceea-467a-9b5c-1d2a3b4c5d6e"}
			return index
		}, `x-request-id "8f14e45f-ceea-467a-9b5c-1d2a3b4c5d6e", which is no fake's`},
		{"a canned ID of another status", func(_, _ fstest.MapFS, index []Entry) []Entry {
			index[1].Response.Status = 503
			return index
		}, `"corr-429", which is no fake's`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disk, index := good()
			embedded := maps.Clone(disk)
			index = tc.change(disk, embedded, index)
			got := checkTestdata(disk, embedded, index)
			if !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("got %q, want one that says %q", got, tc.want)
			}
		})
	}
}

// handMade returns the entries of index that are not recordings: those of
// the files made by hand, which the tests that pin names and values keep
// to. A recording joins the tests that iterate every answer, and its values
// are the value hygiene tests' to check.
func handMade(index []Entry) []Entry {
	var es []Entry
	for _, e := range index {
		if e.Source != "recorded" {
			es = append(es, e)
		}
	}
	return es
}

// handMadeText returns the text of the hand-made files of a testdata
// folder, given as fsys and its index: README.md, each file, and the
// entries of the files, for a test that looks for a value as text.
func handMadeText(fsys fs.FS, index []Entry) ([]byte, error) {
	text, err := fs.ReadFile(fsys, "README.md")
	if err != nil {
		return nil, err
	}
	es := handMade(index)
	entries, err := json.Marshal(es)
	if err != nil {
		return nil, err
	}
	text = append(append(text, '\n'), entries...)
	for _, e := range es {
		file, err := fs.ReadFile(fsys, e.File)
		if err != nil {
			return nil, err
		}
		text = append(append(text, '\n'), file...)
	}
	return text, nil
}

// The hand-made text is README.md's, the hand-made files' and their
// entries', and never a recording's, which may hold spares and UnknownHash
// by design.
func TestHandMadeText(t *testing.T) {
	fsys := fstest.MapFS{
		"README.md":                    {Data: []byte("# Fixtures\n")},
		"a.json":                       {Data: []byte(`{"made":"by hand"}`)},
		"recorded/2026-11-08/001.json": {Data: []byte(`[{"accountNumber":"` + SpareAccountNumbers[0] + `"}]`)},
	}
	var a, r Entry
	a.File, a.Source = "a.json", "hand-made, modeled on nothing"
	a.Request.Method, a.Request.Path, a.Request.Query = "GET", "/trader/v1/accounts/"+FixtureAccounts[0].Hash, "fields=positions"
	a.Response.Headers = map[string][]string{"X-Request-Id": {"00000000-0000-4000-8000-000000000001"}}
	r.File, r.Source, r.Recorded, r.Scrub = "recorded/2026-11-08/001.json", "recorded", "2026-11-08", "abc123 keep"
	r.Request.Method, r.Request.Path = "GET", "/trader/v1/accounts/"+UnknownHash+"/transactions"
	r.Request.Query = "symbol=" + SpareAccountNumbers[1]
	r.Response.Headers = map[string][]string{"X-Request-Id": {"00000000-0000-4000-8000-000000000002"}}
	text, err := handMadeText(fsys, []Entry{r, a})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Fixtures", `{"made":"by hand"}`, FixtureAccounts[0].Hash, "fields=positions", "00000000-0000-4000-8000-000000000001", "modeled on nothing"} {
		if !bytes.Contains(text, []byte(want)) {
			t.Errorf("the hand-made text lacks %s:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{SpareAccountNumbers[0], SpareAccountNumbers[1], UnknownHash, "00000000-0000-4000-8000-000000000002", "2026-11-08"} {
		if bytes.Contains(text, []byte(unwanted)) {
			t.Errorf("the hand-made text holds the recording's %s:\n%s", unwanted, text)
		}
	}
	if es := handMade([]Entry{r, a, r}); len(es) != 1 || es[0].File != "a.json" {
		t.Errorf("handMade kept %+v", es)
	}
	if _, err := handMadeText(fstest.MapFS{"README.md": fsys["README.md"]}, []Entry{a}); err == nil {
		t.Error("a hand-made file that is not there was not reported")
	}
}

// Fixtures names every file under testdata, in lexical order, as Fixture
// takes it: README.md, index.json and each file that Index names.
func TestFixtures(t *testing.T) {
	want := []string{"README.md", "index.json"}
	for _, e := range Index() {
		want = append(want, e.File)
	}
	slices.Sort(want)
	got := Fixtures()
	if !slices.Equal(got, want) {
		t.Errorf("Fixtures() = %q, want %q", got, want)
	}
	for _, name := range got {
		Fixture(t, name)
	}
}

// Index returns the entries in the file's order, each time a copy that the
// caller may change, and each hand-made answer of an account endpoint names
// that endpoint's Route.
func TestIndex(t *testing.T) {
	es := Index()
	if len(es) == 0 || es[0].File != "account_numbers.json" {
		t.Fatalf("Index() = %+v", es)
	}
	es[0].File = "changed"
	es[0].Response.Headers["Content-Type"][0] = "changed"
	es[0].Response.OtherHeaders = append(es[0].Response.OtherHeaders, "changed")
	if again := Index(); again[0].File != "account_numbers.json" ||
		again[0].Response.Headers["Content-Type"][0] != "application/json" || len(again[0].Response.OtherHeaders) != 0 {
		t.Errorf("a change to what Index returned reached the next call: %+v", again[0])
	}
	routes := map[string]Route{}
	for _, e := range handMade(Index()) {
		if r, ok := e.Route(); ok {
			routes[e.File] = r
		}
	}
	want := map[string]Route{
		"account_numbers.json":                 AccountNumbersRoute,
		"accounts.json":                        AccountsRoute,
		"transactions.json":                    TransactionsRoute,
		"transaction.json":                     TransactionRoute,
		"user_preference.json":                 UserPreferenceRoute,
		"errors/invalid_account_400.json":      AccountRoute,
		"errors/invalid_account_list_400.json": TransactionsRoute,
		"errors/gateway_timeout_504.json":      AccountsRoute,
		"errors/not_authorized_401.json":       UserPreferenceRoute,
		"errors/unavailable_503.json":          AccountsRoute,
		"errors/whitelabel_500.html":           AccountsRoute,
	}
	if !maps.Equal(routes, want) {
		t.Errorf("routes %v, want %v", routes, want)
	}
}

// An index with a key that Entry has no field for, or with more than one
// value, is refused.
func TestParseIndex(t *testing.T) {
	if es, err := parseIndex([]byte(`[{"file":"a.json","cr":1}]` + "\n")); err != nil || len(es) != 1 || es[0].File != "a.json" || es[0].CR != 1 {
		t.Errorf("a good index: %+v, %v", es, err)
	}
	for _, bad := range []string{
		`[{"file":"a.json","sorce":"hand-made"}]`,
		`[{"file":"a.json","response":{"header":{}}}]`,
		`[{"file":"a.json"}] []`,
		`[{"file":"a.json"}]]`,
		`{"file":"a.json"}`,
		`[{"file":"a.json"}`,
	} {
		if es, err := parseIndex([]byte(bad)); err == nil {
			t.Errorf("parseIndex(%s) = %+v, want an error", bad, es)
		}
	}
}

func TestRouteOf(t *testing.T) {
	hash := FixtureAccounts[0].Hash
	for _, tc := range []struct {
		method, path string
		want         Route
	}{
		{"GET", "/trader/v1/accounts/accountNumbers", AccountNumbersRoute},
		{"GET", "/trader/v1/accounts", AccountsRoute},
		{"HEAD", "/trader/v1/accounts", AccountsRoute},
		{"GET", "/trader/v1/accounts/" + hash, AccountRoute},
		{"GET", "/trader/v1/accounts/AB%2FC%20D", AccountRoute},
		{"GET", "/trader/v1/accounts/" + hash + "/transactions", TransactionsRoute},
		{"GET", "/trader/v1/accounts/" + hash + "/transactions/1", TransactionRoute},
		{"GET", "/trader/v1/userPreference", UserPreferenceRoute},
		{"GET", "/trader/v1/test", TestRoute},
		{"POST", "/trader/v1/accounts", ""},
		{"GET", "/trader/v1/accounts/" + hash + "/orders", ""},
		{"GET", "/trader/v1/./accounts", ""},
		{"GET", "/trader/v1//accounts", ""},
		{"GET", "/trader/v1/accounts?fields=positions", ""},
		{"GET", "//host/trader/v1/accounts", ""},
		{"GET", "trader/v1/accounts", ""},
		{"POST", "/v1/oauth/token", ""},
		{"", "/trader/v1/accounts", ""},
	} {
		if got, ok := routeOf(tc.method, tc.path); got != tc.want || ok != (tc.want != "") {
			t.Errorf("routeOf(%q, %q) = %q, %v, want %q", tc.method, tc.path, got, ok, tc.want)
		}
	}
}

func TestKeptHeader(t *testing.T) {
	for _, name := range []string{
		"Content-Type", "content-type", "Content-Length", "Retry-After", "Date", "Cache-Control", "Expires",
		"Pragma", "Vary", "Strict-Transport-Security", "Schwab-Client-CorrelId", "Schwab-Client-CorrelID",
		"schwab-client-correlid", "Schwab-Client-Correlid", "X-Request-Id", "X-REQUEST-ID",
		"Access-Control-Allow-Origin", "access-control-expose-headers",
	} {
		if !KeptHeader(name) {
			t.Errorf("KeptHeader(%q) = false, want true", name)
		}
	}
	for _, name := range []string{
		"ETag", "Last-Modified", "Set-Cookie", "Authorization", "Server", "Access-Control-", "Access-Control",
		"X-Request-Ids", "Content-Type2", "Content", "",
	} {
		if KeptHeader(name) {
			t.Errorf("KeptHeader(%q) = true, want false", name)
		}
	}
}

// A fixture's extension follows its bytes, whatever its Content-Type: JSON,
// an HTML page, or neither.
func TestExtensionFor(t *testing.T) {
	for want, bodies := range map[string][]string{
		".json": {`{"message":"Invalid account number"}`, " [ 1, \"a\" ]\n", `"<html>"`, `null`},
		".html": {"<html><body><h1>Whitelabel Error Page</h1>", "<HTML><HEAD>\n<TITLE>Access Denied</TITLE>", "\n  <!DOCTYPE html><p>"},
		".txt":  {" \n", "", `{"message":`, "[] []", "Not Found", `<?xml version="1.0"?><fault/>`},
	} {
		for _, body := range bodies {
			if got := ExtensionFor([]byte(body)); got != want {
				t.Errorf("ExtensionFor(%q) = %q, want %q", body, got, want)
			}
		}
	}
}

// The fakes that stand in for real identifiers are what the hand-made
// fixtures hold, and none is part of another value, so that a test can
// look for each as text. A recording may hold spares and UnknownHash by
// design; the value hygiene tests check its values.
func TestFakes(t *testing.T) {
	all, err := handMadeText(embeddedTestdata(t), Index())
	if err != nil {
		t.Fatal(err)
	}

	var numbers []struct {
		AccountNumber string `json:"accountNumber"`
		HashValue     string `json:"hashValue"`
	}
	if err := json.Unmarshal(Fixture(t, "account_numbers.json"), &numbers); err != nil {
		t.Fatal(err)
	}
	var pref struct {
		Accounts []struct {
			AccountNumber, NickName, DisplayAcctID string
		}
		StreamerInfo []struct {
			SchwabClientCustomerID, SchwabClientCorrelID string
		}
	}
	if err := json.Unmarshal(Fixture(t, "user_preference.json"), &pref); err != nil {
		t.Fatal(err)
	}
	if len(numbers) != len(FixtureAccounts) || len(pref.Accounts) != len(FixtureAccounts) {
		t.Fatalf("%d account numbers and %d preference accounts, want %d", len(numbers), len(pref.Accounts), len(FixtureAccounts))
	}
	for i, a := range FixtureAccounts {
		if a.Hash != HashFor(a.Number) || a.DisplayID != "..."+a.Number[len(a.Number)-3:] {
			t.Errorf("FixtureAccounts[%d] = %+v", i, a)
		}
		if n := numbers[i]; n.AccountNumber != a.Number || n.HashValue != a.Hash {
			t.Errorf("account_numbers.json's account %d is %+v, want %s", i, n, a.Number)
		}
		if p := pref.Accounts[i]; p.AccountNumber != a.Number || p.NickName != a.Nickname || p.DisplayAcctID != a.DisplayID {
			t.Errorf("user_preference.json's account %d is %+v, want %+v", i, p, a)
		}
	}
	if len(pref.StreamerInfo) != 1 || pref.StreamerInfo[0].SchwabClientCustomerID != StreamerCustomerID ||
		pref.StreamerInfo[0].SchwabClientCorrelID != StreamerCorrelID {
		t.Errorf("user_preference.json's streamer info is %+v", pref.StreamerInfo)
	}

	page := html.UnescapeString(string(Fixture(t, "errors/akamai_403.html")))
	if !strings.Contains(page, "Reference #"+AkamaiReference+"\n") {
		t.Errorf("the Akamai page's Reference is not AkamaiReference:\n%s", page)
	}
	var rows []struct{ ActivityID int64 }
	if err := json.Unmarshal(Fixture(t, "transactions.json"), &rows); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, rows[0])
	if err := json.Unmarshal(Fixture(t, "transaction.json"), &rows[len(rows)-1]); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ActivityID == UnknownActivityID {
			t.Errorf("a fixture row has UnknownActivityID, %d", r.ActivityID)
		}
	}
	if bytes.Contains(all, []byte(UnknownHash)) {
		t.Error("a fixture holds UnknownHash")
	}

	others := []string{UnknownHash, StreamerCustomerID, StreamerCorrelID, AkamaiReference, strconv.FormatInt(UnknownActivityID, 10)}
	for _, a := range FixtureAccounts {
		others = append(others, a.Number, a.Hash)
	}
	if len(SpareAccountNumbers) != 20 {
		t.Errorf("%d spare account numbers, want 20", len(SpareAccountNumbers))
	}
	for i, n := range SpareAccountNumbers {
		if len(n) != 8 || strings.Trim(n, "0123456789") != "" || n[0] == '0' {
			t.Errorf("spare %d, %q, is not 8 digits without a leading 0", i, n)
		}
		if bytes.Contains(all, []byte(n)) {
			t.Errorf("spare %s is in a fixture", n)
		}
		for _, o := range others {
			if strings.Contains(o, n) {
				t.Errorf("spare %s is in the fake %s", n, o)
			}
		}
		for j, m := range SpareAccountNumbers {
			if i != j && strings.Contains(m, n) {
				t.Errorf("spare %s is in spare %s", n, m)
			}
		}
		if h := HashFor(n); bytes.Contains(all, []byte(h)) || slices.Contains(others, h) {
			t.Errorf("the hash of spare %s is in a fixture or another fake", n)
		}
	}
}
