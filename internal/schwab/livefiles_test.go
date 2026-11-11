package schwab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// refuseInside returns why dir must not hold a live run's files, which give
// away the owner's accounts: it is inside the module, where git could commit
// them, or inside any git work tree; or nil. It decides by the files
// themselves, not by the path's spelling, so that a link, or on macOS and
// Windows a path in another case, does not get past it: walking up from dir,
// as the system takes the path, its links followed, no folder that exists
// may be the module's root by os.SameFile, and none may hold a .git entry.
func refuseInside(dir, module string) error {
	root, err := os.Stat(module)
	if err != nil {
		return fmt.Errorf("finding the module: %w", err)
	}
	p, err := resolve(dir)
	if err != nil {
		return fmt.Errorf("finding %s: %w", dir, err)
	}
	for {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, root) {
			return fmt.Errorf("%s is inside the module", dir)
		}
		// A .git that cannot be seen is none that git, run by this user,
		// could see either.
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return fmt.Errorf("%s is inside a git work tree", dir)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// makePrivateDir makes dir and the folders above it that are missing,
// readable only by the user.
func makePrivateDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

// createPrivate writes b to path, a file it makes readable only by the user,
// and never one that is there already.
func createPrivate(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return errors.Join(err, f.Close())
}

// replacePrivate replaces path with b, readable only by the user: through a
// file beside it that is renamed over it, so that a crash never leaves half
// of it.
func replacePrivate(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b) // CreateTemp makes it readable only by the user
	if err == nil {
		err = tmp.Sync()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	return rename(tmp.Name(), path)
}

// makePrivate makes path, a file of the run's that others can read, such
// as one copied in by hand, readable only by the user, and says so through
// warn. Windows has no such modes; its files take their folder's.
func makePrivate(path string, warn func(string)) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	warn(path + " could be read by others; it is now readable only by you")
	return nil
}

// moduleRoot returns the module's root: the nearest folder holding go.mod,
// from dir up.
func moduleRoot(dir string) (string, error) {
	p, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if info, err := os.Stat(filepath.Join(p, "go.mod")); err == nil && !info.IsDir() {
			return p, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("no go.mod in %s or above it", dir)
		}
		p = parent
	}
}

// gitCommit returns the commit that root, the module's root, has checked
// out, as git rev-parse HEAD gives it, with ", modified" after it when git
// status --porcelain prints anything; or "unknown" when git is missing or
// fails. A test binary carries no commit of its own, and -buildvcs=true
// stamps none under Go 1.26 in a linked git worktree.
func gitCommit(ctx context.Context, root string) string {
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		out, err := cmd.Output()
		return string(out), err
	}
	head, err := git("rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	if err != nil || head == "" || strings.Trim(head, "0123456789abcdef") != "" {
		return "unknown"
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return "unknown"
	}
	if strings.TrimSpace(status) != "" {
		return head + ", modified"
	}
	return head
}

// plantModule returns a folder that stands for the module's root, holding
// a go.mod, inside a temporary folder of the test's.
func plantModule(t *testing.T) string {
	t.Helper()
	module := filepath.Join(t.TempDir(), "module")
	if err := os.Mkdir(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module zq9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return module
}

// A live run's folder may not be inside the module, nor inside a git work
// tree, whether its .git is a folder or, as in a linked worktree, a file;
// the folder need not exist yet, and a link into either, or on a file system
// that ignores case, a path in another case, does not get past the check.
func TestRefuseInside(t *testing.T) {
	module := plantModule(t)
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	linked := filepath.Join(dir, "linked")
	for _, d := range []string{repo, linked} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dir, want string
	}{
		{filepath.Join(dir, "out"), ""},
		{filepath.Join(dir, "a", "b", "c"), ""},
		{module, "inside the module"},
		{filepath.Join(module, "out"), "inside the module"},
		{filepath.Join(module, "a", "..", "b", "c"), "inside the module"},
		{repo, "inside a git work tree"},
		{filepath.Join(repo, "x", "y"), "inside a git work tree"},
		{filepath.Join(linked, "out"), "inside a git work tree"},
	} {
		err := refuseInside(tc.dir, module)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.HasSuffix(err.Error(), tc.want)) {
			t.Errorf("refuseInside(%s): %v, want %q", tc.dir, err, tc.want)
		}
	}
	if err := refuseInside(dir, filepath.Join(dir, "no such module")); err == nil {
		t.Error("a module that is not there is taken")
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(module, "deep"), filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(module, "deep"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{filepath.Join(dir, "link"), filepath.Join(dir, "link", "out", "more")} {
			if err := refuseInside(d, module); err == nil || !strings.HasSuffix(err.Error(), "inside the module") {
				t.Errorf("refuseInside(%s), through a link into the module: %v", d, err)
			}
		}
	}
	other := filepath.Join(filepath.Dir(module), strings.ToUpper(filepath.Base(module)), "out")
	if _, err := os.Stat(filepath.Dir(other)); err == nil { // the file system ignores case
		if err := refuseInside(other, module); err == nil {
			t.Errorf("refuseInside(%s), the module in another case, took it", other)
		}
	}
}

// A private folder and its parents, and the files of a run, are readable
// only by the user; a file made is never one that was there, and a file
// replaced is replaced whole, by a private one, whatever the old one was.
func TestPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := makePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "f")
	if err := createPrivate(file, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(file); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("a file made: %v, %v; want mode 0600", info.Mode(), err)
	}
	if err := createPrivate(file, []byte("two")); !errors.Is(err, os.ErrExist) {
		t.Errorf("made a file over one: %v", err)
	}
	if err := os.Chmod(file, 0o644); err != nil { // as if copied in by hand
		t.Fatal(err)
	}
	if err := replacePrivate(file, []byte("three")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "three" {
		t.Errorf("the file holds %q, %v", b, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("the folder holds %v, %v; want the file alone", entries, err)
	}
	if runtime.GOOS == "windows" {
		return // no Unix permission bits; the files take their folder's ACLs
	}
	for path, want := range map[string]os.FileMode{filepath.Dir(dir): 0o700, dir: 0o700, file: 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; want mode %v", path, info.Mode(), err, want)
		}
	}
	// A file that its group alone can read is not private either, nor one
	// that only others can.
	for _, mode := range []os.FileMode{0o640, 0o604} {
		if err := os.Chmod(file, mode); err != nil {
			t.Fatal(err)
		}
		var warned []string
		warn := func(msg string) { warned = append(warned, msg) }
		if err := makePrivate(file, warn); err != nil {
			t.Fatal(err)
		}
		if err := makePrivate(file, warn); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 || len(warned) != 1 ||
			warned[0] != file+" could be read by others; it is now readable only by you" {
			t.Errorf("mode %v made private: %v, %v; warned %q", mode, info.Mode(), err, warned)
		}
	}
}

// The module's root is the nearest folder above that holds a go.mod.
func TestModuleRoot(t *testing.T) {
	module := plantModule(t)
	deep := filepath.Join(module, "internal", "x")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(module, "internal", "go.mod"), 0o700); err != nil { // a folder, not a go.mod
		t.Fatal(err)
	}
	for _, d := range []string{module, deep} {
		if got, err := moduleRoot(d); err != nil || got != module {
			t.Errorf("moduleRoot(%s) = %s, %v; want %s", d, got, err, module)
		}
	}
	if got, err := moduleRoot("."); err != nil || filepath.Base(filepath.Dir(got)) == "" {
		t.Errorf("moduleRoot(.) = %s, %v", got, err)
	} else if _, err := os.Stat(filepath.Join(got, "internal", "schwab", "client.go")); err != nil {
		t.Errorf("moduleRoot(.) = %s, which does not hold this package: %v", got, err)
	}
}

// isolateGit keeps the user's git settings from the git that t runs, its own
// and gitCommit's, until t ends: no global or system config, whose hooks,
// templates and settings could reach past t's folders or refuse its
// commits, and none of the GIT_ variables, such as the GIT_DIR and
// GIT_INDEX_FILE that a git hook running the tests sets, which would take
// git to the owner's repository. GIT_EXEC_PATH stays, which says only where
// git's own programs are. It needs git 2.32 or later, which reads
// GIT_CONFIG_GLOBAL.
func isolateGit(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") && !strings.EqualFold(name, "GIT_EXEC_PATH") {
			t.Setenv(name, "") // which puts it back when t ends
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// The commit is git's HEAD, marked modified when git status prints
// anything, and "unknown" when git fails, as outside a repository, or is
// missing. The git that the test runs sees none of the user's: neither the
// global config in the home folder nor the system config, which the test
// names by GIT_CONFIG_SYSTEM since it may not write the machine's, here
// each one whose hook refuses every commit; nor the GIT_DIR and
// GIT_INDEX_FILE that a git hook running the tests sets, here those of a
// repository of the owner's, to which its commit would go.
func TestGitCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	hooks := filepath.Join(home, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\thooksPath = "+filepath.ToSlash(hooks)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	owner := filepath.Join(dir, "owner")
	t.Setenv("GIT_DIR", filepath.Join(owner, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(owner, ".git", "index"))
	isolateGit(t)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(home, ".gitconfig")) // in place of the machine's
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{
			"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
		}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := gitCommit(t.Context(), repo); got != "unknown" {
		t.Errorf("outside a repository: %q", got)
	}
	git("init", "-q")
	if got := gitCommit(t.Context(), repo); got != "unknown" {
		t.Errorf("before the first commit: %q", got)
	}
	git("commit", "-q", "--allow-empty", "-m", "first")
	head := git("rev-parse", "HEAD")
	if got := gitCommit(t.Context(), repo); got != head || len(head) < 40 {
		t.Errorf("a clean tree: %q, want %q", got, head)
	}
	if err := os.WriteFile(filepath.Join(repo, "new"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gitCommit(t.Context(), repo); got != head+", modified" {
		t.Errorf("a modified tree: %q, want %q", got, head+", modified")
	}
	if _, err := os.Stat(owner); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("git went to the owner's repository: %v", err)
	}
	t.Setenv("PATH", "")
	if got := gitCommit(t.Context(), repo); got != "unknown" {
		t.Errorf("without git: %q", got)
	}
}
