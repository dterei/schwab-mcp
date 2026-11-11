package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

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
