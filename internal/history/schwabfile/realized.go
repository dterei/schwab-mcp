package schwabfile

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// A RealizedRow is one closed lot of a Realized Gain/Loss export's Lot
// Details, as the file writes it. Read derives nothing from its columns:
// Schwab's own relations between them are not documented, and two
// conventions of a wash sale's Gain/Loss have been seen.
type RealizedRow struct {
	// Ordinal is the lot's place among the file's lots, and Line the line
	// where it starts, each counting from 1.
	Ordinal, Line int
	// Raw is the row as the file has it, decoded to UTF-8.
	Raw string
	// Symbol and Name are the cells, trimmed. Name is free text, which the
	// store masks.
	Symbol, Name string
	// Closed is the Closed Date, "" when it is not a date (a problem).
	Closed nytime.Date
	// Opened is the Opened Date, "" when it is not a date, which is not a
	// problem: a lot may have none. OpenedText is the cell as written.
	Opened                                                              nytime.Date
	OpenedText                                                          string
	Quantity, ProceedsPerShare, CostPerShare, Proceeds, CostBasis, Gain Decimal
	// GainPct and the other percentages are the cells as written: Schwab
	// writes up to 17 decimals.
	GainPct             string
	LTGain, STGain      Decimal
	Term                string
	UnadjustedCostBasis Decimal
	// WashSale is Wash Sale?: true for Yes, false for No, nil when blank.
	WashSale       *bool
	DisallowedLoss Decimal
	// TxnClosed is the Transaction Closed Date, "" when blank or not a
	// date (a problem).
	TxnClosed             nytime.Date
	TxnCostBasis, TxnGain Decimal
	TxnGainPct            string
	LTTxnGain             Decimal
	LTTxnGainPct          string
	STTxnGain             Decimal
	STTxnGainPct          string
	// Placeholders name the columns that hold a placeholder in place of a
	// number ("--", "N/A" or "Missing"), as a lot without a history, such
	// as one transferred in, does.
	Placeholders []string
	// ColumnsDisagree is whether Gain/Loss ($) is neither Proceeds less
	// Cost Basis nor, on a wash sale, that plus the Disallowed Loss: the
	// two conventions seen. It is false when one of them is not a number.
	ColumnsDisagree bool
	// Problems say what of the lot could not be read, with the cells'
	// free text masked.
	Problems []string
}

// The columns of Realized Gain/Loss's Lot Details, the same since 2018 but
// for Cost Basis, which became Cost Basis (CB) after 2020.
var lotColumns = []column{
	{"Symbol"},
	{"Name"},
	{"Closed Date"},
	{"Opened Date"},
	{"Quantity"},
	{"Proceeds Per Share"},
	{"Cost Per Share"},
	{"Proceeds"},
	{"Cost Basis (CB)", "Cost Basis"},
	{"Gain/Loss ($)"},
	{"Gain/Loss (%)"},
	{"Long Term Gain/Loss"},
	{"Short Term Gain/Loss"},
	{"Term"},
	{"Unadjusted Cost Basis"},
	{"Wash Sale?"},
	{"Disallowed Loss"},
	{"Transaction Closed Date"},
	{"Transaction Cost Basis"},
	{"Total Transaction Gain/Loss ($)"},
	{"Total Transaction Gain/Loss (%)"},
	{"LT Transaction Gain/Loss ($)"},
	{"LT Transaction Gain/Loss (%)"},
	{"ST Transaction Gain/Loss ($)"},
	{"ST Transaction Gain/Loss (%)"},
}

// The indexes of lotColumns.
const (
	lSymbol = iota
	lName
	lClosed
	lOpened
	lQuantity
	lProceedsPerShare
	lCostPerShare
	lProceeds
	lCostBasis
	lGain
	lGainPct
	lLTGain
	lSTGain
	lTerm
	lUnadjusted
	lWashSale
	lDisallowed
	lTxnClosed
	lTxnCostBasis
	lTxnGain
	lTxnGainPct
	lLTTxnGain
	lLTTxnGainPct
	lSTTxnGain
	lSTTxnGainPct
)

// titleRE is a Realized Gain/Loss title, its white space taken as one
// space: the account's name, the time of the export, and the range.
var titleRE = regexp.MustCompile(`(?i)^realized gain/loss - lot details for (.*?) as of (.*?) from ([0-9/]+) to ([0-9/]+)$`)

// readRealized reads a Realized Gain/Loss export's lots, after its header,
// whose columns are at cols, under its title when it has one. A first cell
// Total ends the lots; the Total row's numbers are checked against the
// lots' sums.
func readRealized(title *record, cols []int, recs []record) *File {
	f := &File{Kind: KindRealized, Layout: LayoutRealizedLots}
	if title != nil {
		f.readTitle(title.title())
	}
	var total *record
	var after []int
	for _, r := range recs {
		switch {
		case r.empty():
		case total != nil:
			after = append(after, r.line)
		case strings.EqualFold(r.cell(0), "Total"):
			total = &r
		default:
			f.Realized = append(f.Realized, realizedRow(len(f.Realized)+1, r, cols))
		}
	}
	if len(after) > 0 {
		f.Warnings = append(f.Warnings, Warning{
			WarnRowsAfterTotal,
			fmt.Sprintf("%d %s after the Total line, the first on line %d, not read",
				len(after), plural(len(after), "record", "records"), after[0]),
		})
	}
	var undatedLines []int
	for _, r := range f.Realized {
		if r.Closed == "" {
			undatedLines = append(undatedLines, r.Line)
		}
	}
	if len(undatedLines) > 0 {
		f.Warnings = append(f.Warnings, Warning{WarnUndatedRows, fmt.Sprintf("%d %s whose Closed Date is not MM/DD/YYYY, "+
			"the first on line %d", len(undatedLines), plural(len(undatedLines), "lot", "lots"), undatedLines[0])})
	}
	if total != nil {
		f.Warnings = append(f.Warnings, f.realizedTotals(*total, cols)...)
	}
	return f
}

// readTitle reads what a Realized Gain/Loss title says: the account's
// digits, the time of the export and the range. Each part it cannot read
// stays unsaid.
func (f *File) readTitle(title string) {
	m := titleRE.FindStringSubmatch(strings.Join(strings.Fields(title), " "))
	if m == nil {
		return
	}
	f.AccountHint = accountHint(m[1])
	f.ExportedAt = javaTime(m[2])
	f.StatedFrom, _ = parseDate(m[3])
	f.StatedTo, _ = parseDate(m[4])
}

// javaTime reads a time as Java's Date.toString writes it, as in "Mon Jul
// 06 10:47:11 EDT 2026", taking the zone's abbreviation for New York's
// zone, as the time in UTC; the zero time when it is not one.
func javaTime(s string) time.Time {
	f := strings.Fields(s)
	if len(f) != 6 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join([]string{f[0], f[1], f[2], f[3], f[5]}, " "),
		nytime.NewYork())
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// realizedRow makes a lot of a record whose columns are at cols.
func realizedRow(ordinal int, r record, cols []int) RealizedRow {
	cell := func(c int) string { return r.cell(cols[c]) }
	row := RealizedRow{
		Ordinal: ordinal, Line: r.line, Raw: r.raw,
		Symbol: cell(lSymbol), Name: cell(lName), OpenedText: cell(lOpened), Term: cell(lTerm),
		GainPct: cell(lGainPct), TxnGainPct: cell(lTxnGainPct), LTTxnGainPct: cell(lLTTxnGainPct),
		STTxnGainPct: cell(lSTTxnGainPct),
	}
	problems := make([]string, len(lotColumns))
	var err error
	if row.Closed, err = parseDate(cell(lClosed)); err != nil {
		problems[lClosed] = fmt.Sprintf("Closed Date %q is not MM/DD/YYYY", mask.Text(cell(lClosed)))
	}
	row.Opened, _ = parseDate(cell(lOpened))
	if cell(lTxnClosed) != "" {
		if row.TxnClosed, err = parseDate(cell(lTxnClosed)); err != nil {
			problems[lTxnClosed] = fmt.Sprintf("Transaction Closed Date %q is not MM/DD/YYYY", mask.Text(cell(lTxnClosed)))
		}
	}
	row.WashSale, problems[lWashSale] = washSale(cell(lWashSale))
	for _, n := range realizedNumbers(&row) {
		var err error
		*n.d, err = ParseDecimal(cell(n.c))
		switch {
		case err != nil:
			problems[n.c] = notNumber(lotColumns[n.c][0], cell(n.c))
		case n.d.Placeholder != "":
			row.Placeholders = append(row.Placeholders, lotColumns[n.c][0])
		}
	}
	for _, p := range problems {
		if p != "" {
			row.Problems = append(row.Problems, p)
		}
	}
	row.ColumnsDisagree = disagree(row)
	return row
}

// A number is a number column of a lot: its index in lotColumns, and the
// field that holds it.
type number struct {
	c int
	d *Decimal
}

// realizedNumbers returns the number columns of row, in the order of
// lotColumns.
func realizedNumbers(row *RealizedRow) []number {
	return []number{
		{lQuantity, &row.Quantity},
		{lProceedsPerShare, &row.ProceedsPerShare},
		{lCostPerShare, &row.CostPerShare},
		{lProceeds, &row.Proceeds},
		{lCostBasis, &row.CostBasis},
		{lGain, &row.Gain},
		{lLTGain, &row.LTGain},
		{lSTGain, &row.STGain},
		{lUnadjusted, &row.UnadjustedCostBasis},
		{lDisallowed, &row.DisallowedLoss},
		{lTxnCostBasis, &row.TxnCostBasis},
		{lTxnGain, &row.TxnGain},
		{lLTTxnGain, &row.LTTxnGain},
		{lSTTxnGain, &row.STTxnGain},
	}
}

// washSale reads Wash Sale?: Yes, No or blank, in any case; anything
// else is a problem.
func washSale(s string) (*bool, string) {
	switch strings.ToLower(s) {
	case "":
		return nil, ""
	case "yes":
		yes := true
		return &yes, ""
	case "no":
		no := false
		return &no, ""
	}
	return nil, fmt.Sprintf("Wash Sale? %q is not Yes or No", mask.Text(s))
}

// disagree reports whether a lot's Gain/Loss ($) is neither Proceeds less
// Cost Basis nor, on a wash sale, that plus the Disallowed Loss.
func disagree(row RealizedRow) bool {
	gain, ok1 := row.Gain.rat()
	proceeds, ok2 := row.Proceeds.rat()
	basis, ok3 := row.CostBasis.rat()
	if !ok1 || !ok2 || !ok3 {
		return false
	}
	net := new(big.Rat).Sub(proceeds, basis)
	if gain.Cmp(net) == 0 {
		return false
	}
	if loss, ok := row.DisallowedLoss.rat(); ok && row.WashSale != nil && *row.WashSale {
		return gain.Cmp(net.Add(net, loss)) != 0
	}
	return true
}

// realizedTotals checks a Total row's numbers against the sums of the
// lots' columns: each number column whose Total cell is a number, and
// whose lots' cells are all numbers or blank.
func (f *File) realizedTotals(total record, cols []int) []Warning {
	var off []string
	var columns RealizedRow
	for k, n := range realizedNumbers(&columns) {
		stated, err := ParseDecimal(total.cell(cols[n.c]))
		if err != nil || !stated.Number() {
			continue
		}
		cells := make([]Decimal, len(f.Realized))
		for i := range f.Realized {
			cells[i] = *realizedNumbers(&f.Realized[i])[k].d
		}
		got, ok := sum(cells...)
		if want, _ := stated.rat(); ok && got.Cmp(want) != 0 {
			off = append(off, fmt.Sprintf("%s is %s but the lots sum to %s", lotColumns[n.c][0], stated.Text, ratText(got)))
		}
	}
	if len(off) == 0 {
		return nil
	}
	return []Warning{{WarnTotalMismatch, "the Total line's " + strings.Join(off, "; ")}}
}
