package history

import (
	"database/sql"
	"slices"
	"testing"
)

// TestDriver proves that the package registers the SQLite driver with
// database/sql under the name the store opens it by.
func TestDriver(t *testing.T) {
	if !slices.Contains(sql.Drivers(), "sqlite") {
		t.Fatalf("sql.Drivers() = %q, want it to hold \"sqlite\"", sql.Drivers())
	}
}
