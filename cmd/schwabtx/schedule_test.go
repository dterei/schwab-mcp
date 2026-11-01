package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// TestMain makes this test binary act as schwabtx when it is run the way the
// scheduler scripts in schedule/ run schwabtx, as `<binary> snapshot`, so
// that their tests can use it. `go test` never runs it like that.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "snapshot" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// scriptSetup is how a test runs a scheduler script.
type scriptSetup struct {
	home   string   // the home folder, with the repository at schwab-mcp
	script string   // the script to run
	path   []string // folders to put first on PATH
	args   []string
}

// scriptRun is what a scheduler script did, as far as the test can tell.
type scriptRun struct {
	code           int
	stdout, stderr string
	info           string     // the summary and the rest of the output for the log
	problems       string     // the warnings and errors that reach you
	log            string     // the log, when the script or the scheduler writes one
	notices        [][]string // the arguments of each call to osascript
	sleeps         []string   // the arguments of each call to sleep
	tmpLeft        []string   // what the script left in TMPDIR
}

// scheduler runs one of the scripts in schedule/ the way a scheduler would.
type scheduler struct {
	name   string
	script string // the script's name in schedule/
	binary string // schwabtx's file name
	// split is whether warnings and errors reach you apart from the summary.
	split bool
	run   func(t *testing.T, s scriptSetup) scriptRun
}

// schedulers returns the ways this system can run the scripts: snapshot.sh
// directly and through the README's cron line on Unix systems, and
// snapshot.ps1 with each PowerShell found on Windows.
func schedulers(t *testing.T) []scheduler {
	t.Helper()
	if runtime.GOOS != "windows" {
		line := cronLine(t)
		return []scheduler{
			{name: "sh", script: "snapshot.sh", binary: "schwabtx", split: true, run: func(t *testing.T, s scriptSetup) scriptRun {
				t.Helper()
				r := runUnix(t, s, "sh", append([]string{s.script}, s.args...)...)
				r.info, r.problems = r.stdout, r.stderr
				return r
			}},
			{name: "cron", script: "snapshot.sh", binary: "schwabtx", split: true, run: func(t *testing.T, s scriptSetup) scriptRun {
				t.Helper()
				r := runUnix(t, s, "sh", "-c", line)
				r.log = readIfExists(t, filepath.Join(s.home, "schwabtx-snapshot.log"))
				r.info, r.problems = r.log, r.stdout+r.stderr // cron mails both
				return r
			}},
		}
	}
	var list []scheduler
	for _, shell := range []string{"powershell.exe", "pwsh.exe"} {
		if _, err := exec.LookPath(shell); err != nil {
			t.Logf("%s not found: %v", shell, err)
			continue
		}
		list = append(list, scheduler{
			name: shell, script: "snapshot.ps1", binary: "schwabtx.exe",
			run: func(t *testing.T, s scriptSetup) scriptRun {
				t.Helper()
				args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", s.script, "-RetryDelay", "0"}
				// A notification from Windows PowerShell would pop up on a
				// developer's desktop, so only CI lets the script show one.
				if shell == "powershell.exe" && os.Getenv("CI") == "" {
					args = append(args, "-NoNotification")
				}
				path := slices.Concat(s.path, testPATH("schwabtx.exe"))
				env := slices.Concat(windowsEnv(s.home), []string{"PATH=" + strings.Join(path, string(os.PathListSeparator))})
				r := runScript(t, env, shell, append(args, s.args...)...)
				r.log = readIfExists(t, filepath.Join(s.home, "LocalAppData", "schwabtx", "snapshot.log"))
				r.info, r.problems = r.log, r.log
				return r
			},
		})
	}
	if len(list) == 0 {
		t.Skip("no PowerShell found")
	}
	return list
}

// cronLine returns the command in the cron line of schedule/README.md.
func cronLine(t *testing.T) string {
	t.Helper()
	_, section, _ := strings.Cut(readme(t), "\n## Linux and other Unix systems\n")
	entry := regexp.MustCompile(`^(\S+\s+){5}(sh .*)$`)
	for line := range strings.Lines(section) {
		if m := entry.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return m[2]
		}
	}
	t.Fatal("no cron line under ## Linux and other Unix systems in schedule/README.md")
	return ""
}

// readme returns schedule/README.md, with LF line endings even in a Windows
// checkout.
func readme(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("schedule", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// runUnix runs a command that runs snapshot.sh in the environment that cron
// and launchd give a job, with stand-ins for osascript and sleep first on
// PATH that record their arguments, so that notifications are recorded
// instead of shown and tries come right after each other.
func runUnix(t *testing.T, s scriptSetup, name string, args ...string) scriptRun {
	t.Helper()
	bin := filepath.Join(s.home, "stub-bin")
	tmp := filepath.Join(s.home, "tmp")
	calls := filepath.Join(s.home, "calls")
	for _, dir := range []string{bin, tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\nprintf '%s\\037' \"$@\" >> \"$CALLS.${0##*/}\"\necho >> \"$CALLS.${0##*/}\"\n"
	for _, cmd := range []string{"osascript", "sleep"} {
		if err := os.WriteFile(filepath.Join(bin, cmd), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := append(unixEnv(t, s.home, slices.Concat([]string{bin}, s.path)...), "TMPDIR="+tmp, "CALLS="+calls)

	r := runScript(t, env, name, args...)
	r.notices = stubCalls(t, calls+".osascript")
	for _, call := range stubCalls(t, calls+".sleep") {
		r.sleeps = append(r.sleeps, strings.Join(call, " "))
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		r.tmpLeft = append(r.tmpLeft, e.Name())
	}
	return r
}

// stubCalls returns the arguments of each call that a stand-in recorded.
func stubCalls(t *testing.T, path string) [][]string {
	t.Helper()
	var calls [][]string
	for line := range strings.Lines(readIfExists(t, path)) {
		calls = append(calls, strings.Split(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\x1f"), "\x1f"))
	}
	return calls
}

func runScript(t *testing.T, env []string, name string, args ...string) scriptRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	// A script that hangs is stopped after the timeout, and then a command it
	// started that still holds its output open does not hold up the test.
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := scriptRun{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	default:
		t.Fatalf("running %s: %v", name, err)
	}
	return r
}

func readIfExists(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// unixEnv returns the environment that cron and launchd give a job: HOME,
// SHELL and a short PATH. The PATH has the given folders first, then one of
// links to the tools the scripts use, which are not in /usr/bin on every
// system, so that a schwabtx installed next to them stays out of reach. When
// the tests measure coverage, GOCOVERDIR is passed on too, for schwabtx to
// record its coverage there rather than warn that it cannot.
func unixEnv(t *testing.T, home string, path ...string) []string {
	t.Helper()
	tools := filepath.Join(home, "tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"sh", "uname", "tr", "date", "cat", "rm", "mktemp", "dirname", "basename", "readlink", "grep", "tee"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(tools, tool)); err != nil && !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	env := []string{"HOME=" + home, "SHELL=/bin/sh", "PATH=" + strings.Join(append(path, tools), ":")}
	if dir := os.Getenv("GOCOVERDIR"); dir != "" {
		env = append(env, "GOCOVERDIR="+dir)
	}
	return env
}

// windowsEnv returns the environment Task Scheduler starts a script with,
// apart from PATH: the user's, here without any Schwab settings, and with the
// home and config folders in the test's temp space so the real ones stay out
// of reach.
func windowsEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name = strings.ToUpper(name); {
		case strings.HasPrefix(name, "SCHWAB"), name == "PATH",
			name == "HOME", name == "APPDATA", name == "LOCALAPPDATA", name == "USERPROFILE":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+home,
		"APPDATA="+filepath.Join(home, "AppData"),
		"LOCALAPPDATA="+filepath.Join(home, "LocalAppData"),
		"USERPROFILE="+home,
	)
}

// testPATH returns the folders on this process's PATH that hold no file
// named binary, so that the scripts cannot run a schwabtx installed here.
func testPATH(binary string) []string {
	var dirs []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, binary)); err != nil {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// writeToken saves a login made obtainedAgo before now, in real time, since
// the scripts run schwabtx in a process of its own.
func writeToken(t *testing.T, path, refresh string, obtainedAgo time.Duration) {
	t.Helper()
	now := time.Now()
	b, err := json.Marshal(token{
		AccessToken:       "expired-access",
		RefreshToken:      refresh,
		AccessExpiresAt:   now.Add(-time.Hour),
		RefreshObtainedAt: now.Add(-obtainedAgo),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// copyFile copies src to dst, executable, making dst's folder.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// stamped matches a line of script output: the time, then the message.
var stamped = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d \S+ (.*)$`)

// messages returns the messages in stamped output, failing on a line without
// the time in front.
func messages(t *testing.T, out string) []string {
	t.Helper()
	var msgs []string
	for line := range strings.Lines(out) {
		m := stamped.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m == nil {
			t.Errorf("output line without the time in front: %q", line)
			continue
		}
		msgs = append(msgs, m[1])
	}
	return msgs
}

func TestSnapshotScripts(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const saved = "Saved a snapshot of 1 accounts with 2 positions to "
	const warning = "warning: your Schwab login expires in about 36 hours; run `schwabtx login` to renew it"
	const forbidden = "error: fetching accounts: HTTP 403 from Schwab"
	for _, s := range schedulers(t) {
		t.Run(s.name, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				only        string        // the one script this case is for
				cronToo     bool          // whether the README's cron line can do this too
				binary      string        // where schwabtx is: "quoted" in SCHWABTX, "go bin", "path", "missing", "unset" or "false"
				refresh     string        // the saved refresh token
				obtainedAgo time.Duration // how long ago the login was
				failDetails int           // fetches of the accounts that fail first
				envArg      bool          // the .env file is elsewhere, given as an argument
				noEnvFile   bool
				relative    bool   // the .env file has paths relative to its folder
				crlf        bool   // the .env file has CRLF line endings
				mistake     bool   // the .env file has a value with a space and no quotes
				envFolder   bool   // a folder is given as the .env file
				envPipe     bool   // the .env file is a named pipe that nothing writes to
				link        string // the script is run through a link: "folder" to its folder, "file" to itself

				wantCode     int
				wantSaved    bool
				wantInfo     []string
				wantProblems []string // the warnings and errors that reach you
				wantRetries  int
			}{
				{
					name: "saves a snapshot", cronToo: true, binary: "go bin",
					wantSaved: true, wantInfo: []string{saved},
				},
				{
					name: "finds schwabtx on PATH", cronToo: true, binary: "path",
					wantSaved: true, wantInfo: []string{saved},
				},
				{
					name: "takes the env file as an argument", envArg: true,
					wantSaved: true, wantInfo: []string{saved},
				},
				{
					name: "finds the env file through a linked folder", only: "snapshot.sh", link: "folder",
					wantSaved: true, wantInfo: []string{saved},
				},
				{
					name: "finds the env file through links to the script", only: "snapshot.sh", link: "file",
					wantSaved: true, wantInfo: []string{saved},
				},
				{
					name: "runs in the folder of the env file", cronToo: true, relative: true,
					wantSaved: true, wantInfo: []string{saved},
				},
				{
					name: "warns in the two days before the login lapses", cronToo: true, obtainedAgo: 5*24*time.Hour + 12*time.Hour,
					wantSaved: true, wantInfo: []string{saved}, wantProblems: []string{warning},
				},
				{
					name: "tries again after a failure", cronToo: true, failDetails: 1,
					wantSaved: true, wantInfo: []string{forbidden, saved}, wantRetries: 1,
				},
				{
					name: "fails once the login has lapsed", cronToo: true, refresh: "stale-refresh", obtainedAgo: 8 * 24 * time.Hour,
					wantCode: 1, wantProblems: []string{"error: fetching accounts: Schwab rejected the saved login"}, wantRetries: 2,
				},
				{
					name: "warns and fails in the same run", cronToo: true, obtainedAgo: 5*24*time.Hour + 12*time.Hour, failDetails: 3,
					wantCode: 1, wantProblems: []string{warning, forbidden}, wantRetries: 2,
				},
				{
					name: "fails without an env file", cronToo: true, noEnvFile: true,
					wantCode: 1, wantProblems: []string{"error: cannot read "},
				},
				{
					name: "fails on a folder given as the env file", envFolder: true,
					wantCode: 1, wantProblems: []string{"error: cannot read "},
				},
				{
					name: "fails at once on a named pipe as the env file", only: "snapshot.sh", cronToo: true, envPipe: true,
					wantCode: 1, wantProblems: []string{"error: cannot read ", ": not a regular file"},
				},
				{
					name: "tells you of a mistake in the env file", only: "snapshot.sh", cronToo: true, mistake: true,
					wantCode: 1, wantProblems: []string{"error: cannot load "},
				},
				{
					name: "fails without schwabtx", cronToo: true, binary: "missing",
					wantCode: 1, wantProblems: []string{"error: cannot run "},
				},
				{
					name: "tells you of a variable that is not set", cronToo: true, binary: "unset",
					wantCode: 1, wantProblems: []string{"error: cannot run "},
				},
				{
					name: "reports a failure that prints nothing", only: "snapshot.sh", cronToo: true, binary: "false",
					wantCode: 1, wantProblems: []string{"error: schwabtx exited with status 1"}, wantRetries: 2,
				},
				{
					name: "rejects an env file with CRLF line endings", only: "snapshot.sh", cronToo: true, crlf: true,
					wantCode: 1, wantProblems: []string{"line endings; save it with Unix (LF) ones"},
				},
				{
					name: "reads an env file with CRLF line endings", only: "snapshot.ps1", crlf: true,
					wantSaved: true, wantInfo: []string{saved},
				},
			} {
				if tc.only != "" && tc.only != s.script || s.name == "cron" && !tc.cronToo {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					f := newFakeSchwab(t)
					f.addAccount("11112222", 1000, 1, time.Now(), time.Hour)
					f.failDetails = tc.failDetails
					home := t.TempDir()

					// The script runs from a copy of the repository's layout, with
					// the .env at its root.
					repo := filepath.Join(home, "schwab-mcp")
					script := filepath.Join(repo, "cmd", "schwabtx", "schedule", s.script)
					copyFile(t, filepath.Join("schedule", s.script), script)
					envFile := filepath.Join(repo, ".env")
					var args []string
					if tc.envArg || tc.envFolder {
						if tc.envArg {
							envFile = filepath.Join(home, "elsewhere.env")
						}
						arg := envFile
						if tc.envFolder {
							arg = repo
						}
						args = []string{arg}
						if s.script == "snapshot.ps1" {
							args = []string{"-EnvFile", arg}
						}
					}

					tokenFile := filepath.Join(home, "cfg", "token.json")
					snapshots := filepath.Join(home, "cfg", "snapshots.jsonl")
					tokenValue, snapshotsValue := tokenFile, snapshots
					if tc.relative {
						tokenFile = filepath.Join(repo, "cfg", "token.json")
						snapshots = filepath.Join(repo, "cfg", "snapshots.jsonl")
						tokenValue, snapshotsValue = "cfg/token.json", "cfg/snapshots.jsonl"
					}
					refresh := tc.refresh
					if refresh == "" {
						refresh = testRefresh
					}
					obtainedAgo := tc.obtainedAgo
					if obtainedAgo == 0 {
						obtainedAgo = time.Hour
					}
					writeToken(t, tokenFile, refresh, obtainedAgo)

					lines := []string{
						"# Schwab app settings",
						"SCHWAB_APP_KEY=" + testKey,
						"SCHWAB_APP_SECRET=" + testSecret,
						"SCHWAB_BASE_URL=" + f.srv.URL,
						`SCHWAB_TOKEN_FILE="` + tokenValue + `"`,
						`SCHWAB_SNAPSHOT_FILE="` + snapshotsValue + `"`,
					}
					if tc.mistake {
						lines = append(lines, "NOTE=two words")
					}
					var path []string
					switch tc.binary {
					case "go bin":
						copyFile(t, self, filepath.Join(home, "go", "bin", s.binary))
						lines = append(lines, "SCHWABTX=   # default: ~/go/bin")
					case "path":
						dir := filepath.Join(home, "path-bin")
						copyFile(t, self, filepath.Join(dir, s.binary))
						path = append(path, dir)
					case "missing":
						lines = append(lines, `SCHWABTX="`+filepath.Join(home, "missing", s.binary)+`"`)
					case "unset":
						lines = append(lines, `SCHWABTX="$SCHWABTX_TEST_UNSET/`+s.binary+`"`)
					case "false":
						// A schwabtx that fails without a word.
						silent, err := exec.LookPath("false")
						if err != nil {
							t.Fatal(err)
						}
						lines = append(lines, `SCHWABTX="`+silent+`"`)
					default:
						lines = append(lines, `SCHWABTX="`+self+`"`)
					}
					newline := "\n"
					if tc.crlf {
						newline = "\r\n"
					}
					switch {
					case tc.noEnvFile:
					case tc.envPipe:
						if out, err := exec.CommandContext(t.Context(), "mkfifo", envFile).CombinedOutput(); err != nil {
							t.Fatalf("mkfifo: %v\n%s", err, out)
						}
					default:
						if err := os.WriteFile(envFile, []byte(strings.Join(lines, newline)+newline), 0o600); err != nil {
							t.Fatal(err)
						}
					}

					run := script
					switch tc.link {
					case "folder":
						link := filepath.Join(home, "jobs")
						if err := os.Symlink(filepath.Dir(script), link); err != nil {
							t.Fatal(err)
						}
						run = filepath.Join(link, s.script)
					case "file":
						// A relative link to a link to the script.
						for _, dir := range []string{"bin", "jobs"} {
							if err := os.Mkdir(filepath.Join(home, dir), 0o755); err != nil {
								t.Fatal(err)
							}
						}
						run = filepath.Join(home, "jobs", "snapshot")
						if err := os.Symlink(script, filepath.Join(home, "bin", "snapshot")); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(filepath.Join("..", "bin", "snapshot"), run); err != nil {
							t.Fatal(err)
						}
					}
					r := s.run(t, scriptSetup{home: home, script: run, path: path, args: args})

					// Cron ignores the exit status, which tee's ends the line.
					if s.name != "cron" && r.code != tc.wantCode {
						t.Errorf("exit code %d, want %d\nstdout: %s\nstderr: %s\nlog: %s", r.code, tc.wantCode, r.stdout, r.stderr, r.log)
					}
					info := strings.Join(messages(t, r.info), "\n")
					problems := strings.Join(messages(t, r.problems), "\n")
					for _, want := range tc.wantInfo {
						if want == saved {
							want += snapshotsValue
						}
						if !strings.Contains(info, want) {
							t.Errorf("no %q in the output:\n%s", want, r.info)
						}
					}
					for _, want := range tc.wantProblems {
						if !strings.Contains(problems, want) {
							t.Errorf("no %q in the warnings and errors:\n%s", want, r.problems)
						}
					}
					if got := strings.Count(info, "; trying again in "); got != tc.wantRetries {
						t.Errorf("tried again %d times, want %d:\n%s", got, tc.wantRetries, r.info)
					}

					if s.split {
						// The warnings and errors that reach you are those of the
						// last try, and the log has them too.
						if len(tc.wantProblems) == 0 && problems != "" {
							t.Errorf("unexpected warnings or errors:\n%s", r.problems)
						}
						if strings.Count(problems, "error: ") > 1 {
							t.Errorf("errors of more than one try:\n%s", r.problems)
						}
						if s.name == "cron" && !strings.Contains(info, problems) {
							t.Errorf("warnings and errors %q are not in the log:\n%s", problems, r.log)
						}
						if want := slices.Repeat([]string{"60"}, tc.wantRetries); !slices.Equal(r.sleeps, want) {
							t.Errorf("slept %q, want %q", r.sleeps, want)
						}
						if len(r.tmpLeft) > 0 {
							t.Errorf("left %q in TMPDIR", r.tmpLeft)
						}
						checkNotices(t, home, r.notices, tc.wantProblems)
					} else {
						if got, want := messages(t, r.stdout), messages(t, r.log); !slices.Equal(got, want) {
							t.Errorf("printed %q but logged %q", got, want)
						}
						if r.stderr != "" {
							t.Errorf("stderr: %s", r.stderr)
						}
						// A notification that Windows PowerShell shows leaves no
						// trace; one it cannot show, or that PowerShell 7 does not
						// try, is noted in the log.
						noted := strings.Contains(r.log, "notification")
						if len(tc.wantProblems) == 0 && noted {
							t.Errorf("notified without a warning or error:\n%s", r.log)
						}
						if len(tc.wantProblems) > 0 && s.name == "pwsh.exe" && !noted {
							t.Errorf("no note that PowerShell 7 cannot notify:\n%s", r.log)
						}
						// In CI, Windows PowerShell tries to show the notifications.
						// Whether one shows depends on the session, but an error from
						// PowerShell itself means that the script's code is wrong.
						if s.name == "powershell.exe" && os.Getenv("CI") != "" {
							for line := range strings.Lines(r.log) {
								for _, bug := range []string{
									"Unable to find type", "Cannot find type", "is not recognized as", "Cannot find an overload",
									"Method invocation failed", "parameter cannot be found", "null-valued expression",
								} {
									if strings.Contains(line, "notification") && strings.Contains(line, bug) {
										t.Errorf("the notification code is wrong: %s", line)
									}
								}
							}
						}
					}

					got := 0
					if b, err := os.ReadFile(snapshots); err == nil {
						got = strings.Count(string(b), "\n")
					}
					if want := map[bool]int{true: 1}[tc.wantSaved]; got != want {
						t.Errorf("saved %d snapshots, want %d", got, want)
					}
				})
			}
		})
	}
}

// checkNotices checks the notifications that snapshot.sh showed: none off
// macOS, and on macOS one for the warnings and errors, on one line, with an
// AppleScript that compiles.
func checkNotices(t *testing.T, home string, notices [][]string, problems []string) {
	t.Helper()
	if runtime.GOOS != "darwin" || len(problems) == 0 {
		if len(notices) > 0 {
			t.Errorf("notified %q", notices)
		}
		return
	}
	if len(notices) != 1 {
		t.Fatalf("notified %d times, want once: %q", len(notices), notices)
	}
	args := notices[0]
	msg := args[len(args)-1]
	for _, want := range problems {
		if !strings.Contains(msg, want) || strings.Contains(msg, "\n") {
			t.Errorf("notified %q, want one line with %q", msg, want)
		}
	}
	compile := append([]string{"-o", filepath.Join(home, "notify.scpt")}, args[:len(args)-1]...)
	if out, err := exec.CommandContext(t.Context(), "osacompile", compile...).CombinedOutput(); err != nil {
		t.Errorf("the notification's AppleScript %q does not compile: %v\n%s", args[:len(args)-1], err, out)
	}
}

// TestSnapshotTrace runs snapshot.sh with sh -xv, as you might to find out
// why it fails, and checks that it works all the same and that the trace
// leaves out the secrets in the .env file, but not what comes after.
func TestSnapshotTrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("snapshot.sh is for Unix systems")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeSchwab(t)
	f.addAccount("11112222", 1000, 1, time.Now(), time.Hour)
	home := t.TempDir()
	repo := filepath.Join(home, "schwab-mcp")
	script := filepath.Join(repo, "cmd", "schwabtx", "schedule", "snapshot.sh")
	copyFile(t, filepath.Join("schedule", "snapshot.sh"), script)
	tokenFile := filepath.Join(home, "cfg", "token.json")
	snapshots := filepath.Join(home, "cfg", "snapshots.jsonl")
	writeToken(t, tokenFile, testRefresh, time.Hour)
	env := []string{
		"SCHWAB_APP_KEY=" + testKey,
		"SCHWAB_APP_SECRET=" + testSecret,
		"SCHWAB_BASE_URL=" + f.srv.URL,
		`SCHWAB_TOKEN_FILE="` + tokenFile + `"`,
		`SCHWAB_SNAPSHOT_FILE="` + snapshots + `"`,
		`SCHWABTX="` + self + `"`,
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte(strings.Join(env, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runUnix(t, scriptSetup{home: home}, "sh", "-xv", script)
	if r.code != 0 {
		t.Errorf("exit code %d, want 0\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, self+" snapshot") {
		t.Errorf("the trace stops at the .env file:\n%s", r.stderr)
	}
	if strings.Contains(r.stdout+r.stderr, testSecret) {
		t.Errorf("the trace shows the app secret:\n%s%s", r.stdout, r.stderr)
	}
	if _, err := os.Stat(snapshots); err != nil {
		t.Errorf("no snapshot saved: %v", err)
	}
}

// plistValue decodes the property list element start, whose start tag d has
// just read, into a string, an int, a []any or a map[string]any.
func plistValue(d *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "string":
		var s string
		err := d.DecodeElement(&s, &start)
		return s, err
	case "integer":
		var n int
		err := d.DecodeElement(&n, &start)
		return n, err
	case "array", "dict":
		var list []any
		dict := map[string]any{}
		key := ""
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch tok := tok.(type) {
			case xml.EndElement:
				if start.Name.Local == "array" {
					return list, nil
				}
				return dict, nil
			case xml.StartElement:
				if tok.Name.Local == "key" {
					if err := d.DecodeElement(&key, &tok); err != nil {
						return nil, err
					}
					continue
				}
				v, err := plistValue(d, tok)
				if err != nil {
					return nil, err
				}
				if start.Name.Local == "array" {
					list = append(list, v)
				} else {
					dict[key] = v
				}
			}
		}
	}
	return nil, fmt.Errorf("unexpected <%s>", start.Name.Local)
}

func TestLaunchdPlist(t *testing.T) {
	const name = "com.github.dterei.schwabtx.snapshot.plist"
	path := filepath.Join("schedule", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	var plist any
	for plist == nil {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			t.Fatal("no dict in the plist")
		}
		if err != nil {
			t.Fatal(err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			if plist, err = plistValue(d, se); err != nil {
				t.Fatal(err)
			}
		}
	}

	// install-agent.sh fills in the paths, so the template's need only be
	// right in form. launchd ignores an hour or minute that is not an
	// integer, and %#v shows the types.
	const home = "/Users/YOU"
	got := fmt.Sprintf("%#v", plist)
	want := fmt.Sprintf("%#v", map[string]any{
		"Label":                 strings.TrimSuffix(name, ".plist"),
		"ProgramArguments":      []any{"/bin/sh", home + "/schwab-mcp/cmd/schwabtx/schedule/snapshot.sh"},
		"StartCalendarInterval": map[string]any{"Hour": 21, "Minute": 5},
		"StandardOutPath":       home + "/Library/Logs/schwabtx-snapshot.log",
		"StandardErrorPath":     home + "/Library/Logs/schwabtx-snapshot.log",
	})
	if got != want {
		t.Errorf("plist is\n%s\nwant\n%s", got, want)
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.CommandContext(t.Context(), "plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v\n%s", err, out)
		}
	}
}

// TestInstallAgent runs install-agent.sh from a repository whose path needs
// escaping in a plist, with a stand-in for launchctl, and checks the agent it
// installs.
func TestInstallAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd agents are for macOS")
	}
	const label = "com.github.dterei.schwabtx.snapshot"
	// The script compares paths with links followed, and the temp folder is
	// under /var, a link to /private/var.
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	install := func(t *testing.T, repo string, args ...string) (calls [][]string, out string, err error) {
		t.Helper()
		schedule := filepath.Join(repo, "cmd", "schwabtx", "schedule")
		for _, f := range []string{"install-agent.sh", "snapshot.sh", label + ".plist"} {
			copyFile(t, filepath.Join("schedule", f), filepath.Join(schedule, f))
		}
		bin := filepath.Join(home, "stub-bin")
		stub := "#!/bin/sh\nprintf '%s\\037' \"$@\" >> \"$CALLS\"\necho >> \"$CALLS\"\n"
		copyStub := filepath.Join(bin, "launchctl")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyStub, []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		callsFile := filepath.Join(home, "launchctl-calls")
		if err := os.Remove(callsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "sh", append([]string{filepath.Join(schedule, "install-agent.sh")}, args...)...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "CALLS=" + callsFile}
		b, err := cmd.CombinedOutput()
		return stubCalls(t, callsFile), string(b), err
	}

	repo := filepath.Join(home, "R&D | <code>", "schwab-mcp")
	// A .env that others can read, as cp makes it.
	envFile := filepath.Join(repo, ".env")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, []byte("SCHWAB_APP_SECRET=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(envFile, 0o644); err != nil { // whatever the umask
		t.Fatal(err)
	}
	calls, out, err := install(t, repo, "03:07")
	if err != nil {
		t.Fatalf("install-agent.sh: %v\n%s", err, out)
	}
	if fi, err := os.Stat(envFile); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("left .env as %v, want it readable only by you", fi.Mode().Perm())
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	j, err := exec.CommandContext(t.Context(), "plutil", "-convert", "json", "-o", "-", plist).Output()
	if err != nil {
		t.Fatalf("reading %s: %v", plist, err)
	}
	var agent struct {
		Label                 string
		ProgramArguments      []string
		StartCalendarInterval map[string]any // the values must be integers, not strings
		StandardOutPath       string
		StandardErrorPath     string
	}
	if err := json.Unmarshal(j, &agent); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, "Library", "Logs", "schwabtx-snapshot.log")
	script := filepath.Join(repo, "cmd", "schwabtx", "schedule", "snapshot.sh")
	if agent.Label != label || !slices.Equal(agent.ProgramArguments, []string{"/bin/sh", script}) ||
		agent.StandardOutPath != log || agent.StandardErrorPath != log {
		t.Errorf("installed %+v, want it to run %s and log to %s", agent, script, log)
	}
	if hour, minute := agent.StartCalendarInterval["Hour"], agent.StartCalendarInterval["Minute"]; hour != 3.0 || minute != 7.0 {
		t.Errorf("runs at %#v:%#v, want 3:7 as integers", hour, minute)
	}
	if _, err := os.Stat(filepath.Dir(log)); err != nil {
		t.Errorf("no folder for the log: %v", err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	want := [][]string{{"enable", domain + "/" + label}, {"bootout", domain + "/" + label}, {"bootstrap", domain, plist}}
	if !slices.EqualFunc(calls, want, slices.Equal) {
		t.Errorf("ran launchctl %q, want %q", calls, want)
	}

	// A link from another folder into Documents leads there all the same.
	if err := os.MkdirAll(filepath.Join(home, "Documents", "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Documents", "code"), filepath.Join(home, "code")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, repo, at, want string }{
		{"refuses a folder background jobs cannot read", filepath.Join(home, "Documents", "schwab-mcp"), "21:05", "does not let background jobs read"},
		{"refuses a link into such a folder", filepath.Join(home, "code", "schwab-mcp"), "21:05", "does not let background jobs read"},
		{"refuses a time that is not HH:MM", repo, "9:05", "must be HH:MM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, out, err := install(t, tc.repo, tc.at)
			if err == nil || !strings.Contains(out, tc.want) || len(calls) > 0 {
				t.Errorf("install-agent.sh %s: %v, ran launchctl %q, printed %q; want it to fail with %q", tc.at, err, calls, out, tc.want)
			}
		})
	}
}

// TestRegisterTask runs register-task.ps1 under another task name and checks
// what Task Scheduler stored. It changes the system's scheduled tasks, so it
// runs only in CI.
func TestRegisterTask(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Task Scheduler is part of Windows")
	}
	if os.Getenv("CI") == "" {
		t.Skip("registers a scheduled task, so it runs only in CI; set CI=1 to run it anyway")
	}
	const name = "schwabtx snapshot test"
	run := func(args ...string) (stdout, stderr string, err error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "powershell.exe", append([]string{"-NoProfile", "-NonInteractive"}, args...)...)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err = cmd.Run()
		return out.String(), errOut.String(), err
	}
	// powershell runs script, which must end with an explicit exit code: in
	// -Command mode the code is 1 whenever the last command failed, even with
	// its error silenced. Progress records would come out as CLIXML.
	powershell := func(script string) (string, string, error) {
		t.Helper()
		units := utf16.Encode([]rune("$ErrorActionPreference = 'Stop'\n$ProgressPreference = 'SilentlyContinue'\n" + script))
		raw := make([]byte, 2*len(units))
		for i, u := range units {
			binary.LittleEndian.PutUint16(raw[2*i:], u)
		}
		// -EncodedCommand takes the script as base64 of its UTF-16LE bytes.
		return run("-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	}
	unregister := "if (Get-ScheduledTask -TaskName '" + name + "' -ErrorAction SilentlyContinue) {\n" +
		"    Unregister-ScheduledTask -TaskName '" + name + "' -Confirm:$false\n}\nexit 0"
	if _, errOut, err := powershell(unregister); err != nil {
		t.Fatalf("removing a task left from an earlier run: %v\n%s", err, errOut)
	}
	t.Cleanup(func() {
		if _, errOut, err := powershell(unregister); err != nil {
			t.Errorf("removing the task: %v\n%s", err, errOut)
		}
	})
	// Run the script the way the README does.
	register := func(at string) (string, string, error) {
		return run("-ExecutionPolicy", "Bypass", "-File", filepath.Join("schedule", "register-task.ps1"), "-TaskName", name, "-At", at)
	}

	// Registering again replaces the task, so the second time is the one
	// that counts.
	for _, at := range []string{"21:05", "03:07"} {
		if out, errOut, err := register(at); err != nil || !strings.Contains(out, "Registered the task") {
			t.Fatalf("register-task.ps1 -At %s: %v\n%s%s", at, err, out, errOut)
		}
	}
	if out, errOut, err := register("9pm"); err == nil || !strings.Contains(errOut, "24-hour") {
		t.Errorf("register-task.ps1 -At 9pm: %v\n%s%s\nwant it to fail, saying the time must be in 24-hour form", err, out, errOut)
	}

	out, errOut, err := powershell(`$task = Get-ScheduledTask -TaskName '` + name + `'
[pscustomobject]@{
    StartBoundary              = $task.Triggers[0].StartBoundary
    DaysInterval               = $task.Triggers[0].DaysInterval
    LogonType                  = [string]$task.Principal.LogonType
    RunLevel                   = [string]$task.Principal.RunLevel
    StartWhenAvailable         = $task.Settings.StartWhenAvailable
    DisallowStartIfOnBatteries = $task.Settings.DisallowStartIfOnBatteries
    StopIfGoingOnBatteries     = $task.Settings.StopIfGoingOnBatteries
    ExecutionTimeLimit         = $task.Settings.ExecutionTimeLimit
    Execute                    = $task.Actions[0].Execute
    Arguments                  = $task.Actions[0].Arguments
} | ConvertTo-Json
exit 0`)
	if err != nil {
		t.Fatalf("reading the task: %v\n%s%s", err, out, errOut)
	}
	var task struct {
		StartBoundary              string
		DaysInterval               int
		LogonType, RunLevel        string
		StartWhenAvailable         bool
		DisallowStartIfOnBatteries bool
		StopIfGoingOnBatteries     bool
		ExecutionTimeLimit         string
		Execute, Arguments         string
	}
	if err := json.Unmarshal([]byte(out), &task); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	// An offset such as -07:00 or Z would tie the time to UTC, so the task
	// would move by an hour when daylight saving time starts or ends.
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT03:07:00$`).MatchString(task.StartBoundary) {
		t.Errorf("starts at %q, want 03:07 local time without an offset", task.StartBoundary)
	}
	if task.DaysInterval != 1 {
		t.Errorf("runs every %d days, want every day", task.DaysInterval)
	}
	if task.LogonType != "Interactive" || task.RunLevel != "Limited" {
		t.Errorf("runs with logon type %s at run level %s, want Interactive and Limited", task.LogonType, task.RunLevel)
	}
	if !task.StartWhenAvailable || task.DisallowStartIfOnBatteries || task.StopIfGoingOnBatteries {
		t.Errorf("settings: start when available %v, not on batteries %v, stop on batteries %v; want true, false, false",
			task.StartWhenAvailable, task.DisallowStartIfOnBatteries, task.StopIfGoingOnBatteries)
	}
	if task.ExecutionTimeLimit != "PT30M" {
		t.Errorf("time limit %s, want PT30M", task.ExecutionTimeLimit)
	}
	script, err := filepath.Abs(filepath.Join("schedule", "snapshot.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.ToLower(task.Arguments)
	for _, want := range []string{"-executionpolicy bypass", "-windowstyle hidden", strings.ToLower(`-File "` + script + `"`)} {
		if !strings.EqualFold(task.Execute, "powershell.exe") || !strings.Contains(args, want) {
			t.Errorf("runs %s %s, want powershell.exe with %s", task.Execute, task.Arguments, want)
		}
	}
}
