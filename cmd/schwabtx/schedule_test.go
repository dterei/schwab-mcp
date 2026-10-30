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
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// TestMain lets the tests of the scheduler scripts in schedule/ use this test
// binary as schwabtx: with SCHWABTX_TEST_MAIN=1 in its environment it runs
// the command instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv("SCHWABTX_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// scriptRun is what a scheduler script did: its exit code, what it printed,
// what it logged, and the notifications it showed, as far as the test can
// tell.
type scriptRun struct {
	code           int
	stdout, stderr string
	log            string
	notifications  string
}

// scheduler runs one of the scripts in schedule/ the way a scheduler would.
type scheduler struct {
	name   string
	script string // path in this repository
	binary string // name of schwabtx in the Go bin folder
	run    func(t *testing.T, script, home string, env []string, args ...string) scriptRun
}

// schedulers returns the scripts this system can run: snapshot.sh on Unix
// systems, snapshot.ps1 with each PowerShell found on Windows.
func schedulers(t *testing.T) []scheduler {
	if runtime.GOOS != "windows" {
		return []scheduler{{name: "sh", script: "snapshot.sh", binary: "schwabtx", run: runShScript}}
	}
	var list []scheduler
	for _, shell := range []string{"powershell.exe", "pwsh.exe"} {
		if _, err := exec.LookPath(shell); err != nil {
			t.Logf("%s not found: %v", shell, err)
			continue
		}
		list = append(list, scheduler{
			name: shell, script: "snapshot.ps1", binary: "schwabtx.exe",
			run: func(t *testing.T, script, home string, env []string, args ...string) scriptRun {
				t.Helper()
				return runPowerShellScript(t, shell, script, home, env, args...)
			},
		})
	}
	if len(list) == 0 {
		t.Skip("no PowerShell found")
	}
	return list
}

func runShScript(t *testing.T, script, home string, env []string, args ...string) scriptRun {
	t.Helper()
	// Stand in for osascript, so that the notifications on macOS are recorded
	// instead of shown, and for sleep, so that tries come right after each
	// other.
	bin := filepath.Join(home, "stub-bin")
	notifications := filepath.Join(home, "notifications.txt")
	stubs := map[string]string{
		"osascript": "for arg; do last=$arg; done\nprintf '%s\\n' \"$last\" >> \"$NOTIFICATIONS\"\n",
		"sleep":     "exit 0\n",
	}
	for name, body := range stubs {
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The PATH that cron and launchd give a job, so that schwabtx is not
	// found on it.
	env = append(env, "PATH="+bin+":/usr/bin:/bin:/usr/sbin:/sbin", "NOTIFICATIONS="+notifications)
	r := runScript(t, env, "sh", append([]string{script}, args...)...)
	r.notifications = readIfExists(t, notifications)
	return r
}

func runPowerShellScript(t *testing.T, shell, script, home string, env []string, args ...string) scriptRun {
	t.Helper()
	args = append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script, "-RetryDelay", "0"}, args...)
	r := runScript(t, env, shell, args...)
	r.log = readIfExists(t, filepath.Join(home, "LocalAppData", "schwabtx", "snapshot.log"))
	// Whether a notification shows depends on the session the test runs in;
	// a failure to show one is logged.
	t.Logf("%s log:\n%s", shell, r.log)
	return r
}

func runScript(t *testing.T, env []string, name string, args ...string) scriptRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
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

// scriptEnv returns the environment a scheduler starts a script with: this
// process's, without any Schwab settings, and with the home and config
// directories in the test's temp space so the real ones stay out of reach.
func scriptEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name = strings.ToUpper(name); {
		case strings.HasPrefix(name, "SCHWAB"),
			name == "HOME", name == "XDG_CONFIG_HOME", name == "APPDATA", name == "LOCALAPPDATA", name == "USERPROFILE":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"APPDATA="+filepath.Join(home, "AppData"),
		"LOCALAPPDATA="+filepath.Join(home, "LocalAppData"),
		"USERPROFILE="+home,
	)
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

func copyFile(t *testing.T, src, dst string, perm os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, perm); err != nil {
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
	for _, s := range schedulers(t) {
		t.Run(s.name, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				refresh     string        // the saved refresh token
				obtainedAgo time.Duration // how long ago the login was
				noEnvFile   bool
				envArg      bool // pass the .env file as an argument
				goBin       bool // leave SCHWABTX unset, so the Go bin folder is used
				failDetails int  // fetches of the accounts that fail first
				wantRetries int
				wantCode    int
				wantSaved   bool
				wantInfo    string // in the summary
				wantProblem string // in the warning or error, and its notification
			}{
				{
					name: "saves a snapshot", refresh: testRefresh, obtainedAgo: time.Hour, goBin: true,
					wantSaved: true, wantInfo: "Saved a snapshot of 1 accounts with 2 positions to ",
				},
				{
					name: "takes the env file as an argument", refresh: testRefresh, obtainedAgo: time.Hour, envArg: true,
					wantSaved: true, wantInfo: "Saved a snapshot of 1 accounts with 2 positions to ",
				},
				{
					name: "warns a day before the login lapses", refresh: testRefresh, obtainedAgo: 6*24*time.Hour + 12*time.Hour,
					wantSaved: true, wantInfo: "Saved a snapshot of 1 accounts with 2 positions to ",
					wantProblem: "warning: your Schwab login expires in about 12 hours; run `schwabtx login` to renew it",
				},
				{
					name: "tries again after a failure", refresh: testRefresh, obtainedAgo: time.Hour, failDetails: 1,
					wantSaved: true, wantInfo: "Saved a snapshot of 1 accounts with 2 positions to ", wantRetries: 1,
				},
				{
					name: "fails once the login has lapsed", refresh: "stale-refresh", obtainedAgo: 8 * 24 * time.Hour,
					wantCode: 1, wantProblem: "error: fetching accounts: Schwab rejected the saved login", wantRetries: 2,
				},
				{
					name: "fails without an env file", noEnvFile: true,
					wantCode: 1, wantProblem: "error: cannot read ",
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newFakeSchwab(t)
					f.addAccount("11112222", 1000, 1, time.Now(), time.Hour)
					f.failDetails = tc.failDetails
					home := t.TempDir()
					cfg := filepath.Join(home, "cfg")
					tokenFile := filepath.Join(cfg, "token.json")
					snapshots := filepath.Join(cfg, "snapshots.jsonl")
					writeToken(t, tokenFile, tc.refresh, tc.obtainedAgo)

					// The script runs from a copy of the repository's layout, with
					// the .env at its root.
					repo := filepath.Join(home, "schwab-mcp")
					script := filepath.Join(repo, "cmd", "schwabtx", "schedule", s.script)
					copyFile(t, filepath.Join("schedule", s.script), script, 0o644)
					envFile := filepath.Join(repo, ".env")
					var args []string
					if tc.envArg {
						envFile = filepath.Join(home, "elsewhere.env")
						args = []string{envFile}
						if s.script == "snapshot.ps1" {
							args = []string{"-EnvFile", envFile}
						}
					}
					lines := []string{
						"# Schwab app settings",
						"SCHWAB_APP_KEY=" + testKey,
						"SCHWAB_APP_SECRET=" + testSecret,
						"SCHWAB_BASE_URL=" + f.srv.URL,
						"SCHWAB_TOKEN_FILE=" + tokenFile,
						"SCHWAB_SNAPSHOT_FILE=" + snapshots,
						"SCHWABTX_TEST_MAIN=1",
					}
					if tc.goBin {
						copyFile(t, self, filepath.Join(home, "go", "bin", s.binary), 0o755)
					} else {
						lines = append(lines, "SCHWABTX="+self)
					}
					if !tc.noEnvFile {
						if err := os.WriteFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}

					r := s.run(t, script, home, scriptEnv(home), args...)

					if r.code != tc.wantCode {
						t.Errorf("exit code %d, want %d\nstdout: %s\nstderr: %s\nlog: %s", r.code, tc.wantCode, r.stdout, r.stderr, r.log)
					}
					// snapshot.sh leaves the log to the scheduler: the summary
					// goes to stdout, and warnings and errors to stderr. The
					// PowerShell script writes the log itself and prints it too.
					info, problems := r.stdout, r.stderr
					if s.script == "snapshot.ps1" {
						info, problems = r.log, r.log
						if got := strings.Join(messages(t, r.stdout), "\n"); got != strings.Join(messages(t, r.log), "\n") {
							t.Errorf("printed %q but logged %q", r.stdout, r.log)
						}
						if r.stderr != "" {
							t.Errorf("stderr: %s", r.stderr)
						}
					}
					infoMsgs := strings.Join(messages(t, info), "\n")
					problemMsgs := strings.Join(messages(t, problems), "\n")
					if tc.wantInfo != "" && !strings.Contains(infoMsgs, tc.wantInfo+snapshots) {
						t.Errorf("no %q in the summary:\n%s", tc.wantInfo+snapshots, info)
					}
					if tc.wantProblem == "" && s.script == "snapshot.sh" && problemMsgs != "" {
						t.Errorf("unexpected warnings or errors:\n%s", problems)
					}
					if tc.wantProblem != "" && !strings.Contains(problemMsgs, tc.wantProblem) {
						t.Errorf("no %q in the warnings and errors:\n%s", tc.wantProblem, problems)
					}
					if got := strings.Count(infoMsgs, "; trying again in "); got != tc.wantRetries {
						t.Errorf("tried again %d times, want %d:\n%s", got, tc.wantRetries, info)
					}
					if s.script == "snapshot.sh" && strings.Count(problemMsgs, "error: ") > 1 {
						t.Errorf("errors of earlier tries went to stderr:\n%s", problems)
					}

					switch {
					case s.script == "snapshot.ps1":
						// A notification that Windows PowerShell shows leaves no
						// trace; one it cannot show, or that PowerShell 7 does not
						// try, is noted in the log.
						noted := strings.Contains(r.log, "notification")
						if tc.wantProblem == "" && noted {
							t.Errorf("notified without a warning or error:\n%s", r.log)
						}
						if tc.wantProblem != "" && s.name == "pwsh.exe" && !noted {
							t.Errorf("no note that PowerShell 7 cannot notify:\n%s", r.log)
						}
					case runtime.GOOS != "darwin":
						if r.notifications != "" {
							t.Errorf("notified %q off macOS", r.notifications)
						}
					case tc.wantProblem == "":
						if r.notifications != "" {
							t.Errorf("notified %q, want no notification", r.notifications)
						}
					case !strings.Contains(r.notifications, tc.wantProblem):
						t.Errorf("notified %q, want %q", r.notifications, tc.wantProblem)
					}

					saved := 0
					if b, err := os.ReadFile(snapshots); err == nil {
						saved = strings.Count(string(b), "\n")
					}
					if want := map[bool]int{true: 1}[tc.wantSaved]; saved != want {
						t.Errorf("saved %d snapshots, want %d", saved, want)
					}
				})
			}
		})
	}
}

// plistValue decodes the property list element start, whose start tag d has
// just read, into a string (for string and integer), a []any or a
// map[string]any.
func plistValue(d *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "string", "integer":
		var s string
		err := d.DecodeElement(&s, &start)
		return s, err
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

	const home = "/Users/YOU"
	const repo = home + "/schwab-mcp"
	got := fmt.Sprint(plist)
	want := fmt.Sprint(map[string]any{
		"Label":                 strings.TrimSuffix(name, ".plist"),
		"ProgramArguments":      []any{"/bin/sh", repo + "/cmd/schwabtx/schedule/snapshot.sh"},
		"StartCalendarInterval": map[string]any{"Hour": strconv.Itoa(21), "Minute": strconv.Itoa(5)},
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

// TestTaskSchedulerSteps registers the task the way schedule/README.md says,
// under another name, and checks what Task Scheduler stored. It changes the
// system's scheduled tasks, so it runs only in CI.
func TestTaskSchedulerSteps(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Task Scheduler is part of Windows")
	}
	if os.Getenv("CI") == "" {
		t.Skip("registers a scheduled task, so it runs only in CI; set CI=1 to run it anyway")
	}
	b, err := os.ReadFile(filepath.Join("schedule", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may have CRLF line endings.
	readme := strings.ReplaceAll(string(b), "\r\n", "\n")
	_, steps, ok1 := strings.Cut(readme, "\n## Windows\n")
	_, steps, ok2 := strings.Cut(steps, "```powershell\n")
	steps, _, ok3 := strings.Cut(steps, "\n```")
	if !ok1 || !ok2 || !ok3 || !strings.Contains(steps, "'schwabtx snapshot'") {
		t.Fatalf("no steps registering 'schwabtx snapshot' under ## Windows in schedule/README.md")
	}
	const name = "schwabtx snapshot test"
	steps = strings.ReplaceAll(steps, "'schwabtx snapshot'", "'"+name+"'")
	// A task left from an earlier run would make Register-ScheduledTask
	// fail, and what the steps print would get in the way of the JSON.
	script := "$ErrorActionPreference = 'Stop'\n" +
		"Unregister-ScheduledTask -TaskName '" + name + "' -Confirm:$false -ErrorAction SilentlyContinue\n" +
		"$null = & {\n" + steps + "\n}" + `
$task = Get-ScheduledTask -TaskName '` + name + `'
try {
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
} finally {
    Unregister-ScheduledTask -TaskName '` + name + `' -Confirm:$false
}
`
	// -EncodedCommand takes the script as base64 of its UTF-16LE bytes.
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(raw[2*i:], u)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	cmd.Dir = filepath.Join("..", "..") // the root of the repository, where the steps are run
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("registering the task: %v\n%s%s", err, stdout.String(), stderr.String())
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
	if err := json.Unmarshal(stdout.Bytes(), &task); err != nil {
		t.Fatalf("decoding %q: %v", stdout.String(), err)
	}
	// An offset such as -07:00 or Z would tie the time to UTC, so the task
	// would move by an hour when daylight saving time starts or ends.
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT21:05:00$`).MatchString(task.StartBoundary) {
		t.Errorf("starts at %q, want 21:05 local time without an offset", task.StartBoundary)
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
	script, err = filepath.Abs(filepath.Join("schedule", "snapshot.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(task.Execute, "powershell.exe") ||
		!strings.Contains(strings.ToLower(task.Arguments), strings.ToLower(`-File "`+script+`"`)) {
		t.Errorf("runs %s %s, want powershell.exe with -File %q", task.Execute, task.Arguments, script)
	}
}
