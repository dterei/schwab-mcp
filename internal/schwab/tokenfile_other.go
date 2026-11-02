//go:build !windows

package schwab

// busy reports whether err means that another program has the file open
// for a moment. Only Windows refuses a read or a rename for that.
func busy(error) bool { return false }
