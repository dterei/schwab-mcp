# The live mode

`TestLive` in `internal/schwab` probes Schwab's real API with your saved
login. It checks every answer against this project's records, runs probes
that settle the [open questions](ROADMAP.md#open-questions), and writes a
report without account numbers, hashes, ids or balances, and without
descriptions but Schwab's own wording, for you to read and then share. It
only reads, and it never runs in CI.

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
  run `schwabtx login` when there is no login, when it has lapsed, or when
  Schwab refuses it, as Schwab does once a later login has replaced it,
  which shows only when the run refreshes it; and it warns when the login
  lapses within a day. A refresh that Schwab refuses for the app's key or
  secret stops it too, and it asks you to check them.
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

On Linux and macOS the run makes every file it writes, and every folder
it makes, readable only by you, wherever they are. On Windows they take
their folders' permissions instead, which in your user profile, where the
defaults are, means only you. So on Windows give `-live.out` and
`-live.state` only a folder that only you can read, such as one in your
user profile, never a shared one such as `C:\Users\Public` or a network
drive: the report is meant for you to read first, and the raw folder and
the state with its key give your account numbers back.

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
| Q1 | default | How far back transactions go, and what a window older than that gets | Walks each account's history back in windows of a year, a second apart, to three empty windows past the oldest row or 2000-01-01, and asks for 2000 as a control. A refusal counts only when it says something of the window, and the newest window then still answers. Gives each account's oldest row's New York date, "or earlier" when the walk ran out of requests first; whether older activity exists that Schwab does not serve is yours to judge. An account without rows settles nothing, since Schwab answers [] to every window of one whose history it does not serve |
| Q2 | default | The tradeDate of a fill from 18:00 to 04:00 in New York | Counts the history's evening and overnight executions by what their tradeDate is |
| Q3 | `watch=balances` | When initial balances move, and whether they then hold the close | Reads the accounts every 10 minutes from 15:45 to 10:00 the next day. Needs holdings with prices: cash never moves |
| Q4 | default | Whether the symbol filter keeps dividends and W-8 withholdings | Asks for a symbol's rows with `symbol=` and without; a verdict needs the symbol's own trade back and a row of another symbol left out. It sends only a symbol that the scrubber hides, so that a refusal quoting it shows a label |
| Q5 | default | Whether an account's orders are served for a year or 60 days | Gets the order of the youngest execution over 67 days old by id, then asks the list for two days around when it was entered, if they lie within the last year. An order counts as old only past 67 days, since a list kept for 60 days by calendar dates can hold one a little older. It answers with the bounds the lists show: longer than 60 days by the age of the oldest order that came back, a year only for one of nearly a year, under the age of the order left out when the list answered, rounded up to whole days, or between the two when both show; lists that give back an order at least as old as the one they leave out disagree, which is inconclusive |
| Q6 | default | Whether `scope=readonly` gives a token that cannot trade | Reports the scope Schwab granted, and whether any read that a working login passes was refused as unauthorized: not the reads that Schwab may refuse for reasons of its own, those whose answers the probes ask about, such as Q1's old windows or the extras' unknown hash, and Q12's windows, which end days ahead. Log in with `&scope=readonly` added to the login link to try it; whether such a token cannot trade stays open, since only an order could show it |
| Q8 | default | Whether daily price bars are adjusted for dividends | Keeps SPY's and VTI's daily closes in the state, as each of the last 20 runs read them, with when, and compares them across an ex-date: an earlier run must come before the close of the last session before the ex-date, and the later one after the ex-date, since Schwab may adjust the closes at any time in between. A run in between, as a daily run makes one, is kept beside the earlier run, not over it. Needs the Market Data product |
| Q10 | default | Whether account hashes stay the same across logins and apps | Keeps keyed digests of each account's hash; a run after your weekly login compares them |
| Q11 | default | How kinds of event show up in transactions | Counts the shapes of the history's rows |
| Q12 | default | Whether rows change after they post, and how long after | Keeps keyed digests of the last 60 days' rows; a later run names the keys that changed, the rows gone and the rows that came late. Its window reaches ahead to rows dated up to the fourth weekday after the run's day, since an evening or overnight fill may be dated the next trading day, after a holiday too: a window that ended at the run would leave out such a fill that was there, which a later run could then neither compare nor show gone. So a run after one of an earlier version, whose window did end at the run, does not compare with it, but starts afresh, as though no earlier run had read the rows. Whether Schwab takes a window that ends days ahead is not known: if it refuses them, the report shows its answer, the verdict is inconclusive, and Q6 does not count the refusal against the login. Nor is it known whether Schwab serves such a window past the request: one that served it only up to the request, taking rows by their tradeDate, would leave out such a fill as a window ending at the run does, so a row dated after the last run counts as late only when an answer of that run, of any account, held a row dated after it came, which shows that its reads would have shown the row: Schwab serves the rest, or takes rows by their time; else it may have been there all along, left out, so whether it changed is undecided as well, and no answer can show such a fill gone: "no row went" cannot speak of a fill dated after the last run that such a Schwab left out of that run's answer, nor "no row changed" or "no row came late" of one that it left out of this run's answer too, dated after this run as well, as a Friday evening's fill dated Monday is at a run on the Saturday. A row counts as gone or late only when both its time and its tradeDate lie in the windows that would show it, since which of the two a window goes by is not known, and only against an answer that the cap cannot have cut. A row shown gone stays in the state, so that one that comes back is compared, as it was or changed, rather than called late; while answers leave it out, it is gone still only in one that the cap cannot have cut, whose window holds both its times, and missing from the others. A run after one of an earlier version that dropped such rows starts afresh too, as after one whose window ended at the run. A row counts as late only when its time was more than an hour before the last run, which left it out, since a row may take minutes to post; it posted between the two runs, so the report gives how long after its time that was as a span, such as from 5 hours to 4 days. The verdict says what a run leaves undecided rather than that no row did it: whether rows changed or went, when a row is missing from an answer but not shown gone, or the last run's answer may have been cut at the cap, leaving out rows that never came into the state; whether rows changed, when a new row dated after the last run may have been there, left out of that run's answer; whether rows came late, when a new row from over an hour before the last run is not shown late, or this run's answer may have been cut; and anything of an account that it did not compare, whose window failed or that no earlier run read, beside which it speaks of the accounts compared alone. With no row to compare, as for accounts without activity, or with all three undecided, it is inconclusive |
| Q13 | `watch=fills` | How soon a fill's trade shows up | Polls the orders entered in the last day and their trades every minute for half an hour. Start it, then place a trade by hand: a read before the fill bounds the time from below, as a row there at the watch's first read does not. For a fill from 18:00 to 04:00 in New York, which may be dated the next trading day, after a holiday too, it asks for trades dated up to the fourth weekday after it, a window that ends after the poll. It bounds each time by when its reads were sent and answered, and a read whose window could not show a fill's row says nothing of it. Each execution of an order has a row of its own, with the order's id, so it times an order filled in parts from its first execution, whose row comes first when rows post in the order of their executions. Nor does a read say anything of a row dated after it, as such a fill's may be, since whether Schwab serves a window past the request is not known, unless an answer of the watch held a row dated after it came, which shows that a read can show such a row: Schwab serves a window past the request, or takes rows by their time |
| Q14 | `-live.rate` | How far under 120 a minute Schwab refuses requests, and whether a burst draws a refusal | Sends the account numbers at 20, 40, 60, 80 and 100 a minute, then a burst of 5, one try each, and stops at the first 429, 403 or 5xx. It starts only when nothing has used the key for two minutes, and stops if another process uses it, or if answers come so slowly that a step's request would go more than 5 seconds behind its pace, or a request of the burst more than 3 seconds after the burst's first, since the rate sent would then not be the one named |
| Q15 | default | Whether a refresh ends older access tokens, or changes the refresh token | Refreshes once, with a control before, and tries the old and new tokens after, and the old again 30 seconds later: an old token that still works then has survived the refresh for 30 seconds, which is all a run shows, one refused both times was ended by it, and one refused at once but working 30 seconds later says nothing |
| extras | default | Details milestone 2 relies on | About fifteen single requests: a window's bounds, inclusive or not, and whether Schwab keeps their milliseconds, drops them or rounds them, or keeps a row's time finer than it shows, which the four windows around a row cannot always tell apart; whether a window takes a row by its time or its tradeDate; windows of a year and a second, and of 366 days; no types; a date alone; a row by its id against the list; an unknown hash and id; the 401 for a bad token and for none |

Q7 (`previewOrder`) has no probe, since it would need a POST, and Q9 (how to
confirm an order) is a question of design. Q8, Q10, Q12 and Q15's
comparison across runs need two runs or more; their first run keeps what
the next one compares with.

## The report

`report.txt` in the out folder, readable only by you (on Windows, as its
folder is), written section by section as the run goes, so that a run
that stops keeps what it has:

- the toolchain, the commit, the time, the mode and the probes;
- each answer of the reads made once, the account numbers, the
  preferences, the checks, Q5's orders and Q8's market data, under its
  endpoint's template, such as `accounts/{account 2}/transactions`: its
  status, `Content-Type`, the names of its correlation headers, how many
  records it held, and what the checks found: a record's problems, a key
  that no field takes, a key sent twice in one object; and of the reads a
  probe repeats, such as Q1's walk, Q4's and Q12's windows, the extras and
  the polls of the opt-in modes, only an answer that fails or brings a
  finding new to its endpoint;
- the keys seen on each type of record, against the keys the tests expect;
- each probe's verdict and evidence;
- the guard's accounting: the budget, the answers by status, the token
  requests, the 429s, the failures of the network, what it refused, why the
  run stopped if it did, and what did not run;
- what to decide before sharing it.

It holds key paths, kinds, counts, statuses, header names, New York dates
where a question needs one, Schwab's own wording, and Schwab's error
messages, scrubbed. It never holds a record, an account number, a hash, an
id, a balance, a quantity, a symbol of yours longer than a character, or a
description: a transaction's description is printed only when it is
Schwab's own wording for a kind of row, such as "Ordinary Dividend~SYM",
with the symbol masked. A failure of the network is told by what failed,
such as `read tcp: i/o timeout`, without what its error names of your
computer and its network: the addresses, your computer's among them; the
name of any host but Schwab's, which can only be the proxy's that
`HTTPS_PROXY` names, and shows as the proxy's host; and the names that a
refused certificate gives, a captive portal's or an intercepting proxy's,
and the authority's that may have signed it. Nor does it hold the query of
the request's address; an address that an error quotes some other way
shows as `{address}`.

A scrubber of the run's own replaces what the run has seen with labels:
each account and its hash by its place in the account numbers, as
`{account 2}` and `{hash 2}`; by their kind, nicknames, display IDs,
streamer IDs, activity, order and position IDs, correlation IDs, the
edge's reference numbers, your app's key and secret, every token of the
login that the run holds, sends or is given, and every symbol in an answer
but SPY and VTI, which the run names itself; your home folder as `~`, your
user name as `{user}` where a path holds it as the folder of a home, such
as `/home/<name>`, and the run's files and folders by name. An ID, a key or
a token is found escaped too, as a path or a form spells it. A symbol or a
nickname of a single character is never hidden, since it is too short to
hide without garbling the report: an error of Schwab's that quotes one
shows it. Q4 sends no symbol that the scrubber does not hide: not SPY or
VTI, nor one of a single character.

Nicknames and symbols are hidden only in Schwab's own words, such as an
error message that is not its wording, or an error's code that is not one
of those it is known to send, either of which could quote one. The rest of
the report is the run's own words and Schwab's names, its keys, headers,
wording and known codes, which hold none of yours but can spell one: "no
read of the account API was refused" spells a symbol API, and "Ordinary
Dividend~SYM" a symbol SYM. A label there, in words a reader knows, would
show what it hides, so such a word is printed as it is. So is a nickname
of Schwab, which the report names throughout, and your user name anywhere
but as the folder of a home: the API's paths and the run's own words can
spell it, as `accounts/{account 2}/orders` spells a user named orders, and
nothing but a local path brings it into the report. Every line is checked
for what the run has seen, where the scrubber would hide it, before it is
printed or written, and a line that still holds something is left out,
unshown, and fails the run.

A finding that fails, a probe that panics, a part of the run that stops
before its end, or a line or a section left out fails the test, and the
report's last section says which; a note stands in the report for what
was left out.

## The state

`state.json` holds what probes compare across runs, and `state.key` the key
of its digests, beside it: by default in `<user cache dir>/schwabtx/live/`,
or where `-live.state` puts them, both readable only by you (on Windows,
as their folder is), and refused inside the module or a git work tree, as
the out folder is.

Every account number, hash, id and token in it is an HMAC-SHA256 digest
under the key, 32 random bytes made on the first run. The key keeps the
digests comparable from run to run, since an unkeyed digest of an
8-digit account number falls to trying every number in seconds. The two
files together give the numbers back as quickly, so keep them as private as
the token file, and never share them. The report says only "the same",
"changed" or "new", never a digest. A state without its key is refused;
move both away to start afresh.

It holds Q8's daily closes of SPY and VTI, which are public, as each of the
last 20 runs read them, with when; Q10's hashes under each account number,
with the login's time and the app key's digest; Q12's rows of the last 60
days, with a digest of each key path, their time and tradeDate, the time
each was first seen and, for a row a run showed gone, when; and Q15's
refresh tokens, with the login's time.

## W-8 lines

A line marked `[W-8]` reveals W-8 withholding rows, which show that an
account is a nonresident's. Q4 and Q11 mark theirs, and the report's last
section counts them. Decide on each before you share the report: leave the
line in, or take it out.

## Recording

With `-live.record`, each answer of the API, never a token request, is kept
under `raw/` in the out folder, the folder readable only by you (on
Windows, as the out folder is) and never written over:

- `NNN.body`: the bytes as Schwab sent them;
- `NNN.json`: the method, path and query as sent; the status, the protocol
  and whether the body came compressed; the values of an allowlist of
  headers, such as `Content-Type`, `Date` and the correlation headers, and
  the names alone of the others; when it was sent; and the Go version and
  the commit.

A recording holds everything: account numbers, balances, positions, every
row. Never share it or commit it. What may be committed is what the
scrubber makes of it, once you have read it (below).

## Scrubbing a recording into fixtures

`TestScrub` in `internal/schwab` makes candidate fixtures of a recording.
Like the live mode, it runs only when asked, and never in CI:

```
go test ./internal/schwab -run '^TestScrub$' -scrub=<out>/raw -scrub.out=<new folder> -v
```

| Flag | What it does |
| --- | --- |
| `-scrub=DIR` | The recording's `raw/` folder; without it `TestScrub` is skipped |
| `-scrub.out=DIR` | Where the scrubber writes, a folder it makes: one that exists already is refused, as is one inside this module or inside any git work tree, decided as for the live mode's folders |
| `-scrub.amounts=refuse` | The default: no answer that holds money is written, only its shape |
| `-scrub.amounts=keep` | Every amount is kept as Schwab sent it |
| `-scrub.amounts=scale:K` | Every amount is multiplied by K, a whole number of 2 or more |

`TestScrub` fails where `CI` or `GITHUB_ACTIONS` is set. Choose `keep` or
`scale:K` only for amounts you would publish: scaling hides nothing, since
whole quantities give K away by their greatest common divisor, and prices,
which are kept, are public on their dates.

What it does:

- Every identifier gets a fake by its place in the recording, never one
  made from its value, and the same fake wherever it is: as a string or a
  number, inside other text, in a path, a query or a header's value. The
  accounts take `FixtureAccounts`' numbers in the order of the account
  numbers' answer, then the spares; a hash is `HashFor` of its account's
  fake; a masked number keeps its form and ends with its fake's digits;
  activity, order and position IDs become numbers of as many digits, in the
  same order; nicknames become `Account 1`, `Account 2` and so on; the
  streamer's IDs, correlation IDs, errors' IDs and Akamai's Reference
  become the fixtures' fakes. A recording of more than 23 accounts, or of
  an account whose hash changed during it, is refused. A masked number
  whose fake would read as one of the recording's own, as when an account
  ends with the three digits its fake ends with, leaves out each answer
  that holds it, such as the user's preferences, with why.
- Every other value is kept only where the scrub policy says what it may
  be: an enum of its list, a time, a symbol, a security's name and so on.
  A kept value that holds an identifier keeps every other character, as a
  security's name keeps its words around a nickname's fake; a value of an
  enum's list, such as `CASH`, is Schwab's own word and stays as it is,
  even where a nickname is spelled the same. Free text is kept when it is
  Schwab's own wording, such as "Dividend Reinvest", and else becomes
  `DESCRIPTION N` or `MESSAGE N`, the same text the same N. A value at a
  path that no rule covers, an answer of a kind the policy has no table
  for, such as orders and market data, an answer cut short, or a page
  other than Schwab's known error pages, leaves its file out.
- Each new value is put in place of the old in Schwab's bytes, which are
  never encoded again, so key order, keys sent twice, white space, number
  literals and escapes stay as Schwab sent them.
- Before anything is written, its gates check the output against the
  recording: the same problems when decoded, the same keys, only the
  values that the policy changes changed, each account's hash and masked
  number agreeing with its number, ids equal where they were equal, every
  sum of the records off by K times what it was, and no identifier of the
  recording or local path anywhere, written or decoded: not your home
  folder, the recording's, the out folder or this module's, in any
  spelling, nor your user name as the folder of a home, such as
  `/home/<name>`. Your user name alone is not looked for, since Schwab's
  words can spell it, as `max-age` does `max`. Any failure writes nothing.
- A file that a test CI runs over every committed fixture would fail on
  is left out, with why: the value hygiene tests, `checkAnswer`, which
  fails on a record's problem or a key that no field takes, and the test
  that no account number is part of another value. So is a file that
  would still hold an identifier the scrubber could not replace, such as
  one written with escapes in a value it keeps, or a number in its path
  where no id goes; and so is one whose path asks for a transaction or an
  order by an id that no row of the fixtures would hold.

What it writes, in the out folder:

- `recorded/<date>/`, the recording's day in UTC: the candidate fixtures,
  named by their numbers in the recording, and nothing else;
- `entries.json`: their entries for `index.json`, each with source
  `recorded`, its date, the scrubber's commit and the amounts policy;
- `review.txt`: what it wrote and mapped, what it left out and why, each
  identifier it replaced inside a value it otherwise kept, the
  placeholders, the values mapped at keys in another case, and the rows
  of W-8 withholding, all without a value of the recording;
- `shapes/`: under `refuse`, the shape of each answer left out for its
  money, its key paths with their kinds and counts and no values, to model
  a hand-made fixture on.

To commit a recording:

1. Read `review.txt` whole, and then the candidate files. The scrubber
   replaces what it knows to be an identifier and any text it does not
   know as Schwab's, but you know your own values best. A nickname of 6
   characters or more is replaced in any case, so a word of a public name,
   such as a fund's, that is spelled as one is replaced too: `review.txt`
   lists each value an identifier was replaced inside.
2. Decide on each row of W-8 withholding, which shows that an account is a
   nonresident's: keep its file, or leave it out.
3. Copy `recorded/<date>/` into
   `internal/schwab/schwabtest/testdata/recorded/<date>/`, beside the
   hand-made files and never over them, and add the entries of
   `entries.json` to the list in
   `internal/schwab/schwabtest/testdata/index.json`, the one index of
   every fixture. The module keeps one recording a day, scrubbed whole so
   that its fakes agree, and a second recording's files would take the
   first's names: the scrubber refuses a recording of a day that the
   module holds one of already. If the new one is to replace it, remove
   that folder and its entries first, and scrub again.
4. Run `go test ./...`. The value hygiene tests, the census and
   `checkAnswer` read every indexed file, so a failure names what is wrong.
   A file that fails cannot be committed as it is: leave it out, or extend
   the scrub policy or the records first, and scrub again. A file left out
   for a record's problem or an unknown key is evidence for the records,
   as the live report's findings are.
5. Commit the files and `index.json` together. Never commit the raw folder,
   the out folder's other files, or a file that is only partly scrubbed,
   not even for a moment: a file once pushed stays in the history and in
   every module download.

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

The report, each recording and each scrubbed fixture's entry carry the
commit they ran on, from git in the module's root: `git rev-parse HEAD`, marked modified when
`git status --porcelain` prints anything, or "unknown" when git is missing
or fails. A test binary's build information cannot give it: with
`-buildvcs=true`, Go 1.26, this module's minimum, stamps nothing in a linked
git worktree, though Go 1.27 does.
