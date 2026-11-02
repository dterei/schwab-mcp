//go:build windows

package schwab

import (
	"errors"

	"golang.org/x/sys/windows"
)

// busy reports whether err means that another program has the file open
// for a moment, as antivirus, search indexers and sync clients do.
func busy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
