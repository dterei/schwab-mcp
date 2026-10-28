package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
)

func maskAccount(number string) string {
	if len(number) <= 4 {
		return number
	}
	return "..." + number[len(number)-4:]
}

func printTable(w io.Writer, txs []transaction, showAccounts bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tDATE\tACCOUNT\tTYPE\tSTATUS\tSYMBOL\tQTY\tPRICE\tNET AMOUNT\tDESCRIPTION")
	for i, tx := range txs {
		date := "?"
		switch {
		case tx.When.IsZero():
		case tx.DateOnly:
			date = tx.When.Format("2006-01-02")
		default:
			date = tx.When.Local().Format("2006-01-02 15:04")
		}
		acct := tx.AccountNumber
		if !showAccounts {
			acct = maskAccount(acct)
		}
		symbol, qty, price := "", "", ""
		desc := tx.Description
		if it, ok := tx.primaryItem(); ok {
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

// printJSON writes Schwab's own JSON for each transaction, re-indented but
// otherwise as received. That includes full account numbers.
func printJSON(w io.Writer, txs []transaction) error {
	raws := make([]json.RawMessage, len(txs))
	for i, tx := range txs {
		raws[i] = tx.Raw
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(raws)
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-3]) + "..."
}
