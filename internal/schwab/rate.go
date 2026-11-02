package schwab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

// Schwab counts requests by app key, not by login, and Accounts and Trading
// and Market Data requests count together: 120 a minute. Every process that
// uses the app key on this computer shares one limit below that, kept in a
// small file that each locks, reads and rewrites for every request.
//
// The limit is GCRA ("virtual scheduling"): the file holds the theoretical
// arrival time (TAT), and a request may go once now >= TAT - tolerance, which
// then moves the TAT to max(TAT, now) + interval. So after a quiet spell a
// burst may go at once, and after that one request each interval. A request
// takes its slot only at the moment it can go, so a caller that gives up
// while it waits leaves none behind, and a 429 that moves the TAT holds back
// every waiting request at once.
const (
	// DefaultRatePerMinute is how many requests a minute the processes using
	// an app key make together, unless ClientConfig says otherwise: enough
	// below Schwab's 120 to leave room for another tool and for answers
	// that arrive bunched.
	DefaultRatePerMinute = 100
	// DefaultBurst is how many requests may go at once after a quiet spell.
	DefaultBurst = 5

	// rateLockWait bounds the wait for another process's turn at the rate
	// file, which each holds only to read and write it.
	rateLockWait = 5 * time.Second
	// maxPause bounds how long a 429 holds back every process.
	maxPause = time.Minute
	// maxLead is how far ahead of now a TAT can be that a process using
	// the file put there: a request leaves at most tolerance + interval, and
	// a 429 at most maxPause + tolerance, where NewClient keeps the interval
	// and the tolerance each to a minute, so two minutes, and one to spare.
	// It is the same for every process, whatever its rate and burst, so that
	// none takes another's valid TAT for a corrupt one.
	maxLead = 3 * time.Minute
)

// errBusy is the error for a rate file whose lock another process holds,
// once a wait for it has run out.
var errBusy = errors.New("another process holds its lock")

// rateState is the shared state of the rate limit.
type rateState struct {
	tat  time.Time // the theoretical arrival time
	last time.Time // when the state was last written, by the wall clock
	bad  bool      // the file held something else, so tat and last are unknown
}

// The rate file holds one record of rateRecordLen bytes: a version, then the
// TAT and the time of the last write, each as 20 digits of nanoseconds since
// 1970. It is always written whole, at the start of the file, so the file is
// never empty or half written once it has a record.
const rateRecordLen = 44

func (s rateState) record() []byte {
	return fmt.Appendf(nil, "1 %020d %020d\n", s.tat.UnixNano(), s.last.UnixNano())
}

// parseRate reads a record. An empty file has none yet; anything else that
// is not a record leaves the state unknown.
func parseRate(b []byte) rateState {
	if len(b) == 0 {
		return rateState{}
	}
	if len(b) < rateRecordLen || string(b[:2]) != "1 " || b[22] != ' ' || b[43] != '\n' {
		return rateState{bad: true}
	}
	tat, err1 := strconv.ParseInt(string(b[2:22]), 10, 64)
	last, err2 := strconv.ParseInt(string(b[23:43]), 10, 64)
	if err1 != nil || err2 != nil {
		return rateState{bad: true}
	}
	return rateState{tat: time.Unix(0, tat), last: time.Unix(0, last)}
}

// settle makes s fit now, the wall clock read under the locks: it moves s
// to now's clock, and a TAT further ahead than maxLead, or one that is
// unknown, becomes now + tolerance: no burst, but no long wait.
func (s rateState) settle(now time.Time, tolerance time.Duration) rateState {
	if s.bad {
		return rateState{tat: now.Add(tolerance), last: now}
	}
	s = s.at(now)
	if s.tat.Sub(now) > maxLead {
		s.tat = now.Add(tolerance)
	}
	return s
}

// at moves s to now's clock. When now is before the last write, the clock
// was set back by at least that much, so the TAT moves back with it, and the
// state, saved, makes the move only once.
func (s rateState) at(now time.Time) rateState {
	if !s.bad && now.Before(s.last) {
		s.tat = s.tat.Add(-s.last.Sub(now))
		s.last = now
	}
	return s
}

// admit lets a request go at now if s allows it, taking its slot, and
// otherwise returns how long until it may.
func (s rateState) admit(now time.Time, interval, tolerance time.Duration) (rateState, time.Duration) {
	if wait := s.tat.Add(-tolerance).Sub(now); wait > 0 {
		return s, wait
	}
	s.tat = later(s.tat, now).Add(interval)
	return s, 0
}

// pause holds back every request until now + d: after it, requests resume at
// one each interval, without a burst into a limit that has just refused.
func (s rateState) pause(now time.Time, d, tolerance time.Duration) rateState {
	s.tat = later(s.tat, now.Add(d+tolerance))
	return s
}

// merge returns s with what other holds that is further on: its TAT, if
// later, and its time of last write, if later. Both must be on one clock,
// as at makes them.
func (s rateState) merge(other rateState) rateState {
	if s.bad || other.tat.After(s.tat) {
		s.tat, s.bad = other.tat, false
	}
	s.last = later(s.last, other.last)
	return s
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// limiter is a rate file as this process uses it. Every Client in the
// process that uses the file shares one.
type limiter struct {
	path string
	// sem lets one goroutine of this process at a time at the file, since on
	// some network volumes the lock does not keep out a second open in the
	// same process.
	sem chan struct{}
	// lock and tryLock take the file's lock, and wait is how long a process
	// waits for it in all; tests replace them.
	lock    func(ctx context.Context, path string, wait time.Duration) (*filelock.Lock, error)
	tryLock func(path string) (*filelock.Lock, bool, error)
	wait    time.Duration

	// Guarded by sem:
	// mem is the state as last read or written, used when the file cannot
	// be, so that the process keeps its own limit then.
	mem rateState
	// unsaved is set while mem holds what the file does not, from a time
	// the file could not be used, such as a 429's pause. The file takes it
	// in at its next write.
	unsaved bool
	// busy is set once a wait for the file's lock has run out, as when the
	// process that holds it is stopped. Until the lock is taken again, a
	// check looks once rather than waits, so that only one wait in all, not
	// one for each request, runs out.
	busy bool
	// waited is how long checks have waited for the lock since it was last
	// taken, when their callers' deadlines cut the waits short. Together
	// they count as one wait, so that callers with short deadlines also end
	// it.
	waited time.Duration
	// noFile is set once the file system has turned out not to lock files.
	noFile bool
	// warned is set once a problem with the file has been reported.
	warned bool
}

var (
	limitersMu sync.Mutex
	limiters   = map[string]*limiter{} // by rate file
)

// limiterFor returns the limiter for the rate file at path, which every
// Client on that file in this process shares.
func limiterFor(path string) *limiter {
	limitersMu.Lock()
	defer limitersMu.Unlock()
	l, ok := limiters[path]
	if !ok {
		l = &limiter{path: path, sem: make(chan struct{}, 1), lock: filelock.Acquire, tryLock: filelock.TryAcquire, wait: rateLockWait}
		limiters[path] = l
	}
	return l
}

// defaultRateFile returns where the rate limit for appKey is kept: in the
// user cache directory, which stays on this computer where the config
// directory may roam, named by a hash of the key. Without a cache directory
// it is kept next to tokenFile.
func defaultRateFile(appKey, tokenFile string) string {
	sum := sha256.Sum256([]byte(appKey))
	name := "rate-" + hex.EncodeToString(sum[:8])
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "schwabtx", name)
	}
	return filepath.Join(filepath.Dir(tokenFile), name)
}

// change applies f to the shared state, and saves what f returns, stamped
// with the time, when f says to. f gets the wall-clock time, read once both
// locks are held, and may be called a second time with the process's own
// state if the file cannot be used. The only errors are ctx's; a ctx that
// has ended changes nothing.
func (l *limiter) change(ctx context.Context, now func() time.Time, warn func(string),
	f func(s rateState, now time.Time) (next rateState, save bool),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.sem }()
	if !l.noFile {
		err := l.changeFile(ctx, now, f)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		l.noFile = errors.Is(err, filelock.ErrUnsupported)
		l.busy = errors.Is(err, filelock.ErrTimeout) || errors.Is(err, errBusy)
		if !l.warned {
			l.warned = true
			warn(fmt.Sprintf("%s cannot be used (%v), so until it can, this process keeps its own rate limit for Schwab's API, apart from other processes using the app key", l.path, err))
		}
	}
	t := now().Round(0)
	if next, save := f(l.mem, t); save {
		next.last = t
		l.mem = next
		l.unsaved = true
	}
	return nil
}

// changeFile is change through the file. The file is read and written only
// through its lock: Windows refuses any other handle to a locked file, and
// where a lock belongs to the process, closing another handle can drop it.
func (l *limiter) changeFile(ctx context.Context, now func() time.Time,
	f func(s rateState, now time.Time) (rateState, bool),
) error {
	lk, err := l.take(ctx)
	if err != nil {
		return err
	}
	defer lk.Release()
	buf := make([]byte, 2*rateRecordLen)
	n, err := lk.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading: %w", err)
	}
	t := now().Round(0)
	s := parseRate(buf[:n])
	if l.unsaved {
		s = s.at(t).merge(l.mem.at(t))
	}
	next, save := f(s, t)
	if !save {
		return nil
	}
	next.last = t
	if _, err := lk.WriteAt(next.record(), 0); err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	l.mem, l.unsaved = next, false
	return nil
}

// take takes the file's lock: with a wait of what is left of l.wait, or
// once a wait has run out, with a look.
func (l *limiter) take(ctx context.Context) (*filelock.Lock, error) {
	if !l.busy {
		start := time.Now()
		lk, err := l.lock(ctx, l.path, l.wait-l.waited)
		switch {
		case err == nil:
			l.waited = 0
		case ctx.Err() != nil:
			l.waited += time.Since(start)
		}
		return lk, err
	}
	lk, ok, err := l.tryLock(l.path)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, errBusy
	}
	l.busy, l.waited = false, 0
	return lk, nil
}
