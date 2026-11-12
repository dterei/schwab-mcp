# schwabtx

A small CLI that logs in to the Charles Schwab Trader API, prints your most
recent transactions across every account you share with it, looks up those
accounts' positions and balances, and saves snapshots of them. It keeps a
store of your history from those snapshots and the exports of schwab.com,
and exports it for a calculator. It uses the Go standard library,
golang.org/x/sys for file locks on Windows, and modernc.org/sqlite, SQLite
in pure Go, for the store.

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
-show-accounts     full account numbers in the table instead of the last 3 digits
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
schwabtx positions -account 135                 # one account, by the last digits of its number
schwabtx balances -account "Options & Margin"   # or by its whole nickname on schwab.com
```

They take the same flags:

```
-account X       only the account whose number ends in the digits X (...135, as
                 the tables print it, and 2947-0135 work too), or whose whole
                 nickname is X, as the tables print it, in any case
-json            Schwab's raw JSON for the accounts instead of a table (includes
                 full account numbers)
-show-accounts   full account numbers in the table instead of the last 3 digits
```

Every figure is as of the moment you ask: in the evening, and again from
Sunday evening, Schwab values holdings at extended-hours prices, so the
values are not the day's close.

```
$ schwabtx balances
ACCOUNT  NAME              TYPE       VALUE     CASH  BUYING POWER
...135   Individual        CASH     6997.68  1532.17       1532.17
...398   Options & Margin  MARGIN   5503.84  5210.44       8258.84
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
$ schwabtx positions -account 135
4 positions in 1 account, not counting cash (see schwabtx balances)
ACCOUNT  SYMBOL         QTY  AVG COST    VALUE     COST  OPEN P/L  DESCRIPTION
...135   00507V109       10     81.50     0.00   815.00   -815.00
...135   AAPL        2.1788    190.20   495.70   414.41     81.29
...135   SWVXX      1220.89      1.00  1220.89  1220.89      0.00  SCHWAB VALUE ADVANTAGE MONEY FUND
...135   VTI             12  251.3075  3748.92  3015.69    733.23  VANGUARD TOTAL STOCK MARKET ETF
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

## The store

`schwabtx` keeps your history in a store: one SQLite file, `history.db`, in
your user config directory under `schwabtx/` (on Windows in
`%LocalAppData%\schwabtx\`, which does not roam). Choose another file with
`-store` or `SCHWAB_STORE_FILE`. It holds the snapshots, Schwab's API rows,
the files you export from schwab.com, and your decisions about them, and
keeps all of them; what it derives from them, such as each transaction's
category and whether it counts, it can derive again. None of these commands
logs in or reaches the network. [docs/store.md](../../docs/store.md) is the
reference, with a walk-through of what each command prints.

```sh
schwabtx import                    # the snapshot file that schwabtx snapshot writes
schwabtx import ~/Downloads/Individual_XXX398_Transactions_20261111-120000.json
schwabtx status                    # what each account's sources cover, and what waits for you
schwabtx status -review            # what waits for you, each with the command that settles it
schwabtx status C3a9e21b0.17       # one thing in full
schwabtx edit T73508261949 -category fee
schwabtx export > ledger.csv       # the counted transactions, as a calculator reads them
schwabtx store backup ~/history-backup.db
```

The workflow: import your snapshots, which the store can take as often as
you like; import `schwabtx transactions -json` output, which gives the store
Schwab's own rows until `schwabtx sync` exists; export History from
schwab.com as JSON, every type and no symbol, in ranges of under a year
([Schwab's exports](../../docs/schwab-exports.md) says how), and import it;
then `schwabtx status -review`, and settle what it lists with
`schwabtx edit`. A transaction that several exports hold, or that Schwab's
API rows hold, counts once; one that only a file holds, on days the API's
rows cover, waits for you to say what it is.

Flags may come before or after the arguments
(`schwabtx edit T73508261949 -category fee` and
`schwabtx edit -category fee T73508261949` are one). Every command takes
`-store PATH`. Refs name what the store holds: `A3` an account,
`T73508261941` an API transaction, `C3a9e21b0.17` a transaction of History
files, `F3a9e21b0` an imported file, `L5c1d2e3f.4` a closed lot, `S812` a
snapshot, `K:i:1973757747` a security's key; any start of the hex that names
one thing will do. `-account` takes `A3`, the last digits of the number, or
the nickname.

### schwabtx import

```
schwabtx import [-store PATH] [-account ACCOUNT] [-from DATE] [-to DATE]
                [-all-types | -filtered] [-dry-run] [-force] [FILE...]
schwabtx import [-store PATH] -forget FREF [-undo]
```

Imports each FILE, all of them or none: a snapshot file, a History export of
schwab.com in CSV or JSON, a Realized Gain/Loss export's Lot Details, or the
output of `schwabtx transactions -json`, each known by its content. Any other
file is refused, and then nothing is imported; so is one of the store's own
files, before any file is read. With no FILE it imports the snapshot file.
It makes the store if there is none.

```
-store PATH       the store's file (default: $SCHWAB_STORE_FILE, else history.db in
                  schwabtx's folder of your config directory)
-account ACCOUNT  the account the exports are for, when their names do not say
-from DATE        the first day the History exports cover, for those that state none
-to DATE          the last day they cover (DATE is YYYY-MM-DD or MM/DD/YYYY)
-all-types        the History exports hold every type and no symbol: they are complete
-filtered         the History exports were filtered by type or symbol
-dry-run          report what the import would do, and save nothing
-force            import an export whose name's digits are not its account's (a renamed file)
-forget FREF      set aside an imported file: its transactions that no other file
                  holds leave the ledger
-undo             with -forget, bring the file back
```

### schwabtx status

```
schwabtx status [-store PATH] [-account ACCOUNT] [-show-accounts]
schwabtx status [-store PATH] [-account ACCOUNT] -review [-all]
schwabtx status [-store PATH] [-account ACCOUNT] REF
```

Prints what each account's API rows, exports and snapshots cover, and what
waits for you; with `-review`, what waits for you, grouped, each with the
command that settles it; with a REF, that one thing in full. It is a report,
and exits 0 whatever it finds.

```
-store PATH       the store's file
-account ACCOUNT  only this account
-review           list what waits for you, each with the command that settles it
-all              with -review, list what is flagged for information too
-show-accounts    full account numbers instead of the last 3 digits
```

### schwabtx edit

```
schwabtx edit [-store PATH] [-account ACCOUNT] [-note TEXT] REF OPTION...
schwabtx edit [-store PATH] [-note TEXT] -action LABEL -policy count|hold|auto
```

Records your decisions about REF, which the store applies after its own
rules, and prints what each changed. Each option's value `auto` withdraws
your decision.

```
-store PATH          the store's file
-account ACCOUNT     the account REF is of, when two share it; with a file (F…),
                     the account the file is for, or auto
-note TEXT           why, kept with the decision
-category C          a transaction's category: deposit, withdrawal, transfer_in,
                     transfer_out, journal, buy, sell, dividend, interest, fee, tax, other
-count yes|no        whether a transaction counts
-pair REF2|none      the transaction it is paired with: a journal between your
                     accounts, a sweep, a reversal
-security K:…        a transaction's security
-link T…[,T…]|none   the API transactions a file transaction (C…) is, or none
                     (count it from the file)
-reject T…           an API transaction a file transaction is not
-completeness complete|filtered
                     whether a file (F…) holds every type and symbol
-from DATE           with -to, the first day a file covers
-to DATE             with -from, the last day a file covers
-range auto          give a file back the range it states, or its import gave
-same K:…            another key of the same security (REF is a key, K:…)
-differ K:…          another key of a different security
-auto                withdraw your -same and -differ decisions on a key
-kind TEXT           an account's kind (taxable, ira, roth, ...)
-action LABEL        a History action, such as "Bank Interest", for -policy
-policy count|hold   whether the action's held-out file rows count
```

### schwabtx export

```
schwabtx export [-store PATH] [-what WHAT] [-format csv|jsonl] [-account ACCOUNT]
                [-from DATE] [-to DATE] [-all] [-raw] [-show-accounts] [-o FILE | -dir DIR]
```

Writes the ledger, the counted transactions from the API and from files,
which is what a calculator reads, or another table of the store, as CSV or
JSON Lines: to stdout, to a file with `-o`, or each to a file of its own in a
folder with `-dir`, with a `manifest.json`. Files it writes are readable only
by you. A summary of what it wrote, and of what each account's sources
cover, goes to stderr. The tables (`-what`): `ledger`, `entries`,
`transactions`, `legs`, `file-transactions`, `imports`, `snapshots`,
`positions`, `balances`, `lots`, `gaps`, `flags`, `coverage` and
`securities`; [docs/store.md](../../docs/store.md#exports) has their columns.

```
-store PATH       the store's file
-what WHAT        a table, a comma-separated list of tables, or all (a list or
                  all needs -dir); default ledger
-format FORMAT    csv (the default), or jsonl for JSON Lines
-account ACCOUNT  only this account
-from DATE        only rows of this day or later
-to DATE          only rows of this day or earlier
-all              every entry and leg, counted or not
-raw              add each row's raw bytes as Schwab or the file had them
                  (includes full account numbers)
-show-accounts    add each row's full account number
-o FILE           write to FILE instead of stdout
-dir DIR          write each table to a file of its own in DIR, with manifest.json
```

### schwabtx store

```
schwabtx store [-store PATH] check
schwabtx store [-store PATH] rebuild
schwabtx store [-store PATH] backup FILE
schwabtx store [-store PATH] restore [-dry-run] BACKUP
```

`check` checks the store, SQLite's integrity and foreign keys and what the
derived data must hold, and exits 1 on any problem; `rebuild` derives
everything again from what the store holds; `backup` copies the store, as
of now, to a new file readable only by you, while others go on using it;
`restore` replaces what the store holds with what a backup holds, through
SQLite, and makes the store if there is none.

```
-store PATH       the store's file
-dry-run          with restore, say what the store and the backup hold, and
                  change nothing
```

### Privacy and the store's files

Account numbers show as `...` and their last three digits everywhere, as
Schwab shows them, and so does any run of 8 digits or more in the text the
store keeps. Only `status -show-accounts`, `export -show-accounts` and
`export -raw` (which adds Schwab's JSON and the files' rows as they were)
show full numbers, and `export` says so when it does.

The store is its file, `history.db-wal` and `history.db-shm` together: never
copy, move or delete one of them without the others, and never copy them
while `schwabtx` runs. Copy the store with `store backup`, and put a copy
back with `store restore`. Keep the store on a local disk, not in a synced
folder: a store on a network file system is refused.

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
tables and JSON of the fixtures' accounts, and the store's commands on
stores copied from a new one: imports of snapshots, `-json` output and
History exports, what status reviews and edit settles, exports, backups and
restores, the store's own files refused as inputs and outputs, and no full
account number in their output. They also run the scheduler
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
