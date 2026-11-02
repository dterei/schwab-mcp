package schwab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// token is the login as the token file holds it. The JSON names are those
// that schwabtx has always written, so that existing logins keep working.
type token struct {
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	// RefreshObtainedAt is the time of the browser login, from which the
	// refresh token lasts LoginLifetime.
	RefreshObtainedAt time.Time `json:"refresh_obtained_at"`
}

// readToken reads the token file at path.
func readToken(path string) (*token, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("reading token file: %w", err)
	}
	var t token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("token file %s is corrupt (%w); run `schwabtx login` again", path, err)
	}
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("token file %s has no refresh token; run `schwabtx login` again", path)
	}
	return &t, nil
}

// writeToken replaces the token file at path with t, creating the file, and
// its directory, readable only by you.
func writeToken(path string, t *token) error {
	b, err := json.MarshalIndent(t, "", "  ") //nolint:gosec // G117: holding the tokens is this file's job.
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
		return fmt.Errorf("writing token file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing token file: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("writing token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing token file: %w", err)
	}
	if err := rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing token file: %w", err)
	}
	syncDir(dir)
	return nil
}

// rename moves oldpath over newpath. On Windows that fails while another
// program has newpath open, as antivirus and search indexers do for a moment,
// so there it is tried again for up to a second.
func rename(oldpath, newpath string) error {
	err := os.Rename(oldpath, newpath)
	if runtime.GOOS != "windows" {
		return err
	}
	for wait := 10 * time.Millisecond; err != nil && wait <= 320*time.Millisecond; wait *= 2 {
		time.Sleep(wait)
		err = os.Rename(oldpath, newpath)
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
