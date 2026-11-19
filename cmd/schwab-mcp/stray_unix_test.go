//go:build unix

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// rawWrite writes s to descriptor 1 itself, below every os.File.
func rawWrite(s string) { _, _ = syscall.Write(1, []byte(s)) }

// fd1Open reports whether descriptor 1 is still open.
func fd1Open() bool {
	_, err := unix.FcntlInt(1, unix.F_GETFD, 0)
	return err == nil
}

// descriptor returns f's file descriptor.
func descriptor(f *os.File) int { return int(f.Fd()) }
