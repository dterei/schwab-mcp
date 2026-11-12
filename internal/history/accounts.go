package history

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// ensureAccount returns the id of the account whose number is number,
// adding it, as first seen in from (api, snapshot, file or user), when the
// store does not hold it. Either way the times the account was first and
// last seen take in at.
func ensureAccount(w *wtx, number, from string, at time.Time) (int64, error) {
	var id int64
	err := w.tx.QueryRowContext(w.ctx, `INSERT INTO account (number, created_from, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (number) DO UPDATE SET first_seen_at = min(first_seen_at, excluded.first_seen_at),
		last_seen_at = max(last_seen_at, excluded.last_seen_at)
		RETURNING account_id`, number, from, instant(at), instant(at)).Scan(&id)
	return id, err
}

// RecordAccountNumbers records a read of the accounts shared at login, from
// GET /accounts/accountNumbers: the read, and when Schwab answered, each
// account, added when the store does not hold it, with its hash. Hashes
// are kept with when each was first and last seen; they are looked up again
// for each run and are never what the store keys an account by. An element
// without a number or a hash is left out.
func (s *Store) RecordAccountNumbers(ctx context.Context, r Read, list []schwab.AccountNumber) error {
	if err := checkRead(r); err != nil {
		return err
	}
	rr := readRow{endpoint: "account_numbers"}
	if r.Err == nil {
		rr.rows = len(list)
	}
	return s.withWrite(ctx, "recording the accounts shared at login", func(w *wtx) error {
		if _, err := insertRead(w, r, rr); err != nil || r.Err != nil {
			return err
		}
		at := instant(r.AnsweredAt)
		for _, a := range list {
			if checkNumber(a.AccountNumber) != nil || a.HashValue == "" {
				continue
			}
			id, err := ensureAccount(w, a.AccountNumber, "api", r.AnsweredAt)
			if err != nil {
				return err
			}
			if _, err := w.exec(`INSERT INTO account_hash (account_id, hash, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?)
				ON CONFLICT (account_id, hash) DO UPDATE SET first_seen_at = min(first_seen_at, excluded.first_seen_at),
				last_seen_at = max(last_seen_at, excluded.last_seen_at)`, id, string(a.HashValue), at, at); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordPreferences records a read of the user's preferences, from GET
// /userPreference: the read, and when Schwab answered, each account's
// nickname (masked, as free text is), masked number as Schwab shows it,
// whether it is the primary account, and its method of choosing tax lots,
// for the accounts the store holds; the store adds no account for them.
// The rest, which identifies the user to the streamer, is not kept.
func (s *Store) RecordPreferences(ctx context.Context, r Read, p schwab.UserPreference) error {
	if err := checkRead(r); err != nil {
		return err
	}
	rr := readRow{endpoint: "user_preference"}
	if r.Err == nil {
		rr.rows = len(p.Accounts)
	}
	return s.withWrite(ctx, "recording the user's preferences", func(w *wtx) error {
		if _, err := insertRead(w, r, rr); err != nil || r.Err != nil {
			return err
		}
		for _, a := range p.Accounts {
			if a.AccountNumber == "" {
				continue
			}
			if _, err := w.exec(`UPDATE account SET nickname = ?, display_id = ?, primary_account = ?, lot_method = ?
				WHERE number = ?`, free(a.NickName), nullString(a.DisplayAcctID), a.PrimaryAccount,
				nullString(a.LotSelectionMethod), a.AccountNumber); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetNotShared records that Schwab no longer shares the account whose
// number is number, since since, as the sync learns when Schwab refuses it
// as an invalid account number and the accounts shared at login no longer
// list it; the zero time records that it is shared again.
func (s *Store) SetNotShared(ctx context.Context, number string, since time.Time) error {
	var at sql.NullString
	if !since.IsZero() {
		at = sql.NullString{String: instant(since), Valid: true}
	}
	return s.withWrite(ctx, "recording an account as not shared", func(w *wtx) error {
		res, err := w.exec("UPDATE account SET not_shared_since = ? WHERE number = ?", at, number)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err == nil {
				err = fmt.Errorf("the store holds no account %s", mask.Number(number))
			}
			return err
		}
		return nil
	})
}
