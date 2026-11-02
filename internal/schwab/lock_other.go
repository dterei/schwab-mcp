//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || windows)

package schwab

import "os"

// lockSupported is false here: on this system the token file is used without
// a lock, so two processes can refresh at once.
const lockSupported = false

func tryLock(*os.File) (bool, error) { return true, nil }

func unlock(*os.File) {}
