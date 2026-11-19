//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// guardStdout keeps stdout for the protocol alone (design 5.3). The file of
// the original stdout carries the protocol, and the transport keeps it
// referenced; the process's standard output handle, which a later
// GetStdHandle returns, and os.Stdout become stderr's, so that whatever
// else writes to stdout reaches stderr instead of the client. A handle
// copied before main, such as syscall.Stdout, would still reach the client:
// no package that the server links writes through one.
func guardStdout() (*os.File, error) {
	protocol := os.Stdout //nolint:forbidigo // the guard itself, which keeps the original stdout for the protocol
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(os.Stderr.Fd())); err != nil {
		return nil, fmt.Errorf("pointing stdout at stderr: %w", err)
	}
	os.Stdout = os.Stderr //nolint:forbidigo // the guard itself, which points os.Stdout at stderr
	return protocol, nil
}
