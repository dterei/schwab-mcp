package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// snapshotLines returns the lines of the snapshot file at path, which must
// end with a newline.
func snapshotLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("%s does not end with a newline: %q", path, b)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists or cannot be checked: %v", path, err)
	}
}

func TestSnapshotAppendsSchwabsResponse(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow, time.Hour)
	f.addAccount("33334444", 5000, 1, testNow, time.Hour)
	doLogin(t)
	path := os.Getenv("SCHWAB_SNAPSHOT_FILE")

	out, _, err := runTx(t, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if f.detailsFields != "positions" {
		t.Errorf("asked for fields=%q, want positions", f.detailsFields)
	}
	if want := "Saved a snapshot of 2 accounts with 4 positions to " + path + "\n"; out != want {
		t.Errorf("printed %q, want %q", out, want)
	}

	timeNow = func() time.Time { return testNow.Add(24 * time.Hour) }
	if _, _, err := runTx(t, "snapshot"); err != nil {
		t.Fatal(err)
	}

	// One line per run, holding Schwab's response as sent: "&" is not
	// escaped and numbers such as 250.0 keep their spelling.
	lines := snapshotLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, when := range []string{"2026-10-08T12:00:00Z", "2026-10-09T12:00:00Z"} {
		want := `{"version":1,"fetched_at":"` + when + `","accounts":` + f.detailsBody + `}`
		if lines[i] != want {
			t.Errorf("line %d:\n got %s\nwant %s", i+1, lines[i], want)
		}
	}
	// Windows has no Unix permission bits to check.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("snapshot file mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestSnapshotWarnsBeforeTheLoginExpires(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow, time.Hour)
	doLogin(t)
	timeNow = func() time.Time { return testNow.Add(6*24*time.Hour + 12*time.Hour) }

	_, stderr, err := runTx(t, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "expires in about 12 hours") {
		t.Errorf("no expiry warning: %q", stderr)
	}
	if lines := snapshotLines(t, os.Getenv("SCHWAB_SNAPSHOT_FILE")); len(lines) != 1 {
		t.Errorf("got %d snapshots, want 1", len(lines))
	}
}

func TestSnapshotFailsWithoutLoginOrAccounts(t *testing.T) {
	t.Run("no accounts", func(t *testing.T) {
		setup(t)
		doLogin(t)
		if _, _, err := runTx(t, "snapshot"); err == nil || !strings.Contains(err.Error(), "no accounts") {
			t.Errorf("expected a no-accounts error, got %v", err)
		}
		assertNoFile(t, os.Getenv("SCHWAB_SNAPSHOT_FILE"))
	})
	t.Run("expired login", func(t *testing.T) {
		f, tokenFile := setup(t)
		f.addAccount("11112222", 1000, 1, testNow, time.Hour)
		doLogin(t)
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tokenFile, bytes.ReplaceAll(b, []byte(testRefresh), []byte("stale-refresh")), 0o600); err != nil {
			t.Fatal(err)
		}
		timeNow = func() time.Time { return testNow.Add(8 * 24 * time.Hour) }

		if _, _, err := runTx(t, "snapshot"); err == nil || !strings.Contains(err.Error(), "schwabtx login") {
			t.Errorf("expected a re-login error, got %v", err)
		}
		assertNoFile(t, os.Getenv("SCHWAB_SNAPSHOT_FILE"))
	})
}

func TestSnapshotFileLocation(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow, time.Hour)
	doLogin(t)

	// -file wins over SCHWAB_SNAPSHOT_FILE, and missing directories are made.
	flagPath := filepath.Join(t.TempDir(), "new", "dir", "snaps.jsonl")
	if _, _, err := runTx(t, "snapshot", "-file", flagPath); err != nil {
		t.Fatal(err)
	}
	if lines := snapshotLines(t, flagPath); len(lines) != 1 {
		t.Errorf("got %d snapshots, want 1", len(lines))
	}
	assertNoFile(t, os.Getenv("SCHWAB_SNAPSHOT_FILE"))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Dir(flagPath))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("snapshot directory mode = %v, want 0700", info.Mode().Perm())
		}
	}

	// Without either, the file goes in the user config directory.
	home := t.TempDir()
	t.Setenv("SCHWAB_SNAPSHOT_FILE", "")
	t.Setenv("HOME", home)            // macOS
	t.Setenv("XDG_CONFIG_HOME", home) // Linux and other Unix systems
	t.Setenv("AppData", home)         // Windows
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, home) {
		t.Fatalf("config directory %s is outside the test's %s", dir, home)
	}
	if _, _, err := runTx(t, "snapshot"); err != nil {
		t.Fatal(err)
	}
	if lines := snapshotLines(t, filepath.Join(dir, "schwabtx", "snapshots.jsonl")); len(lines) != 1 {
		t.Errorf("got %d snapshots, want 1", len(lines))
	}
}

func TestSnapshotStartsANewLineAfterACutShortWrite(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow, time.Hour)
	doLogin(t)
	path := os.Getenv("SCHWAB_SNAPSHOT_FILE")
	partial := `{"version":1,"fetched_at":"2026-10-07T12:00:00Z","acc`
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runTx(t, "snapshot"); err != nil {
		t.Fatal(err)
	}
	lines := snapshotLines(t, path)
	if len(lines) != 2 || lines[0] != partial || !json.Valid([]byte(lines[1])) {
		t.Fatalf("got lines %q", lines)
	}
}

func TestSnapshotRejectsBadArguments(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow, time.Hour)
	doLogin(t)
	for _, args := range [][]string{
		{"snapshot", "extra"},
		{"snapshot", "-file"},
		{"snapshot", "-n", "5"},
	} {
		if _, _, err := runTx(t, args...); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
	if f.detailsFields != "" {
		t.Error("bad arguments still fetched the accounts")
	}
	assertNoFile(t, os.Getenv("SCHWAB_SNAPSHOT_FILE"))
}
