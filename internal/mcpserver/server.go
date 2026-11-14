package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Options configures a Server. main fills it from config.ForServer and the
// flags; tests fill every field.
type Options struct {
	// Config holds the paths, the key and secret, and where each came from,
	// and the problems that main found with its own arguments.
	Config  config.Config
	Version string
	// Logger is where the server logs, through the handler of design 5.4,
	// which masks every line: stderr, or a buffer in tests. Nil drops the
	// lines.
	Logger *slog.Logger
	// HTTPClient makes the requests to Schwab. Nil means schwab's default,
	// with a 30-second timeout.
	HTTPClient *http.Client
	// Client tunes the client; its RateFile comes from Config when that
	// names one. Tests set a fast rate and a short backoff.
	Client schwab.ClientConfig
	// Now tells the time that answers are as of and that the login lapses
	// by. Nil means time.Now.
	Now func() time.Time
	// Home is the user's home directory, which paths and log lines show as
	// ~. Empty means os.UserHomeDir's.
	Home string
	// CallTimeout is each call's budget, 45 seconds when 0.
	CallTimeout time.Duration
	// MaxCalls is how many calls may run at once, 4 when 0.
	MaxCalls int
	// SlotWait is how long a call waits for one of them, 10 seconds when 0.
	SlotWait time.Duration
	// PageBytes is the page budget over the whole answer (design 8.3),
	// 20,000 when 0, and MaxBytes the hard cap of every answer, 24,000 when
	// 0. New refuses a PageBytes not below MaxBytes.
	PageBytes, MaxBytes int
	// StoreLockWait and StoreBusy are the store's reader's waits (design
	// 12.4), 5 and 2 seconds when 0.
	StoreLockWait, StoreBusy time.Duration
	// onBackstop is told of every value that the backstop replaced, which
	// the tests fail on; backstopOff turns the backstop off, for the test
	// that shows the layers above it hold on their own.
	onBackstop  func(tool string, n int)
	backstopOff bool
}

// Server is the MCP server. It is safe for concurrent use.
type Server struct {
	o    Options
	log  *slog.Logger
	mask *masker
	mcp  *mcp.Server

	// auth and client are the login and the client for Schwab; nil without
	// the app key and secret, or when the API's address was refused
	// (authErr).
	auth    *schwab.Auth
	authErr error
	client  *schwab.Client

	accounts *directory
	store    *storeHandle

	// tools are the registered tools, and runners their calls by name, for
	// the tests and for resources that answer as a tool does.
	tools   []toolInfo
	runners map[string]func(ctx context.Context, args json.RawMessage) ([]byte, *toolErr)
	// fixedURIs and uriPatterns are what the resources serve, which the
	// middleware lets through to the SDK.
	fixedURIs   []string
	uriPatterns []*regexp.Regexp

	// slots holds a token for each running call that takes a slot.
	slots chan struct{}
	// root is the context that every handler's context derives from; it
	// ends at Close, or when Run's context ends.
	root       context.Context
	cancelRoot context.CancelFunc

	mu      sync.Mutex
	calls   int           // the running calls
	drained chan struct{} // closed when calls falls to 0, while Close waits

	closeOnce sync.Once
	closeErr  error
}

// New returns a server for o: its masker, its login and client when the
// app key and secret are set, its tools, resources and prompt. It reaches
// no network, opens no store and reads no token file: what cannot work is
// said in every tool's answer, since a client shows only "failed" for a
// server that does not start.
func New(o Options) (*Server, error) {
	if err := o.defaults(); err != nil {
		return nil, err
	}
	s := &Server{
		o:        o,
		mask:     newMasker(o.Home, o.Config.AppKey.Value, o.Config.AppSecret.Value),
		accounts: newDirectory(),
		runners:  map[string]func(context.Context, json.RawMessage) ([]byte, *toolErr){},
		slots:    make(chan struct{}, o.MaxCalls),
	}
	s.mask.off = o.backstopOff
	s.root, s.cancelRoot = context.WithCancel(context.Background())
	s.log = slog.New(&logHandler{inner: o.Logger.Handler(), m: s.mask})
	s.store = newStoreHandle(s, o.Config.StoreFile.Value, o.StoreLockWait, o.StoreBusy)
	if err := s.login(); err != nil {
		s.authErr = err
		s.log.Warn("the API's address cannot be used", "error", err)
	}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "schwab-mcp", Title: "Schwab (read-only)", Version: o.Version}, &mcp.ServerOptions{
		Instructions: instructions,
		Logger:       slog.New(atLeast{Handler: s.log.Handler(), min: slog.LevelWarn}),
		Capabilities: &mcp.ServerCapabilities{}, // no logging to the client: the server logs to stderr
		SetCacheable: s.setCacheable,
	})
	s.mcp.AddReceivingMiddleware(s.middleware)
	// Each tool file registers its tools, and the resources and prompts
	// that go with them.
	s.registerAuth()
	s.registerAccounts()
	s.registerTransactions()
	return s, nil
}

// defaults fills o's zero fields with their defaults, and checks the rest.
func (o *Options) defaults() error {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir() // none is shown as itself
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&o.CallTimeout, 45 * time.Second},
		{&o.SlotWait, 10 * time.Second},
		{&o.StoreLockWait, 5 * time.Second},
		{&o.StoreBusy, 2 * time.Second},
	} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	for _, n := range []struct {
		v   *int
		def int
	}{{&o.MaxCalls, 4}, {&o.PageBytes, 20_000}, {&o.MaxBytes, 24_000}} {
		if *n.v == 0 {
			*n.v = n.def
		}
	}
	switch {
	case o.CallTimeout < 0 || o.SlotWait < 0 || o.StoreLockWait < 0 || o.StoreBusy < 0:
		return errors.New("a wait of the server's options is negative")
	case o.MaxCalls < 0:
		return errors.New("the server's options allow no call")
	case o.PageBytes <= 0 || o.PageBytes >= o.MaxBytes:
		return fmt.Errorf("the page budget, %d bytes, is not below the cap, %d", o.PageBytes, o.MaxBytes)
	}
	return nil
}

// login makes the Auth and the client, when the app key and secret are
// set and the token file may be used. It reads nothing: NewAuth resolves
// the token file's path and checks the API's address, which it refuses
// when it is plain http to another computer.
func (s *Server) login() error {
	cfg := s.o.Config
	if cfg.AppKey.Value == "" || cfg.AppSecret.Value == "" || cfg.TokenFile.Value == "" {
		return nil
	}
	auth, err := schwab.NewAuth(schwab.Config{
		AppKey: cfg.AppKey.Value, AppSecret: cfg.AppSecret.Value, BaseURL: cfg.BaseURL.Value, TokenFile: cfg.TokenFile.Value,
		HTTPClient: s.o.HTTPClient, Now: s.o.Now,
		Warn: func(msg string) { s.log.Warn(msg, "component", "schwab") },
	})
	if err != nil {
		return err
	}
	cc := s.o.Client
	if cfg.RateFile.Value != "" {
		cc.RateFile = cfg.RateFile.Value
	}
	s.auth, s.client = auth, schwab.NewClient(auth, cc)
	s.mask.scrub = auth.Scrub
	return nil
}

// apiHost returns the host of the API's address, as config.json names it.
func (s *Server) apiHost() string {
	u, err := url.Parse(s.o.Config.BaseURL.Value)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// schwabHost is the host of Schwab's own API.
var schwabHost = strings.TrimPrefix(schwab.DefaultBaseURL, "https://")

// Run serves t until the client closes it or ctx ends; ctx is the root
// context, which every handler's context derives from (design 5.8), so
// that a signal ends the running handlers at once. It returns nil when the
// input ends or ctx ends, and the SDK's error when the session fails, as on
// a malformed line.
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	stop := context.AfterFunc(ctx, s.cancelRoot)
	defer stop()
	s.log.Info("starting", "version", s.o.Version, "config", s.o.Config)
	if host := s.apiHost(); host != "" && host != schwabHost {
		s.log.Warn("the API's address is not Schwab's", "host", host)
	}
	err := s.mcp.Run(ctx, t)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// MCP returns the SDK's server, for tests' in-memory transports; the root
// context of their calls ends at Close.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// setCacheable gives every result that carries cache fields a private
// scope, so that a client never shares account data, and a time to live of
// 0, but a day for the reference pages, which are compiled in (design
// 11.3). It runs with the SDK's lock held for the list methods, so it only
// sets fields.
func (s *Server) setCacheable(_ context.Context, req mcp.Request, c *mcp.Cacheable) {
	c.CacheScope = "private"
	c.TTLMs = 0
	if r, ok := req.(*mcp.ReadResourceRequest); ok && r.Params != nil && strings.HasPrefix(r.Params.URI, referencePrefix) {
		c.TTLMs = int((24 * time.Hour).Milliseconds())
	}
}

// referencePrefix starts the URI of each reference page.
const referencePrefix = "schwab://reference/"

// addResource registers r, a resource of a fixed URI, read by h, which the
// middleware then lets through.
func (s *Server) addResource(r *mcp.Resource, h mcp.ResourceHandler) {
	s.fixedURIs = append(s.fixedURIs, r.URI)
	s.mcp.AddResource(r, h)
}

// addResourceTemplate registers t, read by h, for the URIs that uri
// matches, which the middleware then lets through: it must match no URI
// that holds a character the template's variables would need to escape.
func (s *Server) addResourceTemplate(t *mcp.ResourceTemplate, uri *regexp.Regexp, h mcp.ResourceHandler) {
	s.uriPatterns = append(s.uriPatterns, uri)
	s.mcp.AddResourceTemplate(t, h)
}

// toolText runs the tool name with args, as a call of it would run, slot
// and budget included, for a resource that answers as the tool does: its
// answer's text, or its error.
func (s *Server) toolText(ctx context.Context, name string, args any) (string, *toolErr) {
	run, ok := s.runners[name]
	if !ok {
		return "", s.errorFor(codeInternal)
	}
	raw, err := encode(args)
	if err != nil {
		return "", s.errorFor(codeInternal)
	}
	b, e := run(ctx, raw)
	return string(b), e
}

// resourceError returns e, a tool's error, as a resource read's: one that
// names no single account is -32602 "resource not found", without the
// URI; any other is -32603 with the tool error's text (design 11.2).
func resourceError(e *toolErr) error {
	if e.code == codeAccountNotFound || e.code == codeAccountAmbiguous || e.code == codeAccountNotShared {
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "resource not found"}
	}
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: e.Error()}
}
