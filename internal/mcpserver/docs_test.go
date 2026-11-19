package mcpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// laterTools are the tools of milestone 3's later stages, whose names are
// fixed now (design 2.1, 10.16): the order tools, the market tools and
// sync_history. Until one is served, docs/mcp.md names it under "Later"
// and nowhere else; once it is, the docs list it among the tools, and this
// list need not change.
var laterTools = []string{
	"get_orders", "get_order",
	"get_quotes", "get_price_history", "get_option_chain", "get_option_expirations", "search_instruments",
	"get_market_hours", "get_movers",
	"sync_history",
}

// TestDocsListEveryTool proves that docs/mcp.md lists exactly what the
// server serves and says: every tool that tools/list serves, in the table
// of its section on the tools; every resource and resource template, in
// the table of its section on resources; each prompt, under a heading of
// its own with a table of the prompt's arguments; every error code, in the
// order of errorCodes; every warning code that the package's code gives;
// and every flag of cmd/schwab-mcp. A tool of a later stage that is not
// served yet is named under "Later" and nowhere else. A tool, resource,
// argument, code or flag added without its row, or a row kept after it is
// gone, fails the test, so the docs cannot drift from the server.
func TestDocsListEveryTool(t *testing.T) {
	t.Parallel()
	doc := readMCPDoc(t)
	e := newEnv(t, withoutLogin(), withoutStore(), withoutFixtures())

	t.Run("tools", func(t *testing.T) {
		var served []string
		for name := range e.tools {
			served = append(served, name)
		}
		compareDocSets(t, "tools", codeCells(t, docSectionOf(t, doc, "The tools")), served)
	})

	t.Run("resources", func(t *testing.T) {
		resources, err := e.cs.ListResources(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		templates, err := e.cs.ListResourceTemplates(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var served []string
		for _, r := range resources.Resources {
			served = append(served, r.URI)
		}
		for _, r := range templates.ResourceTemplates {
			served = append(served, r.URITemplate)
		}
		compareDocSets(t, "resources", codeCells(t, docSectionOf(t, doc, "Resources")), served)
	})

	t.Run("prompts", func(t *testing.T) {
		listed, err := e.cs.ListPrompts(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		section := docSectionOf(t, doc, "The prompt")
		subs := subsections(section)
		var served []string
		for _, p := range listed.Prompts {
			served = append(served, p.Name)
			sub, ok := subs[p.Name]
			if !ok {
				continue // compareDocSets names it
			}
			var args []string
			for _, a := range p.Arguments {
				args = append(args, a.Name)
			}
			compareDocSets(t, p.Name+"'s arguments", codeCells(t, sub), args)
		}
		var listedNames []string
		for name := range subs {
			listedNames = append(listedNames, name)
		}
		compareDocSets(t, "prompts", listedNames, served)
	})

	t.Run("errors", func(t *testing.T) {
		var codes []string
		for _, c := range errorCodes {
			codes = append(codes, string(c))
		}
		got := codeCells(t, docSectionOf(t, doc, "Errors"))
		compareDocSets(t, "error codes", got, codes)
		if !t.Failed() && !slices.Equal(got, codes) {
			t.Errorf("docs/mcp.md lists the error codes in the order %q; errorCodes has %q", got, codes)
		}
	})

	t.Run("warnings", func(t *testing.T) {
		compareDocSets(t, "warning codes", codeCells(t, docSectionOf(t, doc, "Warnings")), warningCodes(t))
	})

	t.Run("flags", func(t *testing.T) {
		var listed []string
		for _, block := range fencedBlocks(docSectionOf(t, doc, "Running the server")) {
			for _, m := range regexp.MustCompile(`(?m)^ *--([a-z-]+)`).FindAllStringSubmatch(block, -1) {
				listed = append(listed, m[1])
			}
		}
		compareDocSets(t, "flags", listed, commandFlags(t))
	})

	t.Run("later", func(t *testing.T) {
		later := docSectionOf(t, doc, "Later")
		rest := strings.Replace(doc, later, "", 1)
		for _, name := range laterTools {
			if _, served := e.tools[name]; served {
				continue
			}
			if !strings.Contains(later, "`"+name+"`") {
				t.Errorf("docs/mcp.md does not name %s under Later, though it is not served yet", name)
			}
			if regexp.MustCompile(`\b` + name + `\b`).MatchString(rest) {
				t.Errorf("docs/mcp.md names %s outside Later, though it is not served", name)
			}
		}
	})
}

// readMCPDoc returns docs/mcp.md, with Windows' line ends made Unix's.
func readMCPDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// docSectionOf returns the section of doc under the heading "## heading",
// up to the next heading of that level.
func docSectionOf(t *testing.T, doc, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(doc, "\n## "+heading+"\n")
	if !ok {
		t.Fatalf("docs/mcp.md has no section %q", heading)
	}
	section, _, _ := strings.Cut(rest, "\n## ")
	return section
}

// subsections returns the parts of section under each heading "### `name`",
// by name, each up to the next such heading.
func subsections(section string) map[string]string {
	subs := map[string]string{}
	parts := strings.Split("\n"+section, "\n### ")
	for _, p := range parts[1:] {
		head, body, _ := strings.Cut(p, "\n")
		if name, ok := strings.CutPrefix(head, "`"); ok {
			subs[strings.TrimSuffix(name, "`")] = body
		}
	}
	return subs
}

// codeCells returns the first cell of each row of the tables in section
// whose first cell is one span of code, without its backticks: a table's
// header and its rule have none.
func codeCells(t *testing.T, section string) []string {
	t.Helper()
	var cells []string
	for line := range strings.Lines(section) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		first, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		first = strings.TrimSpace(first)
		if len(first) > 2 && strings.HasPrefix(first, "`") && strings.HasSuffix(first, "`") && strings.Count(first, "`") == 2 {
			cells = append(cells, strings.Trim(first, "`"))
		}
	}
	if len(cells) == 0 {
		t.Fatalf("a section of docs/mcp.md has no table whose rows start with code:\n%s", section)
	}
	return cells
}

// fencedBlocks returns the contents of the fenced code blocks of s, those
// with an info string after their opening ``` too.
func fencedBlocks(s string) []string {
	var blocks []string
	var block []string
	in := false
	for line := range strings.Lines(s) {
		switch {
		case strings.HasPrefix(line, "```") && in:
			blocks, block, in = append(blocks, strings.Join(block, "")), nil, false
		case strings.HasPrefix(line, "```"):
			in = true
		case in:
			block = append(block, line)
		}
	}
	return blocks
}

// compareDocSets fails t with each entry that the docs list and the server
// does not have, and each that it has and the docs do not list, and with
// any entry that the docs list twice.
func compareDocSets(t *testing.T, what string, docs, served []string) {
	t.Helper()
	for i, d := range docs {
		if !slices.Contains(served, d) {
			t.Errorf("docs/mcp.md lists %s that the server does not have: %s", what, d)
		}
		if slices.Contains(docs[:i], d) {
			t.Errorf("docs/mcp.md lists %s twice: %s", what, d)
		}
	}
	for _, s := range served {
		if !slices.Contains(docs, s) {
			t.Errorf("docs/mcp.md does not list %s that the server has: %s", what, s)
		}
	}
}

// warningCodes returns the code of every warning that the package's code
// gives: the first argument of each call of a method warn, and the Code of
// each warning written whole but in warn itself, each a string constant.
func warningCodes(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	add := func(e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s: a warning's code that is not a string constant", fset.Position(e.Pos()))
			return
		}
		code, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				return n.Name.Name != "warn" // which makes a warning of its argument
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "warn" && len(n.Args) > 0 {
					add(n.Args[0])
				}
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok && id.Name == "warning" {
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Code" {
								add(kv.Value)
							}
						}
					}
				}
			}
			return true
		})
	}
	if len(codes) == 0 {
		t.Fatal("the package gives no warning")
	}
	return codes
}

// commandFlags returns the flags that cmd/schwab-mcp's flagSet defines, read
// from its source: the name each of its calls of a FlagSet's methods gives.
func commandFlags(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "cmd", "schwab-mcp", "main.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flags []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "flagSet" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			i := -1
			switch sel.Sel.Name {
			case "Var", "StringVar", "BoolVar", "IntVar", "Int64Var", "UintVar", "Uint64Var", "Float64Var", "DurationVar", "TextVar":
				i = 1
			case "String", "Bool", "Int", "Int64", "Uint", "Uint64", "Float64", "Duration", "Func", "BoolFunc":
				i = 0
			}
			if i < 0 || len(call.Args) <= i {
				return true
			}
			lit, ok := call.Args[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("a flag of %s whose name is not a string constant", path)
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			flags = append(flags, name)
			return true
		})
	}
	if len(flags) == 0 {
		t.Fatalf("%s's flagSet defines no flag", path)
	}
	return flags
}
