package schwabfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// A HistoryRow is one row of a History export, in CSV or JSON, as the
// file writes it.
type HistoryRow struct {
	// Ordinal is the row's place among the file's rows, and Line the line
	// where it starts, each counting from 1.
	Ordinal, Line int
	// Raw is the row as the file has it, decoded to UTF-8: the CSV record,
	// or the JSON element, which holds the keys Read does not read too.
	Raw string
	// Posted is the date Schwab posted the row, and AsOf the date of
	// "MM/DD/YYYY as of MM/DD/YYYY", when it has one. Both are "" when its
	// Date is not a date; the row is then kept, but undated.
	Posted, AsOf nytime.Date
	// Action, Symbol and Description are the cells, trimmed: History
	// padded actions with spaces in 2014.
	Action, Symbol, Description string
	// Quantity, Price, Fees (Fees & Comm) and Amount are the numbers as
	// written. Quantity keeps the sign written: trades' quantities are
	// unsigned, and the store signs them by the action.
	Quantity, Price, Fees, Amount Decimal
	// ItemIssueID and AcctgRuleCd are a JSON row's ItemIssueId (Schwab's
	// security id, "0" on a row without a symbol) and AcctgRuleCd, when it
	// has them; a CSV row has neither.
	ItemIssueID, AcctgRuleCd string
	// Problems say what of the row could not be read, such as a Date that
	// is not a date, or an Amount that is not a number. Cells are quoted
	// in them with their free text masked (mask.Text).
	Problems []string
}

// The columns of a History export, which every layout has had, in the
// order of today's header. Read binds them by name: one export of January
// 2024 put Price before Quantity.
var historyColumns = []column{
	{"Date"}, {"Action"}, {"Symbol"}, {"Description"}, {"Quantity"}, {"Price"}, {"Fees & Comm"}, {"Amount"},
}

// The indexes of historyColumns.
const (
	hDate = iota
	hAction
	hSymbol
	hDescription
	hQuantity
	hPrice
	hFees
	hAmount
)

// bannerRE is a History CSV's banner until 2023, its white space taken as
// one space: the account, the time of the export (a 24-hour clock until
// 2021, a 12-hour one after), and, in some, the range of the export.
var bannerRE = regexp.MustCompile(`(?i)^transactions for account (.*?) as of ([0-9]{1,2}/[0-9]{1,2}/[0-9]{4}) ` +
	`([0-9]{1,2}:[0-9]{2}:[0-9]{2})(?: ([AP]M))? ET(?: from ([0-9/]+) to ([0-9/]+))?$`)

// readHistoryCSV reads a History CSV's rows, after its header, whose
// columns are at cols, under its banner when it has one.
func readHistoryCSV(banner *record, cols []int, recs []record) *File {
	f := &File{Kind: KindHistoryCSV, Layout: LayoutCurrent}
	if banner != nil {
		f.readBanner(banner.title())
	}
	var after []int
	for _, r := range recs {
		switch {
		case r.empty():
		case after != nil:
			after = append(after, r.line)
		case strings.EqualFold(r.cell(0), "Transactions Total"):
			total := ""
			for _, c := range r.cells[1:] {
				if c != "" {
					total = c
				}
			}
			if total != "" { // a footer of empty cells states no total
				d, _ := ParseDecimal(total)
				f.StatedTotal = &d
			}
			after = []int{}
		default:
			f.History = append(f.History, historyRow(len(f.History)+1, r.line, r.raw, func(c int) (string, string) {
				return r.cell(cols[c]), ""
			}))
		}
	}
	if len(after) > 0 {
		f.Warnings = append(f.Warnings, Warning{
			WarnRowsAfterTotal,
			fmt.Sprintf("%d %s after the Transactions Total line, the first on line %d, not read",
				len(after), plural(len(after), "record", "records"), after[0]),
		})
	}
	f.Warnings = append(f.Warnings, undated(f.History)...)
	return f
}

// readBanner reads what a History CSV's banner says: the account's
// digits, the time of the export and the range, when it has one. A banner
// it cannot read leaves the layout LayoutBanner and says nothing.
func (f *File) readBanner(banner string) {
	f.Layout = LayoutBanner
	m := bannerRE.FindStringSubmatch(strings.Join(strings.Fields(banner), " "))
	if m == nil {
		return
	}
	at, layout := m[2]+" "+m[3], "1/2/2006 15:04:05"
	f.Layout = LayoutBanner24h
	if m[4] != "" {
		// "3", not "03": Schwab wrote the hour in one digit or two.
		at, layout = at+" "+strings.ToUpper(m[4]), "1/2/2006 3:04:05 PM"
		f.Layout = LayoutBanner12h
	}
	if t, err := time.ParseInLocation(layout, at, nytime.NewYork()); err == nil {
		f.ExportedAt = t.UTC()
	}
	f.AccountHint = accountHint(m[1])
	f.StatedFrom, _ = parseDate(m[5])
	f.StatedTo, _ = parseDate(m[6])
}

// historyRow makes a History row of its cells: cell(c) gives the text of
// historyColumns[c], or a problem with it.
func historyRow(ordinal, line int, raw string, cell func(c int) (string, string)) HistoryRow {
	row := HistoryRow{Ordinal: ordinal, Line: line, Raw: raw}
	text := make([]string, len(historyColumns))
	problems := make([]string, len(historyColumns))
	for c := range historyColumns {
		text[c], problems[c] = cell(c)
	}
	row.Posted, row.AsOf = parseHistoryDate(text[hDate])
	if row.Posted == "" && problems[hDate] == "" {
		problems[hDate] = fmt.Sprintf("Date %q is not MM/DD/YYYY", mask.Text(text[hDate]))
	}
	row.Action, row.Symbol, row.Description = text[hAction], text[hSymbol], text[hDescription]
	for c, d := range []*Decimal{hQuantity: &row.Quantity, hPrice: &row.Price, hFees: &row.Fees, hAmount: &row.Amount} {
		if d == nil {
			continue
		}
		var err error
		if *d, err = ParseDecimal(text[c]); err != nil && problems[c] == "" {
			problems[c] = notNumber(historyColumns[c][0], text[c])
		}
	}
	for _, p := range problems {
		if p != "" {
			row.Problems = append(row.Problems, p)
		}
	}
	return row
}

// historyDateRE is a History row's Date: a date, or two of them as
// "MM/DD/YYYY as of MM/DD/YYYY".
var historyDateRE = regexp.MustCompile(`(?i)^(\S+)(?:\s+as\s+of\s+(\S+))?$`)

// parseHistoryDate reads a History row's Date: the date posted, and the
// date "as of", when there is one. Both are "" when s is not a date or
// two of them, however much of it is.
func parseHistoryDate(s string) (posted, asOf nytime.Date) {
	m := historyDateRE.FindStringSubmatch(s)
	if m == nil {
		return "", ""
	}
	posted, err := parseDate(m[1])
	if err != nil {
		return "", ""
	}
	if m[2] != "" {
		if asOf, err = parseDate(m[2]); err != nil {
			return "", ""
		}
	}
	return posted, asOf
}

// notNumber is the problem of a cell of column that is not a number.
func notNumber(column, text string) string {
	return fmt.Sprintf("%s %q is not a number", column, mask.Text(text))
}

// undated returns the warning of rows without a date, if there are any.
func undated(rows []HistoryRow) []Warning {
	var lines []int
	for _, r := range rows {
		if r.Posted == "" {
			lines = append(lines, r.Line)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return []Warning{{WarnUndatedRows, fmt.Sprintf("%d %s whose Date is not MM/DD/YYYY, the first on line %d; "+
		"kept, but they make no transaction", len(lines), plural(len(lines), "row", "rows"), lines[0])}}
}

// plural returns one when n is 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// readHistoryJSON reads a History export in JSON: an object with FromDate,
// ToDate, TotalTransactionsAmount, sometimes TotalFeesAndCommAmount, and
// BrokerageTransactions, a list of rows, each an object of the eight
// columns' names, and in exports since 2025 ItemIssueId and AcctgRuleCd.
// Keys beyond these are not read, but each row's Raw keeps them.
func readHistoryJSON(shown, text string) (*File, error) {
	f := &File{Kind: KindHistoryJSON, Layout: LayoutHistoryJSON}
	top, rows, err := jsonTop(text, "BrokerageTransactions")
	if err != nil {
		return nil, fmt.Errorf("%s is a History export in JSON that schwabtx cannot read: %w", shown, err)
	}
	for _, d := range []struct {
		key  string
		date *nytime.Date
	}{{"FromDate", &f.StatedFrom}, {"ToDate", &f.StatedTo}} {
		s, _, problem := jsonText(top[d.key], d.key)
		switch {
		case problem != "":
			return nil, fmt.Errorf("%s is a History export in JSON whose %s", shown, problem)
		case s == "":
		default:
			if *d.date, err = parseDate(s); err != nil {
				return nil, fmt.Errorf("%s is a History export in JSON whose %s %q is not MM/DD/YYYY",
					shown, d.key, mask.Text(s))
			}
		}
	}
	for _, t := range []struct {
		key   string
		total **Decimal
	}{{"TotalTransactionsAmount", &f.StatedTotal}, {"TotalFeesAndCommAmount", &f.StatedFees}} {
		if s, _, problem := jsonText(top[t.key], t.key); s != "" || problem != "" {
			d, _ := ParseDecimal(s)
			if problem != "" { // not a number, as its text says
				d = Decimal{Text: problem, Present: true}
			}
			*t.total = &d
		}
	}
	for _, e := range rows {
		var keys map[string]json.RawMessage
		if json.Unmarshal([]byte(e.text), &keys) != nil || keys == nil {
			keys = map[string]json.RawMessage{}
		}
		row := historyRow(len(f.History)+1, e.line, e.text, func(c int) (string, string) {
			name := historyColumns[c][0]
			s, ok, problem := jsonText(keys[name], name)
			if !ok && problem == "" {
				problem = "no " + name
			}
			return s, problem
		})
		if len(keys) == 0 {
			row.Problems = []string{"not an object"}
		}
		row.ItemIssueID, _, _ = jsonText(keys["ItemIssueId"], "ItemIssueId")
		row.AcctgRuleCd, _, _ = jsonText(keys["AcctgRuleCd"], "AcctgRuleCd")
		f.History = append(f.History, row)
	}
	f.Warnings = append(f.Warnings, undated(f.History)...)
	return f, nil
}

// An element is one element of a JSON list: its text as the file has it,
// and the line where it starts.
type element struct {
	text string
	line int
}

// jsonTop reads text, a JSON object that json.Unmarshal has read, into
// its keys' values, and the elements of its list under key with where
// each starts. When a key
// is there more than once, the last is read, as encoding/json does.
func jsonTop(text, key string) (map[string]json.RawMessage, []element, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil { // {
		return nil, nil, err
	}
	top := map[string]json.RawMessage{}
	var list []element
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k, _ := t.(string)
		if k != key {
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				return nil, nil, err
			}
			top[k] = v
			continue
		}
		if t, err := dec.Token(); err != nil {
			return nil, nil, err
		} else if t != json.Delim('[') {
			return nil, nil, fmt.Errorf("%s is not a list", key)
		}
		list = list[:0:0]
		// The line where each element starts, counted from the previous
		// element's start, so that the text is counted through once.
		line, counted := 1, 0
		for dec.More() {
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				return nil, nil, err
			}
			end := int(dec.InputOffset())
			start := end - len(v)
			line += strings.Count(text[counted:start], "\n")
			counted = start
			list = append(list, element{text: text[start:end], line: line})
		}
		if _, err := dec.Token(); err != nil { // ]
			return nil, nil, err
		}
		top[k] = json.RawMessage("[]")
	}
	if _, err := dec.Token(); err != nil { // }
		return nil, nil, err
	}
	return top, list, nil
}

// jsonText returns a JSON value as text: a string's, or a number's as
// written; null is "". present is whether there is a value at all. Any
// other value is a problem, which names key.
func jsonText(v json.RawMessage, key string) (s string, present bool, problem string) {
	v = bytes.TrimSpace(v)
	switch {
	case len(v) == 0:
		return "", false, ""
	case string(v) == "null":
		return "", true, ""
	case v[0] == '"':
		if json.Unmarshal(v, &s) != nil {
			return "", true, key + " is not text"
		}
		return strings.TrimSpace(s), true, ""
	case v[0] == '-' || '0' <= v[0] && v[0] <= '9':
		return string(v), true, ""
	}
	return "", true, key + " is not text"
}
