package schwabfile

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// A record is one record of a CSV file.
type record struct {
	// cells are the record's cells, trimmed.
	cells []string
	// line is the line where the record starts, counting from 1.
	line int
	// raw is the record as the text has it, without the line ends around
	// it.
	raw string
}

// empty reports whether every cell of the record is empty.
func (r record) empty() bool {
	return !slices.ContainsFunc(r.cells, func(c string) bool { return c != "" })
}

// cell returns the record's cell i, or "" when there is no such cell.
func (r record) cell(i int) string {
	if i < 0 || i >= len(r.cells) {
		return ""
	}
	return r.cells[i]
}

// title returns the record's only non-empty cell when it is the first
// one, as a banner or a title is: one cell, or one followed by empty
// cells. It returns "" for any other record.
func (r record) title() string {
	if len(r.cells) == 0 || r.cells[0] == "" || !(record{cells: r.cells[1:]}).empty() {
		return ""
	}
	return r.cells[0]
}

// readRecords reads text as CSV records of any number of cells, leniently
// about quotes when lazy, and keeps where each starts and its text.
func readRecords(text string, lazy bool) ([]record, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.LazyQuotes = lazy
	var out []record
	for {
		start := r.InputOffset()
		cells, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		line, _ := r.FieldPos(0)
		for i, c := range cells {
			cells[i] = strings.TrimSpace(c)
		}
		out = append(out, record{cells: cells, line: line, raw: strings.Trim(text[start:r.InputOffset()], "\r\n")})
	}
}

// readCSV reads a CSV file, which it tells by its first record: a History
// banner or header, a Realized Gain/Loss title or header, or one of
// Schwab's other files, which it refuses.
func readCSV(shown, text string) (*File, error) {
	var warnings []Warning
	recs, err := readRecords(text, false)
	if err != nil {
		strict := err
		if recs, err = readRecords(text, true); err != nil {
			return nil, fmt.Errorf("%s is not a CSV file that schwabtx can read: %w", shown, err)
		}
		warnings = append(warnings, Warning{WarnLazyQuotes, strict.Error()})
	}
	i := slices.IndexFunc(recs, func(r record) bool { return !r.empty() })
	if i < 0 {
		return nil, fmt.Errorf("%s is not a file schwabtx knows how to import", shown)
	}
	title := recs[i].title()
	if title == "" {
		return readByHeader(shown, nil, recs[i:], warnings)
	}
	lower := strings.ToLower(title)
	switch {
	case strings.HasPrefix(lower, "transactions"), strings.HasPrefix(lower, "realized gain/loss"):
		return readByHeader(shown, &recs[i], recs[i+1:], warnings)
	case strings.HasPrefix(lower, "positions for"):
		return nil, fmt.Errorf("%s is a Positions export, which schwabtx does not import", shown)
	case strings.Contains(lower, " lot details for "):
		return nil, fmt.Errorf("%s is the Lot Details of an open position, which schwabtx does not import; "+
			"export the Realized Gain/Loss Lot Details", shown)
	}
	return nil, fmt.Errorf("%s is not a file schwabtx knows how to import", shown)
}

// readByHeader reads a CSV file by its header, the first non-empty record
// of recs, under its title when it has one.
func readByHeader(shown string, title *record, recs []record, warnings []Warning) (*File, error) {
	h := slices.IndexFunc(recs, func(r record) bool { return !r.empty() })
	if h < 0 {
		return nil, fmt.Errorf("%s has a first line and nothing else; it is not a file schwabtx knows how to import", shown)
	}
	header := recs[h]
	names := make([]string, len(header.cells))
	for i, c := range header.cells {
		names[i] = strings.ToLower(c)
	}
	has := func(prefix string) bool {
		return slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, prefix) })
	}
	historyCols, historyMissing := bind(names, historyColumns)
	lotCols, lotMissing := bind(names, lotColumns)
	titleText := ""
	if title != nil {
		titleText = strings.ToLower(title.title())
	}
	var f *File
	switch {
	case historyMissing == "":
		f = readHistoryCSV(title, historyCols, recs[h+1:])
	case lotMissing == "" && !strings.HasPrefix(titleText, "transactions"):
		f = readRealized(title, lotCols, recs[h+1:])
	case has("costbasis") && has("total gain/loss"):
		return nil, fmt.Errorf("%s is the Realized Gain/Loss summary, which schwabtx does not import; "+
			"export the Realized Gain/Loss Lot Details", shown)
	case has("withdrawal") && has("runningbalance"):
		return nil, fmt.Errorf("%s is a Schwab Bank export, which schwabtx does not import", shown)
	case has("fairmarketvalueprice") || has("vestfairmarketvalue"):
		return nil, fmt.Errorf("%s is a Schwab Equity Awards export, which schwabtx does not import", shown)
	case strings.HasPrefix(titleText, "transactions"),
		!strings.HasPrefix(titleText, "realized gain/loss") && bound(names, historyColumns) >= len(historyColumns)/2:
		return nil, fmt.Errorf("%s has no %q column; it is not a History export, "+
			"or its layout is new: please report it", shown, historyMissing)
	case strings.HasPrefix(titleText, "realized gain/loss") || bound(names, lotColumns) >= len(lotColumns)/2:
		return nil, fmt.Errorf("%s has no %q column; it is not a Realized Gain/Loss Lot Details export, "+
			"or its layout is new: please report it", shown, lotMissing)
	default:
		return nil, fmt.Errorf("%s is not a file schwabtx knows how to import", shown)
	}
	f.LazyQuotes = slices.ContainsFunc(warnings, func(w Warning) bool { return w.Code == WarnLazyQuotes })
	f.Warnings = append(warnings, f.Warnings...)
	return f, nil
}

// A column is a column that a file's header names, by one of the names it
// has had.
type column []string

// bind finds each of cols in names, a header's cells in lower case: the
// first cell that holds one of a column's names, or -1. It returns the
// columns' indexes, and the first name of the first column that is not
// there, or "" when all are.
func bind(names []string, cols []column) ([]int, string) {
	idx := make([]int, len(cols))
	missing := ""
	for i, c := range cols {
		idx[i] = slices.IndexFunc(names, func(n string) bool {
			return slices.ContainsFunc(c, func(name string) bool { return strings.ToLower(name) == n })
		})
		if idx[i] < 0 && missing == "" {
			missing = c[0]
		}
	}
	return idx, missing
}

// bound counts the columns of cols that names holds.
func bound(names []string, cols []column) int {
	idx, _ := bind(names, cols)
	n := 0
	for _, i := range idx {
		if i >= 0 {
			n++
		}
	}
	return n
}
