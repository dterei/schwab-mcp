//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package filelock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const supported = true

// tryLock takes an exclusive lock on f if no other open file holds one, in
// this process or another, and reports whether it did. The system drops the
// lock when f is closed or the process ends.
func tryLock(f *os.File) (bool, error) {
	err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EINTR):
		return false, nil
	case errors.Is(err, syscall.ENOLCK), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP):
		// Network file systems without a lock manager, and some others.
		return false, fmt.Errorf("the file system of %s cannot lock files; keep it on a local disk: %w", f.Name(), err)
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
