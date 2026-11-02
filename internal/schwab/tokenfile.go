package schwab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// tokenVersion is the format of the token file. It goes up only for a change
// that an earlier version would get wrong if it rewrote the file: fields that
// it does not know, an earlier version keeps as they are. Files that
// schwabtx wrote before there was a version have none.
const tokenVersion = 1

// token is the login as the token file holds it. The JSON names of the first
// four fields are those that schwabtx has always written, so that existing
// logins keep working.
type token struct {
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	// RefreshObtainedAt is the time of the browser login, from which the
	// refresh token lasts LoginLifetime.
	RefreshObtainedAt time.Time `json:"refresh_obtained_at"`

	Version int `json:"version,omitempty"`
	// AccessObtainedAt is when Schwab issued the access token.
	AccessObtainedAt time.Time `json:"access_obtained_at,omitzero"`
	// Scope is what Schwab said it granted, at the login or the last
	// refresh.
	Scope string `json:"scope,omitempty"`

	// other holds the fields of the file that this version does not know,
	// as they were, so that a rewrite keeps them.
	other map[string]json.RawMessage
}

// tokenFields are the JSON names of token's fields.
var tokenFields = []string{
	"access_token", "refresh_token", "access_expires_at", "refresh_obtained_at",
	"version", "access_obtained_at", "scope",
}

// readToken reads the token file at path.
func readToken(path string) (*token, error) {
	var b []byte
	err := retry(func() (err error) {
		b, err = os.ReadFile(path)
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("reading the token file: %w", err)
	}
	var t token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("the token file %s is corrupt (%w); log in again", path, err)
	}
	if t.Version > tokenVersion {
		return nil, fmt.Errorf("the token file %s is from a newer version of this program (format %d, where this one reads up to %d); use that version, or log in again with this one",
			path, t.Version, tokenVersion)
	}
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("the token file %s has no refresh token; log in again", path)
	}
	if err := json.Unmarshal(b, &t.other); err != nil {
		return nil, fmt.Errorf("the token file %s is corrupt (%w); log in again", path, err)
	}
	for _, name := range tokenFields {
		delete(t.other, name)
	}
	return &t, nil
}

// encode returns t as the token file holds it, with any fields that this
// version does not know.
func (t *token) encode() ([]byte, error) {
	known := *t
	known.Version = tokenVersion
	b, err := json.Marshal(known) //nolint:gosec // G117: holding the tokens is this file's job.
	if err != nil {
		return nil, err
	}
	fields := maps.Clone(t.other)
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	return json.MarshalIndent(fields, "", "  ")
}

// writeToken replaces the token file at path with t, creating the file, and
// its directory, readable only by you.
func writeToken(path string, t *token) error {
	b, err := t.encode()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the token directory: %w", err)
	}
	// Write a temp file and rename it over the old one, so that a crash never
	// leaves half a token.
	tmp, err := os.CreateTemp(dir, ".token-*.json")
	if err != nil {
		return fmt.Errorf("writing the token file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the token file: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the token file: %w", err)
	}
	if err := rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing the token file: %w", err)
	}
	syncDir(dir)
	return nil
}

// rename moves oldpath over newpath.
func rename(oldpath, newpath string) error {
	return retry(func() error { return os.Rename(oldpath, newpath) })
}

// retry runs op, and again for about two seconds while it fails because
// another program has the file open for a moment. Only Windows refuses a
// read or a rename for that; there a rename cannot replace a file that
// anyone has open.
func retry(op func() error) error {
	err := op()
	for wait := 10 * time.Millisecond; busy(err) && wait <= 640*time.Millisecond; wait *= 2 {
		time.Sleep(wait)
		err = op()
	}
	return err
}

// syncDir flushes the directory, so that a rename in it survives a crash. On
// Windows there is nothing to do.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // at worst a crash undoes the rename
		d.Close()
	}
}
