package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// snapshotVersion identifies the format of a snapshot line. Change it if the
// fields of snapshot change.
const snapshotVersion = 1

// snapshot is one line of the snapshot file. Accounts is Schwab's response to
// GET /accounts?fields=positions as it was received, full account numbers
// included.
type snapshot struct {
	Version   int             `json:"version"`
	FetchedAt time.Time       `json:"fetched_at"`
	Accounts  json.RawMessage `json:"accounts"`
}

// runSnapshot appends the positions and balances of every account to the
// snapshot file. Schwab only reports them as of now, so a day without a
// snapshot is lost for good.
func runSnapshot(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("schwabtx snapshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "JSON Lines file to append to (default: $SCHWAB_SNAPSHOT_FILE, else snapshots.jsonl in your config directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	path, err := snapshotPath(*file)
	if err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	httpClient := newHTTPClient()
	auth := newAuthenticator(cfg, httpClient)
	if err := auth.load(); err != nil {
		return err
	}
	if w := auth.expiryWarning(); w != "" {
		fmt.Fprintln(stderr, "warning:", w)
	}
	c := newClient(cfg, auth, httpClient)

	body, err := c.get(ctx, "/accounts", url.Values{"fields": {"positions"}})
	if err != nil {
		return fmt.Errorf("fetching accounts: %w", err)
	}
	fetchedAt := timeNow().UTC()

	var accounts []struct {
		SecuritiesAccount struct {
			Positions []json.RawMessage `json:"positions"`
		} `json:"securitiesAccount"`
	}
	if err := json.Unmarshal(body, &accounts); err != nil {
		return fmt.Errorf("decoding accounts: %w", err)
	}
	if len(accounts) == 0 {
		return errors.New("Schwab returned no accounts; run `schwabtx login` again and tick the accounts to share")
	}
	positions := 0
	for _, a := range accounts {
		positions += len(a.SecuritiesAccount.Positions)
	}

	// The encoder puts Schwab's JSON on one line and, with HTML escaping off,
	// leaves characters such as "&" as Schwab sent them.
	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(snapshot{Version: snapshotVersion, FetchedAt: fetchedAt, Accounts: body}); err != nil {
		return fmt.Errorf("encoding the snapshot: %w", err)
	}
	if err := appendLine(path, line.Bytes()); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Saved a snapshot of %d accounts with %d positions to %s\n", len(accounts), positions, path)
	return nil
}

// snapshotPath picks the snapshot file: the -file flag, then
// SCHWAB_SNAPSHOT_FILE, then snapshots.jsonl in the user config directory.
func snapshotPath(flagValue string) (string, error) {
	if p := strings.TrimSpace(flagValue); p != "" {
		return p, nil
	}
	if p := strings.TrimSpace(os.Getenv("SCHWAB_SNAPSHOT_FILE")); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a config directory, use -file or set SCHWAB_SNAPSHOT_FILE: %w", err)
	}
	return filepath.Join(dir, "schwabtx", "snapshots.jsonl"), nil
}

// appendLine adds line to the end of the file at path, creating the file, and
// its directory, readable only by you.
func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the snapshot directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening the snapshot file: %w", err)
	}
	if err := writeLine(f, line); err != nil {
		f.Close()
		return fmt.Errorf("writing the snapshot file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing the snapshot file %s: %w", path, err)
	}
	return nil
}

// writeLine appends line to f and flushes it to disk. If an earlier write was
// cut short, leaving the file without a final newline, line still starts on a
// line of its own.
func writeLine(f *os.File, line []byte) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if size := info.Size(); size > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, size-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(line); err != nil {
		return err
	}
	return f.Sync()
}
