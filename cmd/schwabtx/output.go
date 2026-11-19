package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// printTable prints txs as a table. A transaction whose time is absent, or
// midnight in New York, as a date without a time reads, shows its trade day,
// without a time of day.
func printTable(w io.Writer, txs []schwab.Transaction, showAccounts bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tDATE\tACCOUNT\tTYPE\tSTATUS\tSYMBOL\tQTY\tPRICE\tNET AMOUNT\tDESCRIPTION")
	for i, tx := range txs {
		date := "?"
		switch {
		case !tx.Time.IsZero() && !lookup.NewYorkMidnight(tx.Time):
			date = tx.Time.Local().Format("2006-01-02 15:04")
		case tx.TradeDay() != "":
			date = string(tx.TradeDay())
		}
		acct := tx.AccountNumber
		if !showAccounts && acct != "" {
			acct = mask.Number(acct)
		}
		symbol, qty, price := "", "", ""
		desc := tx.Description
		if it, ok := lookup.PrimaryItem(tx); ok {
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

// accountCell returns an account's number for a table: masked as Schwab
// masks it, "..." and its last three digits, unless show, or ? when Schwab
// sent no number that could be read.
func accountCell(number string, show bool) string {
	switch {
	case number == "":
		return "?"
	case !show:
		number = mask.Number(number)
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

// tailCell returns s for a table as textCell does, but cut at its start,
// for text whose end says the most: ... and the whole words at the end of
// its last limit-3 runes, or those runes when the cut leaves a single
// word.
func tailCell(s string, limit int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= limit {
		return plainCell(string(r))
	}
	tail := r[len(r)-(limit-3):]
	if r[len(r)-len(tail)-1] != ' ' { // the cut fell within a word: start at the next
		tail = tail[slices.Index(tail, ' ')+1:]
	}
	return plainCell("..." + string(tail))
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
