package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The harness (design 15.1): each test starts its own fake Schwab with the
// fixtures' world, logs in to it into a token file of its own, builds a
// server on it and talks to it through the SDK's client over in-memory
// transports, recording every message both ways. Tests run in parallel and
// set no environment variable. env.call checks every answer: its
// structured content against its tool's output schema, its text against
// its structured content, its size against the cap, and that nothing the
// server sent or logged holds a planted account number or a secret.

// testNow is the clock of every test's server and fake, 16:40 in New York
// on a Friday: answers are as of it, and a login made at it lapses 7 days
// later.
var testNow = time.Date(2026, time.November, 13, 21, 40, 0, 0, time.UTC)

// testDir is TestMain's folder, which holds the shared store.
var testDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcpserver-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDir = dir
	code := m.Run()
	// TestServerNeverWritesTheStore's other half: the shared store, which
	// every test reads, holds what it held when it was made.
	if sharedPath != "" && code == 0 {
		if after, err := storeDigest(sharedPath); err != nil {
			fmt.Fprintln(os.Stderr, "TestServerNeverWritesTheStore: digesting the shared store:", err)
			code = 1
		} else if after != sharedDigest {
			fmt.Fprintf(os.Stderr, "TestServerNeverWritesTheStore: the shared store changed while the tests ran:\nbefore %s\nafter  %s\n", sharedDigest, after)
			code = 1
		}
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// The shared store (design 15.1) is made once per test binary, by the first
// test that needs it (sharedStore), so that a run of other tests never
// makes it. It holds the fake's three accounts, their preferences and a
// snapshot of each, and:
//
//   - ...135's fixture transactions of 2023 and 2024, read as one complete
//     read, which gives it api coverage of those two years, not of 2025;
//   - for ...398, a complete read of March to December 2025, whose two
//     rows (sharedRows398) the fixtures do not hold, and a History export
//     of 2025 established complete, which gives it file coverage of the
//     year: its 2025 is whole in the store; two of the export's rows match
//     the read's, and two others inside the read's days, which no API row
//     is, are held out and wait for the user.
//
// Every test reads it through OpenReader, which never writes, and TestMain
// digests it at the end against the digest taken when it was made.
var (
	sharedPath   string
	sharedDigest string
	sharedMade   = sync.OnceValue(makeSharedStore)
)

// sharedStore returns the shared store's path, making the store first if
// no test has.
func sharedStore(t *testing.T) string {
	t.Helper()
	if err := sharedMade(); err != nil {
		t.Fatal("making the shared store:", err)
	}
	return sharedPath
}

// The fixtures' accounts, by number.
const (
	individual = "29470135"
	options    = "61803398"
	empty      = "40221783"
)

// ny returns the instant of y-m-d h:min in New York.
func ny(y int, m time.Month, d, h, minute int) time.Time {
	return time.Date(y, m, d, h, minute, 0, 0, nytime.NewYork())
}

// makeSharedStore makes the shared store in TestMain's folder and digests
// it.
func makeSharedStore() error {
	ctx := context.Background()
	path := filepath.Join(testDir, "shared", "history.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	s, err := history.Open(ctx, history.Options{Path: path, Now: func() time.Time { return testNow }})
	if err != nil {
		return err
	}
	if err := fillSharedStore(ctx, s); err != nil {
		s.Close()
		return err
	}
	if err := s.Close(); err != nil {
		return err
	}
	d, err := storeDigest(path)
	if err != nil {
		return err
	}
	sharedPath, sharedDigest = path, d
	return nil
}

// fillSharedStore records what the shared store holds into s, from the
// fake's fixtures as its testdata folder holds them: TestMain has no
// testing.TB for schwabtest.Fixture, and the fake serves them as they are.
func fillSharedStore(ctx context.Context, s *history.Store) error {
	var numbers []schwab.AccountNumber
	var prefs schwab.UserPreference
	var accounts []schwab.Account
	var rows []schwab.Transaction
	for name, v := range map[string]any{
		"account_numbers.json": &numbers, "user_preference.json": &prefs, "accounts.json": &accounts, "transactions.json": &rows,
	} {
		b, err := os.ReadFile(filepath.Join("..", "schwab", "schwabtest", "testdata", name))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	read := func(at time.Time) history.Read { return history.Read{SentAt: at, AnsweredAt: at.Add(time.Second)} }
	snapshot := ny(2026, time.January, 5, 19, 11)
	if err := s.RecordAccountNumbers(ctx, read(snapshot), numbers); err != nil {
		return err
	}
	if err := s.RecordPreferences(ctx, read(snapshot), prefs); err != nil {
		return err
	}
	if _, err := s.RecordAccounts(ctx, read(snapshot), accounts); err != nil {
		return err
	}
	for _, raw := range sharedRows398 {
		var row schwab.Transaction
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return err
		}
		rows = append(rows, row)
	}
	window := func(number string, from, to time.Time, sent time.Time) history.TransactionsRead {
		r := history.TransactionsRead{Read: read(sent), Account: number, Query: schwab.TransactionQuery{Start: from, End: to}}
		for _, row := range rows {
			if row.AccountNumber == number && !row.Time.Before(from) && row.Time.Before(to) {
				r.Rows = append(r.Rows, row)
			}
		}
		return r
	}
	for _, r := range []history.TransactionsRead{
		window(individual, ny(2023, time.January, 1, 0, 0), ny(2025, time.January, 1, 0, 0), ny(2025, time.January, 2, 9, 0)),
		window(options, ny(2025, time.March, 1, 0, 0), ny(2026, time.January, 1, 0, 0), ny(2026, time.January, 5, 19, 11)),
	} {
		if _, err := s.RecordTransactions(ctx, r); err != nil {
			return err
		}
	}
	csv := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n" +
		`"01/14/2025","Buy","XYZ","XYZ CORP","10","$20.00","$0.65","-$200.65"` + "\n" +
		`"02/14/2025","Qualified Dividend","XYZ","XYZ CORP","","","","$4.00"` + "\n" +
		`"03/27/2025","Cash Dividend","AAPL","APPLE INC","","","","$96.36"` + "\n" +
		`"06/16/2025","Qualified Dividend","XYZ","XYZ CORP","","","","$4.00"` + "\n" +
		`"10/01/2025","Credit Interest","","SCHWAB1 INT 09/01-09/30","","","","$0.07"` + "\n" +
		`"11/20/2025","Buy","XYZ","XYZ CORP","2","$25.00","$0.65","-$50.65"` + "\n"
	_, err := s.Import(ctx, []history.FileInput{{Name: "Options_Margin_XXX398_Transactions_20260105-191200.csv", Content: []byte(csv)}},
		history.ImportOptions{Account: options, From: "2025-01-01", To: "2025-12-31", Completeness: "complete"})
	return err
}

// sharedRows398 are ...398's API rows of 2025 that the shared store holds
// and the fixtures do not: a dividend and a trade, each of which a row of
// the History export matches. A test that reads them from the fake adds
// them to it (schwabtest.Server.AddRows).
var sharedRows398 = []string{
	`{"activityId": 73508262001, "time": "2025-03-27T14:40:03+0000", "description": "Ordinary Dividend~AAPL",
		"accountNumber": "61803398", "type": "DIVIDEND_OR_INTEREST", "status": "VALID", "subAccount": "CASH",
		"tradeDate": "2025-03-27T04:00:00+0000", "settlementDate": "2025-03-27T04:00:00+0000", "netAmount": 96.36,
		"transferItems": [{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD",
		"description": "USD currency", "instrumentId": 1, "closingPrice": 0.0}, "amount": 96.36, "cost": 0.0}]}`,
	`{"activityId": 73508262002, "time": "2025-11-20T15:31:00+0000", "accountNumber": "61803398", "type": "TRADE",
		"status": "VALID", "subAccount": "CASH", "tradeDate": "2025-11-20T05:00:00+0000", "netAmount": -50.65,
		"transferItems": [{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD",
		"description": "USD currency", "instrumentId": 1, "closingPrice": 0.0}, "amount": 0.65, "cost": -0.65,
		"feeType": "COMMISSION"}, {"instrument": {"assetType": "EQUITY", "status": "ACTIVE", "symbol": "XYZ",
		"instrumentId": 7003, "closingPrice": 25.0, "type": "COMMON_STOCK"}, "amount": 2.0, "cost": -50.0,
		"price": 25.0, "positionEffect": "OPENING"}]}`,
}

// storeDigest returns what TestServerNeverWritesTheStore compares of the
// store at path: its main file's SHA-256 and mode, its user_version, and
// every table's rows in their order, read through a connection that takes
// no lock and makes no journal.
func storeDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	var d strings.Builder
	fmt.Fprintf(&d, "%x %v", sha256.Sum256(b), st.Mode().Perm())
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&immutable=1"}).String())
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx := context.Background()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return "", err
	}
	fmt.Fprintf(&d, " user_version %d", version)
	var tables []string
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		tables = append(tables, name)
	}
	rows.Close()
	h := sha256.New()
	for _, tb := range tables {
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+tb+`"`)
		if err != nil {
			return "", err
		}
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return "", err
			}
			fmt.Fprintf(h, "%s %v\n", tb, vals)
		}
		rows.Close()
	}
	fmt.Fprintf(&d, " rows %x", h.Sum(nil))
	return d.String(), nil
}

// An env is one test's fake, server and client.
type env struct {
	t    *testing.T
	fake *schwabtest.Server
	srv  *Server
	cs   *mcp.ClientSession
	// home is the server's home folder, tokenFile the login's file and
	// storePath the store the server reads ("" for none that exists).
	home, tokenFile, storePath string
	// wire holds every message both ways, as mcp.LoggingTransport writes
	// them, and logs the server's log.
	wire, logs *syncBuffer
	// tools are the tools as tools/list served them, and outputs their
	// output schemas, resolved.
	tools   map[string]*mcp.Tool
	outputs map[string]*jsonschema.Resolved
	// planted are the numbers that must never appear in what the server
	// sends or logs: the fixtures' and any that a test plants.
	planted []string
}

// An envConfig is what newEnv's options set.
type envConfig struct {
	protocol  string // the protocol version the client asks for; "" for the newest
	loggedIn  bool
	loginAt   time.Time // when the login was made, by the fake's clock and the Auth's
	store     string    // the store's path; "" for the shared store
	noStore   bool      // a store path where none exists
	fixtures  bool
	home      string
	options   []func(*Options)
	register  []func(*Server)
	fake      []func(*schwabtest.Server)
	cfg       []func(*config.Config)
	hookFails bool   // the backstop's hook fails the test
	tokenFile string // the token file; "" for ~/.config/schwabtx/token.json
	cfgFile   string // the config file; "" for ~/.config/schwabtx/config.json
}

// An envOption sets up newEnv.
type envOption func(*envConfig)

// withProtocol makes the client ask for the protocol version v.
func withProtocol(v string) envOption { return func(c *envConfig) { c.protocol = v } }

// withoutLogin leaves the token file without a login.
func withoutLogin() envOption { return func(c *envConfig) { c.loggedIn = false } }

// withLoginAt makes the login at at, by the fake's clock and the Auth's,
// so that it lapses 7 days later.
func withLoginAt(at time.Time) envOption { return func(c *envConfig) { c.loginAt = at } }

// withStore makes the server read the store at path.
func withStore(path string) envOption { return func(c *envConfig) { c.store = path } }

// withoutStore names a store where there is none.
func withoutStore() envOption { return func(c *envConfig) { c.noStore = true } }

// withoutFixtures leaves the fake's world empty.
func withoutFixtures() envOption { return func(c *envConfig) { c.fixtures = false } }

// withHome makes the server's home folder dir.
func withHome(dir string) envOption { return func(c *envConfig) { c.home = dir } }

// withTokenFile keeps the login in path.
func withTokenFile(path string) envOption { return func(c *envConfig) { c.tokenFile = path } }

// withConfigFile names path as the config file the server read.
func withConfigFile(path string) envOption { return func(c *envConfig) { c.cfgFile = path } }

// withOptions changes the server's options before New.
func withOptions(f func(*Options)) envOption {
	return func(c *envConfig) { c.options = append(c.options, f) }
}

// withConfig changes the server's config before New.
func withConfig(f func(*config.Config)) envOption {
	return func(c *envConfig) { c.cfg = append(c.cfg, f) }
}

// withTools registers test-only tools, resources or prompts on the server,
// after New and before the client connects.
func withTools(f func(*Server)) envOption {
	return func(c *envConfig) { c.register = append(c.register, f) }
}

// withFake sets the fake up, after the fixtures load and the login.
func withFake(f func(*schwabtest.Server)) envOption {
	return func(c *envConfig) { c.fake = append(c.fake, f) }
}

// withBackstopAllowed lets the backstop find values without failing the
// test, for the tests that plant a leak on purpose.
func withBackstopAllowed() envOption { return func(c *envConfig) { c.hookFails = false } }

// newEnv starts a fake with the fixtures' world, logs in to it, builds a
// server that reads the shared store, and connects a client, all of which
// the end of the test closes.
func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	cfg := envConfig{loggedIn: true, loginAt: testNow, fixtures: true, hookFails: true}
	for _, o := range opts {
		o(&cfg)
	}
	e := &env{t: t, wire: &syncBuffer{}, logs: &syncBuffer{}}
	for _, a := range schwabtest.FixtureAccounts {
		e.planted = append(e.planted, a.Number)
	}
	e.home = cfg.home
	if e.home == "" {
		e.home = t.TempDir()
	}
	e.fake = schwabtest.NewTLS(t)
	clock := &fakeClock{now: cfg.loginAt}
	e.fake.SetNow(clock.Now)
	if cfg.fixtures {
		e.fake.LoadFixtures()
	}
	e.tokenFile = cfg.tokenFile
	if e.tokenFile == "" {
		e.tokenFile = filepath.Join(e.home, ".config", "schwabtx", "token.json")
	}
	if cfg.loggedIn {
		auth, err := schwab.NewAuth(schwab.Config{
			AppKey: schwabtest.AppKey, AppSecret: schwabtest.AppSecret, BaseURL: e.fake.URL(),
			TokenFile: e.tokenFile, HTTPClient: e.fake.Client(), Now: clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.Login(t.Context(), schwabtest.Code); err != nil {
			t.Fatal("logging in to the fake:", err)
		}
	}
	clock.set(testNow)
	for _, f := range cfg.fake {
		f(e.fake)
	}
	switch {
	case cfg.noStore:
		e.storePath = filepath.Join(e.home, ".config", "schwabtx", "history.db")
	case cfg.store != "":
		e.storePath = cfg.store
	default:
		e.storePath = sharedStore(t)
	}
	cfgFile := cfg.cfgFile
	if cfgFile == "" {
		cfgFile = filepath.Join(e.home, ".config", "schwabtx", "config.json")
	}
	conf := config.Config{
		File:      cfgFile,
		AppKey:    config.Setting{Value: schwabtest.AppKey, From: config.FromFile},
		AppSecret: config.Setting{Value: schwabtest.AppSecret, From: config.FromFile},
		BaseURL:   config.Setting{Value: e.fake.URL(), From: config.FromFile},
		TokenFile: config.Setting{Value: e.tokenFile, From: config.FromFile},
		StoreFile: config.Setting{Value: e.storePath, From: config.FromFile},
		RateFile:  config.Setting{Value: filepath.Join(e.home, ".cache", "schwabtx", "rate"), From: config.FromFile},
	}
	for _, f := range cfg.cfg {
		f(&conf)
	}
	o := Options{
		Config:      conf,
		Version:     "test",
		Logger:      slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		HTTPClient:  e.fake.Client(),
		Client:      schwab.ClientConfig{RatePerMinute: 60_000, Burst: 1000, Tries: 2, Backoff: time.Millisecond},
		Now:         func() time.Time { return testNow },
		Home:        e.home,
		CallTimeout: 3 * time.Second,
	}
	if cfg.hookFails {
		o.onBackstop = func(tool string, n int) { t.Errorf("the backstop replaced %d values in %s", n, tool) }
	}
	for _, f := range cfg.options {
		f(&o)
	}
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	for _, f := range cfg.register {
		f(srv)
	}
	e.connect(cfg.protocol)
	return e
}

// connect connects a client to the server over in-memory transports,
// recording every message, and reads the tools it serves. The end of the
// test closes both sessions and the server.
func (e *env) connect(protocol string) {
	t := e.t
	t.Helper()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := e.srv.MCP().Connect(t.Context(), serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.LoggingTransport{Transport: clientT, Writer: e.wire},
		&mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	e.cs = cs
	t.Cleanup(func() {
		cs.Close()
		ss.Close()
		if err := e.srv.Close(); err != nil {
			t.Error("closing the server:", err)
		}
	})
	listed, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.tools, e.outputs = map[string]*mcp.Tool{}, map[string]*jsonschema.Resolved{}
	for _, tool := range listed.Tools {
		e.tools[tool.Name] = tool
		if tool.OutputSchema == nil {
			continue
		}
		b, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("%s's output schema: %v", tool.Name, err)
		}
		r, err := s.Resolve(nil)
		if err != nil {
			t.Fatalf("resolving %s's output schema: %v", tool.Name, err)
		}
		e.outputs[tool.Name] = r
	}
}

// A result is a tool's answer, as the client got it.
type result struct {
	// text is the one text block, decoded; isError says it is an error.
	text    string
	isError bool
	// raw is the structured content as the wire carried it, and value as
	// decoded; nil for an error.
	raw   json.RawMessage
	value map[string]any
}

// code returns the error's code, the word in brackets that starts it.
func (r result) code() string {
	if m := regexp.MustCompile(`^\[([a-z_]+)\] `).FindStringSubmatch(r.text); m != nil {
		return m[1]
	}
	return ""
}

// warnings returns the codes of the answer's warnings.
func (r result) warnings() []string {
	var codes []string
	ws, _ := r.value["warnings"].([]any)
	for _, w := range ws {
		if m, ok := w.(map[string]any); ok {
			codes = append(codes, fmt.Sprint(m["code"]))
		}
	}
	return codes
}

// call calls tool with args, a JSON object as a string, json.RawMessage
// or a value to encode, and checks the answer as env.check does.
func (e *env) call(tool string, args any) result {
	e.t.Helper()
	return e.callContext(e.t.Context(), tool, args)
}

// callContext is call with ctx.
func (e *env) callContext(ctx context.Context, tool string, args any) result {
	e.t.Helper()
	params := &mcp.CallToolParams{Name: tool}
	switch a := args.(type) {
	case nil:
	case string:
		params.Arguments = json.RawMessage(a)
	default:
		params.Arguments = a
	}
	res, err := e.cs.CallTool(ctx, params)
	if err != nil {
		e.t.Fatalf("calling %s: %v", tool, err)
	}
	return e.check(tool, res)
}

// check checks one answer of tool: one text block; for an error, a code in
// brackets and no structured content; for a success, structured content
// that validates against the tool's output schema, whose bytes as the wire
// carried them are the text's as decoded, after json.HTMLEscape (design
// 8.2: the SDK escapes HTML in the structured content alone), and within
// the cap; and nothing the server sent or logged holds a planted number or
// a secret.
func (e *env) check(tool string, res *mcp.CallToolResult) result {
	t := e.t
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("%s answered %d content blocks, want 1", tool, len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s answered a %T, want text", tool, res.Content[0])
	}
	r := result{text: tc.Text, isError: res.IsError}
	defer e.assertClean()
	if r.isError {
		if r.code() == "" {
			t.Errorf("%s's error has no code: %s", tool, r.text)
		}
		if res.StructuredContent != nil {
			t.Errorf("%s's error has structured content", tool)
		}
		return r
	}
	r.raw = e.structuredOf(tc.Text)
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, []byte(tc.Text))
	if !bytes.Equal(escaped.Bytes(), r.raw) {
		t.Errorf("%s's text is not its structured content:\ntext       %s\nstructured %s", tool, escaped.Bytes(), r.raw)
	}
	if len(tc.Text) > e.srv.o.MaxBytes {
		t.Errorf("%s answered %d bytes, over the cap of %d", tool, len(tc.Text), e.srv.o.MaxBytes)
	}
	var v any
	if err := json.Unmarshal(r.raw, &v); err != nil {
		t.Fatalf("%s's structured content: %v", tool, err)
	}
	r.value, _ = v.(map[string]any)
	if out, ok := e.outputs[tool]; ok {
		if err := out.Validate(v); err != nil {
			t.Errorf("%s's answer fails its output schema: %v", tool, err)
		}
	} else {
		t.Errorf("%s serves no output schema", tool)
	}
	return r
}

// structuredOf returns the structured content of the answer whose text is
// text, as the wire carried it: the bytes in the recorded message, which
// the client decodes into a map.
func (e *env) structuredOf(text string) json.RawMessage {
	e.t.Helper()
	for _, line := range e.wire.lines() {
		msg, ok := strings.CutPrefix(line, "read: ")
		if !ok {
			continue
		}
		var m struct {
			Result *struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				StructuredContent json.RawMessage `json:"structuredContent"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(msg), &m) != nil || m.Result == nil || len(m.Result.Content) != 1 {
			continue
		}
		if m.Result.Content[0].Text == text && m.Result.StructuredContent != nil {
			return m.Result.StructuredContent
		}
	}
	e.t.Fatalf("no recorded message carries the answer %.200s", text)
	return nil
}

// sent returns every message the server sent, as recorded, and every line
// it logged: what assertClean scans. The client's own requests hold what
// the test sent, and are left out.
func (e *env) sent() []string {
	var out []string
	for _, line := range e.wire.lines() {
		if msg, ok := strings.CutPrefix(line, "read: "); ok {
			out = append(out, msg)
		}
	}
	return append(out, e.logs.lines()...)
}

// assertClean fails the test if a message the server sent or a line it
// logged holds a planted account number, plainly or written as mask.Text
// reads one (groups joined by up to three spaces or a dash), at digit
// boundaries outside a run of 32 or more hex digits, which is an account
// hash; or the app key, the app secret, the login code or a token that the
// fake issued.
func (e *env) assertClean() {
	t := e.t
	t.Helper()
	secrets := append([]string{schwabtest.AppKey}, e.fake.Secrets()...)
	numbers := make([]*regexp.Regexp, len(e.planted))
	for i, n := range e.planted {
		numbers[i] = plantedPattern(n)
	}
	for _, line := range e.sent() {
		for _, s := range secrets {
			if s != "" && strings.Contains(line, s) {
				t.Errorf("the server sent or logged a secret of the fake's: %.300s", line)
			}
		}
		outside := hashRun.ReplaceAllString(line, "#")
		for i, re := range numbers {
			if re.MatchString(outside) {
				t.Errorf("the server sent or logged the planted number %s: %.300s", e.planted[i], line)
			}
		}
	}
}

// hashRun is a run of 32 or more hex digits, an account hash.
var hashRun = regexp.MustCompile(`[0-9A-Fa-f]{32,}`)

// plantedPattern matches n at digit boundaries, written plainly or with
// up to three spaces, or a dash with a space or none on each side, between
// any two of its digits.
func plantedPattern(n string) *regexp.Regexp {
	digits := strings.Split(n, "")
	return regexp.MustCompile(`(?:^|[^0-9])` + strings.Join(digits, `(?:[ \t]{1,3}| ?[-\x{2013}\x{2014}] ?)?`) + `(?:[^0-9]|$)`)
}

// plant adds numbers to those that must never appear.
func (e *env) plant(numbers ...string) { e.planted = append(e.planted, numbers...) }

// readResource reads the resource uri.
func (e *env) readResource(uri string) (*mcp.ReadResourceResult, error) {
	e.t.Helper()
	defer e.assertClean()
	return e.cs.ReadResource(e.t.Context(), &mcp.ReadResourceParams{URI: uri})
}

// A syncBuffer is a buffer that goroutines may write and read at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lines returns the buffer's lines.
func (s *syncBuffer) lines() []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s.String()))
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// A fakeClock is a clock that a test sets.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// testTool registers a test-only tool through addTool: a read-only tool
// that takes In and answers Out, with a slot, and the arguments' rules.
func testTool[In, Out any](s *Server, name string, args map[string]argRule, h handler[In, Out]) {
	addTool(s, toolDef{name: name, title: "A test's tool", description: "A tool of the tests'.", slot: true, args: args}, h)
}

// callParams returns the parameters of a call of tool with no arguments.
func callParams(tool string) *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(`{}`)}
}
