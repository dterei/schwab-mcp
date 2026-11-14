//go:build unix

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// fd1 keeps the os.File of descriptor 1 referenced for good, as os.Stdout
// does while nothing replaces it. Every os.File closes its descriptor when
// the garbage collector finds it unreferenced; were descriptor 1 closed,
// the next file the process opened (a lock, the token file, the store, a
// socket) would take it, and a stray write meant for stdout would land
// inside that file.
var fd1 = os.Stdout

// guardStdout keeps stdout for the protocol alone (design 5.3). It copies
// descriptor 1 to a new descriptor, which no child process inherits, for
// the protocol, and points descriptor 1 at stderr, so that whatever else
// writes to stdout, through os.Stdout, a copy of it taken before main, or
// a raw write to descriptor 1, reaches stderr instead of the client.
func guardStdout() (*os.File, error) {
	fd, err := unix.Dup(int(fd1.Fd()))
	if err != nil {
		return nil, fmt.Errorf("copying stdout: %w", err)
	}
	unix.CloseOnExec(fd)
	if err := unix.Dup2(2, 1); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("pointing stdout at stderr: %w", err)
	}
	return os.NewFile(uintptr(fd), "protocol"), nil
}
