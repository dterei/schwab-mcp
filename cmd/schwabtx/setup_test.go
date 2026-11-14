package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	configfile "github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// configPath returns where schwabtx setup saves config.json in the test's
// user config directory.
func configPath(t *testing.T) string {
	t.Helper()
	p, err := configfile.DefaultFile()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// readSaved returns what config.json holds.
func readSaved(t *testing.T) configfile.File {
	t.Helper()
	f, err := configfile.Read(configPath(t))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// setup saves the app key and secret from the environment and the paths
// as schwabtx finds them in this shell, made absolute, in a file readable
// only by you, and prints them without the secret and with no more of the
// key than its last three characters. -print prints the same and saves
// nothing.
func TestSetup(t *testing.T) {
	f, tokenFile := setup(t)
	store := newStoreFile(t)
	path := configPath(t)
	rate, err := filepath.Abs(schwab.DefaultRateFile(testKey, tokenFile))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	lines := "  app key      from SCHWAB_APP_KEY (ends in ...key)\n" +
		"  app secret   from SCHWAB_APP_SECRET\n" +
		"  token file   " + tokenFile + " (from SCHWAB_TOKEN_FILE)\n" +
		"  store        " + store + " (from SCHWAB_STORE_FILE)\n" +
		"  rate file    " + rate + " (default)\n" +
		"  api host     " + u.Host + " (from SCHWAB_BASE_URL), not Schwab's\n"

	stdout, stderr, err := runTx(t, "setup", "-print")
	if want := "Would save " + path + ", readable only by you:\n" + lines + "nothing was saved (-print)\n"; err != nil || stdout != want || stderr != "" {
		t.Errorf("setup -print: %v, printed\n%s\nwant\n%s\nwarned %q", err, stdout, want, stderr)
	}
	assertNoFile(t, path)
	// A relative SCHWAB_TOKEN_FILE is the token file of this folder, as the
	// commands take it here.
	t.Setenv("SCHWAB_TOKEN_FILE", "token.json")
	stdout, _, err = runTx(t, "setup", "-print")
	if abs, aerr := filepath.Abs("token.json"); err != nil || aerr != nil || !strings.Contains(stdout, "  token file   "+abs+" (from SCHWAB_TOKEN_FILE)\n") {
		t.Errorf("setup -print with a relative SCHWAB_TOKEN_FILE: %v, printed\n%s", err, stdout)
	}
	assertNoFile(t, path)
	t.Setenv("SCHWAB_TOKEN_FILE", tokenFile)

	saved := "Saved " + path + ", readable only by you:\n" + lines +
		"schwabtx now reads the key and secret from this file when the environment\nhas none, so .env needs them no longer.\n"
	for range 2 { // and again over the file
		stdout, stderr, err = runTx(t, "setup")
		if err != nil || stdout != saved || stderr != "" {
			t.Errorf("setup: %v, printed\n%s\nwant\n%s\nwarned %q", err, stdout, saved, stderr)
		}
	}
	got := readSaved(t)
	if got.AppKey != testKey || got.AppSecret != testSecret || got.BaseURL != f.URL() ||
		got.TokenFile != tokenFile || got.StoreFile != store || got.RateFile != rate {
		t.Errorf("saved %+v", got)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("config.json's mode is %v (%v), want 0600", info.Mode().Perm(), err)
		}
	}

	// Without SCHWAB_BASE_URL, the server goes to Schwab: the file loses
	// the fake's address. A field that setup does not know is kept.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte("{"), []byte(`{"later": [1, 2],`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHWAB_BASE_URL", "")
	t.Setenv("SCHWAB_STORE_FILE", "")
	stdout, _, err = runTx(t, "setup", "-store", store)
	if err != nil || strings.Contains(stdout, "api host") || !strings.Contains(stdout, "  store        "+store+" (from -store)\n") {
		t.Errorf("setup -store without SCHWAB_BASE_URL: %v, printed\n%s", err, stdout)
	}
	if b, err := os.ReadFile(path); err != nil || bytes.Contains(b, []byte("base_url")) || !bytes.Contains(b, []byte(`"later": [`)) {
		t.Errorf("config.json holds\n%s, %v; want no base_url, and the field it did not know", b, err)
	}
}

// Without SCHWAB_APP_KEY or SCHWAB_APP_SECRET, setup keeps what the file
// holds; with a new one, it saves the new one.
func TestSetupKeepsTheSavedApp(t *testing.T) {
	setup(t)
	if _, _, err := runTx(t, "setup"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHWAB_APP_KEY", "")
	t.Setenv("SCHWAB_APP_SECRET", "")
	stdout, _, err := runTx(t, "setup")
	if err != nil || !strings.Contains(stdout, "  app key      as config.json had it (ends in ...key)\n  app secret   as config.json had it\n") {
		t.Errorf("setup without the variables: %v, printed\n%s", err, stdout)
	}
	if got := readSaved(t); got.AppKey != testKey || got.AppSecret != testSecret {
		t.Errorf("saved %+v", got)
	}
	t.Setenv("SCHWAB_APP_KEY", "another-app-key")
	stdout, _, err = runTx(t, "setup")
	if err != nil || !strings.Contains(stdout, "  app key      from SCHWAB_APP_KEY (ends in ...key)\n  app secret   as config.json had it\n") {
		t.Errorf("setup with another key: %v, printed\n%s", err, stdout)
	}
	if got := readSaved(t); got.AppKey != "another-app-key" || got.AppSecret != testSecret {
		t.Errorf("saved %+v", got)
	}
	if strings.Contains(stdout, testSecret) || strings.Contains(stdout, "another-app") {
		t.Errorf("setup printed the secret or the key:\n%s", stdout)
	}

	// From a file that holds a relative path, which the server cannot use,
	// setup keeps the key and secret, and replaces the path.
	t.Setenv("SCHWAB_APP_KEY", "")
	if err := os.WriteFile(configPath(t), []byte(`{"version":1,"app_key":"`+testKey+`","app_secret":"`+testSecret+`","token_file":"token.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runTx(t, "setup"); err != nil {
		t.Fatal(err)
	}
	if got := readSaved(t); got.AppKey != testKey || got.AppSecret != testSecret || !filepath.IsAbs(got.TokenFile) {
		t.Errorf("saved %+v", got)
	}
}

// setup refuses, and saves nothing, without a key and secret, over a file
// it cannot read or that a newer schwabtx wrote, for a store whose files
// would take in the token or rate file or config.json, for a base URL that
// a login refuses, and for an argument it does not take.
func TestSetupRefuses(t *testing.T) {
	_, tokenFile := setup(t)
	path := configPath(t)
	for _, tc := range []struct {
		name string
		env  []string // pairs of a variable and its value
		args []string
		want string // the start of the error
	}{
		{"no key", []string{"SCHWAB_APP_KEY", ""}, nil, "SCHWAB_APP_KEY and SCHWAB_APP_SECRET must be set for schwabtx setup"},
		{"no secret", []string{"SCHWAB_APP_SECRET", " "}, nil, "SCHWAB_APP_KEY and SCHWAB_APP_SECRET must be set for schwabtx setup"},
		{"a store that is the token file", nil, []string{"-store", tokenFile}, "the token file, " + tokenFile + ", is the store at"},
		{"a store that is config.json", nil, []string{"-store", path}, "config.json, " + path + ", is the store at"},
		{"plain http to another computer", []string{"SCHWAB_BASE_URL", "http://example.com"}, nil, `the base URL "http://example.com" is plain http`},
		{"an argument", nil, []string{"now"}, `unexpected argument "now"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i+1 < len(tc.env); i += 2 {
				t.Setenv(tc.env[i], tc.env[i+1])
			}
			stdout, _, err := runTx(t, append([]string{"setup"}, tc.args...)...)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || stdout != "" {
				t.Errorf("%v, printed %q; want an error that starts %q", err, stdout, tc.want)
			}
			assertNoFile(t, path)
		})
	}

	// A file that a newer schwabtx wrote, or that is not one, is left as it
	// is.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{`{"version":2,"app_key":"k"}`, `{"app_key":`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := runTx(t, "setup")
		if err == nil || !strings.HasPrefix(err.Error(), path) {
			t.Errorf("over %s: %v, want an error that names the file", content, err)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != content {
			t.Errorf("over %s: the file holds %s, %v", content, b, err)
		}
	}
}

// -h prints setup's usage, which main takes for no error.
func TestSetupUsage(t *testing.T) {
	setup(t)
	stdout, stderr, err := runTx(t, "setup", "-h")
	if !errors.Is(err, flag.ErrHelp) || stdout != "" || !strings.HasPrefix(stderr, "Usage of schwabtx setup:") ||
		!strings.Contains(stderr, "-print") || !strings.Contains(stderr, "history.db in schwabtx's folder of") {
		t.Errorf("%v, printed %q, warned %q", err, stdout, stderr)
	}
}

// The README's section on schwabtx setup lists, in its block of flags,
// exactly the flags that setup -h prints, and quotes the warnings of login
// and of the store commands as they print them.
func TestSetupDocs(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	section := readmeSection(t, strings.ReplaceAll(string(b), "\r\n", "\n"), "schwabtx setup")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"setup", "-h"}, nil, &stdout, &stderr); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("setup -h: %v", err)
	}
	var want, got []string
	for _, m := range regexp.MustCompile(`(?m)^  -([a-z-]+)`).FindAllStringSubmatch(stderr.String(), -1) {
		want = append(want, m[1])
	}
	for _, block := range codeBlocks(section) {
		for _, m := range regexp.MustCompile(`(?m)^-([a-z-]+)`).FindAllStringSubmatch(block, -1) {
			got = append(got, m[1])
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) || len(want) == 0 {
		t.Errorf("the README lists the flags %q of setup, which takes %q", got, want)
	}
	for _, warning := range []string{
		"warning: the MCP server reads its login from ",
		"(config.json); run schwabtx setup in this terminal to point it here",
		"warning: the MCP server reads the store at ",
		"(config.json); run schwabtx setup in this terminal, with the same -store, to point it here",
	} {
		if !strings.Contains(section, warning) {
			t.Errorf("the README's section on setup does not quote %q", warning)
		}
	}
}

// Once setup has saved the key and secret, schwabtx takes them from
// config.json in place of a variable that is not set, one or both; a set
// variable wins. With neither, the error says how to give them.
func TestCredentialsFromConfig(t *testing.T) {
	setupFixtures(t)
	if _, _, err := runTx(t, "setup"); err != nil {
		t.Fatal(err)
	}
	for _, unset := range [][]string{{"SCHWAB_APP_KEY", "SCHWAB_APP_SECRET"}, {"SCHWAB_APP_SECRET"}, {"SCHWAB_APP_KEY"}} {
		for _, k := range unset {
			t.Setenv(k, "")
		}
		if cfg, err := loadConfig(); err != nil || cfg.AppKey != testKey || cfg.AppSecret != testSecret {
			t.Errorf("without %q: %v, the key %q and the secret %q", unset, err, cfg.AppKey, cfg.AppSecret)
		}
		t.Setenv("SCHWAB_APP_KEY", testKey)
		t.Setenv("SCHWAB_APP_SECRET", testSecret)
	}
	t.Setenv("SCHWAB_APP_KEY", "")
	t.Setenv("SCHWAB_APP_SECRET", "")
	if stdout, _, err := runTx(t, "accounts"); err != nil || stdout != accountsOfThree {
		t.Errorf("without the variables, accounts: %v, printed\n%s", err, stdout)
	}
	t.Setenv("SCHWAB_APP_SECRET", "another-secret")
	if cfg, err := loadConfig(); err != nil || cfg.AppKey != testKey || cfg.AppSecret != "another-secret" {
		t.Errorf("with SCHWAB_APP_SECRET set: %v, the key %q and the secret %q; want the variable's secret", err, cfg.AppKey, cfg.AppSecret)
	}
	t.Setenv("SCHWAB_APP_SECRET", "")

	t.Setenv("SCHWAB_APP_KEY", "")
	t.Setenv("SCHWAB_APP_SECRET", "")
	if err := os.WriteFile(configPath(t), []byte(`{"version":1,"app_key":`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := runTx(t, "accounts")
	if want := "SCHWAB_APP_KEY and SCHWAB_APP_SECRET must be set, or saved with schwabtx setup (see README.md); " + configPath(t) +
		" could not be read: "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("with a config.json that is not JSON: %v, want an error that starts %q", err, want)
	}
	if err := os.Remove(configPath(t)); err != nil {
		t.Fatal(err)
	}
	_, _, err = runTx(t, "accounts")
	if want := "SCHWAB_APP_KEY and SCHWAB_APP_SECRET must be set, or saved with schwabtx setup (see README.md)"; err == nil || err.Error() != want {
		t.Errorf("without config.json: %v, want %q", err, want)
	}
}

// The paths that setup saves are those that the commands use in the same
// shell, and those that the MCP server takes from config.json with no
// variable set at all: the token file that login saves to, the store, with
// -store as the store commands take it, and the rate file that the client
// keeps.
func TestSetupPathsAgree(t *testing.T) {
	f, _ := setup(t)
	f.LoadFixtures()
	t.Setenv("SCHWAB_TOKEN_FILE", "")
	t.Setenv("SCHWAB_STORE_FILE", "")
	doLogin(t)
	other := filepath.Join(t.TempDir(), "stores", "history.db")
	for _, tc := range []struct {
		name  string
		store string // -store, or ""
		env   string // SCHWAB_STORE_FILE, or ""
	}{
		{"defaults", "", ""},
		{"-store", other, ""},
		{"a relative -store", filepath.Join("stores", "history.db"), ""},
		{"SCHWAB_STORE_FILE", "", other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCHWAB_STORE_FILE", tc.env)
			args := []string{"setup"}
			if tc.store != "" {
				args = append(args, "-store", tc.store)
			}
			if _, _, err := runTx(t, args...); err != nil {
				t.Fatal(err)
			}
			saved := readSaved(t)

			// The login's token file, which login saved to.
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if want, err := filepath.Abs(cfg.TokenFile); err != nil || saved.TokenFile != want {
				t.Errorf("token file %s, the commands' %s (%v)", saved.TokenFile, want, err)
			}
			if _, err := os.Stat(saved.TokenFile); err != nil {
				t.Errorf("no login at the saved token file: %v", err)
			}
			// The store of the store commands, with the same -store.
			if want, err := storePath(tc.store); err != nil || saved.StoreFile != want || !filepath.IsAbs(saved.StoreFile) {
				t.Errorf("store %s, the store commands' %s (%v)", saved.StoreFile, want, err)
			}
			if tc.store == "" {
				def, err := history.DefaultPath()
				if want, aerr := filepath.Abs(def); err != nil || aerr != nil || saved.StoreFile != want {
					t.Errorf("store %s, history.DefaultPath's %s (%v)", saved.StoreFile, want, err)
				}
			}
			// The client's rate file, which a request makes.
			if err := os.Remove(saved.RateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if _, _, err := runTx(t, "accounts"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(saved.RateFile); err != nil {
				t.Errorf("the client kept no rate limit at %s: %v", saved.RateFile, err)
			}

			// The server reads the same three from config.json with no
			// variable set.
			c := configfile.ForServer(func(string) string { return "" }, configPath(t))
			if c.TokenFile.Value != saved.TokenFile || c.StoreFile.Value != saved.StoreFile || c.RateFile.Value != saved.RateFile ||
				c.TokenFile.From != configfile.FromFile || c.AppSecret != (configfile.Setting{Value: testSecret, From: configfile.FromFile}) ||
				len(c.Problems) != 0 {
				t.Errorf("the server would read %+v", c)
			}
		})
	}
}

// When config.json names another token file than login saves to, login
// says so, naming the server's and what points it here; with the same one,
// or with no config.json, it says nothing.
func TestLoginWarnsOfAnotherTokenFile(t *testing.T) {
	_, tokenFile := setup(t)
	warning := "warning: the MCP server reads its login from " + tokenFile +
		" (config.json); run schwabtx setup in this terminal to point it here\n"
	if out := doLogin(t); strings.Contains(out, "MCP server") {
		t.Errorf("without config.json, login printed\n%s", out)
	}
	if _, _, err := runTx(t, "setup"); err != nil {
		t.Fatal(err)
	}
	if out := doLogin(t); strings.Contains(out, "MCP server") {
		t.Errorf("with the token file config.json names, login printed\n%s", out)
	}
	t.Setenv("SCHWAB_TOKEN_FILE", filepath.Join(t.TempDir(), "elsewhere", "token.json"))
	if out := doLogin(t); strings.Count(out, warning) != 1 {
		t.Errorf("with another token file, login printed\n%s\nwant once\n%s", out, warning)
	}
	// The same file under another name is the same file.
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(filepath.Dir(tokenFile), link); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SCHWAB_TOKEN_FILE", filepath.Join(link, filepath.Base(tokenFile)))
		if out := doLogin(t); strings.Contains(out, "MCP server") {
			t.Errorf("with the token file under a link, login printed\n%s", out)
		}
	}
}

// When config.json names another store than a store command opens, the
// command says so once, naming the server's store and what points it
// here; with the same store, or with no config.json, it says nothing.
func TestStoreCommandsWarnOfAnotherStore(t *testing.T) {
	offline(t)
	other := newStoreFile(t)
	newStoreFile(t) // the commands' store, SCHWAB_STORE_FILE
	dir := t.TempDir()
	snapshots := writeFile(t, dir, "snapshots.jsonl", snapshotFile(t, []time.Time{testNow}, "29470135"))
	backup := filepath.Join(dir, "backup.db")
	warned := func(args ...string) string {
		t.Helper()
		_, stderr, _ := runTx(t, args...)
		return stderr
	}
	if stderr := warned("status"); strings.Contains(stderr, "MCP server") {
		t.Errorf("without config.json, status warned %q", stderr)
	}
	if err := configfile.Save(configPath(t), configfile.File{AppKey: testKey, AppSecret: testSecret, StoreFile: other}); err != nil {
		t.Fatal(err)
	}
	warning := "warning: the MCP server reads the store at " + other +
		" (config.json); run schwabtx setup in this terminal, with the same -store, to point it here\n"
	for _, args := range [][]string{
		{"status"},
		{"export"},
		{"store", "check"},
		{"edit", "-action", "Bank Interest", "-policy", "auto"},
		{"import", snapshots},
		{"import", "-dry-run", snapshots},
		{"store", "backup", backup},
		{"store", "restore", "-dry-run", backup},
	} {
		if stderr := warned(args...); strings.Count(stderr, warning) != 1 {
			t.Errorf("%q warned\n%s\nwant once\n%s", args, stderr, warning)
		}
	}
	// The store that config.json names, by -store or by SCHWAB_STORE_FILE.
	if stderr := warned("status", "-store", other); strings.Contains(stderr, "MCP server") {
		t.Errorf("status -store of the server's store warned %q", stderr)
	}
	t.Setenv("SCHWAB_STORE_FILE", other)
	if stderr := warned("store", "check"); strings.Contains(stderr, "MCP server") {
		t.Errorf("store check of the server's store warned %q", stderr)
	}
}
