package main

import (
	"context"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// client is the CLI's view of Schwab's Trader API.
type client struct {
	api *schwab.Client
	// pageCap is the response size at which a result may have been cut short.
	pageCap int
}

// clientConfig tunes the client. Tests set a fast rate and short waits.
var clientConfig schwab.ClientConfig

func newClient(auth *schwab.Auth) *client {
	return &client{api: schwab.NewClient(auth, clientConfig), pageCap: schwab.MaxTransactions}
}

// accountNumbers lists the accounts shared at login. The endpoints for one
// account take the hash value, not the plain account number. Its errors, as
// the other methods', say what to do about a problem with the login.
func (c *client) accountNumbers(ctx context.Context) ([]schwab.AccountNumber, error) {
	accounts, err := c.api.AccountNumbers(ctx)
	if err != nil {
		return nil, hint(err)
	}
	return accounts, nil
}

// accounts returns every account with its balances and positions.
func (c *client) accounts(ctx context.Context) ([]schwab.Account, error) {
	accounts, err := c.api.Accounts(ctx, schwab.AccountFieldPositions)
	if err != nil {
		return nil, hint(err)
	}
	return accounts, nil
}

// transactions returns one account's transactions of the given types between
// start and end.
func (c *client) transactions(ctx context.Context, accountHash schwab.AccountHash, start, end time.Time, types []schwab.TransactionType) ([]schwab.Transaction, error) {
	txs, err := c.api.Transactions(ctx, accountHash, schwab.TransactionQuery{Start: start, End: end, Types: types})
	if err != nil {
		return nil, hint(err)
	}
	return txs, nil
}
