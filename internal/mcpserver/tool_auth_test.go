package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// auth_status's cases: empty is a server with no config, no login and no
// store; full is logged in, with the shared store, two of whose rows wait.
var _ = addToolCases(
	toolCase{tool: "auth_status", name: "empty", args: `{}`, opts: []envOption{withoutLogin(), withoutStore(), withoutFixtures(), withConfig(func(c *config.Config) {
		c.File = ""
		c.AppKey, c.AppSecret = config.Setting{}, config.Setting{}
		c.BaseURL = config.Setting{Value: schwab.DefaultBaseURL, From: config.FromDefault}
		c.TokenFile.From, c.StoreFile.From = config.FromDefault, config.FromDefault
		c.RateFile = config.Setting{From: config.FromDefault}
	})}, check: func(t *testing.T, _ *env, r result) {
		if r.value["logged_in"] != false || r.value["app_configured"] != false || !strings.Contains(r.text, "no Schwab app key and secret") {
			t.Errorf("an empty server: %s", r.text)
		}
	}},
	toolCase{tool: "auth_status", name: "full", args: `{}`, check: func(t *testing.T, e *env, r result) {
		if r.value["logged_in"] != true || r.value["action"] != nil || r.value["hours_left"] != 168.0 {
			t.Errorf("a server logged in: %s", r.text)
		}
		if store, _ := r.value["store"].(map[string]any); store["usable"] != true || store["waiting"] != 2.0 {
			t.Errorf("the shared store: %v", r.value["store"])
		}
		if n := e.fake.APICalls() + e.fake.TokenRequests(); n != 1 { // the login's own
			t.Errorf("auth_status sent %d requests to Schwab", n-1)
		}
	}},
	toolCase{tool: "auth_status", name: "expires_soon", args: `{}`, opts: []envOption{withLoginAt(testNow.Add(-6 * 24 * time.Hour))}},
	toolCase{
		tool: "auth_status", name: "not_logged_in", args: `{}`, opts: []envOption{withoutLogin()},
		check: func(t *testing.T, _ *env, r result) {
			if action, _ := r.value["action"].(string); !strings.Contains(action, "run `schwabtx setup` there") {
				t.Errorf("the advice lacks the drift hint: %s", action)
			}
		},
	},
	toolCase{tool: "auth_status", name: "config_problems", args: `{}`, opts: []envOption{withConfig(func(c *config.Config) {
		c.Problems = append(c.Problems,
			config.Problem{Code: config.CodeMadePrivate, Setting: "config_file", Path: c.File},
			config.Problem{Code: config.CodeRelativePath, Setting: "SCHWAB_STORE_FILE", Path: "history.db"},
			config.Problem{Code: ProblemStrayArgument, Setting: "arguments"})
	})}},
)

// The world of TestNoAccountNumberLeaks that the core makes: a home
// folder whose name holds a planted number (the world's own), a token file
// and a config file's folder that hold one, and a store in a folder that
// holds one, each of which auth_status shows masked, the token file and
// the store readable by others, so that their warnings, which quote their
// paths, come before the server has read the accounts; two more accounts,
// of 9 digits and of 7, which only the known numbers catch in free text;
// and nicknames that hold numbers, the 7-digit one in groups too. And the
// worlds beside it (coreSides).
var _ = addLeaks([]func(t *testing.T, w *leakWorld){func(t *testing.T, w *leakWorld) {
	w.plant(nineDigits, sevenDigits, unknownLong, unknownShort)
	w.base = leakBase
	w.store = filepath.Join(w.home, "accounts-"+options, "history.db")
	w.opts = append(w.opts, leakBase(t, w.home)...)
	w.masked = append(w.masked, "~/accounts-...398/history.db", "~/.config/schwabtx/token-...135.json", "~/.config-...398/schwabtx/config.json",
		`"nickname":"Savings ...321"`)
	// The store's text is read only from a store that opens: one that does
	// not would leave the store's side of the test with nothing to find.
	w.checks = append(w.checks, func(t *testing.T, e *env) {
		if store := storeOf(e.call("auth_status", `{}`)); store["usable"] != true {
			t.Errorf("the leak world's store is not usable: %v", store)
		}
	})
	w.sides = append(w.sides, coreSides()...)
}}, leakCall{tool: "auth_status", args: `{}`})

// leakBase returns the options of a leak world whose home folder is home:
// the token file, named for a planted number and readable by others once
// the login has saved it; the config file in a folder that holds one; a
// copy of the shared store in a folder that holds one, readable by others;
// the two extra accounts; and nicknames that hold numbers, one of them the
// 7-digit account's own number in two groups, which mask.Text leaves.
func leakBase(t *testing.T, home string) []envOption {
	token := filepath.Join(home, ".config", "schwabtx", "token-"+individual+".json")
	store := filepath.Join(home, "accounts-"+options, "history.db")
	copyFile(t, sharedStore(t), store)
	loosen(t, store)
	return []envOption{
		withFake(func(f *schwabtest.Server) {
			for _, n := range []string{nineDigits, sevenDigits} {
				f.AddAccount(schwabtest.Account{Number: n, Details: `{"securitiesAccount":{"type":"CASH","accountNumber":"` + n +
					`","currentBalances":{"liquidationValue":10.0,"cashBalance":10.0}}}`})
			}
			f.SetUserPreference(`{"accounts":[{"accountNumber":"29470135","nickName":"Joint 2947-0135","primaryAccount":true},` +
				`{"accountNumber":"61803398","nickName":"Margin 61803398"},{"accountNumber":"` + nineDigits + `","nickName":"Kids ` + sevenDigits + `"},` +
				`{"accountNumber":"` + sevenDigits + `","nickName":"Savings 765 4321"}]}`)
			loosen(t, token) // after the login, which saved it private
		}),
		withStore(store),
		withTokenFile(token),
		withConfigFile(filepath.Join(home, ".config-"+options, "schwabtx", "config.json")),
	}
}

// The numbers that the leak worlds plant beyond the fixtures' and the
// extra accounts': no account the server knows has them, so only the
// pattern masks them, in Schwab's bodies and in paths.
const (
	unknownLong  = "27182818284"
	unknownShort = "31415926"
)

// loosen makes the file at path, if there is one, readable by others, on
// the systems where the server makes such a file private or warns of it.
func loosen(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Chmod(path, 0o644); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// coreSides are the core's worlds beside the leak world, each with paths
// that hold planted numbers, its token file and store readable by others:
// a server without its app key; one without a login, at a token file whose
// name holds a number; one whose login has lapsed; one whose app Schwab
// refuses as it refreshes the login; one whose config has problems with
// such paths, run through Run so that the start line is scanned, with a
// request whose params do not decode, and then auth_status first; one
// whose store is missing, then damaged, newer,
// older and kept locked; and one whose every call slot is taken by a call
// that waits on Schwab until its time runs out.
func coreSides() []leakSide {
	balances := leakCall{tool: "get_balances", args: `{}`}
	storeCall := func(e *env) result {
		return e.call("get_transactions", `{"source":"store","account":"135","from":"2025-01-01","to":"2025-12-31"}`)
	}
	waiting := make(chan struct{}, 1) // a request for the accounts waits on the fake
	return []leakSide{
		{
			name: "no app key",
			opts: func(string) []envOption {
				return []envOption{withConfig(func(c *config.Config) { c.AppKey = config.Setting{} })}
			},
			calls: []leakCall{balances, {tool: "auth_status", args: `{}`}},
		},
		{
			name: "no login",
			opts: func(home string) []envOption {
				return []envOption{withoutLogin(), withTokenFile(filepath.Join(home, ".config", "schwabtx", "token-"+individual+"-none.json"))}
			},
			calls: []leakCall{balances, {tool: "list_accounts", args: `{}`}},
		},
		{
			name:  "a lapsed login",
			opts:  func(string) []envOption { return []envOption{withLoginAt(testNow.Add(-8 * 24 * time.Hour))} },
			calls: []leakCall{balances},
		},
		{
			name: "the app refused",
			opts: func(string) []envOption {
				return []envOption{withLoginAt(testNow.Add(-time.Hour)), withFake(func(f *schwabtest.Server) {
					f.FailTokens(schwabtest.Failure{Status: http.StatusUnauthorized, Body: `{"error":"invalid_client","error_description":"Unauthorized"}`})
				})}
			},
			calls: []leakCall{balances},
		},
		{
			name: "config problems",
			opts: func(string) []envOption {
				return []envOption{withConfig(func(c *config.Config) {
					c.Problems = append(c.Problems,
						config.Problem{Code: config.CodeUnreadable, Setting: "config_file", Path: c.File},
						config.Problem{Code: config.CodeRelativePath, Setting: "SCHWAB_STORE_FILE", Path: filepath.Join("acct-"+unknownShort, "history.db")},
						config.Problem{Code: config.CodeMadePrivate, Setting: "config_file", Path: c.File})
				})}
			},
			run: func(_ *testing.T, e *env) []result {
				// Over stdio, a request whose params do not decode, holding a
				// number that no account has, which the SDK's error would quote.
				e.runOnce(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},`+
					`"clientInfo":{"name":"test","version":"1"}}}`,
					`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
					`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"portfolio_review","arguments":{"account":`+unknownLong+`}}}`)
				return []result{e.call("auth_status", `{}`), e.call("get_balances", `{}`)}
			},
		},
		{name: "a broken store", opts: func(home string) []envOption {
			return []envOption{withStore(filepath.Join(home, "accounts-"+options, "broken-"+unknownShort, "history.db")), withOptions(func(o *Options) {
				o.StoreLockWait, o.StoreBusy = 200*time.Millisecond, 50*time.Millisecond
			})}
		}, run: func(t *testing.T, e *env) []result {
			broken := e.storePath
			rs := []result{storeCall(e)} // none yet
			if err := os.MkdirAll(filepath.Dir(broken), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(broken, []byte("not a store, of "+unknownLong), 0o600); err != nil {
				t.Fatal(err)
			}
			rs = append(rs, storeCall(e))
			for _, version := range []string{"2", "0"} { // newer, then older
				copyFile(t, sharedStore(t), broken)
				db := rawStore(t, broken)
				exec(t, db, "PRAGMA user_version = "+version)
				db.Close()
				rs = append(rs, storeCall(e))
			}
			copyFile(t, sharedStore(t), broken)
			db := rawStore(t, broken)
			exec(t, db, "PRAGMA journal_mode = WAL", "PRAGMA locking_mode = EXCLUSIVE", "BEGIN IMMEDIATE", "UPDATE store SET derived_with = derived_with")
			rs = append(rs, storeCall(e))
			exec(t, db, "ROLLBACK")
			return rs
		}},
		{name: "every slot taken", opts: func(string) []envOption {
			return []envOption{withOptions(func(o *Options) {
				o.MaxCalls, o.SlotWait, o.CallTimeout = 1, 50*time.Millisecond, time.Second
			}), withFake(func(f *schwabtest.Server) {
				f.Override(schwabtest.AccountsRoute, func(_ http.ResponseWriter, r *http.Request) {
					waiting <- struct{}{}
					<-r.Context().Done()
				})
			})}
		}, run: func(t *testing.T, e *env) []result {
			type answer struct {
				res *mcp.CallToolResult
				err error
			}
			answered := make(chan answer, 1)
			go func() {
				res, err := e.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_balances", Arguments: json.RawMessage(`{}`)})
				answered <- answer{res, err}
			}()
			select {
			case <-waiting:
			case <-time.After(time.Minute):
				t.Fatal("the call did not reach Schwab's accounts")
			}
			busy := e.call("get_positions", `{}`)
			a := <-answered
			if a.err != nil {
				t.Fatal(a.err)
			}
			return []result{busy, e.check("get_balances", a.res)}
		}},
	}
}

// The leak world's accounts beyond the fixtures'.
const (
	nineDigits  = "123456789"
	sevenDigits = "7654321"
)

// copyFile copies the file at from to to, making to's folder.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeCopy returns a copy of the shared store, in a folder of the test's
// own.
func storeCopy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	copyFile(t, sharedStore(t), path)
	return path
}

// rawStore opens the store at path as another program would, with a
// connection that writes without the store's checks, and closes it when
// the test ends.
func rawStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// exec runs each statement on db, failing the test on an error.
func exec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// storeOf returns auth_status's store field of r.
func storeOf(r result) map[string]any {
	s, _ := r.value["store"].(map[string]any)
	return s
}

// auth_status reports each state of the store (design 15.7), each made on a
// small store of the test's own with a raw connection: missing, newer,
// older, of newer rules only, not in WAL mode, kept locked by another
// process in exclusive locking mode, and changed by a newer schwabtx while
// the server runs, which the next call reports, and the one after it opens
// again.
func TestStoreStates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		make    func(t *testing.T, path string)
		problem any
		usable  bool
		warning string
		action  string
	}{
		{"missing", nil, "no_store", false, "", ""},
		{
			"newer", func(t *testing.T, path string) { exec(t, rawStore(t, path), "PRAGMA user_version = 2") },
			"store_newer", false, "", "update schwab-mcp",
		},
		{
			"older", func(t *testing.T, path string) { exec(t, rawStore(t, path), "PRAGMA user_version = 0") },
			"store_needs_update", false, "", "`schwabtx status`",
		},
		{
			"newer rules", func(t *testing.T, path string) { exec(t, rawStore(t, path), "UPDATE store SET derived_with = 2") },
			nil, true, "store_newer_rules", "",
		},
		{
			"not WAL", func(t *testing.T, path string) { exec(t, rawStore(t, path), "PRAGMA journal_mode = DELETE") },
			"store_needs_update", false, "", "`schwabtx status`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := []envOption{withoutStore()}
			if tc.make != nil {
				path := storeCopy(t)
				tc.make(t, path)
				opts = []envOption{withStore(path)}
			}
			e := newEnv(t, opts...)
			r := e.call("auth_status", `{}`)
			store := storeOf(r)
			if store["problem"] != tc.problem || store["usable"] != tc.usable {
				t.Errorf("store %v, want problem %v and usable %v", store, tc.problem, tc.usable)
			}
			if tc.warning != "" && !slices.Contains(r.warnings(), tc.warning) {
				t.Errorf("warnings %v, want %s", r.warnings(), tc.warning)
			}
			if action, _ := r.value["action"].(string); tc.action != "" && !strings.Contains(action, tc.action) {
				t.Errorf("action %q, want it to say %s", action, tc.action)
			}
		})
	}

	t.Run("busy", func(t *testing.T) {
		t.Parallel()
		path := storeCopy(t)
		db := rawStore(t, path)
		exec(t, db, "PRAGMA journal_mode = WAL", "PRAGMA locking_mode = EXCLUSIVE", "BEGIN IMMEDIATE", "UPDATE store SET derived_with = derived_with")
		e := newEnv(t, withStore(path), withOptions(func(o *Options) {
			o.StoreLockWait, o.StoreBusy = 200*time.Millisecond, 50*time.Millisecond
		}))
		start := time.Now()
		if r := e.call("auth_status", `{}`); storeOf(r)["problem"] != "store_busy" {
			t.Errorf("a store kept locked: %v", storeOf(r))
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("the call took %v", took)
		}
		exec(t, db, "ROLLBACK")
	})

	t.Run("changed while running", func(t *testing.T) {
		t.Parallel()
		path := storeCopy(t)
		e := newEnv(t, withStore(path))
		if r := e.call("auth_status", `{}`); storeOf(r)["usable"] != true {
			t.Fatalf("before: %v", storeOf(r))
		}
		exec(t, rawStore(t, path), "PRAGMA user_version = 2")
		for _, when := range []string{"the next call", "the call after it"} {
			if r := e.call("auth_status", `{}`); storeOf(r)["problem"] != "store_newer" {
				t.Errorf("%s: %v", when, storeOf(r))
			}
		}
	})
}

// The server never writes the store: on a copy of the shared store, its
// main file, mode, user_version and every table are the same after the
// server read it, through auth_status, and closed it. TestMain holds the
// same of the shared store over every test of the binary.
func TestServerNeverWritesTheStore(t *testing.T) {
	t.Parallel()
	path := storeCopy(t)
	before, err := storeDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, withStore(path))
	for range 2 {
		if r := e.call("auth_status", `{}`); storeOf(r)["usable"] != true {
			t.Fatalf("the store: %v", storeOf(r))
		}
	}
	if err := e.srv.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := storeDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("the store changed:\nbefore %s\nafter  %s", before, after)
	}
	for _, j := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + j); err == nil {
			t.Errorf("the reader left %s behind", j)
		}
	}
}

// auth_status sends nothing to Schwab and takes no slot: it answers while
// every slot is taken.
func TestAuthStatusTakesNoSlot(t *testing.T) {
	t.Parallel()
	b := newBlocker()
	e := newEnv(t, withTools(b.register), withOptions(func(o *Options) { o.MaxCalls, o.CallTimeout = 1, 5*time.Second }))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := e.cs.CallTool(context.Background(), callParams("block")); err != nil {
			t.Error(err)
		}
	}()
	<-b.started
	if r := e.call("auth_status", `{}`); r.isError {
		t.Errorf("auth_status while the slot is taken: %s", r.text)
	}
	close(b.release)
	<-done
}

// A read that fails retires the store's reader, which closes once no read
// uses it, so that the next call opens the store again; a read that its
// call's context cut retires nothing.
func TestStoreHandleRetires(t *testing.T) {
	t.Parallel()
	s, err := New(Options{Config: config.Config{StoreFile: config.Setting{Value: storeCopy(t), From: config.FromFile}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.store
	var first *history.Reader
	if err := h.read(t.Context(), func(r *history.Reader) error { first = r; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.read(ctx, func(*history.Reader) error { return ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Errorf("a cut read: %v", err)
	}
	if h.cur == nil || h.cur.r != first {
		t.Fatal("a read that its context cut retired the reader")
	}
	if err := h.read(t.Context(), func(*history.Reader) error { return errors.New("a failed read") }); !errors.Is(err, errStoreRead) {
		t.Errorf("a failed read: %v, want errStoreRead", err)
	}
	if h.cur != nil {
		t.Error("a failed read kept the reader")
	}
	if _, err := first.Accounts(t.Context()); err == nil {
		t.Error("the retired reader is still open")
	}
	var second *history.Reader
	if err := h.read(t.Context(), func(r *history.Reader) error { second = r; return nil }); err != nil || second == first {
		t.Errorf("the next read did not open the store again: %v", err)
	}
}

// Check prints auth_status's answer for a person: the config file, where
// the app's settings came from, the API's host as not Schwab's, each file
// and where it came from, the store, the login in New York time, the
// config's problems and what the model is told; and no secret.
func TestCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts []envOption
		want []string
	}{
		{"full", nil, []string{
			"Reading ~/.config/schwabtx/config.json:\n",
			"\n  app key      set (config file)\n",
			"\n  API          127.0.0.1:PORT, which is not Schwab's\n",
			"\n  token file   ~/.config/schwabtx/token.json (config file)\n",
			"\n  rate file    ~/.cache/schwabtx/rate (config file)\n",
			"\n  store        history.db (config file), usable; 2 waiting for you in schwabtx status\n",
			"\n  login        made 2026-11-13 16:40 ET, lapses 2026-11-20 16:40 ET, in 168 hours\n",
			"\nNothing to do: the server can read Schwab.\n",
		}},
		{"not logged in", []envOption{withoutLogin(), withoutStore(), withConfig(func(c *config.Config) {
			c.Problems = append(c.Problems, config.Problem{Code: ProblemStrayArgument, Setting: "arguments"})
		})}, []string{
			"\n  store        ~/.config/schwabtx/history.db (config file), no_store\n",
			"\n  login        none\n",
			"\n  problem      arguments: the server was started with an argument it does not take; quote a path that holds a space\n",
			"\nThe model is told: The server was started with an argument it does not take;",
		}},
		{"no config", []envOption{withoutLogin(), withConfig(func(c *config.Config) {
			c.File = ""
			c.AppKey, c.AppSecret = config.Setting{}, config.Setting{}
			c.BaseURL = config.Setting{Value: schwab.DefaultBaseURL, From: config.FromDefault}
		})}, []string{
			"Reading no config file:\n",
			"\n  app key      not set\n",
			"\n  API          api.schwabapi.com\n",
			"\nThe model is told: The server has no Schwab app key and secret.",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, tc.opts...)
			var b strings.Builder
			before := e.fake.AllRequests()
			if err := e.srv.Check(t.Context(), &b); err != nil {
				t.Fatal(err)
			}
			got := strings.ReplaceAll(b.String(), e.fake.Addr(), "127.0.0.1:PORT")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("--check printed\n%s\nwhich lacks %q", got, want)
				}
			}
			for _, secret := range append([]string{schwabtest.AppKey}, e.fake.Secrets()...) {
				if strings.Contains(got, secret) {
					t.Errorf("--check printed a secret:\n%s", got)
				}
			}
			if n := e.fake.AllRequests() - before; n != 0 {
				t.Errorf("--check sent %d requests to Schwab", n)
			}
		})
	}
}
