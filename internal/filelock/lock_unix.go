//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package filelock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const supported = true

// tryLock takes an exclusive lock on f if no other open file holds one, and
// reports whether it did. Which opens count as others depends on the file
// system: see the package doc. The system drops the lock when f is closed or
// the process ends.
func tryLock(f *os.File) (bool, error) {
	err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EINTR):
		return false, nil
	case errors.Is(err, syscall.ENOLCK), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOSYS):
		// NFS without a lock manager, some FUSE and WebDAV volumes.
		return false, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return false, err
}

// unlock drops the lock on f. An error is not reported: closing f, which
// always follows, drops the lock too.
func unlock(f *os.File) {
	_ = flock(f, syscall.LOCK_UN)
}

func flock(f *os.File, how int) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := conn.Control(func(fd uintptr) {
		opErr = syscall.Flock(int(fd), how)
	}); err != nil {
		return err
	}
	if opErr != nil {
		return &os.PathError{Op: "flock", Path: f.Name(), Err: opErr}
	}
	return nil
}

// openBusy reports whether err, from opening the lock file, means that
// another program has it open for a moment. That does not happen here.
func openBusy(error) bool { return false }

// openUnsupported reports whether err, from opening the lock file, means
// that its file system cannot hold one: it is read-only.
func openUnsupported(err error) bool {
	return errors.Is(err, syscall.EROFS)
}
