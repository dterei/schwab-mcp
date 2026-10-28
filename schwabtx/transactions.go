package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// allTransactionTypes is every value the Trader API accepts for "types".
var allTransactionTypes = []string{
	"TRADE",
	"RECEIVE_AND_DELIVER",
	"DIVIDEND_OR_INTEREST",
	"ACH_RECEIPT",
	"ACH_DISBURSEMENT",
	"CASH_RECEIPT",
	"CASH_DISBURSEMENT",
	"ELECTRONIC_FUND",
	"WIRE_OUT",
	"WIRE_IN",
	"JOURNAL",
	"MEMORANDUM",
	"MARGIN_CALL",
	"MONEY_MARKET",
	"SMA_ADJUSTMENT",
}

type instrument struct {
	AssetType   string `json:"assetType"`
	Symbol      string `json:"symbol"`
	Description string `json:"description"`
}

type transferItem struct {
	Instrument instrument `json:"instrument"`
	Amount     float64    `json:"amount"`
	Cost       float64    `json:"cost"`
	Price      float64    `json:"price"`
	FeeType    string     `json:"feeType"`
}

// transaction holds the fields the table needs. Raw keeps Schwab's full JSON
// so nothing is lost for -json output.
type transaction struct {
	ActivityID    json.Number    `json:"activityId"`
	Time          string         `json:"time"`
	TradeDate     string         `json:"tradeDate"`
	Description   string         `json:"description"`
	AccountNumber string         `json:"accountNumber"`
	Type          string         `json:"type"`
	Status        string         `json:"status"`
	NetAmount     float64        `json:"netAmount"`
	TransferItems []transferItem `json:"transferItems"`

	Raw  json.RawMessage `json:"-"`
	When time.Time       `json:"-"`
	// DateOnly is set when Schwab gave a calendar date with no time of day.
	DateOnly bool `json:"-"`
}

func decodeTransactions(body []byte) ([]transaction, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		return nil, fmt.Errorf("decoding transactions: %w", err)
	}
	txs := make([]transaction, 0, len(raws))
	for _, raw := range raws {
		var tx transaction
		if err := json.Unmarshal(raw, &tx); err != nil {
			return nil, fmt.Errorf("decoding transaction: %w", err)
		}
		tx.Raw = raw
		tx.When, tx.DateOnly = parseSchwabTime(tx.Time)
		if tx.When.IsZero() {
			tx.When, tx.DateOnly = parseSchwabTime(tx.TradeDate)
		}
		txs = append(txs, tx)
	}
	return txs, nil
}

// Schwab writes offsets without a colon ("+0000"), which RFC 3339 rejects.
var schwabTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999Z0700",
}

// parseSchwabTime returns the zero time when s is not a timestamp. dateOnly
// reports a bare calendar date, which has no meaningful time zone.
func parseSchwabTime(s string) (t time.Time, dateOnly bool) {
	for _, layout := range schwabTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, false
		}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// primaryItem returns the security leg of a transaction, skipping fee and
// cash legs. ok is false when there isn't one (a dividend paid in cash, say).
func (tx transaction) primaryItem() (item transferItem, ok bool) {
	for _, it := range tx.TransferItems {
		if it.FeeType == "" && it.Instrument.AssetType != "CURRENCY" && it.Instrument.Symbol != "" {
			return it, true
		}
	}
	return transferItem{}, false
}

func (tx transaction) key() string {
	if id := tx.ActivityID.String(); id != "" && id != "0" {
		return id
	}
	return string(tx.Raw)
}

type fetchOptions struct {
	N        int
	Lookback time.Duration
	Window   time.Duration
	Types    []string
	Now      time.Time
	Warn     func(string)
}

// fetchRecent returns the N most recent transactions across all accounts,
// newest first. Each account is walked backwards one window at a time until
// it has yielded N transactions or the lookback is used up, which is enough
// to know the overall top N.
func fetchRecent(ctx context.Context, c *client, accounts []account, opt fetchOptions) ([]transaction, error) {
	var all []transaction
	for _, acct := range accounts {
		txs, err := fetchAccount(ctx, c, acct, opt)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", maskAccount(acct.AccountNumber), err)
		}
		all = append(all, txs...)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].When.Equal(all[j].When) {
			return all[i].When.After(all[j].When)
		}
		a, _ := all[i].ActivityID.Int64()
		b, _ := all[j].ActivityID.Int64()
		return a > b
	})
	if len(all) > opt.N {
		all = all[:opt.N]
	}
	return all, nil
}

func fetchAccount(ctx context.Context, c *client, acct account, opt fetchOptions) ([]transaction, error) {
	var got []transaction
	seen := map[string]bool{}
	oldest := opt.Now.Add(-opt.Lookback)

	window := opt.Window
	for end := opt.Now; end.After(oldest) && len(got) < opt.N; {
		start := end.Add(-window)
		if start.Before(oldest) {
			start = oldest
		}
		txs, err := fetchRange(ctx, c, acct.HashValue, start, end, opt)
		if err != nil {
			// Schwab limits how far back it will serve. If an older window is
			// refused after newer ones worked, halve the window to get as close
			// to that limit as we can, then keep what we have.
			var ae *apiError
			if !end.Equal(opt.Now) && errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
				if span := end.Sub(start); span > 24*time.Hour {
					window = span / 2
					continue
				}
				opt.Warn(fmt.Sprintf("account %s: Schwab refused transactions before about %s, so older ones are missing (%v)",
					maskAccount(acct.AccountNumber), end.Format("2006-01-02"), err))
				break
			}
			return nil, err
		}
		// Adjacent windows share a boundary instant, so drop repeats.
		for _, tx := range txs {
			if k := tx.key(); !seen[k] {
				seen[k] = true
				if tx.AccountNumber == "" {
					tx.AccountNumber = acct.AccountNumber
				}
				got = append(got, tx)
			}
		}
		end = start
	}
	return got, nil
}

// fetchRange gets one window, splitting it in half whenever the response is
// big enough that Schwab may have cut it short.
func fetchRange(ctx context.Context, c *client, hash string, start, end time.Time, opt fetchOptions) ([]transaction, error) {
	txs, err := c.transactions(ctx, hash, start, end, opt.Types)
	if err != nil {
		return nil, err
	}
	if len(txs) < c.pageCap {
		return txs, nil
	}
	if end.Sub(start) <= time.Hour {
		opt.Warn(fmt.Sprintf("more than %d transactions within an hour of %s; some may be missing", c.pageCap, start.Format(time.RFC3339)))
		return txs, nil
	}
	mid := start.Add(end.Sub(start) / 2)
	newer, err := fetchRange(ctx, c, hash, mid, end, opt)
	if err != nil {
		return nil, err
	}
	older, err := fetchRange(ctx, c, hash, start, mid, opt)
	if err != nil {
		return nil, err
	}
	return append(newer, older...), nil
}
