package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/history"
)

// TestDocsNameEveryFlag proves that the README's section on each of the
// store's commands lists, in its block of flags, exactly the flags that the
// command's -h prints, that the section on store names each of its
// commands, and that the section on export names each table of -what. A
// flag added to a command, or one the README keeps after it is gone, fails
// the test, so the README cannot drift from the commands.
func TestDocsNameEveryFlag(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.ReplaceAll(string(b), "\r\n", "\n")
	flagLine := regexp.MustCompile(`(?m)^  -([a-z-]+)`)
	listed := regexp.MustCompile(`(?m)^-([a-z-]+)`)
	for _, cmd := range []string{"import", "status", "edit", "export", "store"} {
		t.Run(cmd, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(context.Background(), []string{cmd, "-h"}, nil, &stdout, &stderr); !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("%s -h: %v", cmd, err)
			}
			help := stderr.String()
			var want []string
			for _, m := range flagLine.FindAllStringSubmatch(help, -1) {
				want = append(want, m[1])
			}
			section := readmeSection(t, readme, "schwabtx "+cmd)
			var got []string
			for _, block := range codeBlocks(section) {
				for _, m := range listed.FindAllStringSubmatch(block, -1) {
					got = append(got, m[1])
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("the README lists the flags %q of %s, which takes %q", got, cmd, want)
			}

			var names []string
			switch cmd {
			case "store":
				for _, m := range regexp.MustCompile(`schwabtx store \[-store PATH\] ([a-z]+)`).FindAllStringSubmatch(help, -1) {
					names = append(names, m[0])
				}
			case "export":
				for _, k := range history.ExportKinds() {
					names = append(names, "`"+k.Name+"`")
				}
			}
			for _, name := range names {
				if !strings.Contains(section, name) {
					t.Errorf("the README's section on %s does not name %s", cmd, name)
				}
			}
		})
	}
}

// readmeSection returns the section of the README under the heading "###
// heading", up to the next heading.
func readmeSection(t *testing.T, readme, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(readme, "\n### "+heading+"\n")
	if !ok {
		t.Fatalf("the README has no section %q", heading)
	}
	if i := regexp.MustCompile(`\n#{2,3} `).FindStringIndex(rest); i != nil {
		rest = rest[:i[0]]
	}
	return rest
}

// codeBlocks returns the contents of the fenced code blocks in s.
func codeBlocks(s string) []string {
	var blocks []string
	for {
		_, rest, ok := strings.Cut(s, "```\n")
		if !ok {
			return blocks
		}
		block, after, ok := strings.Cut(rest, "\n```")
		if !ok {
			return blocks
		}
		blocks = append(blocks, block)
		s = after
	}
}
