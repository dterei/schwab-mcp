package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

func maskAccount(number string) string {
	if len(number) <= 4 {
		return number
	}
	return "..." + number[len(number)-4:]
}

// printTable prints txs as a table. A transaction whose time is absent, or
// midnight in New York, as a date without a time reads, shows its trade day,
// without a time of day.
func printTable(w io.Writer, txs []schwab.Transaction, showAccounts bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tDATE\tACCOUNT\tTYPE\tSTATUS\tSYMBOL\tQTY\tPRICE\tNET AMOUNT\tDESCRIPTION")
	for i, tx := range txs {
		date := "?"
		switch {
		case !tx.Time.IsZero() && !newYorkMidnight(tx.Time):
			date = tx.Time.Local().Format("2006-01-02 15:04")
		case tx.TradeDay() != "":
			date = string(tx.TradeDay())
		}
		acct := tx.AccountNumber
		if !showAccounts {
			acct = maskAccount(acct)
		}
		symbol, qty, price := "", "", ""
		desc := tx.Description
		if it, ok := primaryItem(tx); ok {
			symbol = it.Instrument.Symbol
			qty = formatNumber(it.Amount)
			if it.Price != 0 {
				price = formatNumber(it.Price)
			}
			if desc == "" {
				desc = it.Instrument.Description
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%.2f\t%s\n",
			i+1, date, acct, tx.Type, tx.Status, symbol, qty, price, tx.NetAmount, truncate(desc, 50))
	}
	return tw.Flush()
}

// printJSON writes Schwab's own JSON for each transaction, as printRaw
// does. That includes full account numbers.
func printJSON(w io.Writer, txs []schwab.Transaction) error {
	raws := make([]json.RawMessage, len(txs))
	for i, tx := range txs {
		raws[i] = tx.Raw
	}
	return printRaw(w, raws)
}

// printRaw writes raws, Schwab's own JSON of some records, as a list,
// re-indented but otherwise as received. It indents by hand, as
// json.Encoder lays a list out, since an encoder on Go 1.27 escapes U+2028
// and U+2029 and json.Indent does not.
func printRaw(w io.Writer, raws []json.RawMessage) error {
	var out bytes.Buffer
	out.WriteByte('[')
	for i, raw := range raws {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString("\n  ")
		if err := json.Indent(&out, raw, "  ", "  "); err != nil {
			return err
		}
	}
	if len(raws) > 0 {
		out.WriteByte('\n')
	}
	out.WriteString("]\n")
	_, err := w.Write(out.Bytes())
	return err
}

// newYorkMidnight reports whether t is midnight in New York: the instant
// before it is on another date there.
func newYorkMidnight(t time.Time) bool {
	return schwab.DateInNewYork(t) != schwab.DateInNewYork(t.Add(-time.Nanosecond))
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func truncate(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-3]) + "..."
}

// money is an amount in cents, or none: a figure that Schwab did not send,
// or that could not be read.
type money struct {
	cents int64
	ok    bool
}

// moneyOf returns *v in cents, or none for nil.
func moneyOf(v *float64) money {
	if v == nil {
		return money{}
	}
	return amount(*v)
}

// amount returns x in cents, rounded as scaled rounds it.
func amount(x float64) money {
	cents, ok := scaled(x, 2)
	return money{cents: cents, ok: ok}
}

// plus returns m + n, or none when either is none.
func (m money) plus(n money) money {
	if !m.ok || !n.ok {
		return money{}
	}
	return money{cents: m.cents + n.cents, ok: true}
}

// minus returns m - n, or none when either is none.
func (m money) minus(n money) money {
	return m.plus(money{cents: -n.cents, ok: n.ok})
}

// String returns m with two decimals and a leading - when it is negative,
// as the transactions table prints amounts, or "-" for none. Zero is 0.00,
// never -0.00.
func (m money) String() string {
	if !m.ok {
		return "-"
	}
	return formatScaled(m.cents, 2, 2)
}

// scaled returns x rounded to places decimals, half away from zero, as a
// whole number of tenths, hundredths or the like. It rounds the shortest
// decimal that reads back as x, which is the number as Schwab wrote it, so
// that 1.005 rounds up, though the double nearest to it is below it. ok is
// false for a trillion or more, which no real figure reaches, so that sums
// of the results cannot overflow.
func scaled(x float64, places int) (int64, bool) {
	if math.Abs(x) >= 1e12 {
		return 0, false
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(math.Abs(x), 'f', -1, 64), ".")
	frac += strings.Repeat("0", places+1)
	n, err := strconv.ParseInt(whole+frac[:places], 10, 64)
	if err != nil { // NaN, which JSON cannot hold
		return 0, false
	}
	if frac[places] >= '5' {
		n++
	}
	if x < 0 {
		n = -n
	}
	return n, true
}

// formatScaled writes n, a whole number of tenths, hundredths or the like,
// as places says, as a decimal with at least keep decimals: the zeros that
// end the others are dropped, with the point when none is left. It never
// writes -0.
func formatScaled(n int64, places, keep int) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	digits := fmt.Sprintf("%0*d", places+1, n)
	whole, frac := digits[:len(digits)-places], strings.TrimRight(digits[len(digits)-places:], "0")
	frac += strings.Repeat("0", max(0, keep-len(frac)))
	if frac == "" {
		return sign + whole
	}
	return sign + whole + "." + frac
}

// accountCell returns an account's number for a table: masked as
// maskAccount masks it, unless show, or ? when Schwab sent no number that
// could be read.
func accountCell(number string, show bool) string {
	switch {
	case number == "":
		return "?"
	case !show:
		number = maskAccount(number)
	}
	return plainCell(number)
}

// plainCell returns s, text that Schwab or the user wrote, for a table:
// every control or format character in it, and every line or paragraph
// separator, is ?, so that no cell can break the table, move the
// terminal's cursor or turn the text around it.
func plainCell(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return '?'
		}
		return r
	}, s)
}

// textCell returns s for a table: with its white space collapsed and cut
// to limit runes, as truncate does, and made plain, as plainCell does.
func textCell(s string, limit int) string { return plainCell(truncate(s, limit)) }

// formatQuantity returns x rounded to 6 decimals, as scaled rounds it,
// without the zeros that end it: 12, 2.1788, -2; or - when it is too large
// to be one.
func formatQuantity(x float64) string {
	n, ok := scaled(x, 6)
	if !ok {
		return "-"
	}
	return formatScaled(n, 6, 0)
}

// formatPrice returns x rounded to 6 decimals, as scaled rounds it, with
// at least 2: 81.50, 251.3075; or - when it is too large to be one.
func formatPrice(x float64) string {
	n, ok := scaled(x, 6)
	if !ok {
		return "-"
	}
	return formatScaled(n, 6, 2)
}

// plural returns n and noun, which takes an s unless n is 1.
func plural(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return strconv.Itoa(n) + " " + noun
}

// column is one column of a table: its head, and whether its cells are
// numbers, which are aligned right.
type column struct {
	head  string
	right bool
}

// writeTable writes the heads of cols, then rows, a line each: each column
// as wide as its widest cell, counted in runes, two spaces between
// columns, numbers aligned right and text left, and no spaces at the end of
// a line. text/tabwriter aligns every column one way.
func writeTable(w io.Writer, cols []column, rows [][]string) error {
	heads := make([]string, len(cols))
	widths := make([]int, len(cols))
	for i, c := range cols {
		heads[i] = c.head
		widths[i] = utf8.RuneCountInString(c.head)
	}
	for _, r := range rows {
		for i, cell := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(cell))
		}
	}
	var b strings.Builder
	for _, r := range slices.Concat([][]string{heads}, rows) {
		var line strings.Builder
		for i, cell := range r {
			if i > 0 {
				line.WriteString("  ")
			}
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell))
			if cols[i].right {
				line.WriteString(pad + cell)
			} else {
				line.WriteString(cell + pad)
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
