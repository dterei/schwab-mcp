//go:build unix

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// systemShellWords returns the words of line as sh reads them, as the shell of
// the terminal a person pastes the line into would.
func systemShellWords(t *testing.T, line string) []string {
	t.Helper()
	if strings.Contains(line, "\n") {
		t.Fatalf("the line %q holds a newline, which the words would hide", line)
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", `printf '%s\n' `+line).Output()
	if err != nil {
		t.Fatalf("sh reading %s: %v", line, err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}
