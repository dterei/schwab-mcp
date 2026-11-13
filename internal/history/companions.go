package history

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Companions returns the paths of the store's files: the database, its -wal
// and -shm, the lock files, and the backups made before a schema changed
// that are beside it now. The first three are one thing, which is never
// copied, moved or removed in part.
func Companions(store string) []string {
	store = cleanPath(store)
	paths := []string{store}
	for _, suffix := range companionSuffixes {
		paths = append(paths, store+suffix)
	}
	backups, _ := filepath.Glob(escapeGlob(store) + ".schema*.bak")
	return append(paths, backups...)
}

// companionSuffixes are what the names of the store's files other than the
// backups add to the store's.
var companionSuffixes = []string{"-wal", "-shm", ".migrate.lock", ".sync.lock"}

// IsCompanion reports whether p is one of the store's files, or would be
// taken for one: by name, made absolute and cleaned, which also catches an
// output that does not exist yet, such as an export named history.db-wal;
// or by being the same file as one of them under another name, such as a
// hard link. It only reads the files' metadata, never their contents: a
// process with the store open that opened one of them, and closed it, would
// drop the locks that SQLite holds on it.
func IsCompanion(p, store string) bool {
	return isCompanion(p, store, true)
}

// isCompanion is IsCompanion, which counts the backups beside the store
// among its files only when backups says so: a backup is a database of its
// own, which a process with the store open may read, as a restore does.
func isCompanion(p, store string, backups bool) bool {
	p, store = cleanPath(p), cleanPath(store)
	if dirName(p, store) && companionName(filepath.Base(p), filepath.Base(store), backups) {
		return true
	}
	pi, err := os.Stat(p) //nolint:gosec // G703: the path is the caller's to name; only its metadata is read.
	if err != nil {
		return false
	}
	files := Companions(store)
	if !backups {
		files = files[:1+len(companionSuffixes)]
	}
	for _, c := range files {
		if ci, err := os.Stat(c); err == nil && os.SameFile(pi, ci) {
			return true
		}
	}
	return false
}

// cleanPath returns p made absolute and cleaned, or only cleaned if it
// cannot be made absolute.
func cleanPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// dirName reports whether p and store are in the same directory: by name,
// or by being the same directory under two names.
func dirName(p, store string) bool {
	a, b := filepath.Dir(p), filepath.Dir(store)
	if sameName(a, b) {
		return true
	}
	ai, err := os.Stat(a) //nolint:gosec // G703: the path is the caller's to name; only its metadata is read.
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// companionName reports whether name is the name of one of the files of a
// store named store, beside it, its backups among them when backups says so.
func companionName(name, store string, backups bool) bool {
	if sameName(name, store) {
		return true
	}
	for _, suffix := range companionSuffixes {
		if sameName(name, store+suffix) {
			return true
		}
	}
	prefix, suffix := store+".schema", ".bak"
	return backups && len(name) >= len(prefix)+len(suffix) &&
		sameName(name[:len(prefix)], prefix) && sameName(name[len(name)-len(suffix):], suffix)
}

// sameName compares names as the system's usual file systems do: ignoring
// case on Windows and macOS.
func sameName(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// escapeGlob escapes what filepath.Glob would read as a pattern in p.
func escapeGlob(p string) string {
	if runtime.GOOS == "windows" {
		// Windows' separator is Glob's escape; brackets escape there.
		r := strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]")
		return r.Replace(p)
	}
	r := strings.NewReplacer(`\`, `\\`, "[", `\[`, "*", `\*`, "?", `\?`)
	return r.Replace(p)
}
