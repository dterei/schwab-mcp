package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

type fetchOptions struct {
	N        int
	Lookback time.Duration
	Window   time.Duration
	Types    []schwab.TransactionType
	Now      time.Time
	Warn     func(string)
}

// fetchRecent returns the N most recent transactions across all accounts,
// newest first. Each account is walked backwards one window at a time until
// it has yielded N transactions or the lookback is used up, which is enough
// to know the overall top N.
func fetchRecent(ctx context.Context, c *client, accounts []schwab.AccountNumber, opt fetchOptions) ([]schwab.Transaction, error) {
	var all []schwab.Transaction
	for _, acct := range accounts {
		txs, err := fetchAccount(ctx, c, acct, opt)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", mask.Number(acct.AccountNumber), err)
		}
		all = append(all, txs...)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if a, b := lookup.When(all[i]), lookup.When(all[j]); !a.Equal(b) {
			return a.After(b)
		}
		return all[i].ActivityID > all[j].ActivityID
	})
	if len(all) > opt.N {
		all = all[:opt.N]
	}
	return all, nil
}

func fetchAccount(ctx context.Context, c *client, acct schwab.AccountNumber, opt fetchOptions) ([]schwab.Transaction, error) {
	var got []schwab.Transaction
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
			// to that limit as we can, then keep what we have. A login refused
			// meanwhile is a 400 too, from the token endpoint, which no window
			// would get past, and so is an account Schwab no longer accepts.
			var ae *schwab.Error
			if !end.Equal(opt.Now) && errors.As(err, &ae) && ae.Status == http.StatusBadRequest &&
				!errors.Is(err, schwab.ErrLoginExpired) && !errors.Is(err, schwab.ErrAppRejected) &&
				!errors.Is(err, schwab.ErrInvalidAccount) {
				if span := end.Sub(start); span > 24*time.Hour {
					window = span / 2
					continue
				}
				opt.Warn(fmt.Sprintf("account %s: Schwab refused transactions before about %s, so older ones are missing (%v)",
					mask.Number(acct.AccountNumber), end.Format("2006-01-02"), err))
				break
			}
			return nil, err
		}
		// Adjacent windows share a boundary instant, so drop repeats.
		for _, tx := range txs {
			if k := lookup.Key(tx); !seen[k] {
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
func fetchRange(ctx context.Context, c *client, hash schwab.AccountHash, start, end time.Time, opt fetchOptions) ([]schwab.Transaction, error) {
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

// problemsWarning returns a warning about records with problems, values of
// a type that Schwab has not sent before, keys that it has always sent and
// left out, or objects and arrays sent twice, or "" if there were none.
// noun names one record, such as "transaction". It names the places that
// problemPlaces does.
func problemsWarning(noun string, problems [][]schwab.Problem) string {
	records, places := problemPlaces(problems)
	if records == 0 {
		return ""
	}
	if records != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s differed from what Schwab has sent before (%s)", records, noun, places)
}

// problemPlaces returns how many of the records have problems, and names
// the first few places, with one problem each, that lookup.ProblemPlaces
// gives.
func problemPlaces(problems [][]schwab.Problem) (records int, places string) {
	records, first, n := lookup.ProblemPlaces(problems)
	more := ""
	if n > 0 {
		more = fmt.Sprintf("; and %d more", n)
	}
	described := make([]string, len(first))
	for i, p := range first {
		described[i] = p.String()
	}
	return records, strings.Join(described, "; ") + more
}
