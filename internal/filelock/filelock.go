// Package filelock locks files across processes: with flock on Linux, macOS
// and the BSDs, and LockFileEx on Windows. The system drops a lock when its
// file is closed or the process ends, so a crash leaves no stale lock behind.
//
// On a local disk a lock belongs to the open file, so it also keeps out other
// opens of the same file in the same process. On some network volumes, such
// as macOS's SMB and NFS ones, it belongs to the process instead, so callers
// in one process must take turns among themselves too, as internal/schwab
// does with one semaphore per token file.
//
// A lock file is never deleted. A process that opened the path after a
// delete would get a different file, and so a different lock.
package filelock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Supported reports whether this system has file locks. Where it has not,
// Acquire and TryAcquire return ErrUnsupported.
const Supported = supported

var (
	// ErrTimeout is what Acquire returns, wrapped, when another process held
	// the lock for the whole wait.
	ErrTimeout = errors.New("another process kept the lock")
	// ErrUnsupported is what Acquire and TryAcquire return, wrapped, when the
	// lock file's file system cannot lock files, as some network file
	// systems cannot, or is read-only.
	ErrUnsupported = errors.New("the file system cannot lock files")
)

// Lock is a lock that this process holds.
type Lock struct{ f *os.File }

// testHookBeforeLock, if set, runs at the last moment before TryAcquire
// locks the file it opened.
var testHookBeforeLock func()

// Acquire takes the lock on the file at path, creating the file, and its
// directory, readable only by you. While another process holds the lock, it
// waits, polling so that the wait ends when ctx does, and gives up after
// wait.
func Acquire(ctx context.Context, path string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	delay := 5 * time.Millisecond
	for {
		l, ok, err := TryAcquire(path)
		if err != nil || ok {
			return l, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("waited %v for %s: %w", wait, path, ErrTimeout)
		}
		// The last sleep ends at the deadline, so that the wait is wait,
		// not up to a poll longer.
		timer := time.NewTimer(min(delay, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, 250*time.Millisecond)
	}
}

// TryAcquire takes the lock on the file at path if no one holds it, creating
// the file, and its directory, readable only by you. It reports whether it
// took the lock.
func TryAcquire(path string) (*Lock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G703: the path is the caller's own setting.
		return nil, false, fmt.Errorf("creating the directory for the lock file: %w", err)
	}
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G703: the path is the caller's own setting.
		switch {
		case err == nil:
		case openBusy(err):
			return nil, false, nil
		case openUnsupported(err):
			return nil, false, fmt.Errorf("%w: %w", ErrUnsupported, err)
		case errors.Is(err, fs.ErrPermission):
			return nil, false, fmt.Errorf("opening the lock file: %w; if another user, such as root, made it, remove it while nothing is using it", err)
		default:
			return nil, false, fmt.Errorf("opening the lock file: %w", err)
		}
		ok, err := lockOpened(f)
		if err != nil {
			f.Close()
			return nil, false, err
		}
		if !ok {
			f.Close()
			return nil, false, nil
		}
		// A lock file removed and created again after we opened it guards
		// nothing, since the next process locks the new one; lock that one.
		if sameFile(f, path) {
			return &Lock{f: f}, true, nil
		}
		unlock(f)
		f.Close()
	}
}

// lockOpened locks f, which TryAcquire has just opened, after running the
// test hook, so that the hook can replace the lock file after any check
// that comes before the lock.
func lockOpened(f *os.File) (bool, error) {
	if testHookBeforeLock != nil {
		testHookBeforeLock()
	}
	return tryLock(f)
}

// Release drops the lock.
func (l *Lock) Release() {
	unlock(l.f)
	l.f.Close()
}

// ReadAt reads the locked file, as io.ReaderAt does. While the lock is held,
// read and write the file only through the Lock: Windows refuses every other
// handle, and where a lock belongs to the process, as on some network
// volumes, closing another handle to the file can drop the lock.
func (l *Lock) ReadAt(p []byte, off int64) (int, error) {
	return l.f.ReadAt(p, off)
}

// WriteAt writes the locked file, as io.WriterAt does. See ReadAt.
func (l *Lock) WriteAt(p []byte, off int64) (int, error) {
	return l.f.WriteAt(p, off)
}

// sameFile reports whether f is still the file at path.
func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path) //nolint:gosec // G703: the path is the caller's own setting.
	return err == nil && os.SameFile(a, b)
}
