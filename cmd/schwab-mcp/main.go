// Command schwab-mcp is an MCP server that reads your Charles Schwab
// accounts: their balances, positions and transactions, from Schwab's
// Trader API and from the local history that schwabtx keeps. An MCP client
// starts it and speaks the protocol on its stdin and stdout. It only reads:
// it never places, changes or cancels an order, never writes the history,
// and never logs in, which schwabtx login does in a terminal.
//
// Usage:
//
//	schwab-mcp [--config PATH] [--call-timeout D] [--max-calls N] [--log-level L]
//	schwab-mcp --check [--config PATH]
//	schwab-mcp --version
//
// schwabtx setup saves the config file and prints the command and
// arguments to give a client. README.md describes the flags, the config
// file, the exit codes and the logs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/mcpserver"
)

// usage is what -h prints on stderr, and a command line that a person
// typed wrong.
const usage = `Usage:
  schwab-mcp [flags]           serve the MCP client that started it, on stdin and stdout
  schwab-mcp --check [flags]   print what the server finds of its files, the login and the store
  schwab-mcp --version         print the version

Flags:
  --config PATH      the config file that schwabtx setup saved (default: schwabtx/config.json
                     in your user config directory); a leading ~/ is your home folder
  --call-timeout D   each call's budget, from 5s to 240s (default 45s)
  --max-calls N      how many calls may run at once, from 1 to 16 (default 4)
  --log-level L      debug, info, warn or error (default warn); info adds a line per call
  --check            print what auth_status would say, for a person, and exit
  --version          print the version and exit

When a client starts the server, a flag or an argument that it cannot use does
not stop it: auth_status lists it, and every tool says how to fix it.
`

// shutdownWait is how long the process lives after a signal before it
// exits whatever is still running (design 5.8).
const shutdownWait = 250 * time.Millisecond

// testTools, when a test sets it, registers the test's own tools on the
// server before it serves. The command itself has none.
var testTools func(s *mcpserver.Server)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, stdinIsTerminal)) //nolint:forbidigo // --check and --version print to stdout, before the guard runs
}

// run runs the command with args and returns its exit code: 0 when stdin
// ends or a signal comes, and after --check and --version; 1 when the
// server cannot start or its session fails, with a line on stderr; 2 for a
// command line that a person typed wrong. stdout takes only --check's and
// --version's output: the protocol goes to the stdout that guardStdout
// keeps for it. terminal reports whether a person typed the command.
func run(args []string, stdout, stderr io.Writer, terminal func() bool) int {
	o, err := parseArgs(args, stderr, terminal)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2
	}
	if o.version {
		fmt.Fprintln(stdout, "schwab-mcp", version())
		return 0
	}
	if o.check {
		return check(o, stdout, stderr)
	}

	protocol, err := guardStdout()
	if err != nil {
		fmt.Fprintln(stderr, "schwab-mcp: keeping stdout for the protocol:", err)
		return 1
	}
	srv, err := newServer(o, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "schwab-mcp:", err)
		return 1
	}
	if testTools != nil {
		testTools(srv)
	}

	// The first signal ends the root context, which ends every running
	// call at once, and exits soon after whatever still runs: Run may not
	// return while stdin blocks.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
		time.AfterFunc(shutdownWait, func() { os.Exit(0) })
	}()

	err = srv.Run(ctx, &mcp.IOTransport{Reader: os.Stdin, Writer: protocol, MaxLineLength: 4 << 20})
	cancel()
	srv.Close() // Close logs what went wrong in closing the store
	if err != nil {
		return 1 // Run logged why
	}
	return 0
}

// check prints what auth_status says, for a person, and the version.
func check(o options, stdout, stderr io.Writer) int {
	srv, err := newServer(o, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "schwab-mcp:", err)
		return 1
	}
	defer srv.Close()
	fmt.Fprintln(stdout, "schwab-mcp", version())
	if err := srv.Check(context.Background(), stdout); err != nil {
		fmt.Fprintln(stderr, "schwab-mcp:", err)
		return 1
	}
	return 0
}

// newServer reads the config file that o names, and returns the server for
// it and o, which logs on stderr.
func newServer(o options, stderr io.Writer) (*mcpserver.Server, error) {
	cfg := config.ForServer(os.Getenv, o.config)
	cfg.Problems = append(cfg.Problems, o.problems...)
	return mcpserver.New(mcpserver.Options{
		Config:      cfg,
		Version:     version(),
		Logger:      slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: o.logLevel})),
		CallTimeout: o.callTimeout,
		MaxCalls:    o.maxCalls,
	})
}

// version is this build's version: the module's, as go install records
// it, or as go build finds it in a checkout.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// stdinIsTerminal reports whether stdin is a character device, as it is
// when a person typed the command; a client gives the server a pipe.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// options are what the command line asks for.
type options struct {
	config         string
	callTimeout    time.Duration
	maxCalls       int
	logLevel       slog.Level
	check, version bool
	// problems are what a client's command line held that the server could
	// not use, which auth_status and every tool report.
	problems []config.Problem
}

// parseArgs reads args. When a person typed the command, a flag that the
// server does not take, a value that a flag does not take or an argument
// that is no flag prints what is wrong and the usage on stderr, and
// parseArgs returns errUsage. When a client started the server, which shows
// only "failed" for a server that exits, each is a problem that auth_status
// and every tool report, and the server runs with the flags it could read:
// a path with a space that was not quoted is the usual cause. -h prints
// the usage and returns flag.ErrHelp.
func parseArgs(args []string, stderr io.Writer, terminal func() bool) (options, error) {
	o := options{callTimeout: 45 * time.Second, maxCalls: 4, logLevel: slog.LevelWarn}
	fs := flagSet(&o)
	stray, bad := false, map[string]bool{}
	wrong := func(problem string) error {
		if terminal() {
			fmt.Fprintf(stderr, "schwab-mcp: %s\n\n%s", problem, usage)
			return errUsage
		}
		return nil
	}
	for len(args) > 0 {
		err := fs.Parse(args)
		rest := fs.Args()
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprint(stderr, usage)
			return o, flag.ErrHelp
		case err != nil:
			if err := wrong(err.Error()); err != nil {
				return o, err
			}
			setting := "arguments"
			if len(rest) == len(args) {
				rest = rest[1:] // refused without being taken, as "---x" is
			} else if name := lastFlag(args[:len(args)-len(rest)]); fs.Lookup(name) != nil {
				setting = "--" + name
			}
			bad[setting] = true
		case len(rest) > 0:
			if err := wrong(fmt.Sprintf("the argument %q is no flag; quote a path that holds a space", rest[0])); err != nil {
				return o, err
			}
			stray = true
			rest = rest[1:]
		}
		args = rest
	}
	if stray {
		o.problems = append(o.problems, config.Problem{Code: mcpserver.ProblemStrayArgument, Setting: "arguments"})
	}
	for _, setting := range slices.Sorted(maps.Keys(bad)) {
		o.problems = append(o.problems, config.Problem{Code: mcpserver.ProblemBadFlag, Setting: setting})
	}
	o.config = expandHome(o.config)
	return o, nil
}

// flagSet returns the flags, each of which sets its field of o. It prints
// nothing: parseArgs says what is wrong, and the usage is usage's.
func flagSet(o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("schwab-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&o.config, "config", "", "")
	fs.Var(&durationFlag{v: &o.callTimeout, lo: 5 * time.Second, hi: 240 * time.Second}, "call-timeout", "")
	fs.Var(&countFlag{v: &o.maxCalls, lo: 1, hi: 16}, "max-calls", "")
	fs.Var(&levelFlag{v: &o.logLevel}, "log-level", "")
	fs.BoolVar(&o.check, "check", false, "")
	fs.BoolVar(&o.version, "version", false, "")
	return fs
}

// errUsage means that a person typed the command line wrong, and has been
// shown the usage.
var errUsage = errors.New("the command line is wrong")

// lastFlag returns the name of the last flag in args, the ones the flag
// package read before it failed: the flag it failed on, or the one whose
// value it failed on.
func lastFlag(args []string) string {
	for _, arg := range slices.Backward(args) {
		if name, ok := strings.CutPrefix(arg, "-"); ok && name != "" {
			name = strings.TrimPrefix(name, "-")
			name, _, _ = strings.Cut(name, "=")
			return name
		}
	}
	return ""
}

// expandHome writes a leading ~/ of path as the home folder, as a shell
// would have.
func expandHome(path string) string {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok && runtime.GOOS == "windows" {
		rest, ok = strings.CutPrefix(path, `~\`)
	}
	if !ok {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path // a relative path, which the config reports
	}
	return filepath.Join(home, rest)
}

// durationFlag is a duration from lo to hi.
type durationFlag struct {
	v      *time.Duration
	lo, hi time.Duration
}

func (f *durationFlag) String() string {
	if f.v == nil {
		return ""
	}
	return f.v.String()
}

func (f *durationFlag) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil || d < f.lo || d > f.hi {
		return fmt.Errorf("takes a duration from %s to %s, such as 45s", seconds(f.lo), seconds(f.hi))
	}
	*f.v = d
	return nil
}

// seconds writes d in whole seconds, as the usage does.
func seconds(d time.Duration) string { return strconv.Itoa(int(d/time.Second)) + "s" }

// countFlag is a whole number from lo to hi.
type countFlag struct {
	v      *int
	lo, hi int
}

func (f *countFlag) String() string {
	if f.v == nil {
		return ""
	}
	return strconv.Itoa(*f.v)
}

func (f *countFlag) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < f.lo || n > f.hi {
		return fmt.Errorf("takes a number from %d to %d", f.lo, f.hi)
	}
	*f.v = n
	return nil
}

// levelFlag is a log level: debug, info, warn or error.
type levelFlag struct{ v *slog.Level }

func (f *levelFlag) String() string {
	if f.v == nil {
		return ""
	}
	return strings.ToLower(f.v.String())
}

func (f *levelFlag) Set(s string) error {
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	l, ok := levels[strings.ToLower(s)]
	if !ok {
		return errors.New("takes debug, info, warn or error")
	}
	*f.v = l
	return nil
}
