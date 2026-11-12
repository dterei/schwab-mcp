package history

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestDocsListEverything proves that docs/store.md lists, in their order,
// every view with each of its columns and their kinds (viewColumns), every
// flag with what it can be on, its severity and its meaning (flagDefs),
// every kind of decision (decisionKinds), and every table that export
// writes with its view, what it holds and whether -raw adds to it
// (exportTables); and that docs/schwab-exports.md lists every action of a
// History export with its family and category (fileFamilies). Each list is
// compared whole, so a name the code lacks fails as a name the docs lack
// does, and the docs cannot drift from the code.
func TestDocsListEverything(t *testing.T) {
	store := readDoc(t, "store.md")

	t.Run("views", func(t *testing.T) {
		var got, want []string
		for _, c := range viewColumns {
			want = append(want, fmt.Sprintf("%s.%s %s", c.view, c.column, c.kind))
		}
		views := docSection(t, store, "The views")
		view := ""
		for line := range strings.Lines(views) {
			if name, ok := strings.CutPrefix(strings.TrimSpace(line), "### "); ok {
				view = strings.Trim(name, "`")
				continue
			}
			cells := tableRow(line)
			if len(cells) < 3 || !isCode(cells[0]) {
				continue
			}
			got = append(got, fmt.Sprintf("%s.%s %s", view, strings.Trim(cells[0], "`"), cells[1]))
			// A column that holds free text only on some rows says when.
			i := slices.IndexFunc(viewColumns, func(c viewColumn) bool {
				return c.view == view && c.column == strings.Trim(cells[0], "`")
			})
			if i >= 0 && viewColumns[i].when != "" && !strings.Contains(cells[2], "free text when") {
				t.Errorf("%s.%s holds free text only when %s, which its row does not say: %q", view, viewColumns[i].column, viewColumns[i].when, cells[2])
			}
		}
		compareLists(t, "view columns", got, want)
	})

	t.Run("flags", func(t *testing.T) {
		var got, want []string
		for _, d := range flagDefs {
			want = append(want, strings.Join([]string{"`" + d.code + "`", strings.Join(d.subjects, ", "), d.severity, d.meaning}, " | "))
		}
		for _, cells := range codeRows(t, store, "Flags") {
			got = append(got, strings.Join(cells, " | "))
		}
		compareLists(t, "flags", got, want)
	})

	t.Run("decisions", func(t *testing.T) {
		var got, want []string
		for _, k := range decisionKinds {
			want = append(want, "`"+k+"`")
		}
		for _, cells := range codeRows(t, store, "Your decisions") {
			got = append(got, cells[0])
		}
		compareLists(t, "decision kinds", got, want)
	})

	t.Run("exports", func(t *testing.T) {
		var got, want []string
		for _, x := range exportTables {
			raw := ""
			if x.raw != rawNone {
				raw = "yes"
			}
			want = append(want, strings.Join([]string{"`" + x.name + "`", "`" + x.view + "`", x.about, raw}, " | "))
		}
		for _, cells := range codeRows(t, store, "Exports") {
			got = append(got, strings.Join(cells, " | "))
		}
		compareLists(t, "export tables", got, want)
	})

	t.Run("actions", func(t *testing.T) {
		var got, want []string
		for _, f := range fileFamilies {
			category := "by its sign"
			if f.category != "" {
				category = "`" + f.category + "`"
			}
			var actions []string
			for _, a := range f.actions {
				actions = append(actions, "`"+a+"`")
			}
			want = append(want, strings.Join([]string{"`" + f.family + "`", category, strings.Join(actions, ", ")}, " | "))
		}
		for _, cells := range codeRows(t, readDoc(t, "schwab-exports.md"), "The actions") {
			got = append(got, strings.Join(cells, " | "))
		}
		compareLists(t, "History actions", got, want)
	})
}

// readDoc returns the file of the repository's docs folder named name.
func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// docSection returns the section of doc under the heading "## heading", up
// to the next heading of that level.
func docSection(t *testing.T, doc, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, "\n## "+heading+"\n")
	if !ok {
		t.Fatalf("the doc has no section %q", heading)
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	return section
}

// codeRows returns the cells of the rows, in the tables of the doc's
// section heading, whose first cell is code (`...`): a table's header and
// rule have none.
func codeRows(t *testing.T, doc, heading string) [][]string {
	t.Helper()
	var rows [][]string
	for line := range strings.Lines(docSection(t, doc, heading)) {
		if cells := tableRow(line); len(cells) > 0 && isCode(cells[0]) {
			rows = append(rows, cells)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("the section %q has no table", heading)
	}
	return rows
}

// tableRow returns the cells of line, a row of a Markdown table, trimmed,
// with an escaped | taken as a |; nil when line is not one.
func tableRow(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") || len(line) < 2 {
		return nil
	}
	const bar = "\x00"
	line = strings.ReplaceAll(line[1:len(line)-1], `\|`, bar)
	var cells []string
	for c := range strings.SplitSeq(line, "|") {
		cells = append(cells, strings.ReplaceAll(strings.TrimSpace(c), bar, "|"))
	}
	return cells
}

// isCode reports whether a cell is one span of code.
func isCode(cell string) bool {
	return len(cell) > 2 && strings.HasPrefix(cell, "`") && strings.HasSuffix(cell, "`") && strings.Count(cell, "`") == 2
}

// compareLists fails t with each entry that got has and want lacks, and
// each that want has and got lacks, or with the first difference of order.
func compareLists(t *testing.T, what string, got, want []string) {
	t.Helper()
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("the docs list %s that the code does not: %s", what, g)
		}
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("the docs do not list %s that the code does: %s", what, w)
		}
	}
	if !t.Failed() && !slices.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("the docs list %s in another order than the code: %s where the code has %s", what, got[i], want[i])
				break
			}
		}
	}
}
