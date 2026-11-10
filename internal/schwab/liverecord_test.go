package schwab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// A recorder keeps each API answer of a live run raw, under <out>/raw/,
// for the scrubber to make fixtures of: NNN.body, the bytes as the client
// read them, and NNN.json, a rawAnswer. A token request is never kept. The
// folder is never to be shared: it holds the accounts' hashes, values and
// descriptions as Schwab sent them.
type recorder struct {
	dir    string
	commit string // the commit and whether the tree is modified, or "unknown"
	mu     sync.Mutex
	n      int   // the answers kept
	err    error // the first failure to keep one, after which none is kept
}

// A rawAnswer is NNN.json, what a recorder keeps of an answer beside its
// body, in the shape of an entry of the fixtures' index.json.
type rawAnswer struct {
	Request struct {
		Method string `json:"method"`
		Path   string `json:"path"`  // escaped, as sent
		Query  string `json:"query"` // raw, as sent
	} `json:"request"`
	Response struct {
		Status       int    `json:"status"`
		Proto        string `json:"proto"`
		Uncompressed bool   `json:"uncompressed"`
		// Headers are the headers whose values schwabtest.KeptHeader
		// keeps, by name as Go spells it, and OtherHeaders the names alone
		// of the rest, in order.
		Headers      map[string][]string `json:"headers"`
		OtherHeaders []string            `json:"otherHeaders"`
		// CutShort says that the body could not be read to its end, as when
		// the connection broke part way through it; the body file holds
		// what came.
		CutShort bool `json:"cutShort,omitempty"`
	} `json:"response"`
	Sent   time.Time `json:"sent"`   // when the request was sent, by the run's clock
	Go     string    `json:"go"`     // runtime.Version()
	Commit string    `json:"commit"` // the commit, and whether the tree is modified, or "unknown"
}

// newRecorder makes out's raw folder, readable only by the user, for a
// recording. A raw folder that is there already is never written into.
func newRecorder(out, commit string) (*recorder, error) {
	dir := filepath.Join(out, "raw")
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s holds a recording already, which is never written over", dir)
		}
		return nil, err
	}
	return &recorder{dir: dir, commit: commit}, nil
}

// record keeps resp, the answer to req sent at sent, if req is an API
// request. It reads the body whole, as the client would, and gives resp a
// body that reads the same, and fails the same if it was cut short.
func (rec *recorder) record(req *http.Request, sent time.Time, resp *http.Response) {
	if req.Method != http.MethodGet || !underAPI(req.URL.EscapedPath()) {
		return // a token request is never kept
	}
	body, readErr := readWhole(resp)

	var a rawAnswer
	a.Request.Method, a.Request.Path, a.Request.Query = req.Method, req.URL.EscapedPath(), req.URL.RawQuery
	a.Response.Status, a.Response.Proto, a.Response.Uncompressed = resp.StatusCode, resp.Proto, resp.Uncompressed
	a.Response.Headers = map[string][]string{}
	a.Response.OtherHeaders = []string{}
	for _, name := range slices.Sorted(maps.Keys(resp.Header)) {
		if schwabtest.KeptHeader(name) {
			a.Response.Headers[name] = slices.Clone(resp.Header[name])
		} else {
			a.Response.OtherHeaders = append(a.Response.OtherHeaders, name)
		}
	}
	a.Response.CutShort = readErr != nil
	a.Sent, a.Go, a.Commit = sent.UTC(), runtime.Version(), rec.commit
	meta, err := json.MarshalIndent(a, "", "  ")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.err != nil {
		return
	}
	rec.n++
	name := filepath.Join(rec.dir, fmt.Sprintf("%03d", rec.n))
	if err == nil {
		err = createPrivate(name+".body", body)
	}
	if err == nil {
		err = createPrivate(name+".json", append(meta, '\n'))
	}
	if err != nil {
		rec.err = fmt.Errorf("keeping answer %d: %w", rec.n, err)
	}
}

// failure returns why the recording stopped, or nil.
func (rec *recorder) failure() error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.err
}

// readWhole reads resp's body whole, as the Client would, at most maxBody
// and a byte, and gives resp a body that reads the same, and fails the same
// if it was cut short. It returns the bytes, and why the read ended short,
// if it did.
func readWhole(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	_ = resp.Body.Close()
	resp.Body = &readAgain{r: bytes.NewReader(body), err: err}
	return body, err
}

// readAgain is a body read already: it gives the bytes read, and then the
// error that the read ended with, or io.EOF.
type readAgain struct {
	r   *bytes.Reader
	err error
}

func (b *readAgain) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) && b.err != nil {
		err = b.err
	}
	return n, err
}

func (b *readAgain) Close() error { return nil }

// cutBody is a body that breaks after its bytes.
type cutBody struct{ r io.Reader }

func (c *cutBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (c *cutBody) Close() error { return nil }

// readRaw returns the body and the rawAnswer of the nth answer that the
// recording in dir kept.
func readRaw(t *testing.T, dir string, n int) ([]byte, rawAnswer) {
	t.Helper()
	name := filepath.Join(dir, fmt.Sprintf("%03d", n))
	body, err := os.ReadFile(name + ".body")
	if err != nil {
		t.Fatal(err)
	}
	var a rawAnswer
	if err := json.Unmarshal([]byte(readFile(t, name+".json")), &a); err != nil {
		t.Fatal(err)
	}
	return body, a
}

// The recorder keeps each API answer as its bytes and what came with it:
// the request as sent, the status and protocol, the values of the headers
// that KeptHeader keeps and the names alone of the rest, the time sent, the
// Go version and the commit; never a token request. The client reads the
// same bytes as without it, and the same failure for a body cut short. The
// files are readable only by the user, numbered in turn, and a recording
// that is there already is never written into.
func TestRecorder(t *testing.T) {
	out := t.TempDir()
	rec, err := newRecorder(out, "c0ffee, modified")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newRecorder(out, "x"); err == nil || !strings.Contains(err.Error(), "holds a recording already") {
		t.Errorf("a second recording: %v", err)
	}
	sent := time.Date(2026, 11, 2, 15, 4, 5, 6, time.FixedZone("x", 3600))
	// answer has the recorder keep an answer of body to a request of
	// method for url, and returns what the client then reads of it.
	answer := func(method, url, body string, header http.Header, cut bool) ([]byte, error) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp := stubAnswer(req, http.StatusOK, body)
		resp.Proto = "HTTP/2.0"
		maps.Copy(resp.Header, header)
		if cut {
			resp.Body = &cutBody{r: strings.NewReader(body)}
		}
		rec.record(req, sent, resp)
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	header := http.Header{
		"Content-Length": {"7"}, "Schwab-Client-Correlid": {"corr-1"}, "Etag": {`"digest"`}, "Set-Cookie": {"zq9=1"},
		"Access-Control-Allow-Origin": {"*"}, "X-Zq9": {"private"},
	}
	got, err := answer(http.MethodGet, DefaultBaseURL+"/trader/v1/accounts/H%2B1/transactions?types=TRADE&startDate=x", `[ {} ]`, header, false)
	if err != nil || string(got) != `[ {} ]` {
		t.Errorf("the client read %q, %v", got, err)
	}
	if _, err := answer(http.MethodPost, DefaultBaseURL+tokenPath, `{"access_token":"zq9"}`, nil, false); err != nil {
		t.Fatal(err)
	}
	got, err = answer(http.MethodGet, DefaultBaseURL+"/marketdata/v1/quotes", `{"cut`, nil, true)
	if !errors.Is(err, io.ErrUnexpectedEOF) || string(got) != `{"cut` {
		t.Errorf("the client read %q, %v from a body cut short", got, err)
	}
	if rec.failure() != nil || rec.n != 2 {
		t.Fatalf("kept %d answers: %v", rec.n, rec.failure())
	}

	body, a := readRaw(t, rec.dir, 1)
	if string(body) != `[ {} ]` || a.Request.Method != http.MethodGet || a.Request.Path != "/trader/v1/accounts/H%2B1/transactions" ||
		a.Request.Query != "types=TRADE&startDate=x" || a.Response.Status != http.StatusOK || a.Response.Proto != "HTTP/2.0" ||
		a.Response.CutShort || !a.Sent.Equal(sent) || a.Sent.Location() != time.UTC || a.Go != runtime.Version() || a.Commit != "c0ffee, modified" {
		t.Errorf("the first answer kept: %s, %+v", body, a)
	}
	wantHeaders := map[string][]string{
		"Access-Control-Allow-Origin": {"*"}, "Content-Length": {"7"}, "Content-Type": {"application/json"},
		"Schwab-Client-Correlid": {"corr-1"},
	}
	if !maps.EqualFunc(a.Response.Headers, wantHeaders, slices.Equal) || !slices.Equal(a.Response.OtherHeaders, []string{"Etag", "Set-Cookie", "X-Zq9"}) {
		t.Errorf("headers kept %v, names %v", a.Response.Headers, a.Response.OtherHeaders)
	}
	for _, s := range []string{`\"digest\"`, "zq9=1", "private"} {
		if strings.Contains(readFile(t, filepath.Join(rec.dir, "001.json")), s) {
			t.Errorf("001.json holds %s", s)
		}
	}
	body, a = readRaw(t, rec.dir, 2)
	if string(body) != `{"cut` || !a.Response.CutShort || a.Request.Path != "/marketdata/v1/quotes" {
		t.Errorf("the answer cut short: %s, %+v", body, a)
	}
	if entries, err := os.ReadDir(rec.dir); err != nil || len(entries) != 4 {
		t.Errorf("the recording holds %v, %v; want two answers' files", entries, err)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{rec.dir: 0o700, filepath.Join(rec.dir, "001.body"): 0o600, filepath.Join(rec.dir, "002.json"): 0o600} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: %v, %v; want mode %v", path, info.Mode(), err, want)
			}
		}
	}

	// A file that cannot be made stops the recording, and is never written
	// over; the client still reads its answers.
	if err := os.WriteFile(filepath.Join(rec.dir, "003.body"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = answer(http.MethodGet, DefaultBaseURL+"/trader/v1/accounts", "[]", nil, false)
	if err != nil || string(got) != "[]" || rec.failure() == nil || readFile(t, filepath.Join(rec.dir, "003.body")) != "kept" {
		t.Errorf("after a failure: read %q, %v; the recording's failure %v", got, err, rec.failure())
	}
	if _, err := answer(http.MethodGet, DefaultBaseURL+"/trader/v1/accounts", "[]", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rec.dir, "004.body")); !errors.Is(err, fs.ErrNotExist) || rec.n != 3 {
		t.Errorf("an answer kept after the failure: %v", err)
	}
}

// The recorder reads an answer no further than the client would, a byte
// past the most it takes, and keeps what it read.
func TestRecorderReadsAsTheClientDoes(t *testing.T) {
	defer func(n int) { maxBody = n }(maxBody)
	maxBody = 8
	rec, err := newRecorder(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, DefaultBaseURL+"/trader/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := stubAnswer(req, http.StatusOK, "[0123456789012345678]")
	rec.record(req, time.Now(), resp)
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if body, _ := readRaw(t, rec.dir, 1); err != nil || string(got) != "[01234567" || string(body) != "[01234567" {
		t.Errorf("the client read %q, %v; the recording kept %q", got, err, body)
	}
}
