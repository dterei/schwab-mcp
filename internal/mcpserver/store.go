package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dterei/schwab-mcp/internal/history"
)

// The store is read through one history.Reader, opened lazily on the first
// call that needs it (design 12.4): never created, never written, never
// migrated. A failed open is not kept, so a store made or brought up to
// date later is found by the next call; a read that fails closes the
// reader, so the next call opens it again and reports what it finds.

var (
	// errNoStorePath means that the config names no store that may be
	// used, as when config.json is one of the store's own files.
	errNoStorePath = errors.New("the config names no store that may be used")
	// errStoreRead marks any other failure of the store, which a call
	// answers as store_unusable: damaged, on a network file system, not a
	// store, an orphan journal, or a read that failed.
	errStoreRead = errors.New("the store could not be read")
)

// storeHandle is the server's lazy reader of the store. It is safe for
// concurrent use.
type storeHandle struct {
	s    *Server
	path string // "" when the config names none that may be used
	opts history.ReaderOptions
	// open is held, one at a time, by whoever opens the reader; a call that
	// waits for it gives up when its context ends.
	open chan struct{}
	mu   sync.Mutex // guards cur
	cur  *reading
}

// reading is an open reader and the reads that use it, so that a reader
// that a failed read retires closes only once its last read ends.
type reading struct {
	r       *history.Reader
	users   int
	retired bool
}

func newStoreHandle(s *Server, path string, lockWait, busy time.Duration) *storeHandle {
	return &storeHandle{
		s: s, path: path, open: make(chan struct{}, 1),
		opts: history.ReaderOptions{
			Path: path, LockWait: lockWait, BusyTimeout: busy,
			Warn: func(msg string) { s.log.Warn(msg, "component", "store") },
		},
	}
}

// read runs fn with the store's reader, opening it first when none is
// open. A failure of the store is one that storeCode words: the reader's
// own errors, and errStoreRead for any other; a read that fails retires
// the reader, which closes once no read uses it.
func (h *storeHandle) read(ctx context.Context, fn func(r *history.Reader) error) error {
	cur, err := h.acquire(ctx)
	if err != nil {
		return err
	}
	err = fn(cur.r)
	h.release(cur, err != nil && ctx.Err() == nil)
	if err != nil {
		return h.wordable(ctx, err)
	}
	return nil
}

// acquire returns the open reader, opening it when none is, for one read.
func (h *storeHandle) acquire(ctx context.Context) (*reading, error) {
	if h.path == "" {
		return nil, errNoStorePath
	}
	if cur := h.use(); cur != nil {
		return cur, nil
	}
	select {
	case h.open <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-h.open }()
	if cur := h.use(); cur != nil { // another call opened it meanwhile
		return cur, nil
	}
	r, err := history.OpenReader(ctx, h.opts)
	if err != nil {
		return nil, h.wordable(ctx, err)
	}
	numbers, err := r.AccountNumbers(ctx)
	if err != nil {
		r.Close()
		return nil, h.wordable(ctx, err)
	}
	for _, n := range numbers {
		h.s.mask.known.Add(n.Number)
	}
	cur := &reading{r: r, users: 1}
	h.mu.Lock()
	h.cur = cur
	h.mu.Unlock()
	return cur, nil
}

// use returns the open reader, counting one more read of it, or nil.
func (h *storeHandle) use() *reading {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		return nil
	}
	h.cur.users++
	return h.cur
}

// release ends a read of cur; failed retires cur, so that the next call
// opens the store again. A retired reader closes when its last read ends.
func (h *storeHandle) release(cur *reading, failed bool) {
	h.mu.Lock()
	cur.users--
	if failed && h.cur == cur {
		cur.retired = true
		h.cur = nil
	}
	closeIt := cur.retired && cur.users == 0
	h.mu.Unlock()
	if closeIt {
		if err := cur.r.Close(); err != nil {
			h.s.log.Warn("closing the store's reader", "component", "store", "error", err)
		}
	}
}

// wordable returns err, a failure of the store, as storeCode words it:
// the reader's own kinds as they are, and errStoreRead for any other. The
// end of ctx stays itself. The store's message, which holds absolute paths
// and names schwabtx's flags, is logged, never shown.
func (h *storeHandle) wordable(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	if _, ok := storeCode(err); ok {
		return err
	}
	return fmt.Errorf("%w: %w", errStoreRead, err)
}

// accounts returns the store's accounts, each with its full number and as
// the accounts view shows it, read in one go by the directory.
func (h *storeHandle) accounts(ctx context.Context) ([]history.NumberedAccount, []history.ReaderAccount, error) {
	var numbered []history.NumberedAccount
	var views []history.ReaderAccount
	err := h.read(ctx, func(r *history.Reader) error {
		var err error
		if numbered, err = r.AccountNumbers(ctx); err != nil {
			return err
		}
		views, err = r.Accounts(ctx)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	for _, n := range numbered {
		h.s.mask.known.Add(n.Number)
	}
	return numbered, views, nil
}

// close closes the open reader, if any; a read that still uses it closes
// it when it ends.
func (h *storeHandle) close() error {
	h.mu.Lock()
	cur := h.cur
	h.cur = nil
	if cur == nil {
		h.mu.Unlock()
		return nil
	}
	cur.retired = true
	closeIt := cur.users == 0
	h.mu.Unlock()
	if closeIt {
		return cur.r.Close()
	}
	return nil
}
