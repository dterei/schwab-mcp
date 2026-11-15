package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/mcpserver"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The tests of design 15.6: the command as a client runs it, over real
// pipes with raw JSON-RPC lines.

// TestMain runs the command itself, with the test-only tools stray and
// slow, when SCHWAB_MCP_CHILD is set, so that the tests run this test
// binary as the server and nothing is built twice under -race. go test
// never sets it. Nothing here keeps os.Stdout in a variable: one made at
// init would keep descriptor 1's file referenced, and hide a guard that
// let the garbage collector close it.
func TestMain(m *testing.M) {
	if os.Getenv("SCHWAB_MCP_CHILD") != "" {
		testTools = registerTestTools
		main()
	}
	os.Exit(m.Run())
}

// registerTestTools registers the child's tools, which the command itself
// does not have: stray writes to stdout every way it can once the garbage
// collector has run, and answers whether descriptor 1 is still open and
// which descriptor a file it opens gets; slow says on stderr that it
// started, and waits for its context to end; stuck says so too, and never
// returns, as a handler that ignores its context would not, so that only
// the hard exit after a signal ends the process.
func registerTestTools(s *mcpserver.Server) {
	object := json.RawMessage(`{"type":"object"}`)
	s.MCP().AddTool(&mcp.Tool{Name: "stray", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runtime.GC()
		runtime.GC()
		time.Sleep(20 * time.Millisecond) // finalizers run on a goroutine of their own
		fmt.Println("stray: fmt.Println")
		_, _ = os.Stdout.WriteString("stray: os.Stdout.WriteString\n")
		rawWrite("stray: a raw write to descriptor 1\n")
		f, err := os.CreateTemp("", "schwab-mcp-stray-")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		text := fmt.Sprintf(`{"fd1_open":%t,"opened":%d}`, fd1Open(), descriptor(f))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	s.MCP().AddTool(&mcp.Tool{Name: "slow", InputSchema: object}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fmt.Fprintln(os.Stderr, "slow: started")
		<-ctx.Done()
		return nil, ctx.Err()
	})
	s.MCP().AddTool(&mcp.Tool{Name: "stuck", InputSchema: object}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fmt.Fprintln(os.Stderr, "stuck: started")
		select {}
	})
}

// A world is what a child serves from: a fake Schwab over plain HTTP,
// which the server reaches only because it is on the loopback address,
// with a login to it saved; a home folder; and in it the config file that
// schwabtx setup would save, naming the fake, the token file, and a store
// and a rate file that do not exist.
type world struct {
	fake                             *schwabtest.Server
	home, config, token, store, rate string
	// requests is how many requests the fake had once the login was saved.
	requests int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	fake := schwabtest.New(t)
	fake.LoadFixtures()
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "schwabtx")
	w := &world{
		fake: fake, home: home,
		config: filepath.Join(dir, "config.json"), token: filepath.Join(dir, "token.json"),
		store: filepath.Join(dir, "history.db"), rate: filepath.Join(home, ".cache", "schwabtx", "rate"),
	}
	auth, err := schwab.NewAuth(schwab.Config{
		AppKey: schwabtest.AppKey, AppSecret: schwabtest.AppSecret, BaseURL: fake.URL(),
		TokenFile: w.token, HTTPClient: fake.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Login(t.Context(), schwabtest.Code); err != nil {
		t.Fatal("logging in to the fake:", err)
	}
	if err := config.Save(w.config, config.File{
		AppKey: schwabtest.AppKey, AppSecret: schwabtest.AppSecret, BaseURL: fake.URL(),
		TokenFile: w.token, StoreFile: w.store, RateFile: w.rate,
	}); err != nil {
		t.Fatal(err)
	}
	w.requests = fake.AllRequests()
	return w
}

// untouched fails the test if the server has sent the fake a request, or
// made the store or the rate file.
func (w *world) untouched(t *testing.T) {
	t.Helper()
	if n := w.fake.AllRequests() - w.requests; n != 0 {
		t.Errorf("the server sent the fake %d requests", n)
	}
	for _, f := range []string{w.store, w.rate} {
		if _, err := os.Stat(f); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists: %v", filepath.Base(f), err)
		}
	}
}

// secrets are what no output of the server may hold.
func (w *world) secrets() []string {
	return append([]string{schwabtest.AppKey}, w.fake.Secrets()...)
}

// A child is the test binary running as the command.
type child struct {
	t              *testing.T
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout, stderr *stream
	exited         chan struct{} // closed once it has exited and its pipes are read
	code           int           // its exit code, once exited
}

// startChild runs the command with args in w, over pipes. The end of the
// test closes its stdin, and kills it if it has not exited 10 seconds on.
func startChild(t *testing.T, w *world, args ...string) *child {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Env = childEnv(w.home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{t: t, cmd: cmd, stdin: stdin, stdout: newStream(), stderr: newStream(), exited: make(chan struct{})}
	var reading sync.WaitGroup
	for _, p := range []struct {
		r io.Reader
		s *stream
	}{{stdout, c.stdout}, {stderr, c.stderr}} {
		reading.Go(func() { p.s.read(p.r) })
	}
	go func() {
		reading.Wait()
		_ = cmd.Wait()
		c.code = cmd.ProcessState.ExitCode()
		close(c.exited)
	}()
	t.Cleanup(func() {
		stdin.Close()
		select {
		case <-c.exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-c.exited
		}
		if t.Failed() {
			t.Logf("the child's stderr:\n%s", strings.Join(c.stderr.all(), "\n"))
		}
	})
	return c
}

// childEnv is the environment of a child: this process's, but for every
// SCHWAB_ variable, which would win over the config file, with the home
// and user folders in home, so that nothing reads the user's own files,
// and with the race detector's wait at exit off.
func childEnv(home string) []string {
	replaced := []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "GORACE"}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		name = strings.ToUpper(name)
		return strings.HasPrefix(name, "SCHWAB_") || slices.Contains(replaced, name)
	})
	return append(env,
		"SCHWAB_MCP_CHILD=1",
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"),
		"HOME="+home, "USERPROFILE="+home,
		"APPDATA="+filepath.Join(home, "AppData", "Roaming"), "LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"))
}

// send writes line, and a newline, to the child's stdin.
func (c *child) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.stdin, line+"\n"); err != nil {
		c.t.Fatal("writing to the child:", err)
	}
}

// answer waits for the child's answer to the request id, and returns it.
func (c *child) answer(id int) map[string]any {
	c.t.Helper()
	var answer map[string]any
	c.stdout.await(c.t, fmt.Sprintf("an answer to request %d", id), func(line string) bool {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			return false
		}
		if n, ok := m["id"].(float64); ok && int(n) == id {
			answer = m
			return true
		}
		return false
	})
	return answer
}

// initialize starts a 2025-06-18 session, as request 1.
func (c *child) initialize() {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if r := c.answer(1); r["result"] == nil {
		c.t.Fatalf("initialize: %v", r)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
}

// call calls the tool name with args, as request id, and returns the text
// of its answer, failing the test if it is an error.
func (c *child) call(id int, name, args string) string {
	c.t.Helper()
	c.send(callLine(id, name, args))
	r := c.answer(id)
	res, _ := r["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if res == nil || res["isError"] == true || len(content) != 1 {
		c.t.Fatalf("%s: %v", name, r)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// callLine is the request id that calls the tool name with args.
func callLine(id int, name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}

// wait waits at most a minute for the child to exit, and returns its exit
// code and how long it waited.
func (c *child) wait() (int, time.Duration) {
	c.t.Helper()
	start := time.Now()
	select {
	case <-c.exited:
		return c.code, time.Since(start)
	case <-time.After(time.Minute):
		c.t.Fatal("the child did not exit within a minute")
		return 0, 0
	}
}

// protocolOnly fails the test unless every line the child wrote on stdout
// is a JSON-RPC message.
func (c *child) protocolOnly() {
	c.t.Helper()
	for _, line := range c.stdout.all() {
		var m struct{ JSONRPC string }
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.JSONRPC != "2.0" {
			c.t.Errorf("stdout holds %q, which is no JSON-RPC message", line)
		}
	}
}

// A stream is what a child writes on one of its pipes, line by line.
type stream struct {
	mu      sync.Mutex
	lines   []string
	ended   bool
	changed chan struct{} // closed, and made anew, at each line and at the end
}

func newStream() *stream { return &stream{changed: make(chan struct{})} }

// read reads r into s until it ends.
func (s *stream) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		s.mu.Lock()
		s.lines = append(s.lines, sc.Text())
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.ended = true
	close(s.changed)
	s.mu.Unlock()
}

// all returns the lines so far.
func (s *stream) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lines)
}

// await waits at most a minute for a line that match takes, the time the
// repository's child-process tests allow a -race binary on a loaded runner,
// and fails the test, naming what, if none comes.
func (s *stream) await(t *testing.T, what string, match func(string) bool) {
	t.Helper()
	deadline := time.After(time.Minute)
	for seen := 0; ; {
		s.mu.Lock()
		lines, ended, changed := s.lines[seen:], s.ended, s.changed
		seen = len(s.lines)
		s.mu.Unlock()
		if slices.ContainsFunc(lines, match) {
			return
		}
		if ended {
			t.Fatalf("the child ended without %s", what)
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("no %s within a minute", what)
		}
	}
}

// The server answers initialize at 2025-06-18, and server/discover at
// 2026-07-28, with no request to Schwab and no store or rate file made:
// its startup reaches neither. Then every tool, called with "arguments":
// null, answers, and the server runs on (#1340); stdin's end stops it, and
// it exits 0.
func TestStartup(t *testing.T) {
	t.Parallel()
	t.Run("2025-06-18", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		c := startChild(t, w, "--config", w.config)
		c.initialize()
		w.untouched(t)
		c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		var listed struct {
			Result struct{ Tools []struct{ Name string } }
		}
		b, _ := json.Marshal(c.answer(2))
		if err := json.Unmarshal(b, &listed); err != nil || len(listed.Result.Tools) == 0 {
			t.Fatalf("tools/list: %s", b)
		}
		id := 3
		for _, tool := range listed.Result.Tools {
			if slices.Contains([]string{"stray", "slow", "stuck"}, tool.Name) {
				continue
			}
			c.send(callLine(id, tool.Name, "null"))
			if r := c.answer(id); r["result"] == nil {
				t.Errorf("%s with null arguments: %v", tool.Name, r)
			}
			id++
		}
		c.stdin.Close()
		if code, _ := c.wait(); code != 0 {
			t.Errorf("exit code %d at stdin's end", code)
		}
		c.protocolOnly()
	})
	t.Run("2026-07-28", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		c := startChild(t, w, "--config", w.config)
		c.send(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{` +
			`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
			`"io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"},` +
			`"io.modelcontextprotocol/clientCapabilities":{}}}}`)
		r := c.answer(1)
		res, _ := r["result"].(map[string]any)
		if versions, _ := res["supportedVersions"].([]any); !slices.Contains(versions, any("2026-07-28")) {
			t.Fatalf("server/discover: %v", r)
		}
		w.untouched(t)
		c.stdin.Close()
		if code, _ := c.wait(); code != 0 {
			t.Errorf("exit code %d at stdin's end", code)
		}
		c.protocolOnly()
	})
}

// stdout carries the protocol alone (design 5.3): once the garbage
// collector has run twice, writes through fmt.Println, os.Stdout and, on
// Linux and macOS, descriptor 1 itself reach stderr, every line on stdout
// is a JSON-RPC message, descriptor 1 is still open, and a file opened
// afterwards gets another.
func TestStrayWritesReachStderr(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := startChild(t, w, "--config", w.config)
	c.initialize()
	var got struct {
		FD1Open bool `json:"fd1_open"`
		Opened  int  `json:"opened"`
	}
	if err := json.Unmarshal([]byte(c.call(2, "stray", "{}")), &got); err != nil {
		t.Fatal(err)
	}
	c.stdin.Close()
	if code, _ := c.wait(); code != 0 {
		t.Errorf("exit code %d at stdin's end", code)
	}
	c.protocolOnly()
	stderr := strings.Join(c.stderr.all(), "\n")
	want := []string{"stray: fmt.Println", "stray: os.Stdout.WriteString"}
	if runtime.GOOS != "windows" {
		want = append(want, "stray: a raw write to descriptor 1")
	}
	for _, s := range want {
		if !strings.Contains(stderr, s) {
			t.Errorf("stderr lacks %q:\n%s", s, stderr)
		}
	}
	if !got.FD1Open {
		t.Error("descriptor 1 was closed")
	}
	if got.Opened == 1 {
		t.Error("a file opened after the garbage collector ran got descriptor 1")
	}
}

// startCall starts a child, a session with it and a call of tool, slow or
// stuck, and returns once the call has started.
func startCall(t *testing.T, tool string) *child {
	t.Helper()
	w := newWorld(t)
	c := startChild(t, w, "--config", w.config)
	c.initialize()
	c.send(callLine(2, tool, "{}"))
	c.stderr.await(t, tool+"'s start", func(line string) bool { return line == tool+": started" })
	return c
}

// When stdin ends while a call runs, the call ends and the server exits 0
// within 5 seconds.
func TestStdinEndStopsTheServer(t *testing.T) {
	t.Parallel()
	c := startCall(t, "slow")
	c.stdin.Close()
	code, took := c.wait()
	if code != 0 {
		t.Errorf("exit code %d at stdin's end", code)
	}
	if took > 5*time.Second {
		t.Errorf("the server exited %v after stdin's end", took)
	}
	t.Logf("the server exited %v after stdin's end", took)
}

// A SIGINT while a call runs ends the call and exits 0 within 5 seconds,
// though stdin stays open: at once for a call that ends with its context,
// and by the hard exit, 250 ms on, for one that never ends.
func TestSignalStopsTheServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGINT to send to another process")
	}
	t.Parallel()
	for _, tool := range []string{"slow", "stuck"} {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			c := startCall(t, tool)
			if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			code, took := c.wait()
			if code != 0 {
				t.Errorf("exit code %d after SIGINT", code)
			}
			if took > 5*time.Second {
				t.Errorf("the server exited %v after SIGINT", took)
			}
			t.Logf("the server exited %v after SIGINT", took)
		})
	}
}

// A malformed line ends the session: the server exits 1, with one line on
// stderr that says why, beside the warning that the fake is not Schwab,
// and no panic. Only a signal arms the hard exit, which would exit 0.
func TestMalformedLineFailsTheSession(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := startChild(t, w, "--config", w.config)
	c.initialize()
	c.send("this is not JSON")
	if code, _ := c.wait(); code != 1 {
		t.Errorf("exit code %d after a malformed line, want 1", code)
	}
	var lines []string
	for _, line := range c.stderr.all() {
		if !strings.Contains(line, "the API's address is not Schwab's") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "level=ERROR") || strings.Contains(lines[0], "panic") {
		t.Errorf("stderr holds %q, want one line at error", lines)
	}
}

// --version and --check print to stdout and exit 0. --check names where
// each setting came from and the API's host as not Schwab's, sends nothing
// to Schwab, and prints no secret.
func TestCheckAndVersion(t *testing.T) {
	t.Parallel()
	t.Run("version", func(t *testing.T) {
		t.Parallel()
		c := startChild(t, newWorld(t), "--version")
		if code, _ := c.wait(); code != 0 {
			t.Errorf("exit code %d", code)
		}
		if out := c.stdout.all(); len(out) != 1 || !strings.HasPrefix(out[0], "schwab-mcp ") {
			t.Errorf("--version printed %q", out)
		}
	})
	t.Run("check", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		c := startChild(t, w, "--config", w.config, "--check")
		if code, _ := c.wait(); code != 0 {
			t.Errorf("exit code %d", code)
		}
		out := strings.Join(c.stdout.all(), "\n")
		for _, want := range []string{
			"Reading ~/.config/schwabtx/config.json:",
			"app key      set (config file)",
			w.fake.Addr() + ", which is not Schwab's",
			"token file   ~/.config/schwabtx/token.json (config file)",
			"store        ~/.config/schwabtx/history.db (config file), no_store",
			"login        made ",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("--check printed\n%s\nwhich lacks %q", out, want)
			}
		}
		for _, secret := range w.secrets() {
			if strings.Contains(out, secret) || slices.ContainsFunc(c.stderr.all(), func(l string) bool { return strings.Contains(l, secret) }) {
				t.Errorf("--check printed a secret:\n%s", out)
			}
		}
		w.untouched(t)
	})
}

// auth_status reports what the server could not use of its config and
// command line, which stops nothing, and what mends it: a config file that
// is not one, which schwabtx setup replaces, and a --config path split at a
// space, whose second half is a stray argument that a person who typed it
// would have been told of, which quoting the path mends. A flag after the
// stray argument is still read.
func TestConfigProblemsAreReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		args   func(w *world) []string
		want   []string
		action string // what auth_status asks the user to do, in part
	}{
		{
			"unreadable", func(w *world) []string {
				bad := filepath.Join(w.home, "bad.json")
				if err := os.WriteFile(bad, []byte("this is not JSON"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{"--config", bad}
			},
			[]string{"config_file: it could not be read, or is not a config file (~/bad.json)"},
			"The server's config file could not be read, at ~/bad.json",
		},
		// The stray argument, not the config file that names no file,
		// says what mends it.
		{
			"split at a space", func(w *world) []string {
				return []string{"--config", filepath.Join(w.home, "Application"), "Support/schwabtx/config.json", "--log-level", "info"}
			},
			[]string{
				"config_file: it does not exist (~/Application)",
				"arguments: the server was started with an argument it does not take; quote a path that holds a space",
			},
			"a path that holds a space must be quoted in a shell line",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := startChild(t, w, tc.args(w)...)
			c.initialize()
			var status struct {
				ConfigProblems []string `json:"config_problems"`
				Action         string
			}
			if err := json.Unmarshal([]byte(c.call(2, "auth_status", "{}")), &status); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(status.ConfigProblems, tc.want) {
				t.Errorf("config_problems %q, want %q", status.ConfigProblems, tc.want)
			}
			if !strings.Contains(status.Action, tc.action) {
				t.Errorf("auth_status asks %q, want it to say %q", status.Action, tc.action)
			}
			c.stdin.Close()
			if code, _ := c.wait(); code != 0 {
				t.Errorf("exit code %d at stdin's end", code)
			}
			if tc.name == "split at a space" && !slices.ContainsFunc(c.stderr.all(), func(l string) bool { return strings.Contains(l, "level=INFO") }) {
				t.Error("--log-level info, after the stray argument, was not used")
			}
		})
	}
}

// A person who types the command wrong gets what is wrong and the usage
// on stderr, and exit code 2; -h gets the usage and exit code 0.
func TestUsageInATerminal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--config", "/tmp/x/Application", "Support/config.json"}, 2, `the argument "Support/config.json" is no flag`},
		{[]string{"--max-calls", "17"}, 2, "takes a number from 1 to 16"},
		{[]string{"--max-calls", "0"}, 2, "takes a number from 1 to 16"},
		{[]string{"--call-timeout", "4s"}, 2, "takes a duration from 5s to 240s"},
		{[]string{"--call-timeout", "241s"}, 2, "takes a duration from 5s to 240s"},
		{[]string{"--log-level", "loud"}, 2, "takes debug, info, warn or error"},
		{[]string{"--secret", "x"}, 2, "flag provided but not defined"},
		{[]string{"-h"}, 0, ""},
	} {
		var stdout, stderr strings.Builder
		if code := run(tc.args, &stdout, &stderr, func() bool { return true }); code != tc.code {
			t.Errorf("%q: exit code %d, want %d", tc.args, code, tc.code)
		}
		if !strings.Contains(stderr.String(), tc.want) || !strings.Contains(stderr.String(), usage) {
			t.Errorf("%q printed\n%s", tc.args, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%q printed on stdout: %s", tc.args, stdout.String())
		}
	}
}

// When a client starts the server, what it cannot use of the command line
// becomes problems, each named by the flag when it is one of the server's,
// and the rest is read: a path with a space that was not quoted, a value
// out of range, a flag the server does not take and one written wrong.
func TestClientArguments(t *testing.T) {
	t.Parallel()
	var stderr strings.Builder
	o, err := parseArgs([]string{
		"--config", "/x/Application", "Support/config.json", "--max-calls", "99", "--bogus",
		"---x", "--log-level", "debug", "--call-timeout=1m", "two", "--max-calls",
	}, &stderr, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("parseArgs printed %s", stderr.String())
	}
	want := []config.Problem{
		{Code: mcpserver.ProblemStrayArgument, Setting: "arguments"},
		{Code: mcpserver.ProblemBadFlag, Setting: "--max-calls"},
		{Code: mcpserver.ProblemBadFlag, Setting: "arguments"},
	}
	if !slices.Equal(o.problems, want) {
		t.Errorf("problems %v, want %v", o.problems, want)
	}
	if o.config != "/x/Application" || o.maxCalls != 4 || o.callTimeout != time.Minute || o.logLevel.String() != "DEBUG" {
		t.Errorf("read %+v", o)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	o, err = parseArgs([]string{"--config", "~/x/config.json"}, &stderr, func() bool { return false })
	if err != nil || o.config != filepath.Join(home, "x", "config.json") {
		t.Errorf("--config ~/x/config.json is %q, %v", o.config, err)
	}
}

// The README's block of flags lists exactly the command's flags, as the
// usage does, so that neither drifts from the command.
func TestReadmeNamesEveryFlag(t *testing.T) {
	t.Parallel()
	var flags []string
	flagSet(&options{}).VisitAll(func(f *flag.Flag) { flags = append(flags, f.Name) })
	slices.Sort(flags)
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.ReplaceAll(string(b), "\r\n", "\n")
	for name, text := range map[string]string{"README.md": readme, "the usage": usage} {
		var listed []string
		for _, m := range regexp.MustCompile(`(?m)^ *--([a-z-]+)`).FindAllStringSubmatch(text, -1) {
			listed = append(listed, m[1])
		}
		slices.Sort(listed)
		if !slices.Equal(listed, flags) {
			t.Errorf("%s lists the flags %q; the command takes %q", name, listed, flags)
		}
	}
}
