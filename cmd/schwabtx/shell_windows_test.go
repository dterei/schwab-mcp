//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"
)

// systemShellWords returns the words of line as a Windows program reads them
// from its command line (CommandLineToArgvW), which the Command Prompt
// passes on as it was typed.
func systemShellWords(t *testing.T, line string) []string {
	t.Helper()
	words, err := windows.DecomposeCommandLine(line)
	if err != nil {
		t.Fatalf("reading %s: %v", line, err)
	}
	return words
}
