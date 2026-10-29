# schwabtx

A small CLI that logs in to the Charles Schwab Trader API, prints your most
recent transactions across every account you share with it, and saves
snapshots of those accounts' positions and balances. It uses only the Go
standard library.

Before the first run, set up a Schwab developer app as described in the
[top-level README](../../README.md#one-time-setup).

## Run

From the root of the repository. The first line loads `.env`; on Windows,
use the PowerShell line from the [quick start](../../README.md#quick-start)
instead.

```sh
set -a; . ./.env; set +a

go run ./cmd/schwabtx login      # opens nothing itself: follow the printed steps
go run ./cmd/schwabtx            # the 100 most recent transactions, all accounts
go run ./cmd/schwabtx snapshot   # save today's positions and balances
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
`schwabtx/token.json`. Override with `SCHWAB_TOKEN_FILE`. On Linux and macOS
the file is made readable only by you wherever it is. On Windows it takes its
folder's permissions, which at the default location means only you, so keep
an overridden path inside your user profile.

## Snapshots

Schwab reports positions and balances only as of now, so their past values
are gone unless you saved them. `schwabtx snapshot` saves them: each run
appends one line to `snapshots.jsonl` in your user config directory, under
`schwabtx/`. Choose another file with `-file` or `SCHWAB_SNAPSHOT_FILE`.

The file holds full account numbers and balances. On Linux and macOS it and
any directory the tool creates for it are readable only by you; on Windows
the file takes its folder's permissions, as the token file does.

Each line is one JSON object:

```json
{"version":1,"fetched_at":"2026-10-29T21:05:00Z","accounts":[...]}
```

- `fetched_at` is when Schwab's response arrived, in UTC.
- `accounts` is Schwab's response to `GET /accounts?fields=positions`, as
  sent but on one line: for each account its balances (initial, current and
  projected) and positions.
- `version` changes if the line's fields ever do.

A run that fails saves nothing and exits with an error. If a write is ever
cut short, the next run starts on a new line, so a reader can skip a line
that does not parse.

### Every day

Run it once a day in the evening, after the market close; mutual fund prices
can post a few hours after 4 p.m. Eastern. For example, from cron at 9:05 p.m.
on weekdays:

```
5 21 * * 1-5  set -a && . "$HOME/schwab-mcp/.env" && set +a && "$HOME/go/bin/schwabtx" snapshot >> "$HOME/schwabtx-snapshot.log"
```

Adjust the paths to where your `.env` and `schwabtx` are. Cron times are in
the machine's time zone. The line logs what the tool prints on success and
leaves what it prints on errors to cron, which mails it to you: the warning a
day before the weekly login lapses, and the error once it has.

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
truncated responses, rate-limit retries and snapshots.

## Open questions

- How far back Schwab serves transactions is not documented (see
  [the API notes](../../docs/schwab-api.md#transactions)). It only affects
  `-lookback` and `-window`.

## Troubleshooting

- `HTTP 401` at login: the app is not "Ready For Use" yet, or the key/secret
  is wrong.
- `invalid_grant` / `HTTP 400` at login: the pasted code expired (be quicker)
  or `SCHWAB_CALLBACK_URL` does not match one of the app's callback URLs
  exactly.
- "Schwab rejected the saved login": the 7 days are up, run `login` again.
- `0 transactions` although the account has activity: the account is
  probably not enabled for thinkorswim (see the
  [top-level README](../../README.md#one-time-setup)). An account you did not
  tick at login is left out rather than shown as empty, so check N in
  "... from N accounts"; if you ticked none, schwabtx stops with "Schwab
  returned no accounts".
