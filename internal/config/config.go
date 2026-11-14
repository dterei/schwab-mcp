// Package config reads and writes config.json, which schwabtx setup writes
// in the user's own terminal and the MCP server reads: the app key and
// secret, the API's address when it is not Schwab's, and the paths of the
// token file, the store and the rate file, made absolute as that terminal
// resolved them. A client starts the server with --config and the file's
// path, so that no secret sits in the client's configuration, and the
// server finds the files that schwabtx uses whatever environment the
// client gives it. schwabtx itself reads only the key and secret from the
// file, and only when the environment has none.
//
// The file is readable only by the user on Linux and macOS. No error of
// this package holds the file's text, and none holds its path, which the
// caller has.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Version is the format of config.json that this build writes, and the
// newest that it reads.
const Version = 1

// Source says where a setting came from, for auth_status and the errors.
type Source string

// Where a setting can come from.
const (
	FromEnv     Source = "environment"
	FromFile    Source = "config file"
	FromDefault Source = "default"
)

// A Setting is one value and where it came from. From is empty when there
// is no value from anywhere.
type Setting struct {
	Value string
	From  Source
}

// File is what config.json holds. Unknown fields are kept on Save.
type File struct {
	Version   int    `json:"version"`
	AppKey    string `json:"app_key,omitempty"`
	AppSecret string `json:"app_secret,omitempty"`
	BaseURL   string `json:"base_url,omitempty"` // only for another server, such as the fake; the server reads it from here alone
	TokenFile string `json:"token_file,omitempty"`
	StoreFile string `json:"store_file,omitempty"`
	RateFile  string `json:"rate_file,omitempty"`
	// other holds the fields this build does not know.
	other map[string]json.RawMessage
}

// fields are the JSON names of File's fields, in the order Save writes
// them.
var fields = []string{"version", "app_key", "app_secret", "base_url", "token_file", "store_file", "rate_file"}

// DefaultFile returns schwabtx/config.json in the user config directory,
// beside the token file that schwabtx keeps there by default.
func DefaultFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the user config directory: %w", err)
	}
	return filepath.Join(dir, "schwabtx", "config.json"), nil
}

var (
	// ErrMissing means that there is no config file at the path.
	ErrMissing = errors.New("there is no config file")
	// ErrNewer means that the config file is of a newer format than this
	// build reads: a newer schwabtx wrote it.
	ErrNewer = errors.New("the config file was written by a newer schwabtx")
)

// A RelativePathError is Read's error for a file that holds a relative
// path, which names a file only from some working directory, and a client
// may start the server in any. Paths holds each such path by the JSON name
// of its setting, such as "token_file".
type RelativePathError struct {
	Paths map[string]string
}

func (e *RelativePathError) Error() string {
	return "the config file's " + strings.Join(slices.Sorted(maps.Keys(e.Paths)), " and ") + " is not an absolute path"
}

// Read reads the config file at path. A missing file is the zero File and
// ErrMissing. A file of a newer version is ErrNewer. A file with a
// relative path in it is a *RelativePathError, and the File is the rest of
// the file, without those paths, for a caller that can use it. The file's
// text is never in an error, and neither is the path, which the caller
// holds.
func Read(path string) (File, error) {
	var b []byte
	err := retry(func() (err error) {
		b, err = os.ReadFile(path)
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, ErrMissing
	}
	if err != nil {
		return File{}, fmt.Errorf("reading the config file: %w", withoutPath(err))
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, notRead(err)
	}
	if f.Version > Version {
		return File{}, ErrNewer
	}
	if err := json.Unmarshal(b, &f.other); err != nil {
		return File{}, notRead(err)
	}
	for _, name := range fields {
		delete(f.other, name)
	}
	for _, v := range []*string{&f.AppKey, &f.AppSecret, &f.BaseURL, &f.TokenFile, &f.StoreFile, &f.RateFile} {
		*v = strings.TrimSpace(*v)
	}
	relative := map[string]string{}
	for _, p := range []struct {
		name string
		v    *string
	}{{"token_file", &f.TokenFile}, {"store_file", &f.StoreFile}, {"rate_file", &f.RateFile}} {
		if *p.v != "" && !filepath.IsAbs(*p.v) {
			relative[p.name], *p.v = *p.v, ""
		}
	}
	if len(relative) > 0 {
		return f, &RelativePathError{Paths: relative}
	}
	return f, nil
}

// notRead returns the error of a file that encoding/json could not read,
// in words that hold none of the file's text: encoding/json's own quote a
// character of it.
func notRead(err error) error {
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Errorf("the config file is not JSON (at byte %d)", se.Offset)
	}
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok && te.Field != "" {
		want := "a string"
		if te.Type != nil && te.Type.Kind() == reflect.Int {
			want = "a whole number"
		}
		return fmt.Errorf("the config file's %s is not %s", te.Field, want)
	}
	return errors.New("the config file is not a JSON object")
}

// withoutPath returns err without the path that an error of the os
// package names.
func withoutPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return le.Err
	}
	return err
}

// Save writes f to path, readable only by you on Linux and macOS, through
// a temporary file renamed over it, keeping the fields it does not know.
// It writes this build's Version, and makes path's folder, readable only
// by you, when it is missing.
func Save(path string, f File) error {
	b, err := f.encode()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making the config file's folder: %w", withoutPath(err))
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("writing the config file: %w", withoutPath(err))
	}
	defer os.Remove(tmp.Name())
	write := func() error {
		if err := tmp.Chmod(0o600); err != nil {
			return err
		}
		if _, err := tmp.Write(b); err != nil {
			return err
		}
		return tmp.Sync()
	}
	if err := write(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the config file: %w", withoutPath(err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the config file: %w", withoutPath(err))
	}
	if err := retry(func() error { return os.Rename(tmp.Name(), path) }); err != nil {
		return fmt.Errorf("writing the config file: %w", withoutPath(err))
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync() // at worst a crash undoes the rename
			d.Close()
		}
	}
	return nil
}

// encode returns f as Save writes it: an object of its known fields, in
// the order of fields and without the empty ones, then the fields it does
// not know, by name, each on a line of its own, and HTML's characters as
// they are.
func (f File) encode() ([]byte, error) {
	known := map[string]string{
		"app_key": f.AppKey, "app_secret": f.AppSecret, "base_url": f.BaseURL,
		"token_file": f.TokenFile, "store_file": f.StoreFile, "rate_file": f.RateFile,
	}
	var b bytes.Buffer
	b.WriteString("{\n  \"version\": " + strconv.Itoa(Version))
	add := func(name string, v any) error {
		raw, ok := v.(json.RawMessage)
		if !ok {
			var err error
			if raw, err = marshal(v); err != nil {
				return err
			}
		}
		key, err := marshal(name)
		if err != nil {
			return err
		}
		b.WriteString(",\n  ")
		b.Write(key)
		b.WriteString(": ")
		return json.Indent(&b, raw, "  ", "  ")
	}
	for _, name := range fields[1:] {
		if v := known[name]; v != "" {
			if err := add(name, v); err != nil {
				return nil, err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(f.other)) {
		if err := add(name, f.other[name]); err != nil {
			return nil, fmt.Errorf("the config file's %s: %w", name, err)
		}
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// marshal returns v as JSON, with HTML's characters as they are.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// retry runs op, and again for about two seconds while it fails because
// another program has the file open for a moment, as the token file's
// writes do. Only Windows refuses a read or a rename for that.
func retry(op func() error) error {
	err := op()
	for wait := 10 * time.Millisecond; busy(err) && wait <= 640*time.Millisecond; wait *= 2 {
		time.Sleep(wait)
		err = op()
	}
	return err
}

// The codes of the problems that ForServer finds, for the server to word.
const (
	// CodeMissing is a config file that --config names and that does not
	// exist, or a path that has no value anywhere and no default.
	CodeMissing = "missing"
	// CodeUnreadable is a config file that could not be read, or is not
	// one of this format.
	CodeUnreadable = "unreadable"
	// CodeNewerVersion is a config file that a newer schwabtx wrote.
	CodeNewerVersion = "newer_version"
	// CodeRelativePath is a path that is not absolute, in the file, in a
	// variable or as the config file itself.
	CodeRelativePath = "relative_path"
	// CodeMadePrivate is a config file that others could read, which is
	// now readable only by you.
	CodeMadePrivate = "made_private"
	// CodeNotPrivate is a config file that others can read and that could
	// not be made private.
	CodeNotPrivate = "not_private"
	// CodeCompanion is a config, token or rate file that is one of the
	// store's files (history.IsCompanion), which nothing but SQLite may
	// open.
	CodeCompanion = "companion"
)

// A Problem is one thing ForServer could not use. The server words it,
// showing Path as masker.path shows a path; no Problem holds text of the
// file or an error's message.
type Problem struct {
	Code    string // one of the Code constants; main adds stray_argument and bad_flag
	Setting string // the setting it is about: "config_file", "token_file", "SCHWAB_STORE_FILE", ...
	Path    string // the path it is about, or ""
}

// Config is what the server runs with.
type Config struct {
	File              string  // the config file read, or ""
	AppKey, AppSecret Setting // from the environment or the config file, or empty
	BaseURL           Setting // from the config file or the default, never the environment
	// TokenFile, StoreFile and RateFile are absolute. TokenFile and
	// StoreFile are empty, with a Problem, when there is none that may be
	// used, and then nothing may be opened in their place. RateFile ""
	// means the client's default, which schwab.DefaultRateFile names.
	TokenFile, StoreFile, RateFile Setting
	Problems                       []Problem
}

// ForServer resolves the server's settings: the key and secret from
// SCHWAB_APP_KEY and SCHWAB_APP_SECRET, then the config file at path; the
// token file and store from SCHWAB_TOKEN_FILE and SCHWAB_STORE_FILE when
// those hold an absolute path, then the config file, then the default; the
// rate file and the base URL from the config file, then the default. A
// relative path in a variable is a Problem and the file's value is used. It
// never fails, so that the server can start and report; what it could not
// use is in Problems.
//
// path is the file that --config names, or "" for DefaultFile, whose
// absence is no problem: the server can run on the environment alone.
// getenv reads the variables; the defaults are the process's own user
// folders. A file that others can read is made private on Linux and macOS,
// as schwab.Auth makes the token file. A token, rate or config file that is
// one of the store's files is a problem: the token or rate file is not
// used, and nor is the store beside a config file that is one of its own.
func ForServer(getenv func(string) string, path string) Config {
	var c Config
	f := c.read(path)

	fromEnv := func(name, fileValue string) Setting {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return Setting{Value: v, From: FromEnv}
		}
		if fileValue != "" {
			return Setting{Value: fileValue, From: FromFile}
		}
		return Setting{}
	}
	c.AppKey = fromEnv("SCHWAB_APP_KEY", f.AppKey)
	c.AppSecret = fromEnv("SCHWAB_APP_SECRET", f.AppSecret)
	c.BaseURL = Setting{Value: schwab.DefaultBaseURL, From: FromDefault}
	if f.BaseURL != "" {
		c.BaseURL = Setting{Value: strings.TrimRight(f.BaseURL, "/"), From: FromFile}
	}
	c.TokenFile = c.pathOf(getenv, "SCHWAB_TOKEN_FILE", "token_file", f.TokenFile, schwab.DefaultTokenFile)
	c.StoreFile = c.pathOf(getenv, "SCHWAB_STORE_FILE", "store_file", f.StoreFile, defaultStore)
	c.RateFile = Setting{From: FromDefault}
	if f.RateFile != "" {
		c.RateFile = Setting{Value: f.RateFile, From: FromFile}
	}

	// Nothing but SQLite may open the store's files, so none of them is
	// used as another file: the store is not used when config.json is one
	// of its files, and else neither is a token or rate file that is.
	if store := c.StoreFile.Value; store != "" {
		if c.File != "" && history.IsCompanion(c.File, store) {
			c.problem(CodeCompanion, "config_file", c.File)
			c.StoreFile = Setting{}
		} else {
			if p := c.TokenFile.Value; p != "" && history.IsCompanion(p, store) {
				name := "token_file"
				if c.TokenFile.From == FromEnv {
					name = "SCHWAB_TOKEN_FILE"
				}
				c.problem(CodeCompanion, name, p)
				c.TokenFile = Setting{}
			}
			if p := c.RateFile.Value; p != "" && history.IsCompanion(p, store) {
				c.problem(CodeCompanion, "rate_file", p)
				c.RateFile = Setting{From: FromDefault}
			}
		}
	}
	return c
}

// read reads the config file for ForServer, as it describes, and returns
// what may be used of it, noting each problem.
func (c *Config) read(path string) File {
	named := path != ""
	if !named {
		var err error
		if path, err = DefaultFile(); err != nil {
			return File{}
		}
	}
	if !filepath.IsAbs(path) {
		c.problem(CodeRelativePath, "config_file", path)
		return File{}
	}
	f, err := Read(path)
	if rel, ok := errors.AsType[*RelativePathError](err); ok {
		for _, name := range slices.Sorted(maps.Keys(rel.Paths)) {
			c.problem(CodeRelativePath, name, rel.Paths[name])
		}
		err = nil // the rest of the file is used
	}
	switch {
	case errors.Is(err, ErrMissing):
		if named {
			c.problem(CodeMissing, "config_file", path)
		}
		return File{}
	case errors.Is(err, ErrNewer):
		c.problem(CodeNewerVersion, "config_file", path)
		return File{}
	case err != nil:
		c.problem(CodeUnreadable, "config_file", path)
		return File{}
	}
	c.File = path
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				c.problem(CodeNotPrivate, "config_file", path)
			} else {
				c.problem(CodeMadePrivate, "config_file", path)
			}
		}
	}
	return f
}

// pathOf returns a path setting: the variable's value when it is absolute,
// else the file's value, else the default, noting a relative variable or
// default, and a default that cannot be found, as problems.
func (c *Config) pathOf(getenv func(string) string, variable, field, fileValue string, def func() (string, error)) Setting {
	if v := strings.TrimSpace(getenv(variable)); v != "" {
		if filepath.IsAbs(v) {
			return Setting{Value: v, From: FromEnv}
		}
		c.problem(CodeRelativePath, variable, v)
	}
	if fileValue != "" {
		return Setting{Value: fileValue, From: FromFile}
	}
	p, err := def()
	switch {
	case err != nil:
		c.problem(CodeMissing, field, "")
		return Setting{}
	case !filepath.IsAbs(p):
		c.problem(CodeRelativePath, field, p)
		return Setting{}
	}
	return Setting{Value: p, From: FromDefault}
}

// defaultStore returns the store's default path as history.DefaultPath
// gives it when SCHWAB_STORE_FILE is not set, which the server reads
// through getenv instead: history.db in schwabtx's folder of
// %LocalAppData% on Windows, and of the user config directory elsewhere.
func defaultStore() (string, error) {
	dir, err := os.UserConfigDir()
	if runtime.GOOS == "windows" {
		dir, err = os.UserCacheDir()
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "schwabtx", "history.db"), nil
}

func (c *Config) problem(code, setting, path string) {
	c.Problems = append(c.Problems, Problem{Code: code, Setting: setting, Path: path})
}

// LogValue lets slog print a Config without its secrets: where the app key
// and secret came from, never their values; the API's host alone, since a
// URL may hold a user and password; each path and where it came from; and
// the problems.
func (c Config) LogValue() slog.Value {
	source := func(s Setting) string {
		if s.From == "" {
			return "unset"
		}
		return string(s.From)
	}
	host := ""
	if u, err := url.Parse(c.BaseURL.Value); err == nil {
		host = u.Host
	}
	path := func(name string, s Setting) slog.Attr {
		return slog.Group(name, slog.String("path", s.Value), slog.String("from", source(s)))
	}
	attrs := []slog.Attr{
		slog.String("file", c.File),
		slog.String("app_key", source(c.AppKey)),
		slog.String("app_secret", source(c.AppSecret)),
		slog.Group("api", slog.String("host", host), slog.String("from", source(c.BaseURL))),
		path("token_file", c.TokenFile),
		path("store_file", c.StoreFile),
		path("rate_file", c.RateFile),
	}
	if len(c.Problems) > 0 {
		ps := make([]string, len(c.Problems))
		for i, p := range c.Problems {
			ps[i] = strings.TrimSpace(p.Code + " " + p.Setting + " " + p.Path)
		}
		attrs = append(attrs, slog.String("problems", strings.Join(ps, "; ")))
	}
	return slog.GroupValue(attrs...)
}
