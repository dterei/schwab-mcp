//go:build windows

package filelock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const supported = true

const allBytes = ^uint32(0)

// tryLock takes an exclusive lock on f if no other open file holds one, in
// this process or another, and reports whether it did. Windows drops the
// lock when f is closed or the process ends.
func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, allBytes, allBytes, new(windows.Overlapped))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	case errors.Is(err, windows.ERROR_NOT_SUPPORTED), errors.Is(err, windows.ERROR_INVALID_FUNCTION):
		// A network redirector without byte-range locks.
		return false, fmt.Errorf("%w: %s: %w", ErrUnsupported, f.Name(), err)
	}
	return false, &os.PathError{Op: "LockFileEx", Path: f.Name(), Err: err}
}

// unlock drops the lock on f. An error is not reported: closing f, which
// always follows, drops the lock too.
func unlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, new(windows.Overlapped))
}

// openBusy reports whether err, from opening the lock file, means that
// another program has it open for a moment, as antivirus does.
func openBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// openUnsupported reports whether err, from opening the lock file, means
// that its file system cannot hold one.
func openUnsupported(error) bool { return false }
