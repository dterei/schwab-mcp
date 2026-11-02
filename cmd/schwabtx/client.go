package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Schwab caps a transactions response at 3000 entries.
const schwabMaxTransactions = 3000

// client is the CLI's view of Schwab's Trader API.
type client struct {
	api *schwab.Client
	// pageCap is the response size at which a result may have been cut short.
	pageCap int
}

func newClient(auth *schwab.Auth) *client {
	return &client{api: schwab.NewClient(auth), pageCap: schwabMaxTransactions}
}

// get fetches a Trader API path and returns the response body. Its errors
// say what to do about a problem with the login.
func (c *client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	body, err := c.api.Get(ctx, "/trader/v1"+path, query)
	if err != nil {
		return nil, hint(err)
	}
	return body, nil
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
