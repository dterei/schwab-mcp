package schwabtest

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
)

// files holds the fixtures, embedded so that a test in any package reads
// the same bytes wherever it runs. The all: prefix takes in names that
// start with "." or "_" too, which git commits like any other, so that the
// hygiene tests see every file that ships.
//
//go:embed all:testdata
var files embed.FS

// Fixture returns a copy of the fixture with the given name, such as
// "accounts.json" or "errors/akamai_403.html", failing the test if there is
// none.
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := files.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("schwabtest: no fixture %s: %v", name, err)
	}
	return b
}

// Fixtures returns the names of every file under testdata, in lexical
// order, as Fixture takes them: the answers, README.md and index.json.
// Index says what each answer is.
func Fixtures() []string {
	var names []string
	err := fs.WalkDir(files, "testdata", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, strings.TrimPrefix(path, "testdata/"))
		}
		return err
	})
	if err != nil {
		panic("schwabtest: reading the embedded fixtures: " + err.Error())
	}
	slices.Sort(names)
	return names
}

// mustFixture returns the fixture with the given name, for the package's
// own values, which are read once.
func mustFixture(name string) []byte {
	b, err := files.ReadFile("testdata/" + name)
	if err != nil {
		panic("schwabtest: no fixture " + name + ": " + err.Error())
	}
	return b
}

// HashFor returns the hash the fake gives an account number: upper-case
// hex of the SHA-256 of "schwabtest:" and the number. Schwab's hashes are
// opaque; the fake derives its own so that it can serve any account, and so
// that a test can check every hash in the fixtures.
func HashFor(number string) string {
	sum := sha256.Sum256([]byte("schwabtest:" + number))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// A FixtureAccount is one of the accounts the fixtures describe.
type FixtureAccount struct {
	Number    string // accountNumber
	Hash      string // hashValue, HashFor(Number)
	Nickname  string // nickName in the user preferences
	DisplayID string // displayAcctId: "..." and the last three digits
}

// FixtureAccounts are the fixtures' accounts, in the order the account
// numbers fixture lists them. No other value in the fixtures contains one
// of their numbers.
var FixtureAccounts = []FixtureAccount{
	{Number: "29470135", Hash: HashFor("29470135"), Nickname: "Individual", DisplayID: "...135"},
	{Number: "61803398", Hash: HashFor("61803398"), Nickname: "Options & Margin", DisplayID: "...398"},
	{Number: "40221783", Hash: HashFor("40221783"), Nickname: "Contributory", DisplayID: "...783"},
}

// SpareAccountNumbers are fake account numbers for a recording of more
// accounts than FixtureAccounts holds: its accounts take FixtureAccounts'
// numbers and then these, in order. They are the first twenty runs of
// eight of π's decimals that do not start with 0. None is in any fixture,
// so every account number in a committed fixture is one of
// FixtureAccounts' or one of these.
var SpareAccountNumbers = []string{
	"14159265", "35897932", "38462643", "38327950", "28841971",
	"69399375", "10582097", "49445923", "28620899", "86280348",
	"25342117", "80865132", "82306647", "95505822", "31725359",
	"40812848", "11174502", "84102701", "93852110", "55596446",
}

// UnknownHash is a hash that is no account's, for a probe to send to learn
// how Schwab answers a hash it does not know.
var UnknownHash = HashFor("no such account")

// UnknownActivityID is an id that no fixture row has, for a probe to ask
// for to learn how Schwab answers an id it does not know.
const UnknownActivityID int64 = 99999999999

// AkamaiReference is the one Reference # that any fixture may hold, as the
// edge's page in errors/akamai_403.html does.
const AkamaiReference = "18.9efa3f17.1792539138.32dec4a"

// The streamer's customer and correlation IDs that user_preference.json
// holds, which stand in for the user's own in every fixture.
var (
	StreamerCustomerID = "5f0c2e9a8b7d4e31a6c9f2b0d8e7a4c15f0c2e9a8b7d4e31a6c9f2b0d8e7a4c1"
	StreamerCorrelID   = "c3a9e2f4-7b1d-4e8a-9f6c-2d5b8a1e0f7c"
)

// An Entry is one file's line in testdata/index.json: the request the file
// answers, the answer's status and headers, and where the file came from.
type Entry struct {
	File    string `json:"file"` // as Fixture takes it, such as "errors/akamai_403.html"
	Request struct {
		Method string `json:"method"`
		Path   string `json:"path"`  // escaped, as sent
		Query  string `json:"query"` // raw, as sent
	} `json:"request"`
	Response struct {
		Status       int    `json:"status"`
		Proto        string `json:"proto,omitempty"`
		Uncompressed bool   `json:"uncompressed,omitempty"`
		// Headers are the answer's headers that KeptHeader keeps, but
		// Content-Length, with their values, each name as it came.
		Headers map[string][]string `json:"headers"`
		// OtherHeaders are the names of the rest, Content-Length's among
		// them: its value would be wrong once a scrubbed value changes
		// length.
		OtherHeaders []string `json:"otherHeaders"`
	} `json:"response"`
	// Source says where the file came from: "hand-made, modeled on …",
	// "rebuilt from …" or "recorded".
	Source string `json:"source"`
	// Recorded is the day of a recording, as YYYY-MM-DD.
	Recorded string `json:"recorded,omitempty"`
	// Scrub is, for a recording, the scrubber's version and its policy for
	// amounts.
	Scrub string `json:"scrub,omitempty"`
	// CR is how many carriage returns the file holds. A recording keeps
	// whatever Schwab sent, and a change of line endings shows.
	CR int `json:"cr"`
}

// index is testdata/index.json, read once.
var index = sync.OnceValue(func() []Entry {
	es, err := parseIndex(mustFixture("index.json"))
	if err != nil {
		panic("schwabtest: reading testdata/index.json: " + err.Error())
	}
	return es
})

// parseIndex reads an index. It refuses a key that no field of Entry
// takes, so that a misspelled one is not lost, and anything after the list.
func parseIndex(b []byte) ([]Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var es []Entry
	if err := dec.Decode(&es); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one value")
	}
	return es, nil
}

// Index returns every entry of testdata/index.json, in the order the file
// holds them. Every file under testdata has one, but README.md and
// index.json itself.
func Index() []Entry {
	es := slices.Clone(index())
	for i := range es {
		r := &es[i].Response
		r.Headers = maps.Clone(r.Headers)
		for k, v := range r.Headers {
			r.Headers[k] = slices.Clone(v)
		}
		r.OtherHeaders = slices.Clone(r.OtherHeaders)
	}
	return es
}

// routes finds the Route of a method and path.
var routes = sync.OnceValue(func() *http.ServeMux {
	mux := http.NewServeMux()
	for _, r := range allRoutes {
		mux.HandleFunc(string(r), func(http.ResponseWriter, *http.Request) {})
	}
	return mux
})

// routeOf returns the Route that serves method and p, an escaped path.
// A path that the fake would first redirect, being unclean, has none.
func routeOf(method, p string) (Route, bool) {
	u, err := url.Parse(p)
	if err != nil || !strings.HasPrefix(p, "/") || u.Host != "" || u.RawQuery != "" || u.Fragment != "" ||
		path.Clean(u.EscapedPath()) != u.EscapedPath() {
		return "", false
	}
	_, pattern := routes().Handler(&http.Request{Method: method, URL: u, Host: "schwabtest"})
	if !slices.Contains(allRoutes, Route(pattern)) {
		return "", false
	}
	return Route(pattern), true
}

// Route returns the Route that the entry's request names, if any.
func (e Entry) Route() (Route, bool) {
	return routeOf(e.Request.Method, e.Request.Path)
}

// keptHeaders are the headers whose values a recording keeps; see
// KeptHeader.
var keptHeaders = []string{
	"Content-Type", "Content-Length", "Retry-After", "Date", "Cache-Control", "Expires", "Pragma", "Vary",
	"Strict-Transport-Security", "Schwab-Client-CorrelId", "X-Request-Id",
}

// KeptHeader reports whether a recording keeps the value of the header
// with the given name: Content-Type, Content-Length, Retry-After, Date,
// Cache-Control, Expires, Pragma, Vary, Access-Control-*,
// Strict-Transport-Security and the correlation headers
// Schwab-Client-CorrelId and X-Request-Id, in any case. Of every other
// header a recording keeps only the name: an ETag, say, can be a digest of
// the private body. The recorder, the scrubber and the hygiene tests all
// decide by it.
func KeptHeader(name string) bool {
	for _, k := range keptHeaders {
		if strings.EqualFold(name, k) {
			return true
		}
	}
	const cors = "Access-Control-"
	return len(name) > len(cors) && strings.EqualFold(name[:len(cors)], cors)
}
