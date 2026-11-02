//go:build windows

package schwab

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const lockSupported = true

// The standard library has no binding for these. kernel32.dll is one of the
// system's known DLLs, which Windows loads only from the system folder.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33) // ERROR_LOCK_VIOLATION
	allBytes                = ^uint32(0)
)

// tryLock takes an exclusive lock on f if no other open file holds one, in
// this process or another, and reports whether it did. Windows drops the
// lock when f is closed or the process ends.
func tryLock(f *os.File) (bool, error) {
	ol := new(syscall.Overlapped)
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0,
		uintptr(allBytes), uintptr(allBytes), uintptr(unsafe.Pointer(ol)))
	if r != 0 {
		return true, nil
	}
	if errors.Is(err, errorLockViolation) {
		return false, nil
	}
	return false, &os.PathError{Op: "LockFileEx", Path: f.Name(), Err: err}
}

// unlock drops the lock on f. An error is not reported: closing f, which
// always follows, drops the lock too.
func unlock(f *os.File) {
	ol := new(syscall.Overlapped)
	_, _, _ = procUnlockFileEx.Call(f.Fd(), 0, uintptr(allBytes), uintptr(allBytes), uintptr(unsafe.Pointer(ol)))
}
