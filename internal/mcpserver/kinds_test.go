package mcpserver

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// tenth is 0.1 as a float64, so that sums of it are float64's, not exact
// constants.
var tenth = 0.1

// Num writes Schwab's float64 in its shortest form, as encoding/json
// would, and none as null: nil, NaN and the infinities, and 0 too when
// NumNonZero made it.
func TestNum(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		n    Num
		want string
	}{
		{Num{}, "null"},
		{NumOf(nil), "null"},
		{NumOf(new(1.5)), "1.5"},
		{NumValue(0), "0"},
		{NumNonZero(0), "null"},
		{NumNonZero(-0.25), "-0.25"},
		{NumValue(tenth + 2*tenth), "0.30000000000000004"},
		{NumValue(math.NaN()), "null"},
		{NumValue(math.Inf(1)), "null"},
		{NumValue(1e21), "1e+21"},
		{NumValue(1e-7), "1e-7"},
		{NumValue(123456789.125), "123456789.125"},
	} {
		got, err := tc.n.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("%+v marshals as %s, want %s", tc.n, got, tc.want)
		}
		if tc.want != "null" {
			want, _ := json.Marshal(tc.n.v)
			if string(got) != string(want) {
				t.Errorf("%+v marshals as %s, and encoding/json writes %s", tc.n, got, want)
			}
		}
	}
}

// An Instant is New York time with its offset, EST or EDT, with
// milliseconds only when they are not zero; the zero time is none.
func TestInstantOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   time.Time
		want Instant
	}{
		{time.Date(2026, 11, 13, 20, 59, 58, 0, time.UTC), "2026-11-13T15:59:58-05:00"},
		{time.Date(2026, 7, 13, 19, 59, 58, 0, time.UTC), "2026-07-13T15:59:58-04:00"},
		{time.Date(2026, 7, 13, 19, 59, 58, 120_999_999, time.UTC), "2026-07-13T15:59:58.120-04:00"},
		{time.Date(2026, 7, 13, 19, 59, 58, 999_999, time.UTC), "2026-07-13T15:59:58-04:00"},
	} {
		if got := instantOf(tc.in); got != tc.want {
			t.Errorf("instantOf(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if instantPtr(time.Time{}) != nil || datePtr("") != nil || identPtr("") != nil {
		t.Error("none is not nil")
	}
	if p := identPtr("AAPL"); p == nil || *p != "AAPL" {
		t.Error("identPtr lost its value")
	}
}

// packageFiles parses the package's files, its tests' too when tests is
// true.
func packageFiles(t *testing.T, tests bool) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if !tests && strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return fset, files
}

// maskedTextMakers returns where files make a FreeText, a MaskedNumber or
// a Path outside masker.go: a conversion to one, or a constant of one, an
// untyped constant given where one is wanted included. It type-checks the
// files alone, every import an empty package: the three types are the
// package's own, so a constant of one is typed whatever else fails, and a
// conversion to one is found by the name it calls.
func maskedTextMakers(fset *token.FileSet, files []*ast.File) []string {
	conf := types.Config{Importer: emptyImporter{}, Error: func(error) {}}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}}
	pkg, _ := conf.Check("mcpserver", fset, files, info)
	masked := map[types.Object]bool{}
	for _, name := range []string{"FreeText", "MaskedNumber", "Path"} {
		if obj := pkg.Scope().Lookup(name); obj != nil {
			masked[obj] = true
		}
	}
	isMasked := func(typ types.Type) bool {
		named, ok := typ.(*types.Named)
		return ok && masked[named.Obj()]
	}
	// A constant compared with one, as in detail != "", makes none.
	compared := map[ast.Expr]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if b, ok := n.(*ast.BinaryExpr); ok && (b.Op == token.EQL || b.Op == token.NEQ) {
				compared[b.X], compared[b.Y] = true, true
			}
			return true
		})
	}
	var found []string
	for expr, tv := range info.Types {
		pos := fset.Position(expr.Pos())
		if filepath.Base(pos.Filename) == "masker.go" || tv.Value == nil || tv.Type == nil || !isMasked(tv.Type) || compared[expr] {
			continue
		}
		found = append(found, pos.String()+": a constant "+tv.Type.String())
	}
	for _, f := range files {
		if filepath.Base(fset.Position(f.Pos()).Filename) == "masker.go" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || len(c.Args) != 1 {
				return true
			}
			fun := c.Fun
			for {
				p, ok := fun.(*ast.ParenExpr)
				if !ok {
					break
				}
				fun = p.X
			}
			if id, ok := fun.(*ast.Ident); ok && masked[info.Uses[id]] {
				if tv, ok := info.Types[c]; !ok || tv.Value == nil { // a constant's is found above
					found = append(found, fset.Position(c.Pos()).String()+": a conversion to "+id.Name)
				}
			}
			return true
		})
	}
	slices.Sort(found)
	return found
}

// emptyImporter imports every package as an empty one.
type emptyImporter struct{}

func (emptyImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	name := path[strings.LastIndex(path, "/")+1:]
	pkg := types.NewPackage(path, name)
	pkg.MarkComplete()
	return pkg, nil
}

// Only the masker makes FreeText, MaskedNumber and Path, the kinds that
// hold text from outside: no other file of the package converts to one or
// holds a constant of one. The check itself finds a conversion, a typed
// constant and an untyped constant given where one is wanted.
func TestOnlyTheMaskerMakesMaskedText(t *testing.T) {
	t.Parallel()
	fset, files := packageFiles(t, false)
	if found := maskedTextMakers(fset, files); len(found) > 0 {
		t.Errorf("FreeText, MaskedNumber or Path made outside masker.go:\n%s", strings.Join(found, "\n"))
	}
	planted, err := parser.ParseFile(fset, "planted.go", `package mcpserver
import "strings"
func plantedConversion(s string) FreeText { return FreeText(strings.Join([]string{s}, "")) }
const plantedConstant MaskedNumber = "...135"
func plantedUntyped() warning { return warning{Code: "x", Message: "a sentence"} }
func plantedPath() Path { var p Path = "~/x"; return p }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := maskedTextMakers(fset, append(slices.Clone(files), planted))
	if len(found) != 4 {
		t.Errorf("the check found %d of the 4 planted makers:\n%s", len(found), strings.Join(found, "\n"))
	}
}

// kindTypes are the types a string field of an output may have.
var kindTypes = []reflect.Type{
	reflect.TypeFor[FreeText](), reflect.TypeFor[MaskedNumber](), reflect.TypeFor[Path](), reflect.TypeFor[Ident](),
	reflect.TypeFor[Date](), reflect.TypeFor[Instant](), reflect.TypeFor[Cursor](),
}

// fieldsWithoutKind returns the fields of t, an output type, at every
// depth, of kind string, or a slice, array or pointer of one, whose type is
// not one of the kinds, and the fields whose JSON names hold account_number,
// accountnumber or tag.
func fieldsWithoutKind(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t.Kind() == reflect.String {
		if !slices.Contains(kindTypes, t) {
			return []string{path + " is a " + t.String()}
		}
		return nil
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return nil
	}
	seen[t] = true
	defer delete(seen, t)
	var found []string
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		lower := strings.ToLower(name)
		if strings.Contains(lower, "account_number") || strings.Contains(lower, "accountnumber") || strings.Contains(lower, "tag") {
			found = append(found, path+"."+name+" is named for an account number or a tag")
		}
		found = append(found, fieldsWithoutKind(f.Type, path+"."+name, seen)...)
	}
	return found
}

// Every string field of every registered tool's output, at every depth,
// has one of the seven kinds, and no field is named for an account number
// or a tag, so that an author adding a field must decide what it holds.
// The check itself finds a plain string, a list of them, a pointer to one
// and the names.
func TestOutputStringFieldsHaveKinds(t *testing.T) {
	t.Parallel()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	outs := []reflect.Type{
		reflect.TypeFor[envelope](), reflect.TypeFor[page](), reflect.TypeFor[cuts](), reflect.TypeFor[accountRef](),
		reflect.TypeFor[span](), reflect.TypeFor[problem](),
	}
	for _, tool := range s.tools {
		outs = append(outs, tool.out)
	}
	for _, typ := range outs {
		if found := fieldsWithoutKind(typ, typ.Name(), map[reflect.Type]bool{}); len(found) > 0 {
			t.Errorf("%s:\n%s", typ, strings.Join(found, "\n"))
		}
	}
	type bad struct {
		envelope
		Plain    string                 `json:"plain"`
		List     []string               `json:"list"`
		Pointer  *string                `json:"pointer"`
		Number   Ident                  `json:"account_number"`
		OrderTag Ident                  `json:"order_tag"`
		Nested   struct{ Inner string } `json:"nested"`
		Fine     FreeText               `json:"fine"`
	}
	if found := fieldsWithoutKind(reflect.TypeFor[bad](), "bad", map[reflect.Type]bool{}); len(found) != 6 {
		t.Errorf("the check found %d of 6 planted fields:\n%s", len(found), strings.Join(found, "\n"))
	}
}

// The server keeps to what lint cannot hold (design 3.8): no go statement
// outside lifecycle.go, where safeGo recovers what the goroutine panics,
// and no panic or os.Exit outside registration, addTool, which runs at
// startup.
func TestServerConventions(t *testing.T) {
	t.Parallel()
	fset, files := packageFiles(t, false)
	check := func(files []*ast.File) []string {
		var found []string
		for _, f := range files {
			name := filepath.Base(fset.Position(f.Pos()).Filename)
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				ast.Inspect(d, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.GoStmt:
						if name != "lifecycle.go" {
							found = append(found, fset.Position(n.Pos()).String()+": a go statement")
						}
					case *ast.CallExpr:
						registration := ok && fd.Name.Name == "addTool"
						switch fun := n.Fun.(type) {
						case *ast.Ident:
							if fun.Name == "panic" && !registration {
								found = append(found, fset.Position(n.Pos()).String()+": panic")
							}
						case *ast.SelectorExpr:
							if x, ok := fun.X.(*ast.Ident); ok && x.Name == "os" && fun.Sel.Name == "Exit" {
								found = append(found, fset.Position(n.Pos()).String()+": os.Exit")
							}
						}
					}
					return true
				})
			}
		}
		return found
	}
	if found := check(files); len(found) > 0 {
		t.Errorf("the server's conventions are broken:\n%s", strings.Join(found, "\n"))
	}
	planted, err := parser.ParseFile(fset, "planted.go", `package mcpserver
import "os"
func planted() { go func() {}(); panic("x"); os.Exit(1) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if found := check([]*ast.File{planted}); len(found) != 3 {
		t.Errorf("the check found %d of 3 planted breaches: %v", len(found), found)
	}
}
