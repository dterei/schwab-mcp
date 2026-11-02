//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || windows)

package filelock

import "os"

// This system has no file locks that this package knows how to use.
const supported = false

func tryLock(f *os.File) (bool, error) {
	return false, &os.PathError{Op: "lock", Path: f.Name(), Err: ErrUnsupported}
}

func unlock(*os.File) {}

func openBusy(error) bool { return false }

func openUnsupported(error) bool { return false }
