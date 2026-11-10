# The live mode

`TestLive` in `internal/schwab` probes Schwab's real API with your saved
login. It checks every answer against this project's records, runs probes
that settle the [open questions](ROADMAP.md#open-questions), and writes a
report without account numbers, hashes, ids, balances or descriptions, for
you to read and then share. It only reads, and it never runs in CI.

## Running it

Export the app's key and secret, log in, then run the default mode:

```
export SCHWAB_APP_KEY=... SCHWAB_APP_SECRET=...
schwabtx login
go test ./internal/schwab -run '^TestLive$' -live -v
```

- The key and secret come from `SCHWAB_APP_KEY` and `SCHWAB_APP_SECRET`.
  `.env` is not read, so export it first.
- The login comes from `SCHWAB_TOKEN_FILE`, or else the default token file,
  as for `schwabtx`. The live mode never logs in: it stops and asks you to
  run `schwabtx login` when there is no login, when Schwab has refused it, or
  when it has lapsed, and warns when it lapses within a day.
- `-v` prints the report's lines as the run writes them, and where the
  report is; without it, `go test` prints them only when the run fails.

The flags:

| Flag | What it does |
| --- | --- |
| `-live` | Runs the live mode; without it `TestLive` is skipped |
| `-live.out=DIR` | Where the report goes. Default `<user cache dir>/schwabtx/live/<UTC time>/`. A folder that holds a report already is refused |
| `-live.state=DIR` | Where the state and its key go (below). Default `<user cache dir>/schwabtx/live/` |
| `-live.record` | Also keeps each answer raw (below) |
| `-live.budget=N` | Sends at most N requests, token requests included, in place of the mode's budget |
| `-live.rate` | Runs the rate probe alone |
| `-live.watch=balances` | Runs the watch of balances alone |
| `-live.watch=fills` | Runs the watch of fills alone |
| `-live.snapshot=HH:MM` | When your scheduled snapshot runs, in local time; 21:05 by default. The rate probe keeps 30 minutes clear of it |

The out and state folders are refused inside this module and inside any
git work tree, decided by the folders' identity, not by their paths, so a
link or a path in another case cannot slip one in. When the default is
refused, as for a home folder kept in git, name another with the flag.

### Modes, budgets and time

| Mode | What runs | Budget, token requests included | How long |
| --- | --- | --- | --- |
| default | the checks and the default probes | 60 and 40 for each account | a few minutes for three accounts |
| `-live.rate` | the rate probe alone | 320 | about ten minutes |
| `-live.watch=balances` | the watch of balances alone | 170 | from 15:45 in New York to 10:00 the next day |
| `-live.watch=fills` | the watch of fills alone | 10 and 60 for each account | half an hour |

Every mode reads the account numbers and your preferences first, since the
report's scrubber is built from them. The run keeps a deadline of its own,
a minute before the test's time limit, so that it stops and finishes its
report before `go test` gives up on it; the default time limit, 10
minutes, is enough for the default mode. A mode that cannot finish by then
refuses to start and says to add `-timeout 0`, which every opt-in mode
needs; a mode that would outlast your login refuses too, and says to log in
first.

## What it sends, and never sends

Every request goes through a guard of the run's own, which lets through:

- to `https://api.schwabapi.com` alone, with no other host or port;
- a GET under `/trader/v1/` or `/marketdata/v1/`;
- a POST to `/v1/oauth/token` that refreshes the access token, and nothing
  else in its form.

So it sends no order and no `previewOrder`, and it cannot log in. It follows
no redirect: Go sends the bearer token on to a redirect to the same host,
over plain http too, and a 307 or 308 sends a refresh's form, refresh token
and all, again. A request the guard refuses stops the run at once, as do a
spent budget, three failures of the network in a row, the run's deadline,
and an Access Denied page from Schwab's edge network, whichever request
draws it, after which more requests can be refused for about a quarter of
an hour.

The requests keep to the rate limit that every process using the app key
on this computer shares, 100 a minute. The history walk waits a second
between windows.

## Why CI never runs it

- CI's command, `go test -race -shuffle=on ./...`, has no `-live`. A test run
  with a flag is never served from the test cache, and `GOFLAGS` cannot carry
  `-live`.
- `go test ./... -live` does reach `internal/schwab`: every other package
  fails on the unknown flag, but this one runs `TestLive`. So `TestLive`
  first refuses to run where `CI`, `GITHUB_ACTIONS` or `SCHWAB_BASE_URL` is
  set, before it reads any other variable or file, and fails rather than
  skips. `SCHWAB_BASE_URL` would point it at another server, which a report
  would be taken to stand for.
- CI holds no login.

CI runs the same harness against the fake Schwab instead, as
`TestLiveOnTheFake`: every probe, under the fake's settings for each thing
that is not known, on a fake clock, with every file in a temporary folder.

## The probes

Each probe runs under a recover, so that one that panics is reported
without values and the others still run. A verdict of "inconclusive" says
why; the question stays open until a run settles it.

| Probe | Mode | Question | What it does |
| --- | --- | --- | --- |
| Q1 | default | How far back transactions go, and what a window older than that gets | Walks each account's history back in windows of a year, a second apart, to three empty windows past the oldest row or 2000-01-01, and asks for 2000 as a control. A refusal counts only when it says something of the window, and the newest window then still answers. Gives each account's oldest row's New York date, "or earlier" when the walk ran out of requests first; whether older activity exists that Schwab does not serve is yours to judge |
| Q2 | default | The tradeDate of a fill from 18:00 to 04:00 in New York | Counts the history's evening and overnight executions by what their tradeDate is |
| Q3 | `watch=balances` | When initial balances move, and whether they then hold the close | Reads the accounts every 10 minutes from 15:45 to 10:00 the next day. Needs holdings with prices: cash never moves |
| Q4 | default | Whether the symbol filter keeps dividends and W-8 withholdings | Asks for a symbol's rows with `symbol=` and without; a verdict needs the symbol's own trade back and a row of another symbol left out. It sends only a symbol that the scrubber hides, so that a refusal quoting it shows a label |
| Q5 | default | Whether an account's orders are served for a year or 60 days | Gets the order of the youngest execution over 67 days old by id, then asks the list for two days around when it was entered, if they lie within the last year. An order counts as old only past 67 days, since a list kept for 60 days by calendar dates can hold one a little older |
| Q6 | default | Whether `scope=readonly` gives a token that cannot trade | Reports the scope Schwab granted, and whether any read was refused as unauthorized. Log in with `&scope=readonly` added to the login link to try it; whether such a token cannot trade stays open, since only an order could show it |
| Q8 | default | Whether daily price bars are adjusted for dividends | Keeps SPY's and VTI's daily closes in the state, with when it read them, and compares them across an ex-date: the earlier run must come before the close of the last session before the ex-date, and the later one after the ex-date, since Schwab may adjust the closes at any time in between. Needs the Market Data product |
| Q10 | default | Whether account hashes stay the same across logins and apps | Keeps keyed digests of each account's hash; a run after your weekly login compares them |
| Q11 | default | How kinds of event show up in transactions | Counts the shapes of the history's rows |
| Q12 | default | Whether rows change after they post, and how long after | Keeps keyed digests of the last 60 days' rows; a later run names the keys that changed, the rows gone and the rows that came late. A row counts as gone or late only when both its time and its tradeDate lie in the windows that would show it, since which of the two a window goes by is not known, and only against an answer that the cap cannot have cut. A row counts as late only when its time was more than an hour before the last run, which left it out, since a row may take minutes to post; it posted between the two runs, so the report gives how long after its time that was as a span, such as from 5 hours to 4 days |
| Q13 | `watch=fills` | How soon a fill's trade shows up | Polls the orders entered in the last day and their trades every minute for half an hour. Place a trade by hand, then start it. For a fill from 18:00 to 04:00 in New York, which may be dated the next trading day, it asks for trades dated up to that day too, a window that ends after the poll |
| Q14 | `-live.rate` | How far under 120 a minute Schwab refuses requests, and whether a burst draws a refusal | Sends the account numbers at 20, 40, 60, 80 and 100 a minute, then a burst of 5, one try each, and stops at the first 429, 403 or 5xx. It starts only when nothing has used the key for two minutes, and stops if another process uses it |
| Q15 | default | Whether a refresh ends older access tokens, or changes the refresh token | Refreshes once, with a control before, and tries the old and new tokens after |
| extras | default | Details milestone 2 relies on | About fifteen single requests: a window's bounds, inclusive or not; whether a window takes a row by its time or its tradeDate; windows of a year and a second, and of 366 days; no types; a date alone; a row by its id against the list; an unknown hash and id; the 401 for a bad token and for none |

Q7 (`previewOrder`) has no probe, since it would need a POST, and Q9 (how to
confirm an order) is a question of design. Q8, Q10, Q12 and Q15's
comparison across runs need two runs or more; their first run keeps what
the next one compares with.

## The report

`report.txt` in the out folder, readable only by you, written section by
section as the run goes, so that a run that stops keeps what it has:

- the toolchain, the commit, the time, the mode and the probes;
- each answer, under its endpoint's template, such as
  `accounts/{account 2}/transactions`: its status, `Content-Type`, the names
  of its correlation headers, how many records it held, and what the checks
  found: a record's problems, a key that no field takes, a key sent twice in
  one object;
- the keys seen on each type of record, against the keys the tests expect;
- each probe's verdict and evidence;
- the guard's accounting: the budget, the answers by status, the token
  requests, the 429s, the failures of the network, what it refused, why the
  run stopped if it did, and what did not run;
- what to decide before sharing it.

It holds key paths, kinds, counts, statuses, header names, New York dates
where a question needs one, Schwab's own wording, and Schwab's error
messages, scrubbed. It never holds a record, an account number, a hash, an
id, a balance, a quantity, a symbol of yours or a description: a
transaction's description is printed only when it is Schwab's own wording
for a kind of row, such as "Ordinary Dividend~SYM", with the symbol masked.
A failure of the network is told by what failed, such as
`read tcp: i/o timeout`, without the addresses that its error names, your
computer's among them; an address that an error quotes some other way
shows as `{address}`.

A scrubber of the run's own replaces what the run has seen with labels:
each account and its hash by its place in the account numbers, as
`{account 2}` and `{hash 2}`; by their kind, nicknames, display IDs,
streamer IDs, activity, order and position IDs, correlation IDs, the
edge's reference numbers, your app's key and secret, every token of the
login that the run holds, sends or is given, and every symbol in an answer
but SPY and VTI, which the run names itself; your home folder as `~`, your
user name as `{user}`, and the run's files and folders by name. An ID, a
key or a token is found escaped too, as a path or a form spells it. Q4
sends no symbol that the scrubber does not hide: not SPY or VTI, nor one
of a single character, which is too short to hide. Every line is checked
for what the run has seen before it is printed or written, and a line that
still holds something is left out, unshown, and fails the run.

A finding that fails, a probe that panics, a part of the run that stops
before its end, or a line left out fails the test; the report says which.

## The state

`state.json` holds what probes compare across runs, and `state.key` the key
of its digests, beside it: by default in `<user cache dir>/schwabtx/live/`,
or where `-live.state` puts them, both readable only by you, and refused
inside the module or a git work tree, as the out folder is.

Every account number, hash, id and token in it is an HMAC-SHA256 digest
under the key, 32 random bytes made on the first run. The key keeps the
digests comparable from run to run, since an unkeyed digest of an
8-digit account number falls to trying every number in seconds. The two
files together give the numbers back as quickly, so keep them as private as
the token file, and never share them. The report says only "the same",
"changed" or "new", never a digest. A state without its key is refused;
move both away to start afresh.

It holds Q8's daily closes of SPY and VTI, which are public, with when they
were read; Q10's hashes under each account number, with the login's time and
the app key's digest; Q12's rows of the last 60 days, with a digest of each
key path, their time and tradeDate, and the time each was first seen; and
Q15's refresh tokens, with the login's time.

## W-8 lines

A line marked `[W-8]` reveals W-8 withholding rows, which show that an
account is a nonresident's. Q4 and Q11 mark theirs, and the report's last
section counts them. Decide on each before you share the report: leave the
line in, or take it out.

## Recording

With `-live.record`, each answer of the API, never a token request, is kept
under `raw/` in the out folder, the folder readable only by you and never
written over:

- `NNN.body`: the bytes as Schwab sent them;
- `NNN.json`: the method, path and query as sent; the status, the protocol
  and whether the body came compressed; the values of an allowlist of
  headers, such as `Content-Type`, `Date` and the correlation headers, and
  the names alone of the others; when it was sent; and the Go version and
  the commit.

A recording holds everything: account numbers, balances, positions, every
row. Never share it or commit it. Scrubbing a recording into fixtures that
can be committed is a step of its own, not yet built; until it is, a
recording stays on your computer.

## Sharing a report

The report is meant to be read by you, then shared, as an issue or a change
to [the API notes](schwab-api.md). Before you share one:

1. Read it whole. The scrubber knows what the run saw, such as account
   numbers and ids, but not everything private: an error message from
   Schwab could hold a name it has never seen.
2. Decide on each `[W-8]` line.
3. Share `report.txt` alone: never the state, its key, the raw folder or
   the token file.

## The commit

The report and each recording carry the commit they ran on, from git in the
module's root: `git rev-parse HEAD`, marked modified when
`git status --porcelain` prints anything, or "unknown" when git is missing
or fails. A test binary's build information cannot give it: with
`-buildvcs=true`, Go 1.26, this module's minimum, stamps nothing in a linked
git worktree, though Go 1.27 does.
