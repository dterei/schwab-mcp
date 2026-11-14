package mcpserver

import "testing"

// A definition's schemas are built once: building them again gives the
// same, and so do the same rules in a map of their own, whose bounds are
// pointers of their own; a definition of the same name and types but other
// rules, or another name, gets its own. A bad definition is never kept.
func TestSchemasBuiltOnce(t *testing.T) {
	t.Parallel()
	type idIn struct {
		ID    string `json:"id"`
		Limit int    `json:"limit,omitempty"`
	}
	rules := func(desc string, most float64) map[string]argRule {
		return map[string]argRule{
			"id":    {desc: desc, pattern: idPattern},
			"limit": {desc: "The most rows.", min: new(1.0), max: new(most)},
		}
	}
	def := toolDef{name: "built_once", args: rules("An ID.", 200)}
	first, err := schemasOf[idIn, echoOut](def)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := schemasOf[idIn, echoOut](def); err != nil || again != first {
		t.Errorf("the same definition built again: %p, not %p (%v)", again, first, err)
	}
	same := def
	same.args = rules("An ID.", 200)
	if again, err := schemasOf[idIn, echoOut](same); err != nil || again != first {
		t.Errorf("the same rules in a new map: %p, not %p (%v)", again, first, err)
	}
	other := def
	other.args = rules("Another ID.", 200)
	built, err := schemasOf[idIn, echoOut](other)
	if err != nil || built == first || built.in.Properties["id"].Description != "Another ID." {
		t.Errorf("other rules: %+v (%v)", built, err)
	}
	other.args = rules("An ID.", 100)
	built, err = schemasOf[idIn, echoOut](other)
	if err != nil || built == first || built.in.Properties["limit"].Maximum == nil || *built.in.Properties["limit"].Maximum != 100 {
		t.Errorf("another bound: %+v (%v)", built, err)
	}
	renamed := def
	renamed.name = "built_twice"
	if built, err := schemasOf[idIn, echoOut](renamed); err != nil || built == first {
		t.Errorf("another name: %p (%v)", built, err)
	}
	bad := toolDef{name: "built_bad", args: map[string]argRule{"id": {desc: "An ID."}}}
	for range 2 {
		if _, err := schemasOf[idIn, struct{ Limit int }](bad); err == nil {
			t.Error("an output that does not embed envelope was taken")
		}
	}
}

// A second server takes every tool's schemas from the first, wherever the
// tool's file gives its rules: in a variable, as get_transactions' are, or
// in the registration, as get_positions' are, where each server's map is
// new.
func TestServersShareSchemas(t *testing.T) {
	t.Parallel()
	var tools [2][]toolInfo
	for i := range tools {
		s, err := New(Options{})
		if err != nil {
			t.Fatal(err)
		}
		tools[i] = s.tools
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}
	if len(tools[0]) == 0 || len(tools[0]) != len(tools[1]) {
		t.Fatalf("%d and %d tools", len(tools[0]), len(tools[1]))
	}
	for i, a := range tools[0] {
		if b := tools[1][i]; a.name != b.name || a.inSch != b.inSch || a.outSch != b.outSch {
			t.Errorf("%s: the second server built its schemas again", a.name)
		}
	}
}
