package history

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// IsCompanion knows the store's files by name, under other spellings of
// the same path, and by being the same file under another name, without
// opening any; an unrelated file is not one, and an output that does not
// exist yet is known by its name.
func TestIsCompanion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := filepath.Join(dir, "history.db")
	backup := store + ".schema1.20261112T150405Z.bak"
	for _, p := range []string{store, store + "-wal", store + "-shm", store + ".migrate.lock", backup, filepath.Join(dir, "other.csv")} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "export.csv")
	if err := os.Link(store, link); err != nil {
		t.Logf("no hard link here: %v", err)
		link = ""
	}
	if got, want := Companions(store), []string{store, store + "-wal", store + "-shm", store + ".migrate.lock", store + ".sync.lock", backup}; !slices.Equal(got, want) {
		t.Errorf("Companions = %q, want %q", got, want)
	}
	yes := []string{
		store,
		store + "-wal",
		store + "-shm",
		store + ".migrate.lock",
		store + ".sync.lock", // does not exist
		backup,
		store + ".schema7.20300101T000000Z.bak", // does not exist
		filepath.Join(dir, ".", "history.db"),
		filepath.Join(dir, "sub", "..", "history.db-wal"),
	}
	if link != "" {
		yes = append(yes, link)
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		yes = append(yes, filepath.Join(dir, "HISTORY.DB"), filepath.Join(dir, "History.db-WAL"))
	}
	for _, p := range yes {
		if !IsCompanion(p, store) {
			t.Errorf("IsCompanion(%s) = false, want true", p)
		}
	}
	no := []string{
		filepath.Join(dir, "other.csv"),
		filepath.Join(dir, "history.db.csv"),
		filepath.Join(dir, "history.db-journal.csv"),
		filepath.Join(dir, "sub", "history.db"),
		filepath.Join(t.TempDir(), "history.db-wal"),
	}
	if runtime.GOOS == "linux" {
		no = append(no, filepath.Join(dir, "HISTORY.DB"))
	}
	for _, p := range no {
		if IsCompanion(p, store) {
			t.Errorf("IsCompanion(%s) = true, want false", p)
		}
	}
}
