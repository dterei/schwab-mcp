// Command schwabtx connects to the Charles Schwab Trader API and prints the
// most recent transactions across every account linked to the login.
//
// Usage:
//
//	schwabtx login           one-time browser login (repeat every 7 days)
//	schwabtx [flags]         print the most recent transactions
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	defaultBaseURL     = "https://api.schwabapi.com"
	defaultCallbackURL = "https://127.0.0.1"

	// maxDays bounds -lookback and -window so they cannot overflow a Duration.
	maxDays = 36500
)

// timeNow is swapped out in tests.
var timeNow = time.Now

type config struct {
	AppKey      string
	AppSecret   string
	CallbackURL string
	BaseURL     string
	TokenFile   string
}

func loadConfig() (config, error) {
	cfg := config{
		AppKey:      strings.TrimSpace(os.Getenv("SCHWAB_APP_KEY")),
		AppSecret:   strings.TrimSpace(os.Getenv("SCHWAB_APP_SECRET")),
		CallbackURL: strings.TrimSpace(os.Getenv("SCHWAB_CALLBACK_URL")),
		BaseURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("SCHWAB_BASE_URL")), "/"),
		TokenFile:   strings.TrimSpace(os.Getenv("SCHWAB_TOKEN_FILE")),
	}
	if cfg.AppKey == "" || cfg.AppSecret == "" {
		return cfg, errors.New("SCHWAB_APP_KEY and SCHWAB_APP_SECRET must be set (see README.md)")
	}
	if cfg.CallbackURL == "" {
		cfg.CallbackURL = defaultCallbackURL
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.TokenFile == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return cfg, fmt.Errorf("cannot find a config directory, set SCHWAB_TOKEN_FILE: %w", err)
		}
		cfg.TokenFile = filepath.Join(dir, "schwabtx", "token.json")
	}
	return cfg, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case errors.Is(err, context.Canceled):
		os.Exit(130)
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := "transactions"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "login":
		if len(args) > 0 {
			return errors.New("login takes no arguments")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		return login(ctx, newAuthenticator(cfg, newHTTPClient()), stdin, stdout)
	case "transactions":
		return runTransactions(ctx, args, stdout, stderr)
	case "help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `schwabtx prints your most recent Charles Schwab transactions.

  schwabtx login        one-time browser login (Schwab makes you repeat it every 7 days)
  schwabtx [flags]      print the most recent transactions across all accounts

Run "schwabtx -h" for flags. Credentials come from SCHWAB_APP_KEY and
SCHWAB_APP_SECRET; see README.md.
`)
}

func runTransactions(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("schwabtx", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 100, "how many of the most recent transactions to print")
	lookback := fs.Int("lookback", 365, "how many days back to search")
	window := fs.Int("window", 60, "days per API request")
	types := fs.String("types", "", "comma-separated transaction types (default: all)")
	asJSON := fs.Bool("json", false, "print Schwab's raw JSON instead of a table (includes full account numbers)")
	showAccounts := fs.Bool("show-accounts", false, "print full account numbers in the table instead of the last 4 digits")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *n < 1 || *lookback < 1 || *window < 1 {
		return errors.New("-n, -lookback and -window must be at least 1")
	}
	if *lookback > maxDays || *window > maxDays {
		return fmt.Errorf("-lookback and -window must be at most %d days", maxDays)
	}
	typeList, err := parseTypes(*types)
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

	accounts, err := c.accountNumbers(ctx)
	if err != nil {
		return fmt.Errorf("listing accounts: %w", err)
	}
	if len(accounts) == 0 {
		return errors.New("Schwab returned no accounts; run `schwabtx login` again and tick the accounts to share")
	}

	txs, err := fetchRecent(ctx, c, accounts, fetchOptions{
		N:        *n,
		Lookback: time.Duration(*lookback) * 24 * time.Hour,
		Window:   time.Duration(*window) * 24 * time.Hour,
		Types:    typeList,
		Now:      timeNow(),
		Warn:     func(msg string) { fmt.Fprintln(stderr, "warning:", msg) },
	})
	if err != nil {
		return err
	}

	if *asJSON {
		return printJSON(stdout, txs)
	}
	fmt.Fprintf(stderr, "%d transactions from %d accounts (searched the last %d days)\n", len(txs), len(accounts), *lookback)
	return printTable(stdout, txs, *showAccounts)
}

// parseTypes turns the -types flag into a list, defaulting to every type.
func parseTypes(flagValue string) ([]string, error) {
	if strings.TrimSpace(flagValue) == "" {
		return allTransactionTypes, nil
	}
	var list []string
	for t := range strings.SplitSeq(flagValue, ",") {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if !slices.Contains(allTransactionTypes, t) {
			return nil, fmt.Errorf("unknown transaction type %q in -types; valid types: %s", t, strings.Join(allTransactionTypes, ", "))
		}
		list = append(list, t)
	}
	if len(list) == 0 {
		return nil, errors.New("-types has no transaction types in it")
	}
	return list, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
