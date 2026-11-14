package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	configfile "github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// runSetup saves config.json, for the MCP server: the app key and secret,
// from SCHWAB_APP_KEY and SCHWAB_APP_SECRET or as the file had them, and
// the token file, the store and the rate file as schwabtx resolves them in
// this shell, made absolute, the token file with every link followed, as
// login saves to it, so that the server uses the same files whatever
// environment its client gives it. With -print it says what it
// would save, and saves nothing. The secret is never printed, and the key
// only as its last three characters.
func runSetup(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("schwabtx setup", flag.ContinueOnError)
	fl.SetOutput(stderr)
	store := storeFlag(fl)
	dryRun := fl.Bool("print", false, "print what setup would save, and save nothing")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), `Usage of schwabtx setup:

  schwabtx setup [-store PATH] [-print]

Saves config.json, readable only by you, in schwabtx's folder of your
config directory: the app key and secret, from SCHWAB_APP_KEY and
SCHWAB_APP_SECRET or as config.json has them already, and the token file,
the store and the rate file as schwabtx finds them in this terminal. The MCP
server reads all of it, and schwabtx reads the key and secret from it when
the environment has none. Give -store as the store commands take it.

`)
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", plainCell(fl.Arg(0)))
	}
	path, err := configfile.DefaultFile()
	if err != nil {
		return err
	}
	old, err := configfile.Read(path)
	if _, relative := errors.AsType[*configfile.RelativePathError](err); relative || errors.Is(err, configfile.ErrMissing) {
		err = nil // its paths are replaced
	}
	switch {
	case errors.Is(err, configfile.ErrNewer):
		return fmt.Errorf("%s was written by a newer schwabtx; run that version's setup", path)
	case err != nil:
		return fmt.Errorf("%s: %w; move it aside, and run schwabtx setup again", path, err)
	}

	// The key and secret from the environment, else as the file had them.
	type setting struct{ value, from string }
	pick := func(variable, had string) setting {
		if v := strings.TrimSpace(os.Getenv(variable)); v != "" {
			return setting{v, "from " + variable}
		}
		return setting{had, "as config.json had it"}
	}
	key, secret := pick("SCHWAB_APP_KEY", old.AppKey), pick("SCHWAB_APP_SECRET", old.AppSecret)
	if key.value == "" || secret.value == "" {
		return errors.New("SCHWAB_APP_KEY and SCHWAB_APP_SECRET must be set for schwabtx setup, which saves them (see README.md)")
	}

	// The paths as every command finds them in this shell, each made
	// absolute: the token file where a login keeps the login (loadConfig's,
	// as schwab.Auth resolves it), the store (-store as the store commands
	// take it) and the client's rate file. The server takes the base URL
	// that a login takes.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	baseURL := "" // another server than Schwab's, such as the fake, or none
	if strings.TrimSpace(os.Getenv("SCHWAB_BASE_URL")) != "" && !schwabsAddress(cfg.BaseURL) {
		baseURL = cfg.BaseURL
	}
	auth, err := schwab.NewAuth(schwab.Config{AppKey: key.value, AppSecret: secret.value, BaseURL: baseURL, TokenFile: cfg.TokenFile})
	if err != nil {
		return err
	}
	tokenFile := auth.TokenFile() // with links followed, as login saves to it
	storeFile, err := storePath(*store)
	if err != nil {
		return err
	}
	rateFile, err := filepath.Abs(schwab.DefaultRateFile(key.value, tokenFile))
	if err != nil {
		return fmt.Errorf("finding the rate file's path: %w", err)
	}
	// Nothing that the server opens may be one of the store's files. The
	// token file is checked under the name that its path gives it too:
	// IsCompanion finds two names of one file by the file or its folder,
	// and a file not made yet, in a folder not made yet, has neither, so
	// that a link in one path and not the other would hide it.
	named, err := filepath.Abs(cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("finding the token file's path: %w", err)
	}
	for _, f := range []struct{ what, path string }{
		{"config.json", path}, {"the token file", tokenFile}, {"the token file", named}, {"the rate file", rateFile},
	} {
		if name, ok := companionOf(f.path, storeFile); ok {
			return fmt.Errorf("%s, %s, is the store at %s or one of its files (%s); give the store another path", f.what, f.path, storeFile, name)
		}
	}

	f := old
	f.AppKey, f.AppSecret, f.BaseURL = key.value, secret.value, baseURL
	f.TokenFile, f.StoreFile, f.RateFile = tokenFile, storeFile, rateFile
	if !*dryRun {
		if err := configfile.Save(path, f); err != nil {
			return fmt.Errorf("saving %s: %w", path, err)
		}
	}

	var b strings.Builder
	head := "Saved"
	if *dryRun {
		head = "Would save"
	}
	fmt.Fprintf(&b, "%s %s, readable only by you:\n", head, path)
	keyLine := key.from
	if r := []rune(key.value); len(r) >= 12 {
		keyLine += " (ends in ..." + string(r[len(r)-3:]) + ")"
	}
	from := func(variable string) string {
		if strings.TrimSpace(os.Getenv(variable)) != "" {
			return "from " + variable
		}
		return "default"
	}
	storeFrom := from("SCHWAB_STORE_FILE")
	if strings.TrimSpace(*store) != "" {
		storeFrom = "from -store"
	}
	for _, line := range [][2]string{
		{"app key", keyLine},
		{"app secret", secret.from},
		{"token file", tokenFile + " (" + from("SCHWAB_TOKEN_FILE") + ")"},
		{"store", storeFile + " (" + storeFrom + ")"},
		{"rate file", rateFile + " (default)"},
	} {
		fmt.Fprintf(&b, "  %-12s %s\n", line[0], line[1])
	}
	if baseURL != "" {
		host := apiHost(baseURL) + " (from SCHWAB_BASE_URL)"
		if u, err := url.Parse(baseURL); err != nil || !strings.EqualFold(u.Hostname(), schwabHost) {
			host += ", not Schwab's"
		}
		fmt.Fprintf(&b, "  %-12s %s\n", "api host", host)
	}
	if *dryRun {
		b.WriteString("nothing was saved (-print)\n")
	} else {
		b.WriteString("schwabtx now reads the key and secret from this file when the environment\nhas none, so .env needs them no longer.\n")
	}
	_, err = io.WriteString(stdout, b.String())
	return err
}

// apiHost returns the host of the base URL, the API's address.
func apiHost(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return baseURL
}

// schwabHost is the host of Schwab's own API, schwab.DefaultBaseURL's.
var schwabHost = apiHost(schwab.DefaultBaseURL)

// schwabsAddress reports whether baseURL, without its trailing "/", is
// schwab.DefaultBaseURL, where every client goes without a base URL,
// written in any case and with or without its port.
func schwabsAddress(baseURL string) bool {
	u, err := url.Parse(baseURL)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), schwabHost) && (u.Port() == "" || u.Port() == "443") &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

// readConfig reads config.json where setup saves it, and returns its path:
// ErrMissing when there is none, or no config directory to hold one. A
// file with a relative path in it gives the rest of what it holds, since
// schwabtx takes no path from it.
func readConfig() (string, configfile.File, error) {
	path, err := configfile.DefaultFile()
	if err != nil {
		return "", configfile.File{}, configfile.ErrMissing
	}
	f, err := configfile.Read(path)
	if _, relative := errors.AsType[*configfile.RelativePathError](err); relative {
		err = nil
	}
	return path, f, err
}

// savedApp returns the app key and secret that config.json holds, or ""
// for each when there is no config.json, or it holds none. A file that
// cannot be read is an error that names it.
func savedApp() (key, secret string, err error) {
	path, f, err := readConfig()
	switch {
	case errors.Is(err, configfile.ErrMissing):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("%s could not be read: %w", path, err)
	}
	return f.AppKey, f.AppSecret, nil
}

// savedConfig returns what config.json holds for the MCP server, and
// whether there is one that can be read.
func savedConfig() (configfile.File, bool) {
	_, f, err := readConfig()
	return f, err == nil
}

// warnLoginDrift says on stderr when config.json, which the MCP server
// reads, names another token file than tokenFile, the file that login has
// just saved the login to (schwab.Auth's TokenFile), with what points the
// server here. Without config.json, as before schwabtx setup, it says
// nothing.
func warnLoginDrift(tokenFile string, stderr io.Writer) {
	f, ok := savedConfig()
	// One file by name, or under two names (sameStore compares any files).
	if !ok || f.TokenFile == "" || sameStore(tokenFile, f.TokenFile) {
		return
	}
	fmt.Fprintf(stderr, "warning: the MCP server reads its login from %s (config.json); run schwabtx setup in this terminal to point it here\n", f.TokenFile)
}

// warnStoreDrift says on stderr when config.json, which the MCP server
// reads, names another store than the one at path, which a store command
// opens, with what points the server here. Without config.json it says
// nothing.
func warnStoreDrift(path string, stderr io.Writer) {
	f, ok := savedConfig()
	if !ok || f.StoreFile == "" || sameStore(path, f.StoreFile) {
		return
	}
	fmt.Fprintf(stderr, "warning: the MCP server reads the store at %s (config.json); "+
		"run schwabtx setup in this terminal, with the same -store, to point it here\n", f.StoreFile)
}
