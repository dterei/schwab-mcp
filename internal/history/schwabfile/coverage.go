package schwabfile

import (
	"fmt"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// rowLimits are the numbers of rows that a History export is reported to
// stop at, by third parties, never by Schwab: an export with exactly as
// many may have lost its oldest days.
var rowLimits = []int{1500, 10000}

// limits returns the warnings of a History or Realized Gain/Loss file
// that its rows give as a whole: none, exactly as many as a limit, a stated
// range that ends two years after its oldest row, a late start, and a
// stated total that is not the rows' sum.
func (f *File) limits() []Warning {
	if f.Kind != KindHistoryCSV && f.Kind != KindHistoryJSON && f.Kind != KindRealized {
		return nil
	}
	var w []Warning
	n := len(f.History) + len(f.Realized)
	if n == 0 {
		w = append(w, Warning{WarnEmpty, "no rows"})
	}
	for _, limit := range rowLimits {
		if n == limit {
			w = append(w, Warning{WarnAtCap, fmt.Sprintf("exactly %d rows", n)})
		}
	}
	oldest, _ := f.span()
	if f.StatedFrom != "" && f.StatedTo != "" && oldest != "" {
		twoYears := addYears(f.StatedTo, -2)
		if f.StatedFrom < twoYears && days(oldest, twoYears) <= 7 && days(twoYears, oldest) <= 7 {
			w = append(w, Warning{WarnTwoYears, fmt.Sprintf("the range %s to %s is longer than two years, and its oldest "+
				"row, of %s, is within a week of %s", f.StatedFrom, f.StatedTo, oldest, twoYears)})
		}
	}
	if f.Kind != KindRealized && f.StatedFrom != "" && oldest != "" && days(f.StatedFrom, oldest) > 7 {
		w = append(w, Warning{WarnLateStart, fmt.Sprintf("the range starts on %s, but its oldest row is of %s",
			f.StatedFrom, oldest)})
	}
	if f.Kind != KindRealized {
		w = append(w, f.historyTotals()...)
	}
	return w
}

// historyTotals checks a History file's stated totals, of its Amounts and
// its Fees & Comm, against the rows' sums.
func (f *File) historyTotals() []Warning {
	var off []string
	for _, t := range []struct {
		name   string
		stated *Decimal
		cell   func(HistoryRow) Decimal
	}{
		{"total", f.StatedTotal, func(r HistoryRow) Decimal { return r.Amount }},
		{"total of fees", f.StatedFees, func(r HistoryRow) Decimal { return r.Fees }},
	} {
		if t.stated == nil {
			continue
		}
		cells := make([]Decimal, len(f.History))
		for i, r := range f.History {
			cells[i] = t.cell(r)
		}
		got, ok := sum(cells...)
		switch want, isNumber := t.stated.rat(); {
		case !isNumber:
			off = append(off, fmt.Sprintf("the stated %s, %q, is not a number", t.name, mask.Text(t.stated.Text)))
		case ok && got.Cmp(want) != 0:
			off = append(off, fmt.Sprintf("the stated %s is %s but the rows sum to %s", t.name, t.stated.Text, ratText(got)))
		}
	}
	if len(off) == 0 {
		return nil
	}
	return []Warning{{WarnTotalMismatch, strings.Join(off, "; ")}}
}

// span returns the oldest and newest dates of a file's rows: History's
// posted dates, Realized Gain/Loss's closed dates. Rows without one are
// left out; both are "" when no row has one.
func (f *File) span() (oldest, newest nytime.Date) {
	add := func(d nytime.Date) {
		if d == "" {
			return
		}
		if oldest == "" || d < oldest {
			oldest = d
		}
		if newest == "" || d > newest {
			newest = d
		}
	}
	for _, r := range f.History {
		add(r.Posted)
	}
	for _, r := range f.Realized {
		add(r.Closed)
	}
	return oldest, newest
}

// A Coverage is the span of days on which a History or Realized Gain/Loss
// file holds every row it would hold, before any question of filters.
type Coverage struct {
	// RangeFrom and RangeTo are its range: what the file states, else
	// what you gave, else its oldest and newest rows' dates, each side on
	// its own; widened to hold every row. Source says which: "stated",
	// "user" or "rows", the first that gave a side. All three are "" for a
	// file without a range or a row.
	RangeFrom, RangeTo nytime.Date
	Source             string
	// From and To are the days it covers: its range, narrowed by what its
	// warnings show. Both are "" when it covers none.
	From, To nytime.Date
	// Warnings say where the range came from, when it was widened, and
	// how it was narrowed.
	Warnings []Warning
}

// Coverage returns the days that f, a History or Realized Gain/Loss file,
// covers. from and to are a range you gave, each "" when you gave none,
// which counts only where the file states none; exported is the New York
// date of the export, from ExportedAt, or else of the import. The range
// is narrowed as its warnings show: an empty file covers nothing; one at a
// limit, starting late, or stopping at two years starts the day after its
// oldest row, which may be partial, since rows come newest first; and a
// range that ends on or after the day of the export ends the day before,
// since an export made during a day holds only part of it.
func (f *File) Coverage(from, to, exported nytime.Date) Coverage {
	var c Coverage
	oldest, newest := f.span()
	sides := []struct {
		stated, given, rows nytime.Date
		at                  *nytime.Date
		name                string
	}{
		{f.StatedFrom, from, oldest, &c.RangeFrom, "start"},
		{f.StatedTo, to, newest, &c.RangeTo, "end"},
	}
	var fromRows []string // the sides of the range that its rows give
	for _, s := range sides {
		switch {
		case s.stated != "":
			*s.at = s.stated
			c.Source = firstOf(c.Source, "stated")
		case s.given != "":
			*s.at = s.given
			c.Source = firstOf(c.Source, "user")
		case s.rows != "":
			*s.at = s.rows
			fromRows = append(fromRows, s.name)
		}
	}
	if c.RangeFrom == "" || c.RangeTo == "" {
		return Coverage{}
	}
	if len(fromRows) > 0 {
		c.Source = firstOf(c.Source, "rows")
		detail := fmt.Sprintf("it states no range, and none was given: its range is its rows', %s to %s", c.RangeFrom, c.RangeTo)
		if len(fromRows) == 1 {
			detail = fmt.Sprintf("it states no %s of its range, and none was given: its rows give it, %s to %s",
				fromRows[0], c.RangeFrom, c.RangeTo)
		}
		c.Warnings = append(c.Warnings, Warning{WarnCoverageFromRows, detail})
	}
	var outside []string
	if oldest != "" && oldest < c.RangeFrom {
		outside = append(outside, fmt.Sprintf("rows from %s, before %s", oldest, c.RangeFrom))
		c.RangeFrom = oldest
	}
	if newest != "" && newest > c.RangeTo {
		outside = append(outside, fmt.Sprintf("rows to %s, after %s", newest, c.RangeTo))
		c.RangeTo = newest
	}
	if len(outside) > 0 {
		c.Warnings = append(c.Warnings, Warning{WarnRowsOutsideCoverage, strings.Join(outside, "; ")})
	}
	c.From, c.To = c.RangeFrom, c.RangeTo
	var clipped []string
	for _, w := range f.Warnings {
		switch w.Code {
		case WarnEmpty:
			clipped = append(clipped, "empty: it holds no rows, as an export over a limit is reported to")
			c.From, c.To = "", ""
		case WarnAtCap, WarnLateStart, WarnTwoYears:
			if day := addDays(oldest, 1); c.From != "" && day > c.From {
				clipped = append(clipped, fmt.Sprintf("%s: it starts on %s, the day after its oldest row", w.Code, day))
				c.From = day
			}
		}
	}
	if day := addDays(exported, -1); exported != "" && c.To != "" && c.To > day {
		clipped = append(clipped, fmt.Sprintf("exported on %s: it ends on %s, the day before", exported, day))
		c.To = day
	}
	if c.From > c.To {
		c.From, c.To = "", ""
	}
	if len(clipped) > 0 {
		c.Warnings = append(c.Warnings, Warning{WarnCoverageClipped, strings.Join(clipped, "; ")})
	}
	return c
}

// firstOf returns a, or b when a is "".
func firstOf(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// addDays returns the date n days after d, or "" when d is "".
func addDays(d nytime.Date, n int) nytime.Date {
	t, err := time.Parse(time.DateOnly, string(d))
	if err != nil {
		return ""
	}
	return nytime.Date(t.AddDate(0, 0, n).Format(time.DateOnly))
}

// addYears returns the date n years after d, or "" when d is "".
func addYears(d nytime.Date, n int) nytime.Date {
	t, err := time.Parse(time.DateOnly, string(d))
	if err != nil {
		return ""
	}
	return nytime.Date(t.AddDate(n, 0, 0).Format(time.DateOnly))
}

// days returns the number of days from a to b, negative when b is before
// a.
func days(a, b nytime.Date) int {
	ta, err1 := time.Parse(time.DateOnly, string(a))
	tb, err2 := time.Parse(time.DateOnly, string(b))
	if err1 != nil || err2 != nil {
		return 0
	}
	return int(tb.Sub(ta).Hours() / 24)
}
