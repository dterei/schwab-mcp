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
	// args are the rules the schemas were built from, which a definition's
	// must equal, by value, for these to be its schemas.
	args map[string]argRule
}

// A schemaKey names a tool's schemas but for its rules: its name and its
// types.
type schemaKey struct {
	name    string
	in, out reflect.Type
}

// A schemaCache holds the schemas built so far, by schemaKey, each with the
// rules they were built from. Rules are compared by value, not by their
// map: a tool file may make its rules anew for each server, as
// registerAccounts makes get_positions', bounds' pointers and all.
type schemaCache struct {
	mu sync.Mutex
	m  map[schemaKey][]*toolSchemas
}

// builtSchemas are the schemas built so far. A definition's schemas are the
// same for every server, and building them costs more than the rest of New:
// the tests, which make a server each, build each tool's once.
var builtSchemas schemaCache

// built returns the schemas of k built from rules equal to args, or nil.
func (sc *schemaCache) built(k schemaKey, args map[string]argRule) *toolSchemas {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.find(k, args)
}

// keep keeps sch as the schemas of k built from sch.args, unless another
// server kept equal ones first, and returns the schemas it keeps.
func (sc *schemaCache) keep(k schemaKey, sch *toolSchemas) *toolSchemas {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if kept := sc.find(k, sch.args); kept != nil {
		return kept
	}
	if sc.m == nil {
		sc.m = map[schemaKey][]*toolSchemas{}
	}
	sc.m[k] = append(sc.m[k], sch)
	return sch
}

// find returns the schemas of k built from rules equal to args, or nil. It
// is called with sc.mu held.
func (sc *schemaCache) find(k schemaKey, args map[string]argRule) *toolSchemas {
	for _, sch := range sc.m[k] {
		if reflect.DeepEqual(sch.args, args) {
			return sch
		}
	}
	return nil
}

// schemasOf returns the schemas of a tool of def, In and Out, built once,
// or why the tool is bad.
func schemasOf[In, Out any](def toolDef) (*toolSchemas, error) {
	k := schemaKey{name: def.name, in: reflect.TypeFor[In](), out: reflect.TypeFor[Out]()}
	if sch := builtSchemas.built(k, def.args); sch != nil {
		return sch, nil
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
	return builtSchemas.keep(k, sch), nil
}
