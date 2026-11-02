//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || windows)

package filelock

import "os"

// This system has no file locks that this package knows how to use, so locks
// always succeed.
const supported = false

func tryLock(*os.File) (bool, error) { return true, nil }

func unlock(*os.File) {}
