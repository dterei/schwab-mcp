package lookup

import (
	"slices"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The places of the records' problems are counted once each, in the order
// met, the first problem at each kept, and only the first three given,
// with how many more there are.
func TestProblemPlaces(t *testing.T) {
	p := func(path, what string) schwab.Problem { return schwab.Problem{Path: path, What: what} }
	for _, tc := range []struct {
		name     string
		problems [][]schwab.Problem
		records  int
		first    []schwab.Problem
		more     int
	}{
		{"no records", nil, 0, nil, 0},
		{"records without problems", [][]schwab.Problem{nil, {}}, 0, nil, 0},
		{"one", [][]schwab.Problem{nil, {p("time", `Schwab sent "x", which is not a time`)}}, 1, []schwab.Problem{p("time", `Schwab sent "x", which is not a time`)}, 0},
		{"the record itself", [][]schwab.Problem{{p("", "not an object")}}, 1, []schwab.Problem{p("", "not an object")}, 0},
		{"one place on every record", [][]schwab.Problem{{p("time", "a")}, {p("time", "b")}, {p("time", "c")}}, 3, []schwab.Problem{p("time", "a")}, 0},
		{
			"three places",
			[][]schwab.Problem{{p("a", "1"), p("b", "2")}, {p("b", "3"), p("c", "4")}},
			2,
			[]schwab.Problem{p("a", "1"), p("b", "2"), p("c", "4")},
			0,
		},
		{
			"more than three places",
			[][]schwab.Problem{{p("a", "1"), p("b", "2")}, nil, {p("c", "3"), p("d", "4"), p("a", "5")}, {p("e", "6")}},
			3,
			[]schwab.Problem{p("a", "1"), p("b", "2"), p("c", "3")},
			2,
		},
	} {
		records, first, more := ProblemPlaces(tc.problems)
		if records != tc.records || !slices.Equal(first, tc.first) || more != tc.more {
			t.Errorf("%s: ProblemPlaces = %d, %q, %d; want %d, %q, %d", tc.name, records, first, more, tc.records, tc.first, tc.more)
		}
	}
}
