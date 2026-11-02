//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package schwab

import (
	"errors"
	"os"
	"syscall"
)

const lockSupported = true

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
