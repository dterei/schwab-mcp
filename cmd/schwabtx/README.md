# schwabtx

A small CLI that logs in to the Charles Schwab Trader API and prints your
most recent transactions across every account you share with it. It uses only
the Go standard library.

Before the first run, set up a Schwab developer app as described in the
[top-level README](../../README.md#one-time-setup).

## Run

From the root of the repository:

```sh
set -a; source .env; set +a

go run ./cmd/schwabtx login   # opens nothing itself: follow the printed steps
go run ./cmd/schwabtx         # the 100 most recent transactions, all accounts
```

Or install it:

```sh
go install github.com/dterei/schwab-mcp/cmd/schwabtx@latest
schwabtx login
schwabtx
```

`login` prints a Schwab link. Sign in, tick the accounts to share, approve,
and your browser lands on `https://127.0.0.1/?code=...` with a connection
error. That is expected. Paste that whole address back into the terminal
within about 30 seconds.

Flags:

```
-n 100             how many transactions to print
-lookback 365      how many days back to search
-window 60         days per API request
-types TRADE,...   only these transaction types (default: all)
-json              Schwab's raw JSON instead of a table (best input for calculators;
                   includes full account numbers)
-show-accounts     full account numbers in the table instead of the last 4 digits
```

The login token is stored in your user config directory under
`schwabtx/token.json` with owner-only permissions. Override with
`SCHWAB_TOKEN_FILE`.

## How it fetches

- It covers **brokerage accounts**, not Schwab Bank checking or savings.
- Schwab makes you log in through the browser again **every 7 days**.
- History is served in limited date windows, so the tool walks backwards 60
  days at a time (`-window`) up to one year (`-lookback`). If Schwab refuses an
  older window the tool narrows it to get as close to the limit as it can,
  prints what it got and warns you.
- A response with 3000 transactions may have been cut short, so the tool splits
  that window in half and asks again.
- On a 401 it refreshes the access token once; on a 429 or 5xx it backs off
  and retries.

## Tests

```sh
go test ./cmd/schwabtx
```

The tests run the whole tool against a fake Schwab server: login, token
refresh, paging backwards through windows, merging accounts, splitting
truncated responses and rate-limit retries.

## Open questions

- How far back Schwab actually serves transactions (sources disagree between
  60 days and one year), which only affects `-lookback` and `-window`.
- An empty list usually means the account was not ticked during login.

## Troubleshooting

- `HTTP 401` at login: the app is not "Ready For Use" yet, or the key/secret
  is wrong.
- `invalid_grant` / `HTTP 400` at login: the pasted code expired (be quicker)
  or `SCHWAB_CALLBACK_URL` does not match the app's callback URL exactly.
- "Schwab rejected the saved login": the 7 days are up, run `login` again.
- Login works but API calls fail: some guides say the brokerage account must
  be thinkorswim-enabled; turn that on at schwab.com.
