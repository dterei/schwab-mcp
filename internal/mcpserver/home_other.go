//go:build !windows

package mcpserver

// shortPath returns p's short form, which only Windows has: "" here.
func shortPath(string) string { return "" }
