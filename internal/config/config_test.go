package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The made-up app of these tests, which no error or log line may show.
const (
	testKey    = "made-up-app-key-0000000000000000"
	testSecret = "made-up-secret"
)

// writeConfig writes content to config.json in a folder of the test's own
// and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schwabtx", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// env returns a getenv that has the variables given, in pairs of a name
// and its value, and no others.
func env(pairs ...string) func(string) string {
	return func(name string) string {
		for i := 0; i+1 < len(pairs); i += 2 {
			if pairs[i] == name {
				return pairs[i+1]
			}
		}
		return ""
	}
}

// quoted returns s as a JSON string's contents, for a path in a file that
// a test writes: a Windows path's backslashes escaped.
func quoted(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

// Save writes every field in a fixed order, the fields it does not know
// after them, readable only by you, and Read reads back what it wrote.
func TestSaveAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new", "config.json")
	f := File{
		AppKey: testKey, AppSecret: testSecret, BaseURL: "https://127.0.0.1:41234",
		TokenFile: filepath.Join(dir, "a&b", "token.json"), StoreFile: filepath.Join(dir, "history.db"),
		RateFile: filepath.Join(dir, "rate-3f9a0c1d2e4b5a69"),
	}
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		`  "version": 1,` + "\n" +
		`  "app_key": "` + testKey + `",` + "\n" +
		`  "app_secret": "` + testSecret + `",` + "\n" +
		`  "base_url": "https://127.0.0.1:41234",` + "\n" +
		`  "token_file": "` + quoted(f.TokenFile) + `",` + "\n" +
		`  "store_file": "` + quoted(f.StoreFile) + `",` + "\n" +
		`  "rate_file": "` + quoted(f.RateFile) + `"` + "\n" +
		"}\n"
	if string(b) != want {
		t.Errorf("Save wrote\n%s\nwant\n%s", b, want)
	}
	got, err := Read(path)
	f.Version = Version
	if err != nil || !equalFiles(got, f) {
		t.Errorf("Read gave %+v, %v; want %+v", got, err, f)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: mode %v (%v), want %v", p, info.Mode().Perm(), err, want)
			}
		}
	}
	// Saved again over itself, without the fields left empty, and with no
	// temporary file left beside it.
	if err := Save(path, File{AppKey: testKey}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "{\n  \"version\": 1,\n  \"app_key\": \""+testKey+"\"\n}\n" {
		t.Errorf("saved again: %s, %v", b, err)
	}
	if names, err := os.ReadDir(filepath.Dir(path)); err != nil || len(names) != 1 {
		t.Errorf("the folder holds %v, %v; want config.json alone", names, err)
	}
}

// equalFiles reports whether a and b hold the same fields, those they do
// not know included, as JSON that means the same, whatever its spacing.
func equalFiles(a, b File) bool {
	compact := func(raw json.RawMessage) string {
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			return "not JSON: " + string(raw)
		}
		return b.String()
	}
	return a.Version == b.Version && a.AppKey == b.AppKey && a.AppSecret == b.AppSecret && a.BaseURL == b.BaseURL &&
		a.TokenFile == b.TokenFile && a.StoreFile == b.StoreFile && a.RateFile == b.RateFile &&
		maps.EqualFunc(a.other, b.other, func(x, y json.RawMessage) bool { return compact(x) == compact(y) })
}

// A field that this build does not know, as a newer schwabtx of the same
// version may write, is kept by Save, as it was, after the known ones; a
// known field spelled in another case is not one of them.
func TestSaveKeepsUnknownFields(t *testing.T) {
	path := writeConfig(t, `{"zone":"a<b>&c","version":1,"d":4,"app_key":"`+testKey+`","later":{"list":[1,2],"on":true},"b":2,"c":3}`)
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	f.AppSecret = testSecret
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		`  "version": 1,` + "\n" +
		`  "app_key": "` + testKey + `",` + "\n" +
		`  "app_secret": "` + testSecret + `",` + "\n" +
		`  "b": 2,` + "\n" +
		`  "c": 3,` + "\n" +
		`  "d": 4,` + "\n" +
		`  "later": {` + "\n" +
		`    "list": [` + "\n" +
		`      1,` + "\n" +
		`      2` + "\n" +
		`    ],` + "\n" +
		`    "on": true` + "\n" +
		`  },` + "\n" +
		`  "zone": "a<b>&c"` + "\n" +
		"}\n"
	if string(b) != want {
		t.Errorf("Save wrote\n%s\nwant\n%s", b, want)
	}
	if again, err := Read(path); err != nil || !equalFiles(again, f) || len(again.other) != 5 {
		t.Errorf("read back %+v, %v; want %+v", again, err, f)
	}

	// A known field's name in another case, as a file edited by hand may
	// spell it, is that field, as encoding/json matches names, and not a
	// field to keep beside it: Save writes it once, by its own name, so that
	// what was saved is what is read back.
	rate := filepath.Join(t.TempDir(), "rate-3f9a0c1d2e4b5a69")
	path = writeConfig(t, `{"version":1,"App_Secret":"old-secret","Token_File":"token.json","RATE_FILE":"`+quoted(rate)+`","later":1}`)
	f, err = Read(path)
	if rel, ok := errors.AsType[*RelativePathError](err); !ok || rel.Paths["token_file"] != "token.json" || f.AppSecret != "old-secret" ||
		f.RateFile != rate || len(f.other) != 1 {
		t.Fatalf("Read: %+v, %v; want the secret, the rate file, a RelativePathError of token_file and one field it does not know", f, err)
	}
	f.AppSecret, f.TokenFile, f.RateFile = testSecret, filepath.Join(t.TempDir(), "token.json"), ""
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	again, err := Read(path)
	if err != nil || !equalFiles(again, f) {
		t.Errorf("read back %+v, %v; want %+v", again, err, f)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"App_Secret", "old-secret", "Token_File", "RATE_FILE", "rate-3f9a0c1d2e4b5a69"} {
		if bytes.Contains(b, []byte(old)) {
			t.Errorf("Save wrote\n%s\nwhich holds %s", b, old)
		}
	}
}

// Read's errors say what is wrong with the file without its text, which
// may hold the secret, and without its path, which the caller holds.
func TestReadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Read(filepath.Join(dir, "none.json")); !errors.Is(err, ErrMissing) {
		t.Errorf("a missing file: %v, want ErrMissing", err)
	}
	for _, tc := range []struct {
		name, content, want string
		is                  error
	}{
		{"a newer version", `{"version":2,"app_secret":"` + testSecret + `"}`, "newer schwabtx", ErrNewer},
		{"a newer version with a field of another type", `{"version":2,"app_key":{"keychain":"` + testSecret + `"}}`, "newer schwabtx", ErrNewer},
		{"not JSON", `{"app_secret":"` + testSecret + `" x}`, "not JSON (at byte", nil},
		{"cut short", `{"app_secret":"` + testSecret, "not JSON", nil},
		{"a field of another type", `{"app_secret":` + testSecret[:4] + `, "version":1}`, "not JSON", nil},
		{"a number for a string", `{"app_key":12345678,"app_secret":"` + testSecret + `"}`, "app_key is not a string", nil},
		{"a string for the version", `{"version":"` + testSecret + `"}`, "version is not a whole number", nil},
		{"not an object", `["` + testSecret + `"]`, "not a JSON object", nil},
	} {
		path := writeConfig(t, tc.content)
		f, err := Read(path)
		switch {
		case err == nil:
			t.Errorf("%s: no error, read %+v", tc.name, f)
			continue
		case tc.is != nil && !errors.Is(err, tc.is):
			t.Errorf("%s: %v, want %v", tc.name, err, tc.is)
		case !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: %q, want it to say %q", tc.name, err, tc.want)
		}
		if msg := err.Error(); strings.Contains(msg, testSecret[:4]) || strings.Contains(msg, filepath.Dir(path)) {
			t.Errorf("%s: the error %q holds the file's text or its path", tc.name, msg)
		}
		if !equalFiles(f, File{}) {
			t.Errorf("%s: read %+v beside the error", tc.name, f)
		}
	}
	// A folder in place of the file cannot be read, and the error does not
	// name it either.
	folder := filepath.Join(dir, "config.json")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(folder); err == nil || errors.Is(err, ErrMissing) || strings.Contains(err.Error(), dir) {
		t.Errorf("a folder: %v", err)
	}
}

// A relative path in the file is an error that names its setting, beside
// the rest of the file, without that path.
func TestReadRelativePath(t *testing.T) {
	store := filepath.Join(t.TempDir(), "history.db")
	path := writeConfig(t, `{"version":1,"app_key":"`+testKey+`","token_file":"token.json","store_file":"`+quoted(store)+`",`+
		`"rate_file":"cache/rate-3f9a0c1d2e4b5a69"}`)
	f, err := Read(path)
	rel, ok := errors.AsType[*RelativePathError](err)
	if !ok || len(rel.Paths) != 2 || rel.Paths["token_file"] != "token.json" || rel.Paths["rate_file"] != "cache/rate-3f9a0c1d2e4b5a69" {
		t.Fatalf("Read: %v, want a RelativePathError of token_file and rate_file", err)
	}
	if want := "the config file's rate_file and token_file is not an absolute path"; err.Error() != want {
		t.Errorf("the error says %q, want %q", err, want)
	}
	if f.AppKey != testKey || f.StoreFile != store || f.TokenFile != "" || f.RateFile != "" {
		t.Errorf("read %+v beside the error, want the key and the store alone", f)
	}
}

// ForServer takes the key and secret from the environment, then the file;
// the token file and the store from an absolute variable, then the file,
// then the default; and the rate file and the base URL from the file, then
// the default. What it cannot use is a problem, never a failure.
func TestForServer(t *testing.T) {
	dir := t.TempDir()
	token, store, rate := filepath.Join(dir, "token.json"), filepath.Join(dir, "history.db"), filepath.Join(dir, "rate-3f9a0c1d2e4b5a69")
	other := filepath.Join(dir, "other")
	full := `{"version":1,"app_key":"` + testKey + `","app_secret":"` + testSecret + `","base_url":"https://127.0.0.1:41234/",` +
		`"token_file":"` + quoted(token) + `","store_file":"` + quoted(store) + `","rate_file":"` + quoted(rate) + `"}`
	defaultToken, err := schwab.DefaultTokenFile()
	if err != nil {
		t.Fatal(err)
	}
	defaultStore, err := defaultStore()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		content  string // config.json, or "" for none
		getenv   func(string) string
		key      Setting
		secret   Setting
		base     Setting
		token    Setting
		store    Setting
		rate     Setting
		problems []Problem // with Path "?" for the config file's
	}{
		{
			name: "all from the file", content: full, getenv: env(),
			key: Setting{testKey, FromFile}, secret: Setting{testSecret, FromFile}, base: Setting{"https://127.0.0.1:41234", FromFile},
			token: Setting{token, FromFile}, store: Setting{store, FromFile}, rate: Setting{rate, FromFile},
		},
		{
			name: "values with white space around them",
			content: `{"version":1,"app_key":" ` + testKey + ` ","app_secret":"\t` + testSecret + `\n","base_url":" https://127.0.0.1:41234/ ",` +
				`"token_file":" ` + quoted(token) + ` ","store_file":"\n` + quoted(store) + `","rate_file":"` + quoted(rate) + ` "}`,
			getenv: env(),
			key:    Setting{testKey, FromFile}, secret: Setting{testSecret, FromFile}, base: Setting{"https://127.0.0.1:41234", FromFile},
			token: Setting{token, FromFile}, store: Setting{store, FromFile}, rate: Setting{rate, FromFile},
		},
		{
			name: "the environment first", content: full,
			getenv: env("SCHWAB_APP_KEY", " other-key ", "SCHWAB_APP_SECRET", "other-secret", "SCHWAB_TOKEN_FILE", other+".json",
				"SCHWAB_STORE_FILE", other+".db", "SCHWAB_BASE_URL", "http://example.com", "SCHWAB_RATE_FILE", other),
			key: Setting{"other-key", FromEnv}, secret: Setting{"other-secret", FromEnv}, base: Setting{"https://127.0.0.1:41234", FromFile},
			token: Setting{other + ".json", FromEnv}, store: Setting{other + ".db", FromEnv}, rate: Setting{rate, FromFile},
		},
		{
			name: "relative variables", content: full, getenv: env("SCHWAB_TOKEN_FILE", "token.json", "SCHWAB_STORE_FILE", "history.db"),
			key: Setting{testKey, FromFile}, secret: Setting{testSecret, FromFile}, base: Setting{"https://127.0.0.1:41234", FromFile},
			token: Setting{token, FromFile}, store: Setting{store, FromFile}, rate: Setting{rate, FromFile},
			problems: []Problem{{CodeRelativePath, "SCHWAB_TOKEN_FILE", "token.json"}, {CodeRelativePath, "SCHWAB_STORE_FILE", "history.db"}},
		},
		{
			name:    "relative paths in the file",
			content: `{"version":1,"app_key":"` + testKey + `","token_file":"token.json","store_file":"` + quoted(store) + `","rate_file":"rate"}`,
			getenv:  env("SCHWAB_APP_SECRET", testSecret),
			key:     Setting{testKey, FromFile}, secret: Setting{testSecret, FromEnv}, base: Setting{schwab.DefaultBaseURL, FromDefault},
			token: Setting{defaultToken, FromDefault}, store: Setting{store, FromFile}, rate: Setting{"", FromDefault},
			problems: []Problem{{CodeRelativePath, "rate_file", "rate"}, {CodeRelativePath, "token_file", "token.json"}},
		},
		{
			name: "a newer version", content: `{"version":2,"app_key":"` + testKey + `","store_file":"` + quoted(store) + `"}`,
			getenv: env("SCHWAB_APP_KEY", "other-key"),
			key:    Setting{"other-key", FromEnv}, base: Setting{schwab.DefaultBaseURL, FromDefault},
			token: Setting{defaultToken, FromDefault}, store: Setting{defaultStore, FromDefault}, rate: Setting{"", FromDefault},
			problems: []Problem{{CodeNewerVersion, "config_file", "?"}},
		},
		{
			name: "a newer version with fields of other types", content: `{"version":2,"token_file":["a","b"],"store_file":7}`, getenv: env(),
			base:  Setting{schwab.DefaultBaseURL, FromDefault},
			token: Setting{defaultToken, FromDefault}, store: Setting{defaultStore, FromDefault}, rate: Setting{"", FromDefault},
			problems: []Problem{{CodeNewerVersion, "config_file", "?"}},
		},
		{
			name: "not JSON", content: `{"app_key":`, getenv: env(),
			base:  Setting{schwab.DefaultBaseURL, FromDefault},
			token: Setting{defaultToken, FromDefault}, store: Setting{defaultStore, FromDefault}, rate: Setting{"", FromDefault},
			problems: []Problem{{CodeUnreadable, "config_file", "?"}},
		},
		{
			name: "a missing file", getenv: env("SCHWAB_APP_KEY", testKey, "SCHWAB_APP_SECRET", testSecret),
			key: Setting{testKey, FromEnv}, secret: Setting{testSecret, FromEnv}, base: Setting{schwab.DefaultBaseURL, FromDefault},
			token: Setting{defaultToken, FromDefault}, store: Setting{defaultStore, FromDefault}, rate: Setting{"", FromDefault},
			problems: []Problem{{CodeMissing, "config_file", "?"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if tc.content != "" {
				path = writeConfig(t, tc.content)
			}
			c := ForServer(tc.getenv, path)
			for _, s := range []struct {
				name      string
				got, want Setting
			}{
				{"key", c.AppKey, tc.key},
				{"secret", c.AppSecret, tc.secret},
				{"base URL", c.BaseURL, tc.base},
				{"token file", c.TokenFile, tc.token},
				{"store", c.StoreFile, tc.store},
				{"rate file", c.RateFile, tc.rate},
			} {
				if s.got != s.want {
					t.Errorf("%s %+v, want %+v", s.name, s.got, s.want)
				}
			}
			var want []Problem
			for _, p := range tc.problems {
				if p.Path == "?" {
					p.Path = path
				}
				want = append(want, p)
			}
			if !slices.Equal(c.Problems, want) {
				t.Errorf("problems %+v, want %+v", c.Problems, want)
			}
			if read := c.File != ""; read != (tc.content != "" && !slices.ContainsFunc(want, func(p Problem) bool {
				return p.Setting == "config_file"
			})) || read && c.File != path {
				t.Errorf("File %q", c.File)
			}
		})
	}
}

// The API's address comes from the config file alone: SCHWAB_BASE_URL,
// which a client's environment may set, never sends the app key, the secret
// and the refresh token elsewhere. A relative SCHWAB_STORE_FILE, which the
// server's working directory would decide, is a problem, and the file's
// path is used.
func TestBaseURLOnlyFromConfig(t *testing.T) {
	store := filepath.Join(t.TempDir(), "history.db")
	getenv := env("SCHWAB_BASE_URL", "http://example.com", "SCHWAB_STORE_FILE", "history.db")
	c := ForServer(getenv, writeConfig(t, `{"version":1,"store_file":"`+quoted(store)+`"}`))
	if c.BaseURL != (Setting{schwab.DefaultBaseURL, FromDefault}) {
		t.Errorf("without base_url in the file: %+v, want Schwab's", c.BaseURL)
	}
	if want := []Problem{{CodeRelativePath, "SCHWAB_STORE_FILE", "history.db"}}; !slices.Equal(c.Problems, want) || c.StoreFile != (Setting{store, FromFile}) {
		t.Errorf("store %+v with problems %+v, want the file's with %+v", c.StoreFile, c.Problems, want)
	}
	c = ForServer(getenv, writeConfig(t, `{"version":1,"base_url":"http://127.0.0.1:41234"}`))
	if c.BaseURL != (Setting{"http://127.0.0.1:41234", FromFile}) {
		t.Errorf("with base_url in the file: %+v, want the file's", c.BaseURL)
	}
}

// Without --config, ForServer reads the default file, and its absence is no
// problem: the server can run on the environment alone. A relative path
// given for the file is a problem, and is not read.
func TestForServerDefaultFile(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "XDG_CACHE_HOME", "LocalAppData"} {
		t.Setenv(k, home)
	}
	c := ForServer(env("SCHWAB_APP_KEY", testKey), "")
	if c.File != "" || len(c.Problems) != 0 || c.AppKey != (Setting{testKey, FromEnv}) {
		t.Errorf("without a default file: %+v", c)
	}
	path, err := DefaultFile()
	if err != nil || !strings.HasPrefix(path, home) || filepath.Base(path) != "config.json" || filepath.Base(filepath.Dir(path)) != "schwabtx" {
		t.Fatalf("DefaultFile = %q, %v", path, err)
	}
	if err := Save(path, File{AppKey: testKey, AppSecret: testSecret}); err != nil {
		t.Fatal(err)
	}
	if c := ForServer(env(), ""); c.File != path || c.AppSecret != (Setting{testSecret, FromFile}) || len(c.Problems) != 0 {
		t.Errorf("with a default file: %+v", c)
	}
	c = ForServer(env(), filepath.Join("schwabtx", "config.json"))
	if want := []Problem{{CodeRelativePath, "config_file", filepath.Join("schwabtx", "config.json")}}; c.File != "" || !slices.Equal(c.Problems, want) {
		t.Errorf("a relative config file: %+v, want the problem %+v", c, want)
	}

	// A relative user folder, as a relative HOME gives, makes every default
	// relative, which names a file only from some working directory: the
	// default file, the token file and the store are each a problem, and
	// none is used.
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, "")
	}
	for _, k := range []string{"HOME", "AppData", "LocalAppData"} {
		t.Setenv(k, "home")
	}
	path, perr := DefaultFile()
	token, terr := schwab.DefaultTokenFile()
	store, serr := defaultStore()
	if perr != nil || terr != nil || serr != nil || filepath.IsAbs(path) || filepath.IsAbs(token) || filepath.IsAbs(store) {
		t.Fatalf("with a relative HOME, the defaults are %q, %q and %q (%v, %v, %v)", path, token, store, perr, terr, serr)
	}
	c = ForServer(env(), "")
	want := []Problem{{CodeRelativePath, "config_file", path}, {CodeRelativePath, "token_file", token}, {CodeRelativePath, "store_file", store}}
	if c.File != "" || !slices.Equal(c.Problems, want) || c.TokenFile != (Setting{}) || c.StoreFile != (Setting{}) {
		t.Errorf("with a relative HOME: %+v, want no token file, no store and the problems %+v", c, want)
	}
}

// The store's default is history.DefaultPath's without SCHWAB_STORE_FILE,
// which ForServer reads through getenv instead.
func TestDefaultStoreAgreesWithHistory(t *testing.T) {
	t.Setenv("SCHWAB_STORE_FILE", "")
	got, err := defaultStore()
	want, werr := history.DefaultPath()
	if err != nil || werr != nil || got != want {
		t.Errorf("defaultStore = %q, %v; history.DefaultPath = %q, %v", got, err, want, werr)
	}
}

// A config file that others can read is made private, as the token file is,
// and the problem says so.
func TestForServerMakesTheFilePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits on Windows")
	}
	path := writeConfig(t, `{"version":1,"app_key":"`+testKey+`"}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	c := ForServer(env(), path)
	if want := []Problem{{CodeMadePrivate, "config_file", path}}; !slices.Equal(c.Problems, want) || c.AppKey.Value != testKey {
		t.Errorf("problems %+v, want %+v", c.Problems, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v (%v), want 0600", info.Mode().Perm(), err)
	}
	if c := ForServer(env(), path); len(c.Problems) != 0 {
		t.Errorf("read again: problems %+v, want none", c.Problems)
	}
}

// A token, rate or config file that is one of the store's files, which
// nothing but SQLite may open, is a problem, and is not used.
func TestForServerCompanions(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "history.db")
	for _, tc := range []struct {
		name     string
		content  string
		getenv   func(string) string
		problems []Problem
		token    Setting
		store    Setting
		rate     Setting
	}{
		{
			name:     "a token file beside the store under a companion's name",
			content:  `{"version":1,"token_file":"` + quoted(store+"-wal") + `","store_file":"` + quoted(store) + `"}`,
			getenv:   env(),
			problems: []Problem{{CodeCompanion, "token_file", store + "-wal"}},
			store:    Setting{store, FromFile}, rate: Setting{"", FromDefault},
		},
		{
			name:     "a token file from the environment that is the store",
			content:  `{"version":1,"store_file":"` + quoted(store) + `"}`,
			getenv:   env("SCHWAB_TOKEN_FILE", store),
			problems: []Problem{{CodeCompanion, "SCHWAB_TOKEN_FILE", store}},
			store:    Setting{store, FromFile}, rate: Setting{"", FromDefault},
		},
		{
			name:     "a rate file that is the store's lock",
			content:  `{"version":1,"token_file":"` + quoted(filepath.Join(dir, "token.json")) + `","rate_file":"` + quoted(store+".sync.lock") + `"}`,
			getenv:   env("SCHWAB_STORE_FILE", store),
			problems: []Problem{{CodeCompanion, "rate_file", store + ".sync.lock"}},
			token:    Setting{filepath.Join(dir, "token.json"), FromFile}, store: Setting{store, FromEnv}, rate: Setting{"", FromDefault},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ForServer(tc.getenv, writeConfig(t, tc.content))
			if !slices.Equal(c.Problems, tc.problems) || c.TokenFile != tc.token || c.StoreFile != tc.store || c.RateFile != tc.rate {
				t.Errorf("problems %+v, token %+v, store %+v, rate %+v; want %+v, %+v, %+v, %+v",
					c.Problems, c.TokenFile, c.StoreFile, c.RateFile, tc.problems, tc.token, tc.store, tc.rate)
			}
		})
	}
	// A config file that is the store: the store is not used.
	path := writeConfig(t, `{"version":1}`)
	c := ForServer(env("SCHWAB_STORE_FILE", path), path)
	if want := []Problem{{CodeCompanion, "config_file", path}}; !slices.Equal(c.Problems, want) || c.StoreFile != (Setting{}) {
		t.Errorf("problems %+v, store %+v; want %+v and no store", c.Problems, c.StoreFile, want)
	}
}

// slog prints where the app key and secret came from, never their values,
// and the API's host alone, never a password in its URL.
func TestLogValue(t *testing.T) {
	dir := t.TempDir()
	c := Config{
		File:      filepath.Join(dir, "config.json"),
		AppKey:    Setting{testKey, FromFile},
		AppSecret: Setting{testSecret, FromEnv},
		BaseURL:   Setting{"https://user:hunter2@127.0.0.1:41234", FromFile},
		TokenFile: Setting{filepath.Join(dir, "token.json"), FromFile},
		StoreFile: Setting{filepath.Join(dir, "history.db"), FromDefault},
		RateFile:  Setting{"", FromDefault},
		Problems:  []Problem{{CodeRelativePath, "SCHWAB_STORE_FILE", "history.db"}},
	}
	for _, h := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var b bytes.Buffer
		slog.New(h(&b)).Info("start", "config", c)
		out := b.String()
		for _, secret := range []string{testKey, testSecret, "hunter2", "user"} {
			if strings.Contains(out, secret) {
				t.Errorf("the log line holds %q: %s", secret, out)
			}
		}
		for _, want := range []string{"config file", "environment", "127.0.0.1:41234", "token.json", "history.db", "relative_path SCHWAB_STORE_FILE history.db"} {
			if !strings.Contains(out, want) {
				t.Errorf("the log line lacks %q: %s", want, out)
			}
		}
	}
	var b bytes.Buffer
	slog.New(slog.NewTextHandler(&b, nil)).Info("start", "config", Config{})
	if out := b.String(); !strings.Contains(out, "config.app_key=unset") {
		t.Errorf("an empty config: %s", out)
	}
}
