# schwabtx

A small CLI that logs in to the Charles Schwab Trader API, prints your most
recent transactions across every account you share with it, looks up those
accounts' positions and balances, and saves snapshots of them. It uses only
the Go standard library, and golang.org/x/sys for file locks on Windows.

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
go run ./cmd/schwabtx accounts   # your accounts, with their nicknames and values
go run ./cmd/schwabtx positions  # what each account holds
go run ./cmd/schwabtx balances   # each account's value, cash and buying power
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

The transactions command, the default, takes these flags:

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

## Accounts, positions and balances

Three commands look up what your accounts hold, as Schwab reports it when
you ask. They only read, and save nothing.

```sh
schwabtx accounts                               # each account's nickname, type and value
schwabtx balances                               # each account's value, cash and buying power
schwabtx positions                              # what each account holds
schwabtx positions -account 0135                # one account, by the last digits of its number
schwabtx balances -account "Options & Margin"   # or by its whole nickname on schwab.com
```

They take the same flags:

```
-account X       only the account whose number ends in the digits X (...0135, as
                 the tables print it, and 2947-0135 work too), or whose whole
                 nickname is X, as the tables print it, in any case
-json            Schwab's raw JSON for the accounts instead of a table (includes
                 full account numbers)
-show-accounts   full account numbers in the table instead of the last 4 digits
```

Every figure is as of the moment you ask: in the evening, and again from
Sunday evening, Schwab values holdings at extended-hours prices, so the
values are not the day's close.

```
$ schwabtx balances
ACCOUNT  NAME              TYPE       VALUE     CASH  BUYING POWER
...0135  Individual        CASH     6997.68  1532.17       1532.17
...3398  Options & Margin  MARGIN   5503.84  5210.44       8258.84
TOTAL                              12501.52  6742.61
```

- **VALUE** is what Schwab calls the account's liquidation value: its cash
  and holdings at their current prices, short positions subtracted.
- **CASH** is the account's cash, not counting money market funds, which
  Schwab holds as positions. A margin account with a loan or short
  positions can keep its cash in its margin and short balances, and CASH is
  then their sum: negative while you owe more than your short sales
  brought in. Schwab's figures have always added up, the value being the
  cash and the holdings' market values; where they do not, as when part of
  the account's money is in a figure that the tool does not take for cash,
  such as `savings`, which has been 0 in every answer seen, CASH is `-`,
  with a warning.
- **BUYING POWER** is, for a margin account, Schwab's buying power for
  stock that it can buy on margin, which counts what you can borrow, so it
  can exceed the account's value; options and most mutual funds can use
  less (`buyingPowerNonMarginableTrade` in `-json`). For a cash account it
  is the cash available to trade. It has no total, since buying power does
  not pass between accounts.
- **TYPE** is CASH or MARGIN. An IRA is one of the two: the API does not say
  which accounts are retirement accounts, though their nicknames usually
  do.
- `accounts` prints ACCOUNT, NAME, TYPE and VALUE: NAME is the account's
  nickname on schwab.com.
- `-` is a figure Schwab did not send, or sent in a form the tool could not
  read, and a warning says which. A total with a `-` in its column is `-`
  too, rather than a total that leaves an account out.

```
$ schwabtx positions -account 0135
4 positions in 1 account, not counting cash (see schwabtx balances)
ACCOUNT  SYMBOL         QTY  AVG COST    VALUE     COST  OPEN P/L  DESCRIPTION
...0135  00507V109       10     81.50     0.00   815.00   -815.00
...0135  AAPL        2.1788    190.20   495.70   414.41     81.29
...0135  SWVXX      1220.89      1.00  1220.89  1220.89      0.00  SCHWAB VALUE ADVANTAGE MONEY FUND
...0135  VTI             12  251.3075  3748.92  3015.69    733.23  VANGUARD TOTAL STOCK MARKET ETF
TOTAL                                  5465.51  5465.99     -0.48
```

- Positions are listed account by account, and by symbol within an
  account, so that an option usually comes straight after its underlying
  stock.
- **QTY** is negative for a short position.
- **AVG COST** is Schwab's average cost per share, fees included, or for a
  short position what each share brought in, fees taken out. For an option
  it is the premium per share, so a contract of 100 shares costs 100 times
  it. No answer seen holds a bond or a CD, whose AVG COST is as Schwab sends
  it.
- **VALUE** is the market value, negative for a short position.
- **OPEN P/L** is the profit or loss since the position was opened, positive
  for a gain whether long or short.
- **COST** is VALUE less OPEN P/L: what a long position cost, fees included,
  or for a short one the proceeds, as a negative amount. So VALUE - COST =
  OPEN P/L on every row and in the totals.
- The totals are of the positions alone. Cash is not a position, so they
  are not the accounts' value: `balances` shows the cash and the value.
- A figure is `-` as in `balances`, and so is a COST that needs it. An
  account whose positions could not be read has one row, with the symbol
  `?`, and the count before the table says how many such accounts it
  leaves out.
- Today's profit or loss is left out. For some stocks and ETFs, Schwab's
  figure counts the whole cost basis as the day's loss, and a position sold
  to open that day can show its whole value (see
  [the API notes](../../docs/schwab-api.md#positions-and-balances)). Prices
  are left out too: positions do not carry them.
- A security that is no longer listed can come back under its CUSIP as its
  symbol, like the first row above.
- A long DESCRIPTION is cut short, an option's at its start, so that its
  expiration, strike and type show.

For a table, `accounts` and `balances` make two requests. The first is for
the accounts and their balances, and the second is for your preferences,
which hold the nicknames; if those cannot be read, the table shows no
nicknames and a warning says why. With `-json` they make the first alone,
and `positions` makes one request, for the accounts with their positions;
either reads your preferences too only when `-account` names a nickname,
or names no account or several, to list them by their names. `-account`
picks from the answer, so one account costs no more than all of them.

`-json` prints each account as Schwab sent it, re-indented. For `positions`
it includes the account's positions, as a snapshot keeps them. For
`accounts` and `balances` it leaves them out, so those two print the same
JSON, without nicknames, which come from your preferences.

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

The tests run the whole tool against the fake Schwab server that the
project's tests share,
[`internal/schwab/schwabtest`](../../internal/schwab/schwabtest): a login
through the fake's authorize page, from the link the tool prints, token
refresh, paging backwards through windows, windows older than an account's
history answered with an empty list, as Schwab was seen to answer them,
merging accounts, splitting truncated responses however the fake caps them,
Schwab's own error answers, rate-limit retries, snapshots, and the lookups'
tables and JSON of the fixtures' accounts. They also run the scheduler
scripts as a scheduler would: `snapshot.sh` on Linux and macOS,
`snapshot.ps1` with each PowerShell on Windows.

## Open questions

- How far back Schwab serves transactions is not documented (see
  [the API notes](../../docs/schwab-api.md#transactions)). It only affects
  `-lookback` and `-window`. Probe Q1 of
  [the live mode](../../docs/live.md#the-probes) walks your accounts'
  history back and reports the date of each account's oldest row and what
  an older window gets; whether Schwab holds older activity that it does
  not serve stays yours to judge.

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
- "differed from what Schwab has sent before" (a warning): Schwab sent a
  value in a form it has not sent before, such as a string where a number
  belongs, left out a key that it has always sent, or sent an object or
  array twice. The table may show such a field as 0, blank or `-` or, for one
  sent twice, as a mix of the copies; `-json` prints Schwab's own JSON, and
  the snapshot keeps it. The warning names the fields.
- "Schwab sent no current value" (a warning), or no cash or buying power:
  Schwab's answer lacks the figure for that account, as answers of 2024 did
  for some margin accounts. The table shows `-` for it, and for its total.
- "Schwab's cash for ... is not its value less its holdings" (a warning):
  Schwab's figures for the account do not add up as they always have: part
  of its money may be in a figure that the tool does not take for cash,
  such as `savings`. CASH shows `-`; `-json` prints Schwab's figures.
- "the table leaves out the accounts' nicknames" (a warning): your
  preferences, which hold the nicknames, could not be read. The rest of the
  table is complete.
- `no account matches -account`: the error lists the accounts this login
  shares. An account you did not tick at `schwabtx login` is not among them;
  log in again and tick it.
- `-account "..." matches 2 accounts`: give the nickname of the one you
  want, or more digits of its number, which
  `schwabtx accounts -show-accounts` prints; when two accounts share a
  nickname, give the last digits of the number instead.
- "Invalid account number" (`HTTP 400`): Schwab refused an account that it
  had just listed, for a reason not known. Run the command again; if it
  keeps failing, write to Schwab with the correlation ID.
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
