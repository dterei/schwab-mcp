package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/lookup"
)

// The three patterns that an input schema may hold (design 9.1).
const (
	datePattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}$` // a New York date
	idPattern   = `^[0-9]{1,19}$`                // a numeric ID, such as an order's
	codePattern = `^[$A-Za-z_]{1,40}$`           // one of Schwab's codes, such as a transaction type
)

// typeSchemas are the schemas of the types that marshal themselves: the
// figures, which are null for none, and Schwab's raw JSON, which is never
// {}: that serializes as true, which some clients cannot load.
var typeSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[lookup.Money](): {Types: []string{"null", "number"}},
	reflect.TypeFor[lookup.Qty]():   {Types: []string{"null", "number"}},
	reflect.TypeFor[lookup.Price](): {Types: []string{"null", "number"}},
	reflect.TypeFor[Num]():          {Types: []string{"null", "number"}},
	reflect.TypeFor[json.RawMessage](): {
		Types:       []string{"null", "object", "array"},
		Description: "Schwab's JSON, account numbers masked",
	},
}

// An argRule is what an input schema says of an argument beyond its type:
// its description, and the bounds that the server checks before a handler
// runs. Defaults live in the description and the handler, never in a
// default keyword.
type argRule struct {
	desc           string
	enum           []string // the server's own vocabularies only (design 9.4)
	min, max       *float64 // for a number
	minLen, maxLen *int     // for a string
	maxItems       *int     // for a list
	pattern        string   // one of datePattern, idPattern and codePattern
	// itemEnum, itemPattern and itemMaxLen hold each item of a list.
	itemEnum    []string
	itemPattern string
	itemMaxLen  *int
}

// argRules are the rules of the arguments that several tools take, by
// name; a tool's own (toolDef.args) are applied over them.
var argRules = map[string]argRule{
	"account": {
		desc: "One account: its account_hash (from list_accounts), the last 3 or more digits of its number " +
			"(135 or ...135), its store ref (A1) or its nickname; every account when left out.",
		minLen: new(1), maxLen: new(100),
	},
	"detail": {desc: "compact (the default) or full, which adds fields.", enum: []string{"compact", "full"}},
	"source": {
		desc: "Where to read each account from: auto (the default: the local history when it holds every asked day, " +
			"else Schwab), store or schwab.",
		enum: []string{"auto", "store", "schwab"},
	},
	"categories": {
		desc:     "Only these categories of the local history's ledger; every category when left out.",
		maxItems: new(12), itemEnum: history.Categories(),
	},
	"types": {
		desc:     "Only these of Schwab's transaction types, such as TRADE; every type when left out.",
		maxItems: new(12), itemPattern: codePattern,
	},
	"from":   {desc: "The first New York date, YYYY-MM-DD.", pattern: datePattern},
	"to":     {desc: "The last New York date, YYYY-MM-DD, included.", pattern: datePattern},
	"cursor": {desc: "next_cursor from the previous page, with the same other arguments.", minLen: new(1), maxLen: new(cursorMax)},
	"symbol": {desc: "One symbol, in any case.", minLen: new(1), maxLen: new(32)},
}

// enumProperties are the properties that may hold an enum: the server's
// own vocabularies (design 9.4).
var enumProperties = []string{"detail", "source", "categories", "contract_type", "interval", "by", "sort"}

// inputSchema returns In's schema, with each property's rule applied:
// argRules', then def's own over them. Every property must have a
// description.
func inputSchema[In any](def toolDef) (*jsonschema.Schema, error) {
	s, err := jsonschema.For[In](&jsonschema.ForOptions{TypeSchemas: typeSchemas})
	if err != nil {
		return nil, err
	}
	if s.Type != "object" {
		return nil, fmt.Errorf("the input of %s is not an object", def.name)
	}
	if s.Properties == nil {
		s.Properties = map[string]*jsonschema.Schema{} // {"properties":{}}, as design 9.3 has it
	}
	for name := range def.args {
		if _, ok := s.Properties[name]; !ok {
			return nil, fmt.Errorf("%s has a rule for %s, which it does not take", def.name, name)
		}
	}
	for name, p := range s.Properties {
		rule := argRules[name]
		if own, ok := def.args[name]; ok {
			rule = merge(rule, own)
		}
		apply(p, rule)
		if p.Description == "" {
			return nil, fmt.Errorf("the argument %s of %s has no description", name, def.name)
		}
	}
	return s, nil
}

// merge returns base with own's settings over it.
func merge(base, own argRule) argRule {
	if own.desc != "" {
		base.desc = own.desc
	}
	if own.enum != nil {
		base.enum = own.enum
	}
	if own.min != nil {
		base.min = own.min
	}
	if own.max != nil {
		base.max = own.max
	}
	if own.minLen != nil {
		base.minLen = own.minLen
	}
	if own.maxLen != nil {
		base.maxLen = own.maxLen
	}
	if own.maxItems != nil {
		base.maxItems = own.maxItems
	}
	if own.pattern != "" {
		base.pattern = own.pattern
	}
	if own.itemEnum != nil {
		base.itemEnum = own.itemEnum
	}
	if own.itemPattern != "" {
		base.itemPattern = own.itemPattern
	}
	if own.itemMaxLen != nil {
		base.itemMaxLen = own.itemMaxLen
	}
	return base
}

// apply sets r's keywords on p, a property's schema. A description in the
// field's jsonschema tag wins over the rule's.
func apply(p *jsonschema.Schema, r argRule) {
	if p.Description == "" {
		p.Description = r.desc
	}
	if r.enum != nil {
		p.Enum = anys(r.enum)
	}
	p.Minimum, p.Maximum = orElse(p.Minimum, r.min), orElse(p.Maximum, r.max)
	p.MinLength, p.MaxLength = orElse(p.MinLength, r.minLen), orElse(p.MaxLength, r.maxLen)
	p.MaxItems = orElse(p.MaxItems, r.maxItems)
	if r.pattern != "" {
		p.Pattern = r.pattern
	}
	if p.Items != nil {
		if r.itemEnum != nil {
			p.Items.Enum = anys(r.itemEnum)
		}
		if r.itemPattern != "" {
			p.Items.Pattern = r.itemPattern
		}
		p.Items.MaxLength = orElse(p.Items.MaxLength, r.itemMaxLen)
	}
}

// orElse returns v, or else w when v is nil.
func orElse[T any](v, w *T) *T {
	if v != nil {
		return v
	}
	return w
}

// anys returns ss as []any, as a schema's enum holds them.
func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// outputSchema returns Out's schema.
func outputSchema[Out any]() (*jsonschema.Schema, error) {
	return jsonschema.For[Out](&jsonschema.ForOptions{TypeSchemas: typeSchemas})
}

// The keywords that lintSchema allows in every schema, and in input
// schemas alone.
var (
	everyKeyword = []string{"type", "properties", "required", "items", "additionalProperties", "description", "title"}
	inputKeyword = []string{"enum", "pattern", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"}
)

// lintSchema checks that schema, as served, holds only the portable subset
// of JSON Schema (design 9.2): an object at the root; type, properties,
// required, items, additionalProperties (false only), description and
// title; and in an input schema also an enum of at most 20 strings on a
// property that design 9.4 lists, one of the three patterns, and numeric
// and length bounds. It refuses $schema, $id, $ref, $defs, const, format,
// default, examples, combinators, boolean subschemas, and an enum or a
// pattern in an output schema.
func lintSchema(schema []byte, input bool) error {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	obj, ok := root.(map[string]any)
	if !ok || obj["type"] != "object" {
		return errors.New(`the root is not {"type":"object"}`)
	}
	return lintNode(obj, "", "", input)
}

// lintNode checks one schema, at path, the schema of the property named
// property ("" for none).
func lintNode(node map[string]any, path, property string, input bool) error {
	for key, v := range node {
		at := path + "/" + key
		switch {
		case slices.Contains(everyKeyword, key):
		case input && slices.Contains(inputKeyword, key):
		default:
			return fmt.Errorf("%s: the keyword %s is not in the portable subset", at, key)
		}
		switch key {
		case "type":
			if err := lintType(v); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: not an object", at)
			}
			for name, p := range props {
				sub, ok := p.(map[string]any)
				if !ok {
					return fmt.Errorf("%s/%s: a boolean subschema", at, name)
				}
				if err := lintNode(sub, at+"/"+name, name, input); err != nil {
					return err
				}
			}
		case "items":
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: a boolean subschema, or a list of them", at)
			}
			if err := lintNode(sub, at, property, input); err != nil {
				return err
			}
		case "additionalProperties":
			if v != false {
				return fmt.Errorf("%s: only false is allowed", at)
			}
		case "required":
			names, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%s: not a list", at)
			}
			for _, n := range names {
				if _, ok := n.(string); !ok {
					return fmt.Errorf("%s: not a list of names", at)
				}
			}
		case "enum":
			values, ok := v.([]any)
			if !ok || len(values) == 0 || len(values) > 20 {
				return fmt.Errorf("%s: not a list of 1 to 20 strings", at)
			}
			for _, e := range values {
				if _, ok := e.(string); !ok {
					return fmt.Errorf("%s: not a list of 1 to 20 strings", at)
				}
			}
			if !slices.Contains(enumProperties, property) {
				return fmt.Errorf("%s: an enum on %q, which is not one of the server's own vocabularies", at, property)
			}
		case "pattern":
			if p, _ := v.(string); p != datePattern && p != idPattern && p != codePattern {
				return fmt.Errorf("%s: not one of the three patterns", at)
			}
		case "description", "title":
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s: not a string", at)
			}
		default: // the bounds
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("%s: not a number", at)
			}
		}
	}
	return nil
}

// jsonTypes are the names of JSON Schema's types.
var jsonTypes = []string{"null", "boolean", "object", "array", "number", "integer", "string"}

// lintType checks a type keyword's value: a type's name or a list of them.
func lintType(v any) error {
	names := []any{v}
	if list, ok := v.([]any); ok {
		names = list
	}
	for _, n := range names {
		if s, ok := n.(string); !ok || !slices.Contains(jsonTypes, s) {
			return fmt.Errorf("%v is not a type", n)
		}
	}
	return nil
}

// propertyAt is the first property that a validator's error names, as in
// "validating /properties/limit: ...".
var propertyAt = regexp.MustCompile(`/properties/([A-Za-z0-9_]+)`)

// takes says, in the server's own words, what the argument whose schema is
// p takes, from its bounds, its enum, its pattern or its description.
func takes(p *jsonschema.Schema) string {
	typ := p.Type
	if typ == "" {
		for _, t := range p.Types {
			if t != "null" {
				typ = t
			}
		}
	}
	switch {
	case len(p.Enum) > 0:
		return "one of " + joinEnum(p.Enum)
	case typ == "array" && p.Items != nil && len(p.Items.Enum) > 0:
		return fmt.Sprintf("a list of at most %d of %s", deref(p.MaxItems, 0), joinEnum(p.Items.Enum))
	case typ == "array" && p.Items != nil && p.Items.Pattern == codePattern:
		return fmt.Sprintf("a list of at most %d of Schwab's codes, each of letters, $ and _", deref(p.MaxItems, 0))
	case p.Pattern == datePattern:
		return "a New York date, YYYY-MM-DD"
	case p.Pattern == idPattern:
		return "1 to 19 digits"
	case p.Pattern == codePattern:
		return "one of Schwab's codes, of letters, $ and _"
	case typ == "integer" && p.Minimum != nil && p.Maximum != nil:
		return fmt.Sprintf("a whole number from %v to %v", *p.Minimum, *p.Maximum)
	case typ == "integer":
		return "a whole number"
	case typ == "boolean":
		return "true or false"
	case typ == "string" && p.MaxLength != nil:
		return fmt.Sprintf("a string of %d to %d characters", deref(p.MinLength, 0), *p.MaxLength)
	}
	return strings.TrimSuffix(p.Description, ".")
}

// joinEnum writes an enum's values as a list.
func joinEnum(values []any) string {
	s := make([]string, len(values))
	for i, v := range values {
		s[i] = fmt.Sprint(v)
	}
	return strings.Join(s, ", ")
}

// deref returns *v, or def for nil.
func deref[T any](v *T, def T) T {
	if v == nil {
		return def
	}
	return *v
}
