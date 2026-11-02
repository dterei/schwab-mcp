package schwab

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

// resolveTree makes a tree of folders, files and links, the same for each
// case below, and returns its root with links followed.
func resolveTree(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(p string) { must(os.MkdirAll(filepath.Join(root, p), 0o700)) }
	write := func(p string) { must(os.WriteFile(filepath.Join(root, p), []byte("{\"refresh_token\":\"x\"}"), 0o600)) }
	link := func(target, name string) {
		must(os.Symlink(strings.ReplaceAll(target, "{root}", root), filepath.Join(root, name)))
	}
	mkdir("real/proj/sub")
	mkdir("ü/é")
	write("real/token.json")
	write("real/proj/token.json")
	write("plainfile")
	link("{root}/real/proj", "link")
	link("real/proj", "rlink")
	link("link", "chain1")
	link("chain1", "chain2")
	link("{root}/loop2", "loop1")
	link("{root}/loop1", "loop2")
	link("{root}/nowhere/dir", "dangdir")
	link("{root}/real/token.json", "toklink.json")
	link("{root}/real/tok2.json", "dangtok.json")
	link("tok3.json", "real/dangrel.json") // relative dangling link to a file
	link("../../real", "real/proj/up")
	link("real/proj/..", "relup")
	link("{root}/ü/é", "uelink")
	link("/", "rootlink")
	// A project whose token file is a link to a store elsewhere.
	mkdir("real2/proj")
	mkdir("store")
	write("store/token.json")
	link("{root}/store/token.json", "real2/token.json")
	link("{root}/real2/proj", "link2")
	// A chain of links ending at a file not made yet, and a link into a folder not made yet.
	link("a2", "a1")
	link("{root}/real/tok4.json", "a2")
	link("{root}/nodir/tok.json", "dl")
	return root
}

type resolveCase struct {
	name   string
	wd     string   // chdir to {root}/wd first, with PWD naming it as spelled
	rmwd   bool     // remove the working directory after the chdir
	path   string   // the configured path; {root} is replaced
	want   string   // the file the system opens for path, relative to root; "" when it opens none
	mkdirs []string // folders, relative to root, that must exist for path to open
}

// resolveCases are spellings of token files, with the file that the system
// opens for each.
var resolveCases = []resolveCase{
	{name: "abs plain", path: "{root}/real/token.json", want: "real/token.json"},
	{name: "abs dir link then ..", path: "{root}/link/../token.json", want: "real/token.json"},
	{name: "abs relative dir link then ..", path: "{root}/rlink/../token.json", want: "real/token.json"},
	{name: "abs link chain then ..", path: "{root}/chain2/../token.json", want: "real/token.json"},
	{name: "abs link to ../../real", path: "{root}/real/proj/up/token.json", want: "real/token.json"},
	{name: "abs link whose target has ..", path: "{root}/relup/token.json", want: "real/token.json"},
	{name: "abs link, sub, .. ..", path: "{root}/link/sub/../../token.json", want: "real/token.json"},
	{name: "abs . and //", path: "{root}/./real//./token.json", want: "real/token.json"},
	{name: "abs /.. prefix", path: "/../{root}/real/token.json", want: "real/token.json"},
	{name: "abs link to / then ..", path: "{root}/rootlink/../{root}/real/token.json", want: "real/token.json"},
	{name: "file link", path: "{root}/toklink.json", want: "real/token.json"},
	{name: "file link after .. ..", path: "{root}/link/../../toklink.json", want: "real/token.json"},
	{name: "dangling file link", path: "{root}/dangtok.json", want: "real/tok2.json"},
	{name: "relative dangling file link", path: "{root}/real/dangrel.json", want: "real/tok3.json"},
	{name: "dangling file link via dir link", path: "{root}/link/../dangrel.json", want: "real/tok3.json"},
	{name: "new file under link", path: "{root}/link/new.json", want: "real/proj/new.json"},
	{name: "new folders under link", path: "{root}/link/n1/n2/n3/token.json", want: "real/proj/n1/n2/n3/token.json", mkdirs: []string{"real/proj/n1/n2/n3"}},
	{name: "new folder then ..", path: "{root}/link/n1/../token.json", want: "real/proj/token.json", mkdirs: []string{"real/proj/n1"}},
	{name: "new folder then .. ..", path: "{root}/link/n1/../../token.json", want: "real/token.json", mkdirs: []string{"real/proj/n1"}},
	{name: "missing, .., link, ..", path: "{root}/missing/../link/../token.json", want: "real/token.json", mkdirs: []string{"missing"}},
	{name: "missing, .., file link", path: "{root}/missing/../toklink.json", want: "real/token.json", mkdirs: []string{"missing"}},
	{name: "missing, .., dangling file link", path: "{root}/missing/../dangtok.json", want: "real/tok2.json", mkdirs: []string{"missing"}},
	{name: "non-ASCII through link", path: "{root}/uelink/../é/x.json", want: "ü/é/x.json"},
	{name: "dangling dir link", path: "{root}/dangdir/token.json", want: "nowhere/dir/token.json", mkdirs: []string{"nowhere/dir"}},
	{name: "rel from link: ../token.json", wd: "link", path: "../token.json", want: "real/token.json"},
	{name: "rel from link: ./../././token.json", wd: "link", path: "./../././token.json", want: "real/token.json"},
	{name: "rel from link: ../cfg/../token.json", wd: "link", path: "../cfg/../token.json", want: "real/token.json", mkdirs: []string{"real/cfg"}},
	{name: "rel from link: token.json", wd: "link", path: "token.json", want: "real/proj/token.json"},
	{name: "rel from chain: ../token.json", wd: "chain2", path: "../token.json", want: "real/token.json"},
	{name: "rel from link: new/../../new.json", wd: "link", path: "new/../../new.json", want: "real/new.json", mkdirs: []string{"real/proj/new"}},
	{name: "rel from link sub: ../../token.json", wd: "link/sub", path: "../../token.json", want: "real/token.json"},
	{name: "rel from link: ../../ü/é/../é/t.json", wd: "link", path: "../../ü/é/../é/t.json", want: "ü/é/t.json"},
	{name: "rel from link2: ../token.json (a file link)", wd: "link2", path: "../token.json", want: "store/token.json"},
	{name: "rel from link2: ../cfg/../token.json (a file link)", wd: "link2", path: "../cfg/../token.json", want: "store/token.json", mkdirs: []string{"real2/cfg"}},
	{name: "dangling chain", path: "{root}/a1", want: "real/tok4.json"},
	{name: "dangling link into a new folder", path: "{root}/dl", want: "nodir/tok.json", mkdirs: []string{"nodir"}},
	// Paths that the system opens nothing for: resolve need only not hang.
	{name: "trailing slash on a file", path: "{root}/real/token.json/"},
	{name: "trailing slash, new", path: "{root}/real/new.json/"},
	{name: "root", path: "/"},
	{name: "link loop", path: "{root}/loop1/token.json"},
	{name: "through a file", path: "{root}/plainfile/../real/token.json"},
	{name: "removed wd", wd: "real/proj/sub", rmwd: true, path: "token.json"},
}

// TestResolveMatchesTheSystem checks that resolve names the file that the
// system opens or creates for each path, so that a process using that file
// by its own name finds the login and shares the lock, and that a write
// through a link leaves the link in place.
func TestResolveMatchesTheSystem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows takes .. by name, and links need extra rights")
	}
	for _, tc := range resolveCases {
		t.Run(tc.name, func(t *testing.T) {
			// One tree shows what the system opens for the path.
			rootA := resolveTree(t)
			pathA := strings.ReplaceAll(tc.path, "{root}", rootA)
			if tc.wd != "" {
				t.Chdir(filepath.Join(rootA, tc.wd))
				if tc.rmwd {
					if err := os.Remove(filepath.Join(rootA, tc.wd)); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, d := range tc.mkdirs {
				if err := os.MkdirAll(filepath.Join(rootA, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			f, kerr := os.OpenFile(pathA, os.O_RDWR|os.O_CREATE, 0o600)
			kernelOK := false
			if kerr == nil {
				fi, _ := f.Stat()
				f.Close()
				wi, err := os.Stat(filepath.Join(rootA, tc.want))
				kernelOK = tc.want != "" && err == nil && os.SameFile(fi, wi)
				if !kernelOK {
					t.Errorf("the test is wrong: the system opened %s for %s, not %s", fi.Name(), tc.path, tc.want)
				}
			} else if tc.want != "" {
				t.Errorf("the test is wrong: the system cannot open %s: %v", tc.path, kerr)
			}

			// Another shows what this package does with it.
			rootB := resolveTree(t)
			pathB := strings.ReplaceAll(tc.path, "{root}", rootB)
			if tc.wd != "" {
				t.Chdir(filepath.Join(rootB, tc.wd))
				if tc.rmwd {
					if err := os.Remove(filepath.Join(rootB, tc.wd)); err != nil {
						t.Fatal(err)
					}
				}
			}
			var linkBefore bool
			if li, err := os.Lstat(pathB); err == nil && li.Mode()&os.ModeSymlink != 0 {
				linkBefore = true
			}
			r, err := resolve(pathB)
			rel := func(p string) string {
				if s, ok := strings.CutPrefix(p, rootB); ok {
					return "{root}" + s
				}
				return p
			}
			if tc.want == "" {
				t.Logf("system: %v; resolve: %q, %v", kerr, rel(r), err)
				return
			}
			if err != nil {
				t.Errorf("resolve(%q): %v (system opens %s)", tc.path, err, tc.want)
				return
			}
			wantPath := filepath.Join(rootB, tc.want)
			early, _ := resolve(wantPath)
			if werr := writeToken(r, &token{AccessToken: "A", RefreshToken: "R"}); werr != nil {
				t.Errorf("resolve=%s; writeToken through it: %v", rel(r), werr)
			}
			r2, _ := resolve(wantPath)
			if r != r2 || r != early {
				t.Errorf("resolve(%q) = %s, but resolve of the file the system opens, %s, = %s (before the write: %s)", tc.path, rel(r), tc.want, rel(r2), rel(early))
			}
			if got, err := readToken(r2); err != nil || got.AccessToken != "A" {
				t.Errorf("a process using %s does not see the login written through %s: %v", tc.want, rel(r), err)
			}
			l1, ok1, err1 := filelock.TryAcquire(r + ".lock")
			l2, ok2, err2 := filelock.TryAcquire(r2 + ".lock")
			if ok1 && ok2 {
				t.Errorf("two locks: %s.lock and %s.lock are held at once", rel(r), rel(r2))
			}
			if err1 != nil || err2 != nil {
				t.Logf("lock errors: %v, %v", err1, err2)
			}
			if ok1 {
				l1.Release()
			}
			if ok2 {
				l2.Release()
			}
			if linkBefore {
				if li, err := os.Lstat(pathB); err != nil || li.Mode()&os.ModeSymlink == 0 {
					t.Errorf("the write replaced the link %s: it is now %v, %v", tc.path, li.Mode(), err)
				}
			}
			t.Logf("resolve -> %s (kernel ok: %v)", rel(r), kernelOK)
		})
	}
}
