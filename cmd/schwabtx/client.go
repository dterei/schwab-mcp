package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Schwab caps a transactions response at 3000 entries.
const schwabMaxTransactions = 3000

const maxRetries = 3

type client struct {
	cfg  config
	auth *schwab.Auth
	http *http.Client

	// pageCap is the response size at which a result may have been cut short.
	pageCap int
	// retryWait is the first backoff delay after a 429 or 5xx.
	retryWait time.Duration
}

func newClient(cfg config, auth *schwab.Auth, httpClient *http.Client) *client {
	return &client{cfg: cfg, auth: auth, http: httpClient, pageCap: schwabMaxTransactions, retryWait: 2 * time.Second}
}

// get fetches a Trader API path and returns the response body. On a 401 it
// gets a new access token once, and it backs off on 429 and 5xx.
func (c *client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.cfg.BaseURL + "/trader/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	refreshed := false
	wait := c.retryWait
	for attempt := 0; ; attempt++ {
		tok, err := c.auth.AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusUnauthorized && !refreshed:
			refreshed = true
			if _, err := c.auth.Refresh(ctx, tok); err != nil {
				return nil, err
			}
			continue
		case (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < maxRetries:
			delay := wait
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 120 {
				delay = time.Duration(s) * time.Second
			}
			wait *= 2
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		return nil, &schwab.Error{Status: resp.StatusCode, Body: string(body)}
	}
}

type account struct {
	AccountNumber string `json:"accountNumber"`
	HashValue     string `json:"hashValue"`
}

// accountNumbers lists the accounts shared at login. Every other endpoint
// takes the hash value, not the plain account number.
func (c *client) accountNumbers(ctx context.Context) ([]account, error) {
	body, err := c.get(ctx, "/accounts/accountNumbers", nil)
	if err != nil {
		return nil, err
	}
	var accounts []account
	if err := json.Unmarshal(body, &accounts); err != nil {
		return nil, fmt.Errorf("decoding account list: %w", err)
	}
	return accounts, nil
}

const schwabTimeFormat = "2006-01-02T15:04:05.000Z"

func (c *client) transactions(ctx context.Context, accountHash string, start, end time.Time, types []string) ([]transaction, error) {
	q := url.Values{
		"startDate": {start.UTC().Format(schwabTimeFormat)},
		"endDate":   {end.UTC().Format(schwabTimeFormat)},
		"types":     {strings.Join(types, ",")},
	}
	body, err := c.get(ctx, "/accounts/"+url.PathEscape(accountHash)+"/transactions", q)
	if err != nil {
		return nil, err
	}
	return decodeTransactions(body)
}
