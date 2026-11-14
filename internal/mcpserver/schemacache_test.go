package mcpserver

import "testing"

// A definition's schemas are built once: building them again gives the
// same, and a definition of the same name and types but other rules, or
// another name, gets its own. A bad definition is never kept.
func TestSchemasBuiltOnce(t *testing.T) {
	t.Parallel()
	type idIn struct {
		ID string `json:"id"`
	}
	def := toolDef{name: "built_once", args: map[string]argRule{"id": {desc: "An ID.", pattern: idPattern}}}
	first, err := schemasOf[idIn, echoOut](def)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := schemasOf[idIn, echoOut](def); err != nil || again != first {
		t.Errorf("the same definition built again: %p, not %p (%v)", again, first, err)
	}
	other := def
	other.args = map[string]argRule{"id": {desc: "Another ID.", pattern: idPattern}}
	built, err := schemasOf[idIn, echoOut](other)
	if err != nil || built == first || built.in.Properties["id"].Description != "Another ID." {
		t.Errorf("other rules: %+v (%v)", built, err)
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
