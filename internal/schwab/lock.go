package schwab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// defaultLockWait is how long a process waits for another to finish with the
// token file before it gives up. The holder keeps the lock only for a file
// read, at most one token request and a file write.
const defaultLockWait = 2 * time.Minute

// fileLock is a lock on the lock file next to a token file. While a process
// holds it, no other process, and no other fileLock in the same process,
// can.
type fileLock struct{ f *os.File }

// lockFile takes the lock file at path, creating it, and its directory,
// readable only by you. It polls, so that the wait ends when ctx does, and
// gives up after wait.
func lockFile(ctx context.Context, path string, wait time.Duration) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G703: the path is the user's own setting.
		return nil, fmt.Errorf("creating the token directory: %w", err)
	}
	deadline := time.Now().Add(wait)
	delay := 5 * time.Millisecond
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G703: the path is the user's own setting.
		if err != nil {
			return nil, fmt.Errorf("opening the lock file: %w", err)
		}
		for {
			ok, err := tryLock(f)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("locking %s: %w", path, err)
			}
			if ok {
				break
			}
			if !time.Now().Before(deadline) {
				f.Close()
				return nil, fmt.Errorf("gave up after %v waiting for another process to finish with the Schwab login; it holds %s", wait, path)
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				f.Close()
				return nil, ctx.Err()
			case <-timer.C:
			}
			delay = min(2*delay, 250*time.Millisecond)
		}
		// A lock file removed or replaced while we waited guards nothing,
		// since the next process locks the new one, so lock that one too.
		if sameFile(f, path) {
			return &fileLock{f: f}, nil
		}
		unlock(f)
		f.Close()
	}
}

// release drops the lock. Closing the file would drop it too.
func (l *fileLock) release() {
	unlock(l.f)
	l.f.Close()
}

// sameFile reports whether f is still the file at path.
func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path) //nolint:gosec // G703: the path is the user's own setting.
	return err == nil && os.SameFile(a, b)
}
