package mcpserver

import "golang.org/x/sys/windows"

// shortPath returns p's short form (8.3), such as C:\Users\ABCDEF~1 for
// C:\Users\abcdefghij, by which a program may name the home directory too,
// or "" when it has none or it cannot be found.
func shortPath(p string) string {
	long, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return ""
	}
	n, err := windows.GetShortPathName(long, nil, 0)
	if err != nil || n == 0 {
		return ""
	}
	buf := make([]uint16, n)
	if n, err = windows.GetShortPathName(long, &buf[0], n); err != nil || n == 0 || int(n) > len(buf) {
		return ""
	}
	if short := windows.UTF16ToString(buf[:n]); short != p {
		return short
	}
	return ""
}
