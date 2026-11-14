package mcpserver

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// A toolSchemas is a tool's schemas, built, held to the portable subset and
// resolved.
type toolSchemas struct {
	in, out       *jsonschema.Schema
	inRes, outRes *jsonschema.Resolved
	// args are the definition's rules, kept so that the map, whose address
	// is in the key, is not freed and its address used again.
	args map[string]argRule
}

// A schemaKey names a tool's schemas: its name, its types and its rules'
// map, by address.
type schemaKey struct {
	name    string
	in, out reflect.Type
	args    uintptr
}

// builtSchemas are the schemas built so far, by schemaKey. A definition's
// schemas are the same for every server, and building them costs more than
// the rest of New: the tests, which make a server each, build each tool's
// once.
var builtSchemas sync.Map

// schemasOf returns the schemas of a tool of def, In and Out, built once,
// or why the tool is bad.
func schemasOf[In, Out any](def toolDef) (*toolSchemas, error) {
	k := schemaKey{name: def.name, in: reflect.TypeFor[In](), out: reflect.TypeFor[Out](), args: reflect.ValueOf(def.args).Pointer()}
	if v, ok := builtSchemas.Load(k); ok {
		return v.(*toolSchemas), nil
	}
	in, err := inputSchema[In](def)
	if err != nil {
		return nil, fmt.Errorf("the input schema of %s: %w", def.name, err)
	}
	out, err := outputSchema[Out]()
	if err != nil {
		return nil, fmt.Errorf("the output schema of %s: %w", def.name, err)
	}
	outType := reflect.TypeFor[Out]()
	if !reflect.PointerTo(outType).Implements(reflect.TypeFor[enveloped]()) {
		return nil, errors.New(def.name + "'s output does not embed envelope")
	}
	if reflect.PointerTo(outType).Implements(reflect.TypeFor[fitter]()) &&
		!reflect.PointerTo(outType).Implements(reflect.TypeFor[cutter]()) {
		return nil, errors.New(def.name + "'s output is a fitter that does not embed cuts")
	}
	for _, sch := range []struct {
		s     *jsonschema.Schema
		input bool
	}{{in, true}, {out, false}} {
		b, err := encode(sch.s)
		if err == nil {
			err = lintSchema(b, sch.input)
		}
		if err != nil {
			return nil, fmt.Errorf("a schema of %s: %w", def.name, err)
		}
	}
	sch := &toolSchemas{in: in, out: out, args: def.args}
	if sch.inRes, err = in.Resolve(nil); err != nil {
		return nil, fmt.Errorf("resolving the input schema of %s: %w", def.name, err)
	}
	if sch.outRes, err = out.Resolve(nil); err != nil {
		return nil, fmt.Errorf("resolving the output schema of %s: %w", def.name, err)
	}
	v, _ := builtSchemas.LoadOrStore(k, sch)
	return v.(*toolSchemas), nil
}
