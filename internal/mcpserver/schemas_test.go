package mcpserver

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// update rewrites the goldens: the schemas TestSchemaGoldens holds, and
// the answers TestToolsEmptyAndFull holds.
var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// The tests of design 9.6, over every tool that a server of the default
// options serves, as tools/list serves it to a client.

// servedSchemas returns each served tool's input and output schemas as the
// wire carried them, by tool name.
func servedSchemas(t *testing.T) (*env, map[string][2]json.RawMessage) {
	t.Helper()
	e := newEnv(t)
	schemas := map[string][2]json.RawMessage{}
	for _, line := range e.wire.lines() {
		msg, ok := strings.CutPrefix(line, "read: ")
		if !ok {
			continue
		}
		var m struct {
			Result *struct {
				Tools []struct {
					Name         string          `json:"name"`
					InputSchema  json.RawMessage `json:"inputSchema"`
					OutputSchema json.RawMessage `json:"outputSchema"`
				} `json:"tools"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(msg), &m) != nil || m.Result == nil {
			continue
		}
		for _, tool := range m.Result.Tools {
			schemas[tool.Name] = [2]json.RawMessage{tool.InputSchema, tool.OutputSchema}
		}
	}
	if len(schemas) == 0 {
		t.Fatal("no tools/list answer was recorded")
	}
	return e, schemas
}

// Every served schema keeps to the portable subset (design 9.2).
func TestSchemasArePortable(t *testing.T) {
	t.Parallel()
	_, schemas := servedSchemas(t)
	for name, s := range schemas {
		if err := lintSchema(s[0], true); err != nil {
			t.Errorf("%s's input schema: %v", name, err)
		}
		if s[1] == nil {
			t.Errorf("%s serves no output schema", name)
			continue
		}
		if err := lintSchema(s[1], false); err != nil {
			t.Errorf("%s's output schema: %v", name, err)
		}
	}
}

// Every served schema resolves in jsonschema-go with no $schema, which is
// 2020-12, and as draft-07.
func TestSchemasCompile(t *testing.T) {
	t.Parallel()
	_, schemas := servedSchemas(t)
	for name, s := range schemas {
		for i, raw := range s {
			for _, draft := range []string{"", "http://json-schema.org/draft-07/schema#"} {
				var sch jsonschema.Schema
				if err := json.Unmarshal(raw, &sch); err != nil {
					t.Fatalf("%s schema %d: %v", name, i, err)
				}
				sch.Schema = draft
				if _, err := sch.Resolve(nil); err != nil {
					t.Errorf("%s schema %d as %q: %v", name, i, draft, err)
				}
			}
		}
	}
}

// The schemas are what testdata/schemas holds, byte for byte, as served
// and indented; -update rewrites them, so that a change shows in review.
// They are what CI's schemas job compiles with Ajv.
func TestSchemaGoldens(t *testing.T) {
	t.Parallel()
	_, schemas := servedSchemas(t)
	for name, s := range schemas {
		for i, suffix := range []string{".in.json", ".out.json"} {
			var b bytes.Buffer
			if err := json.Indent(&b, s[i], "", "  "); err != nil {
				t.Fatal(err)
			}
			b.WriteByte('\n')
			checkGolden(t, filepath.Join("testdata", "schemas", name+suffix), b.Bytes())
		}
	}
	files, err := filepath.Glob(filepath.Join("testdata", "schemas", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(f), ".json"), ".in")
		name = strings.TrimSuffix(name, ".out")
		if _, ok := schemas[name]; !ok {
			t.Errorf("%s is the golden of no served tool", f)
		}
	}
}

// checkGolden compares got with the golden file at path, or rewrites it
// under -update.
func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%v (run go test -update to write it)", err)
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from what is served (go test -update rewrites it):\ngot\n%s\nwant\n%s", path, got, want)
	}
}

// Every tool's name and argument names keep to the shapes that clients
// take (design 9.5), and its description to 600 characters; the server's
// instructions to 2,000.
func TestToolNames(t *testing.T) {
	t.Parallel()
	e, schemas := servedSchemas(t)
	toolName := regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)
	argName := regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	for name, s := range schemas {
		if !toolName.MatchString(name) {
			t.Errorf("the tool name %q", name)
		}
		var in struct{ Properties map[string]any }
		if err := json.Unmarshal(s[0], &in); err != nil {
			t.Fatal(err)
		}
		for arg := range in.Properties {
			if !argName.MatchString(arg) {
				t.Errorf("%s's argument %q", name, arg)
			}
		}
		if n := len([]rune(e.tools[name].Description)); n > 600 {
			t.Errorf("%s's description has %d characters, over 600", name, n)
		}
	}
	if n := len([]rune(instructions)); n > 2000 {
		t.Errorf("the instructions have %d characters, over 2,000", n)
	}
}

// requiredArguments are the only required arguments (design 9.1); every
// other tool requires none.
var requiredArguments = map[string][]string{
	"get_transaction": {"id"}, "get_order": {"id"}, "get_quotes": {"symbols"},
	"get_price_history": {"symbol"}, "get_option_chain": {"symbol"}, "get_option_expirations": {"symbol"},
	"search_instruments": {"query"}, "get_movers": {"index"},
}

// Each served input schema's required list is design 9.1's.
func TestRequiredArguments(t *testing.T) {
	t.Parallel()
	_, schemas := servedSchemas(t)
	for name, s := range schemas {
		var in struct{ Required []string }
		if err := json.Unmarshal(s[0], &in); err != nil {
			t.Fatal(err)
		}
		if want := requiredArguments[name]; !slices.Equal(in.Required, want) {
			t.Errorf("%s requires %v, want %v", name, in.Required, want)
		}
	}
}

// Every answer of every case of TestToolsEmptyAndFull validates against
// its tool's output schema, which env.call checks on every call; here, an
// answer that fails it, which addTool would have refused, is caught by that
// check too.
func TestOutputsMatchSchemas(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	out, ok := e.outputs["auth_status"]
	if !ok {
		t.Fatal("auth_status serves no output schema")
	}
	r := e.call("auth_status", `{}`)
	var v map[string]any
	if err := json.Unmarshal(r.raw, &v); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(v); err != nil {
		t.Errorf("auth_status's answer: %v", err)
	}
	v["logged_in"] = "yes"
	if out.Validate(v) == nil {
		t.Error("the output schema takes a string for logged_in")
	}
	delete(v, "store")
	v["logged_in"] = true
	if out.Validate(v) == nil {
		t.Error("the output schema takes an answer without store")
	}
}
