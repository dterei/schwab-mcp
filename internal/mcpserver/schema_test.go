package mcpserver

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/dterei/schwab-mcp/internal/lookup"
)

// lintSchema allows the portable subset and refuses everything else: the
// keywords some clients cannot load, a root that is not an object, an
// enum outside the server's vocabularies, a pattern but the three, and an
// enum or a pattern in an output schema.
func TestLintSchema(t *testing.T) {
	t.Parallel()
	ok := `{"type":"object","properties":{"detail":{"type":"string","enum":["compact","full"],"description":"d"},` +
		`"from":{"type":"string","pattern":"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"},"limit":{"type":"integer","minimum":1,"maximum":200},` +
		`"categories":{"type":["null","array"],"items":{"type":"string","enum":["buy"]},"maxItems":12},` +
		`"rows":{"type":["null","array"],"items":{"type":"object","properties":{},"additionalProperties":false}}},` +
		`"required":["from"],"additionalProperties":false,"title":"t"}`
	if err := lintSchema([]byte(ok), true); err != nil {
		t.Errorf("a portable input schema was refused: %v", err)
	}
	for _, tc := range []struct {
		name, schema string
		input        bool
	}{
		{"an array root", `{"type":"array"}`, true},
		{"no type at the root", `{"properties":{}}`, false},
		{"$schema", `{"type":"object","$schema":"https://json-schema.org/draft/2020-12/schema"}`, false},
		{"$ref", `{"type":"object","properties":{"a":{"$ref":"#"}}}`, false},
		{"$defs", `{"type":"object","$defs":{}}`, false},
		{"const", `{"type":"object","properties":{"a":{"const":1}}}`, true},
		{"format", `{"type":"object","properties":{"a":{"type":"string","format":"date"}}}`, true},
		{"default", `{"type":"object","properties":{"a":{"type":"string","default":"x"}}}`, true},
		{"examples", `{"type":"object","properties":{"a":{"examples":[1]}}}`, true},
		{"anyOf", `{"type":"object","anyOf":[{"type":"object"}]}`, false},
		{"oneOf", `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"}]}}}`, false},
		{"allOf", `{"type":"object","allOf":[]}`, false},
		{"not", `{"type":"object","not":{}}`, false},
		{"if", `{"type":"object","if":{}}`, false},
		{"a boolean subschema", `{"type":"object","properties":{"raw":true}}`, false},
		{"additionalProperties true", `{"type":"object","additionalProperties":true}`, false},
		{"an enum in an output schema", `{"type":"object","properties":{"detail":{"type":"string","enum":["a"]}}}`, false},
		{"a pattern in an output schema", `{"type":"object","properties":{"from":{"type":"string","pattern":"^[0-9]{1,19}$"}}}`, false},
		{"bounds in an output schema", `{"type":"object","properties":{"n":{"type":"integer","minimum":1}}}`, false},
		{"an enum on another property", `{"type":"object","properties":{"status":{"type":"string","enum":["a"]}}}`, true},
		{"an enum of 21", `{"type":"object","properties":{"detail":{"type":"string","enum":["1","2","3","4","5","6","7","8","9","10","11","12","13","14","15","16","17","18","19","20","21"]}}}`, true},
		{"an enum of numbers", `{"type":"object","properties":{"detail":{"enum":[1]}}}`, true},
		{"another pattern", `{"type":"object","properties":{"a":{"type":"string","pattern":"^a$"}}}`, true},
		{"an unknown type", `{"type":"object","properties":{"a":{"type":"date"}}}`, true},
	} {
		if err := lintSchema([]byte(tc.schema), tc.input); err == nil {
			t.Errorf("%s: lintSchema took %s", tc.name, tc.schema)
		}
	}
}

// ruledIn is an input whose arguments take the shared rules and a tool's.
type ruledIn struct {
	Account string   `json:"account,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	From    string   `json:"from,omitempty"`
	Types   []string `json:"types,omitempty"`
	Limit   int      `json:"limit,omitempty"`
	ID      string   `json:"id"`
}

// The input schema takes each argument's rules by its name, a tool's own
// over the shared ones; every field but the required ones carries
// omitempty; an argument without a description, or a rule for one the
// tool does not take, is refused at registration.
func TestInputSchemaRules(t *testing.T) {
	t.Parallel()
	def := toolDef{name: "ruled", args: map[string]argRule{
		"limit": {desc: "Rows per page.", min: new(1.0), max: new(200.0)},
		"id":    {desc: "An order's ID.", pattern: idPattern},
	}}
	s, err := inputSchema[ruledIn](def)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encode(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := lintSchema(b, true); err != nil {
		t.Errorf("lintSchema: %v\n%s", err, b)
	}
	if got := s.Required; !slices.Equal(got, []string{"id"}) {
		t.Errorf("required = %v, want [id]", got)
	}
	p := s.Properties
	switch {
	case p["account"].MaxLength == nil || *p["account"].MaxLength != 100:
		t.Error("account lacks its maxLength")
	case len(p["detail"].Enum) != 2:
		t.Error("detail lacks its enum")
	case p["from"].Pattern != datePattern:
		t.Error("from lacks the date pattern")
	case p["types"].Items.Pattern != codePattern || p["types"].MaxItems == nil:
		t.Error("types lacks its items' pattern or its maxItems")
	case *p["limit"].Maximum != 200 || p["limit"].Description != "Rows per page.":
		t.Error("limit lacks its tool's rule")
	case p["id"].Pattern != idPattern:
		t.Error("id lacks its tool's pattern")
	}
	type undescribed struct {
		Mystery string `json:"mystery,omitempty"`
	}
	if _, err := inputSchema[undescribed](toolDef{name: "x"}); err == nil {
		t.Error("an argument without a description was taken")
	}
	if _, err := inputSchema[ruledIn](toolDef{name: "x", args: map[string]argRule{"nope": {desc: "x"}}}); err == nil {
		t.Error("a rule for an argument the tool does not take was taken")
	}
	type none struct{}
	s, err = inputSchema[none](toolDef{name: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := encode(s); string(b) != `{"type":"object","properties":{},"additionalProperties":false}` {
		t.Errorf("a tool without arguments has the input schema %s", b)
	}
}

// takes says what an argument takes, from its schema, in the server's own
// words.
func TestTakes(t *testing.T) {
	t.Parallel()
	s, err := inputSchema[ruledIn](toolDef{name: "ruled", args: map[string]argRule{
		"limit": {desc: "Rows per page.", min: new(1.0), max: new(200.0)},
		"id":    {desc: "An order's ID.", pattern: idPattern},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"account": "a string of 1 to 100 characters",
		"detail":  "one of compact, full",
		"from":    "a New York date, YYYY-MM-DD",
		"types":   "a list of at most 12 of Schwab's codes, each of letters, $ and _",
		"limit":   "a whole number from 1 to 200",
		"id":      "1 to 19 digits",
	} {
		if got := takes(s.Properties[name]); got != want {
			t.Errorf("takes(%s) = %q, want %q", name, got, want)
		}
	}
}

// nullable is an output with every kind of field that must take null.
type nullable struct {
	envelope
	Money   lookup.Money    `json:"money"`
	Qty     lookup.Qty      `json:"qty"`
	Price   lookup.Price    `json:"price"`
	Num     Num             `json:"num"`
	Pointer *Ident          `json:"pointer"`
	List    []Ident         `json:"list"`
	Raw     json.RawMessage `json:"raw"`
	Count   int             `json:"count"`
}

// nonNullable returns the properties of schema, at every depth, whose Go
// type in t is a pointer, a slice, a figure or raw JSON and whose schema
// does not take null (design 9.3).
func nonNullable(t reflect.Type, s *jsonschema.Schema, path string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var found []string
	if t.Kind() == reflect.Slice && t != reflect.TypeFor[json.RawMessage]() && s.Items != nil {
		found = append(found, nonNullable(t.Elem(), s.Items, path+"[]")...)
	}
	if t.Kind() != reflect.Struct || slices.Contains([]reflect.Type{
		reflect.TypeFor[lookup.Money](), reflect.TypeFor[lookup.Qty](), reflect.TypeFor[lookup.Price](), reflect.TypeFor[Num](),
	}, t) {
		return found
	}
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		p, ok := s.Properties[name]
		if !ok {
			continue
		}
		figure := slices.Contains([]reflect.Type{
			reflect.TypeFor[lookup.Money](), reflect.TypeFor[lookup.Qty](), reflect.TypeFor[lookup.Price](), reflect.TypeFor[Num](),
			reflect.TypeFor[json.RawMessage](),
		}, f.Type)
		if (figure || f.Type.Kind() == reflect.Pointer || f.Type.Kind() == reflect.Slice) && !slices.Contains(p.Types, "null") {
			found = append(found, path+"."+name)
		}
		found = append(found, nonNullable(f.Type, p, path+"."+name)...)
	}
	return found
}

// Every figure and every pointer, list and raw JSON of every output is
// nullable in its schema (design 9.3), the registered tools' and a test's
// that holds one of each.
func TestFiguresAreNullable(t *testing.T) {
	t.Parallel()
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sch, err := outputSchema[nullable]()
	if err != nil {
		t.Fatal(err)
	}
	if found := nonNullable(reflect.TypeFor[nullable](), sch, "nullable"); len(found) > 0 {
		t.Errorf("not nullable: %v", found)
	}
	if !slices.Contains(sch.Properties["count"].Types, "null") && sch.Properties["count"].Type != "integer" {
		t.Errorf("count's schema is %v", sch.Properties["count"])
	}
	for _, tool := range s.tools {
		if found := nonNullable(tool.out, tool.outSch, tool.name); len(found) > 0 {
			t.Errorf("%s: not nullable: %v", tool.name, found)
		}
	}
}
