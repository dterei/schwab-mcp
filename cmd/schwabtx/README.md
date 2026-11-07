# schwabtx

A small CLI that logs in to the Charles Schwab Trader API, prints your most
recent transactions across every account you share with it, and saves
snapshots of those accounts' positions and balances. It uses only the Go
standard library, and golang.org/x/sys for file locks on Windows.

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
-window 60         days per API request, at most 365
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

Every process that uses the login shares that file: the scheduled snapshot,
a `schwabtx` you run by hand and, later, the MCP server. Next to it,
`token.json.lock` lets one of them at a time refresh the login or replace it.
Leave the lock file in place; it stays empty. Keep the token file on a local
disk of the one machine that uses it, not in iCloud Drive, Dropbox, OneDrive
or another synced folder: the lock does not reach other machines, a login on
one machine ends the login on the other, and the sync provider would hold
your token. If others can read the token file, as one copied in by hand may,
`schwabtx` makes it private and says so.

Schwab limits each app key to 120 requests a minute, whichever program
sends them, so the processes on your computer that use the key keep to 100 a
minute together, with bursts of 5. They share that limit through a small file
in your user cache directory, `schwabtx/rate-` followed by a hash of the app
key; it holds only two times. Another program that uses the same key, or the
key used on another computer, draws from Schwab's 120 too, which the 20 left
over leave room for.

## Snapshots

Schwab reports positions and balances only as of now. Holdings on a past
date can be rebuilt later from your transactions, and stocks and ETFs valued
with Schwab's daily prices. What cannot be rebuilt is the value of holdings
that price history does not cover (mutual funds, money-market funds, options
and bonds) and Schwab's own balance figures for the day, such as buying
power. `schwabtx snapshot` saves those, along with a daily record to check a
rebuild against: each run appends one line to `snapshots.jsonl` in your user
config directory, under `schwabtx/`. Choose another file with `-file` or
`SCHWAB_SNAPSHOT_FILE`.

The file holds full account numbers and balances. On Linux and macOS the tool
keeps it readable only by you: it creates the file, and any directory it
makes for it, that way, and if it finds the file readable by others, it
tightens it and tells you. On Windows the file takes its folder's
permissions, as the token file does.

Each line is one JSON object:

```json
{"version":1,"fetched_at":"2026-10-30T01:05:00Z","accounts":[...]}
```

- `fetched_at` is when Schwab's response arrived: UTC, to the second, in RFC
  3339. An evening run in the US is dated the next day.
- `accounts` is Schwab's response to `GET /accounts?fields=positions`, as
  sent but on one line: for each account its balances (initial, current and
  projected) and positions.
- `version` changes if the line's fields ever do.

A run that fails saves nothing and exits with an error. If a write is ever
cut short, the next run starts on a new line, so a reader can skip a line
that does not parse. If an account in the last snapshot is missing from
Schwab's response, usually because it was not ticked at the last login, the
tool warns you and saves the rest.

### Every day

Run it every day in the evening, after the US market close: mutual fund
prices can post a few hours after 4 p.m. Eastern. The weekend runs mostly
repeat Friday's, but without them a login that lapses on a Monday would get
little or no warning: the warning starts two days before.

[`schedule/`](schedule) has scripts and steps for launchd on macOS, cron on
Linux and Task Scheduler on Windows. They try a failed run again, and tell
you, with a notification or through cron's mail, when the weekly login is
about to lapse or has, and when a run fails. Even so, check now and then that
the newest snapshot is recent: the last line of the snapshot file starts with
its `fetched_at`.

## How it fetches

- It covers **brokerage accounts**, not Schwab Bank checking or savings.
- Schwab makes you log in through the browser again **every 7 days**.
- History is served in limited date windows, so the tool walks backwards 60
  days at a time (`-window`) up to one year (`-lookback`). If Schwab refuses an
  older window the tool narrows it to get as close to the limit as it can,
  prints what it got and warns you. Schwab has also been seen to answer a
  window past an account's history with no transactions, rather than refuse
  it.
- A response with 3000 transactions may have been cut short, so the tool splits
  that window in half and asks again.
- On a 401 it gets a new access token once: one that another process saved
  meanwhile, or a refresh. A 401 for a token that Schwab issued moments ago is
  reported as it is, since a new token would not help.
- After a 429, a 5xx answer or a failure of the network it tries again, up to
  four times in all. It waits 1, 2, then 4 seconds (after a 5xx or a failure
  of the network, between half and all of that, at random), or what Schwab
  asks in Retry-After: after a 429 always, after a 5xx only if that is longer.
  If Schwab asks for more than a minute, it stops at once. A 429 holds back
  every process that shares the rate limit for that wait, at most a minute,
  not only this one, and the last one, after which it stops, for 8 seconds.
  A request that times out is tried once more, not three times; a refresh of
  the login that fails on the network or at Schwab is tried again too.

## Tests

```sh
go test ./cmd/schwabtx
```

The tests run the whole tool against a fake Schwab server: login, token
refresh, paging backwards through windows, merging accounts, splitting
truncated responses, rate-limit retries and snapshots. They also run the
scheduler scripts as a scheduler would: `snapshot.sh` on Linux and macOS,
`snapshot.ps1` with each PowerShell on Windows.

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
- "Schwab rejected the saved login": the 7 days are up, or you logged in
  elsewhere since; run `login` again.
- "another process kept the Schwab login locked": another `schwabtx`, or the
  MCP server, was still refreshing the login after two minutes, which it
  should never take. Find and stop the stuck process.
- "cannot be locked" (a warning): the token file is on a file system that
  cannot lock, as some network ones cannot. `schwabtx` carries on without the
  lock, but set `SCHWAB_TOKEN_FILE` to a file on a local disk.
- "opening the lock file: ... permission denied": another user, often root
  after a `sudo`, made `token.json.lock`. Remove it while no `schwabtx` runs.
- "keeps its own rate limit" (a warning): the rate limit's file in your user
  cache directory cannot be used, so this run keeps the limit by itself, apart
  from other processes. The warning says why.
- "held values that could not be read" (a warning): Schwab sent a value in a
  form it has not sent before, such as a string where a number belongs. The
  table may show such a value as 0 or blank; `-json` prints Schwab's own
  JSON, and the snapshot keeps it. The warning names where the values were.
- "Invalid account number" (`HTTP 400`): Schwab no longer accepted an
  account's hash. Run the command again, which looks the hashes up afresh.
- An error from Schwab includes "(correlation ID ...)" when Schwab gave one.
  Quote it when you ask Schwab's Trader API team (traderapi@schwab.com) about
  a failed request.
- `HTTP 429 from Schwab after 4 tries`: Schwab kept refusing requests from
  your app key. It counts every program that uses the key; wait a minute and
  try again.
- `HTTP 403 from Schwab: an HTML page, Access Denied (reference #...)`:
  Schwab's network turned the request away, not its API. It usually passes
  within hours; if it does not, write to traderapi@schwab.com with the
  reference.
- `HTTP 500 from Schwab after 4 tries: an HTML page, Whitelabel Error Page`:
  the app is not "Ready For Use" yet, or an account is of a type the API does
  not serve.
- `0 transactions` although the account has activity: the account is
  probably not enabled for thinkorswim (see the
  [top-level README](../../README.md#one-time-setup)). An account you did not
  tick at login is left out rather than shown as empty, so check N in
  "... from N accounts"; if you ticked none, schwabtx stops with "Schwab
  returned no accounts".
