# The store

`schwabtx` keeps your history in a store: one SQLite file on your computer.
It holds what Schwab's API returned, the snapshots that `schwabtx snapshot`
takes, the files you export from schwab.com, and your decisions about them,
and it never deletes any of them. From those it derives, by rules with a
version, what a calculator reads: each transaction once, with a category,
whether it counts, and why.

This page is the reference: what the store holds and where, how it is
opened and written, the commands with what they print, importing
schwab.com's files and the snapshot file, counting each transaction once and
settling what the store flags, exports, backups, privacy, the views that
later milestones read, and the tests' time. The commands' flags are in the
[CLI's README](../cmd/schwabtx/README.md#the-store), and what schwab.com's
exports hold is in [Schwab's exports](schwab-exports.md).

None of the store's commands logs in or reaches the network; none reads
`SCHWAB_APP_KEY` or the token file.

Each command shown after a `$` is followed by what it printed, on the store
of the [walk-through](#a-walk-through); the messages quoted without a
command name the store's file as `PATH`.

## Where it lives

The store is the file that `-store PATH` names, else `SCHWAB_STORE_FILE`,
else `history.db` in a folder `schwabtx` of your user config directory:

| System | Default |
| --- | --- |
| Linux | `~/.config/schwabtx/history.db` (or under `$XDG_CONFIG_HOME`) |
| macOS | `~/Library/Application Support/schwabtx/history.db` |
| Windows | `%LocalAppData%\schwabtx\history.db` |

On Windows it is in `%LocalAppData%`, not in `%AppData%` beside the token
file, because `%AppData%` roams: a roaming profile copies its files when you
sign in and out, a scheduled run can overlap that, and copying an SQLite
file while it is in use, with its `-wal`, can leave a damaged copy. On
Linux and macOS it is never in the cache directory, which those systems may
empty: the store holds history that Schwab no longer serves, the files you
imported and your decisions.

**Permissions.** On Linux and macOS the store is made readable only by you,
before SQLite opens it, so that its `-wal` and `-shm` are private too, and a
folder it makes for it can be entered only by you. A store that others
could read is made private once it has been read as a schwabtx store, with
a warning:

```
warning: PATH could be read by others; it is now readable only by you
```

On Windows the files take their folder's permissions, which at the default
location means only you; keep a store you name with `-store` inside your
user profile.

**Local disks only.** SQLite's write-ahead log, which the store uses so that
a sync, a scheduled snapshot import and the MCP server can share it, needs
shared memory and file locks that network file systems do not give. A store
on NFS, SMB, AFP, WebDAV, 9P (WSL's `/mnt/c`) or a FUSE mount of a network
(sshfs, rclone and the like) is refused:

```
error: the store must be on a local disk: PATH is on a network file system (nfs), where SQLite's WAL mode can damage it; put it on a local disk with -store PATH or SCHWAB_STORE_FILE
```

A store on another FUSE file system, or in a folder that a sync service
copies (Dropbox, OneDrive, Google Drive, iCloud Drive), is opened with a
warning. Keep it out of them: a sync service that copies the store while it
is open can leave a damaged copy, and two computers that each open their
copy make two histories that cannot be merged.

## Its files are one thing

The store is three files: `history.db`, `history.db-wal` and
`history.db-shm`. A writer that is killed can leave committed changes only
in `history.db-wal`, which the next process to open the store moves into
`history.db`. So:

- Never copy, move or delete one of the three without the others, and never
  copy any of them while `schwabtx` (or the MCP server, later) has the store
  open.
- Copy the store with `schwabtx store backup FILE`, which makes a
  consistent, private copy while others go on using the store.
- Put a backup back with `schwabtx store restore BACKUP`, which writes it
  into the store through SQLite, never by copying a file over
  `history.db`. A file copied over `history.db` beside a `-wal` left by a
  killed writer is read with that `-wal`, which turns it into neither the
  backup nor the store.

Beside them are the store's other files: `history.db.migrate.lock`, which a
change of schema takes, `history.db.sync.lock`, which the sync will take,
and `history.db.schema<N>.<time>.bak`, a backup made before a newer
`schwabtx` changes the schema. Leave the lock files in place; they stay
empty.

**An orphan journal.** If `history.db` is missing, or empty, but
`history.db-wal` or `history.db-shm` is there, the store refuses to open
rather than adopt a journal that belongs to another store:

```
error: PATH is missing but PATH-wal is here: the journal of an earlier store whose database was moved or deleted; move history.db-wal and history.db-shm to where history.db went, or remove them if you mean to discard that store
```

Put the database back beside its journal, or, if you mean to start again,
remove the two journal files.

**The store's files are never inputs or outputs.** On Linux and macOS,
closing any descriptor of a file drops every lock that a process holds on
it, so a process that read the store's own files by path while it had the
store open could lose another process's commits. `schwabtx import` checks
every file it is given before it reads any, reads them all before it opens
the store, and refuses the store's files; `export -o`, `export -dir` and
`store backup` refuse to write over them, by name, so that a file that does
not exist yet is refused too:

```
$ schwabtx import .config/schwabtx/history.db-wal
error: .config/schwabtx/history.db-wal is the store itself (or one of its files: history.db-wal); nothing was imported
$ schwabtx export -o .config/schwabtx/history.db-wal
error: -o .config/schwabtx/history.db-wal would replace the store itself (or one of its files); choose another name
```

So `schwabtx import ~/.config/schwabtx/*` is safe while a sync runs: it
refuses, and imports nothing.

## Opening and writing

`schwabtx import` and `schwabtx store restore` make the store when it is
missing, and say so (`new store at PATH`); their dry runs (`-dry-run`) do
not, nor does an import of a file it refuses. Every other command refuses a
missing store and makes nothing:

```
$ schwabtx status -store nothing.db
error: no store at /home/you/nothing.db yet; schwabtx import makes one
```

Before anything can change a file, the store reads it through a connection
that cannot write: a file that is not a schwabtx store, such as another
program's SQLite database named by mistake, is refused and left exactly as
it was (`PATH is not a schwabtx store (it is another program's SQLite file);
nothing in it was changed`), and a file that SQLite cannot read is reported
damaged, with what to do (see [Backup and restore](#backup-and-restore)).

**Writes.** Each command's writes are one transaction: an import of several
files saves all of them or none, and a refused file leaves the store as it
was. A write takes the store's write lock as it starts, so that it never
reads and then fails to write. SQLite does not queue processes that wait for
the lock, so a write that finds it taken is rolled back and tried again,
after a pause that grows, for up to two minutes. Once it has waited a few
seconds (SQLite's own wait for the lock, five seconds, comes first) it says
so:

```
waiting for another schwabtx process that is writing to the store...
```

and after two minutes it gives up and saves nothing:

```
error: the store at PATH stayed locked by another process for 2m0s while importing 2 files; nothing was saved; is a sync or an import running?
```

Reads never wait for writes: they see the store as of the last commit.

**Versions.** The store records three versions: of its schema, of the rules
it derives with, and of the NYSE calendar. A `schwabtx` that finds an older
store brings it up to its own: before it changes the schema it backs the
store up beside it (`backed the store up to FILE before updating its
schema`), and when its rules or calendar are newer it derives again (`updated
the store's derived data for this schwabtx (rules 1 -> 2)`, `recomputed
market dates for calendar 2`). A store written by a newer `schwabtx` is
refused, and so is every write of an older process that is still running
when a newer one has changed the store:

```
error: the store at PATH was written by a newer schwabtx (schema N, rules R, calendar C); update this schwabtx, and the one your scheduled task runs
```

Update every `schwabtx` that uses the store, the one your scheduled task
runs included. There are no downgrades: to go back to an older `schwabtx`,
restore with it a backup made before the newer one changed the store, such
as the `history.db.schema<N>.<time>.bak` it made.

## A walk-through

These are the commands as a first session uses them, with what they printed
on a store made for this page from the project's test fixtures: three
accounts ending in 135, 398 and 783, the snapshots of six evenings, the
output of `schwabtx transactions -json`, and a History export, its CSV twin
and a Realized Gain/Loss export of the account ending in 135. Every account
number, amount and file in it is made up. The store is in its default place
for a user whose home is `/home/you`, on Linux.

### Snapshots

`schwabtx import` with no file imports the snapshot file that `schwabtx
snapshot` writes:

```
$ schwabtx import
snapshots.jsonl: snapshot file, 6 lines: 18 new snapshots, 0 already held
new store at /home/you/.config/schwabtx/history.db
1 file imported into /home/you/.config/schwabtx/history.db
```

A snapshot file only grows, so import it as often as you like: lines the
store holds are known, and only new ones are added. A scheduled script can
run `schwabtx import` after `schwabtx snapshot`; the scheduler scripts in
[`cmd/schwabtx/schedule`](../cmd/schwabtx/schedule) do not, so that the
snapshot, which cannot be taken again later, never waits for the store.

Each snapshot stands for the last NYSE session that had closed when it was
taken: an evening snapshot stands for that day's close, one taken after
midnight in New York still for the day before, one on a weekend or a
holiday for the last session before it, from a calendar with the early
closes. A snapshot taken while the market is open stands for the session
before and is marked `in_session`; it fills no gap. The sessions between an
account's first and last snapshot that no snapshot stands for are gaps,
which `status` counts and `export -what gaps` lists.

### API rows, until the sync exists

`schwabtx sync`, which will read your transactions from Schwab's API into
the store, waits for the live probes (see the
[roadmap](ROADMAP.md#m2-full-transaction-history-stored-locally)). Until
then, the output of `schwabtx transactions -json` gives the store Schwab's
own rows:

```sh
schwabtx transactions -json -n 100000 -lookback 3650 > ~/Downloads/transactions.json
```

```
$ schwabtx import Downloads/transactions.json
transactions.json: schwabtx transactions -json output, 14 rows
  A2 ...398: 5 rows, 5 new to the store, 0 already held
  A1 ...135: 9 rows, 9 new to the store, 0 already held
1 file imported into /home/you/.config/schwabtx/history.db
```

Such a file only adds: a transaction the store holds stays as it is. Its
rows give their account weaker coverage than the sync will, `api_file`: the
days strictly inside the span of the rows of each account. Its first and
last days are left out, since `-n` may have cut the oldest day and the
newest may have gained rows since.

### History exports

Export History from schwab.com (Accounts > History, Export) as JSON, which
states its dates and a total, for every type and no symbol (see
[Schwab's exports](schwab-exports.md)). `-dry-run` shows what an import
would do and saves nothing:

```
$ schwabtx import -dry-run Downloads/Individual_XXX135_Transactions_20240601-090000.json
Individual_XXX135_Transactions_20240601-090000.json: History export (JSON) of A1 ...135,
    2023-02-01 to 2024-05-31 (as the file states), 11 rows
  11 transactions new to the store, 0 already held (another file had them)
  linked to Schwab's API rows: 8; uncertain: 0; held out (inside the API's dates, no API row): 1;
    outside the API's dates (counted from the file): 2
  completeness: unknown; if you exported every type and no symbol, say so: schwabtx edit Fe7a63ed0 -completeness complete
nothing was saved (-dry-run)
```

The account comes from the file's name, which holds the last three digits of
its number (`XXX135`); give `-account` when the name has none, or when two of
your accounts end in the same digits. Several files go in one command, all
or none:

```
$ schwabtx import Downloads/Individual_XXX135_Transactions_20240601-090000.json Downloads/Individual_XXX135_Transactions_20240601-090000.csv Downloads/XXXX0135_GainLoss_Realized_Details_20240601-090000.csv
Individual_XXX135_Transactions_20240601-090000.json: History export (JSON) of A1 ...135,
    2023-02-01 to 2024-05-31 (as the file states), 11 rows
  11 transactions new to the store, 0 already held (another file had them)
  linked to Schwab's API rows: 8; uncertain: 0; held out (inside the API's dates, no API row): 1;
    outside the API's dates (counted from the file): 2
  completeness: unknown; if you exported every type and no symbol, say so: schwabtx edit Fe7a63ed0 -completeness complete
Individual_XXX135_Transactions_20240601-090000.csv: History export (CSV) of A1 ...135,
    2023-02-01 to 2024-05-01 (from its rows), 11 rows
  0 transactions new to the store, 11 already held (another file had them)
  linked to Schwab's API rows: 8; uncertain: 0; held out (inside the API's dates, no API row): 1;
    outside the API's dates (counted from the file): 2
  warning: coverage_from_rows: it states no range, and none was given: its range is its rows', 2023-02-01 to 2024-05-01
  completeness: unknown; if you exported every type and no symbol, say so: schwabtx edit F0869ae0a -completeness complete
XXXX0135_GainLoss_Realized_Details_20240601-090000.csv: Realized Gain/Loss export (Lot Details) of A1 ...135,
    2024-01-01 to 2024-05-31 (as the file states), 1 lot
  1 lot new to the store, 0 already held (another file had them)
3 files imported into /home/you/.config/schwabtx/history.db; 1 item waits for you: schwabtx status -review
```

The CSV is the same export as the JSON, so its 11 rows are the JSON's 11
transactions and add none. Of the 11, 8 are rows the API holds, which the
API's record stands for; 2 are older than the API's history, so the file is
their record; and one, a Bank Interest row, has no API row although the
API's rows cover its day, so it is held out until you say what it is. A file
imported again adds nothing:

```
$ schwabtx import Downloads/Individual_XXX135_Transactions_20240601-090000.json
Individual_XXX135_Transactions_20240601-090000.json: History export (JSON)
  warning: already imported as Fe7a63ed0 for A1 ...135; if it is for another account: schwabtx edit Fe7a63ed0 -account ACCOUNT
0 files imported (1 already held) into /home/you/.config/schwabtx/history.db; 1 item waits for you: schwabtx status -review
```

A file that is none of these is refused by what it is, and then nothing is
imported:

```
$ schwabtx import Downloads/notes.txt
error: notes.txt is not a file schwabtx knows how to import; nothing was imported
```

### Status

```
$ schwabtx status
store     /home/you/.config/schwabtx/history.db (618.5 kB), schema 1, rules 1, calendar 1 (checked to 2028)
ACCOUNT    API DATES                 FILE DATES                SNAPSHOTS                 GAPS
A1 ...135  2023-06-13 to 2025-06-01  2023-02-01 to 2024-05-31  2026-11-02 to 2026-11-10  1, and 1 session since
A2 ...398  2025-01-15 to 2026-08-03  -                         2026-11-02 to 2026-11-10  1, and 1 session since
A3 ...783  -                         -                         2026-11-02 to 2026-11-10  1, and 1 session since
ledger: 14 from the API, 2 from files only; 8 file rows linked to API rows
to review: 1 held out
  (schwabtx status -review)
```

API DATES are the days the API's rows cover (here, the `api_file` days of the
`-json` file), FILE DATES those of History files, SNAPSHOTS the first and
last market dates of the account's snapshots, and GAPS the sessions between
them without a snapshot, and those that have closed since the last one.
`-account` limits it to one account, and `-show-accounts` prints full
account numbers.

`status -review` lists what waits for you, each with the command that
settles it; `-all` adds what is flagged for information only:

```
$ schwabtx status -review
Held out: Bank Interest (1): no API transaction is these, though the API's dates hold them; not counted until you say
  Ce7a63ed0.6  A1 ...135  2024-03-14  Bank Interest  0.37
      api_file: the API's dates here are those of a schwabtx transactions -json file
      if no API row is it: schwabtx edit Ce7a63ed0.6 -link none
  if the API never shows such rows: schwabtx edit -action 'Bank Interest' -policy count
Snapshot gaps (3): market sessions that no snapshot taken after their close stands for
  A1 ...135
      1 session without a snapshot: 2026-11-04
      1 session has closed since the last snapshot, of 2026-11-10; schwabtx snapshot takes one
  A2 ...398
      1 session without a snapshot: 2026-11-04
      1 session has closed since the last snapshot, of 2026-11-10; schwabtx snapshot takes one
  A3 ...783
      1 session without a snapshot: 2026-11-04
      1 session has closed since the last snapshot, of 2026-11-10; schwabtx snapshot takes one
```

A gap cannot be filled: Schwab reports positions and balances only as of
now. It is listed so that you know which days milestone 4 will have to
rebuild rather than read.

`status REF` prints one thing in full: an entry with its flags, revisions,
match and rows; a file with its coverage and what became of its rows; a lot,
a snapshot, a security's key or an account:

```
$ schwabtx status Ce7a63ed0.6
Ce7a63ed0.6: file transaction of A1 ...135
  source          file
  day             2024-03-14
  posted_date     2024-03-15
  kind            Bank Interest
  description     BANK INT ...424 SCHWAB BANK
  amount          0.37
  fees            0.00
  gross           0.37
  quantity_as_of  2024-06-01
  category        interest
  category_rule   file.income
  counted         0
  counted_why     held_out
  flags           held_out
Flags:
  held_out: api_file
Rows:
  Fe7a63ed0 row 6  line 66  2024-03-15  Bank Interest  BANK INT ...424 SCHWAB BANK  0.37
  F0869ae0a row 6  line 7  2024-03-15  Bank Interest  BANK INT ...424 SCHWAB BANK  0.37  (shown)
```

```
$ schwabtx status T73508261946
T73508261946: API transaction of A1 ...135
  source          api
  day             2024-04-04
  time            2024-04-04T15:56:07Z
  kind            TRADE
  description     Dividend Reinvest
  amount          -2.70
  fees            0.00
  gross           -2.70
  quantity        0.0378
  price           71.42857
  symbol          BND
  security_key    i:4417302
  category        buy
  category_rule   trade.reinvest
  counted         1
  counted_why     api
  matched_with    Ce7a63ed0.2
  activity_id     73508261946
  trade_day       2024-04-04
  settlement_day  2024-04-08
  status          VALID
  sub_account     CASH
  position_id     3926184811
  revisions       1
  first_seen_at   2026-11-12T12:40:42.617Z
  first_seen_by   file
  latest_seen_at  2026-11-12T12:40:42.617Z
Revisions:
  revision 1  first seen 2026-11-12T12:40:42.617Z by file  last seen 2026-11-12T12:40:42.617Z
Matched with:
  Ce7a63ed0.2  unique  score 95.0
```

### Refs

Commands print and take refs, in any case; none starts with `#`, which a
shell reads as a comment:

| Ref | Names | Stable because |
| --- | --- | --- |
| `A3` | an account, by its number in the store | accounts are never deleted |
| `T73508261941` | an API transaction, by Schwab's activity ID (with `-account` when two accounts share one) | it is Schwab's id |
| `T~20260302T143027.1f2e3d4c5b6a` | an API transaction without an activity ID: its time and the start of its digest | while the row is unchanged |
| `C3a9e21b0.17` | a transaction of History files: the start of the SHA-256 of the file whose row opened it, and that row's number | while that file is imported and the row opens it |
| `F3a9e21b0` | an imported file: the start of its SHA-256 | the file's bytes |
| `L5c1d2e3f.4` | a closed lot of Realized Gain/Loss files, as for `C` | as for `C` |
| `S812` | a snapshot | snapshots are never deleted |
| `K:i:1973757747` | a security's key: `i:` Schwab's instrument ID, `c:` a CUSIP, `x:` a History export's `ItemIssueId`, `s:EQUITY:XYZ` a symbol of a position, `s:FILE:XYZ@2026-11-11` a symbol of a file exported that day | Schwab's ids, or the symbol |

The hex of `F`, `C` and `L` refs is the shortest start of 8 digits or more
that no other file shares, and any start that names one thing will do
(`C3a9e.17`); one that names several is an error that lists them. Your
decisions do not depend on `C` and `L` refs: a decision names a row of a
file and follows the transaction that holds it (see [Your
decisions](#your-decisions)).

### Edit

`schwabtx edit REF OPTION...` records your decisions, which the store
applies after its own rules, and prints what each changed:

```
$ schwabtx edit Ce7a63ed0.6 -link none -note "Bank Interest is not in the API"
Ce7a63ed0.6 -link none: recorded
  Ce7a63ed0.6: not counted (held out: the API covers its dates) -> counted (your decision)
$ schwabtx edit Fe7a63ed0 -completeness complete -note "every type, no symbol"
Fe7a63ed0 -completeness complete: recorded; no transaction is booked otherwise
$ schwabtx edit T73508261949 -category fee
T73508261949 -category fee: recorded
  T73508261949: interest -> fee (yours; the rule said doi.interest)
$ schwabtx edit T73508261949 -category auto
T73508261949 -category auto: recorded
  T73508261949: fee -> interest (the rule doi.interest)
$ schwabtx edit -action "Bank Interest" -policy count
held-out rows of "Bank Interest": count: recorded; no transaction is booked otherwise
```

`auto` withdraws a decision; `-note` says why, and is kept with it. Commands
that `status` prints for you to copy are quoted for your shell where a ref
needs it.

### Export

`schwabtx export` writes the ledger, the counted transactions, as CSV to
stdout, or to a file with `-o`; a summary goes to stderr:

```
$ schwabtx export -o ledger.csv
wrote 17 rows ledger to ledger.csv
  A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
  A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
  A3 ...783: snapshot 2026-11-02 to 2026-11-10
```

`ledger.csv` then holds:

```
ref,account_id,account,source,day,time,kind,description,amount,fees,gross,quantity,quantity_as_of,price,symbol,security_key,category,pair_kind,pair_ref,pair_account_id,flags
Ce7a63ed0.11,1,...135,file,2023-02-01,,MoneyLink Transfer,"Tfr EXAMPLE BANK, N, HOLDER NAME",500.00,0.00,500.00,,2024-06-01,,,,deposit,,,,
Ce7a63ed0.10,1,...135,file,2023-03-10,,Buy,VANGUARD SMALL CAP VALUE ETF,-320.00,0.00,-320.00,2,2024-06-01,160,VBR,x:66000002,buy,,,,
T73508261953,1,...135,api,2023-06-12,2023-06-12T14:20:31Z,CASH_RECEIPT,Funds Received,150.00,0.00,150.00,,,,,,deposit,,,,
T73508261952,1,...135,api,2023-07-19,2023-07-19T07:32:10Z,ELECTRONIC_FUND,ACH Credit Received,2500.00,0.00,2500.00,,,,,,deposit,,,,
T73508261951,1,...135,api,2023-12-15,2023-12-15T16:02:55Z,WIRE_IN,Wire Incoming,10000.00,0.00,10000.00,,,,,,deposit,,,,
Ce7a63ed0.6,1,...135,file,2024-03-14,,Bank Interest,BANK INT ...424 SCHWAB BANK,0.37,0.00,0.37,,2024-06-01,,,,interest,,,,
T73508261947,1,...135,api,2024-03-15,2024-03-15T23:36:04Z,TRADE,Sell Trade,83.27,0.00,83.27,-0.4513,,184.51,VBR,i:4417355,sell,,,,
T73508261950,1,...135,api,2024-04-04,2024-04-04T15:54:14Z,DIVIDEND_OR_INTEREST,Ordinary Dividend~BND,3.86,0.00,3.86,,,,BND,i:4417302,dividend,,,,
T73508261954,1,...135,api,2024-04-04,2024-04-04T15:54:15Z,JOURNAL,W-8 Withholding~BND,-1.16,0.00,-1.16,,,,BND,i:4417302,tax,,,,
T73508261946,1,...135,api,2024-04-04,2024-04-04T15:56:07Z,TRADE,Dividend Reinvest,-2.70,0.00,-2.70,0.0378,,71.42857,BND,i:4417302,buy,,,,
T73508261949,1,...135,api,2024-04-30,2024-05-01T01:18:44Z,DIVIDEND_OR_INTEREST,Free Balance Interest Adjustment,0.42,0.00,0.42,,,,,,interest,,,,
T73508261945,2,...398,api,2025-01-14,2025-01-14T05:15:12Z,TRADE,,-1052.82,2.82,-1050.00,1,,21,/ESH25_P5050:XCME,i:205187341,buy,,,,
T73508261944,2,...398,api,2025-01-21,2025-01-21T16:05:12Z,TRADE,,-400.67,0.67,-400.00,2,,2,XYZ   250221P00040000,i:219771034,buy,,,,
T73508261943,1,...135,api,2025-06-02,2025-06-02T14:30:27Z,TRADE,,-605.46,0.00,-605.46,3,,201.82,AAPL,i:1973757747,buy,,,,
T73508261942,2,...398,api,2026-03-13,2026-03-13T13:55:09Z,TRADE,,603.36,0.04,603.40,-1,,603.4,META,i:1940083512,sell,,,,
T73508261948,2,...398,api,2026-07-15,2026-07-15T14:40:03Z,DIVIDEND_OR_INTEREST,Ordinary Dividend~XYZ,25.00,0.00,25.00,,,,XYZ,i:1854092,dividend,,,,security_by_name
T73508261941,2,...398,api,2026-08-04,2026-08-04T15:22:41Z,TRADE,,311.31,0.69,312.00,-1,,3.12,XYZ   260918C00050000,i:219834765,sell,,,,
```

`-account`, `-from` and `-to` select rows, and `-format jsonl` writes JSON
Lines:

```
$ schwabtx export -account A1 -from 2024-04-01 -to 2024-04-30 -format jsonl
{"ref":"T73508261950","account_id":1,"account":"...135","source":"api","day":"2024-04-04","time":"2024-04-04T15:54:14Z","kind":"DIVIDEND_OR_INTEREST","description":"Ordinary Dividend~BND","amount":3.86,"fees":0.00,"gross":3.86,"quantity":null,"quantity_as_of":null,"price":null,"symbol":"BND","security_key":"i:4417302","category":"dividend","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"T73508261954","account_id":1,"account":"...135","source":"api","day":"2024-04-04","time":"2024-04-04T15:54:15Z","kind":"JOURNAL","description":"W-8 Withholding~BND","amount":-1.16,"fees":0.00,"gross":-1.16,"quantity":null,"quantity_as_of":null,"price":null,"symbol":"BND","security_key":"i:4417302","category":"tax","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"T73508261946","account_id":1,"account":"...135","source":"api","day":"2024-04-04","time":"2024-04-04T15:56:07Z","kind":"TRADE","description":"Dividend Reinvest","amount":-2.70,"fees":0.00,"gross":-2.70,"quantity":0.0378,"quantity_as_of":null,"price":71.42857,"symbol":"BND","security_key":"i:4417302","category":"buy","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"T73508261949","account_id":1,"account":"...135","source":"api","day":"2024-04-30","time":"2024-05-01T01:18:44Z","kind":"DIVIDEND_OR_INTEREST","description":"Free Balance Interest Adjustment","amount":0.42,"fees":0.00,"gross":0.42,"quantity":null,"quantity_as_of":null,"price":null,"symbol":null,"security_key":null,"category":"interest","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
wrote 4 rows ledger
  A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
```

`-what` names another table, or with `-dir` several, or `all`:

```
$ schwabtx export -what all -dir export
wrote 17 rows ledger, 25 rows entries, 14 rows transactions, 41 rows legs, 11 rows file-transactions, 4 rows imports, 18 rows snapshots, 36 rows positions, 1050 rows balances, 1 row lots, 3 rows gaps, 3 rows flags, 7 rows coverage, 14 rows securities to export
  A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
  A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
  A3 ...783: snapshot 2026-11-02 to 2026-11-10
$ schwabtx export -what ledger,snapshots
error: -what ledger,snapshots needs -dir
```

[Exports](#exports) has the tables, their columns and formats.

### Upkeep

```
$ schwabtx store check
the store is sound: /home/you/.config/schwabtx/history.db
$ schwabtx store rebuild
derived everything in /home/you/.config/schwabtx/history.db again from the evidence
$ schwabtx store backup history-backup.db
backed the store up to /home/you/history-backup.db; schwabtx store restore /home/you/history-backup.db puts it back
$ schwabtx store backup history-backup.db
error: backing the store up: /home/you/history-backup.db exists; choose a new name
```

`store check` runs SQLite's integrity and foreign key checks and checks
what the derived data must hold (no transaction counted twice, every match
with both sides in one account, pairs that agree), and exits 1 on any
problem. `store rebuild` derives everything again from the evidence: what
the derivation at the end of each write gives, so it changes nothing unless
something is wrong. A command that schwabtx prints for you to copy, such as
the restore after a backup, or one in a flag's detail, a review item, a
warning or a refusal, names the store with `-store` when it is not the one a
command opens without it (an export, which is data, keeps a flag's detail as
the store holds it). `-forget` sets an imported file aside, and `-undo` brings
it back:

```
$ schwabtx import -forget F0869ae0a
set aside F0869ae0a (Individual_XXX135_Transactions_20240601-090000.csv): 0 transactions that no other file holds left the ledger, 11 stay (also in Fe7a63ed0), and your decisions on them follow them
bring it back with: schwabtx import -forget F0869ae0a -undo
$ schwabtx import -forget F0869ae0a -undo
brought back F0869ae0a (Individual_XXX135_Transactions_20240601-090000.csv): 0 transactions that no other file holds are in the ledger again, 11 stay (also in Fe7a63ed0)
```

A file set aside stays in the store, bytes and all: setting aside is a
decision, which `-undo` withdraws. Imported again, it is refused, with the
command that brings it back, and nothing is imported.

## Importing files

`schwabtx import FILE...` tells each file by its content, never its name or
extension:

| File | What the store takes from it |
| --- | --- |
| a snapshot file of `schwabtx snapshot` | each account's snapshot, once whatever file or sync brings it; its accounts |
| `schwabtx transactions -json` output | Schwab's API rows, attributed by each row's account number (or `-account`), and `api_file` coverage |
| a History export of schwab.com, CSV (every layout since 2014) or JSON | its rows, each transaction once across files, matched to the API's |
| a Realized Gain/Loss export's Lot Details (CSV) | each closed lot once across files, with its versions |

Anything else, Schwab's other exports included (Positions, Equity Awards,
Schwab Bank, the open lots' details, the Realized Gain/Loss summary, the XML
History export), is refused with what it is, and then nothing is imported.
[Schwab's exports](schwab-exports.md) says how to export each file, and what
the import reads and checks.

- **The account.** A History file's name holds the account's last three
  digits (`Individual_XXX135_...`), a Realized file's its last four
  (`XXXX0135_...`), and older files say it in their first line. `-account`
  names it when the file does not, or settles digits that two accounts
  share (`A1`, more digits, or the nickname); a file whose digits are not its
  account's is refused unless you say `-force`, as for a renamed file. A file
  imported for the wrong account is moved with `schwabtx edit F… -account
  A2`.
- **Its dates.** A History JSON states its range, and so do the CSVs of
  2021 and before. The current CSV states none: give it with `-from` and
  `-to`, or later with `edit F… -from DATE -to DATE`; else its rows' oldest
  and newest dates stand for it, with a warning. See [Coverage](#coverage).
- **Its completeness.** Nothing in an export says whether you filtered it by
  type or symbol. `-all-types` says you did not, `-filtered` that you did;
  else the store establishes it, or leaves it `unknown`. See
  [Completeness](#completeness).
- **Each file once.** The store knows a file by its SHA-256: the same bytes
  again add nothing, whatever the file's name.

The file's name is kept as Schwab wrote it, its stamp
(`_20240601-090000`) and masked digits included; only free text in it that
holds 8 digits or more, a nickname say, is masked.

## Evidence and what is derived

Everything the store holds is one of two things:

- **Evidence**: what Schwab, a file or you gave. Each API read (what it
  asked, when, and what came back), each transaction's first and latest raw
  JSON and the first raw JSON of each revision, what a complete read did not
  return, each snapshot element as the file held it, each imported file's
  bytes, and each of your decisions. Evidence is never deleted, and SQLite
  triggers refuse to change it, but for what only moves forward, such as a
  transaction's latest raw JSON.
- **Derived data**: what versioned rules make of the evidence, in tables of
  its own: each transaction's columns and legs, revisions' changed paths,
  securities, each file's rows, transactions and lots, coverage,
  completeness, entries with their categories, pairs, matches, counting and
  flags, each snapshot's balances, positions and market date.

What one piece of evidence gives by itself is derived when it arrives. What
relates the evidence of an account (which rows are one transaction, what is
covered, what is matched and counted) is derived again from scratch for each
account a write touched, at the end of that write; so are the securities and
the journal pairs across accounts when they may have changed. A rebuild
(`schwabtx store rebuild`) derives everything from the evidence alone, which
gives the same as those derivations: a property test runs random sequences
of imports, decisions and set-asides and checks that the derived data after
each sequence is what a rebuild derives.

So an answer to one of the [open questions](ROADMAP.md#open-questions) is a
new version of the rules and a rebuild, never a loss: the raw JSON, the
reads and the files are all still there.

**Revisions.** Schwab's transactions change: a status, a new key. The store
compares a re-read row with what it holds in a cut form: the row's JSON with
each instrument cut to its asset type and instrument ID (an instrument is
reference data as of the request: its `closingPrice` moves, and a delisted
equity comes back under its CUSIP), keys sorted, and numbers compared by
the value they denote (`0` and `0.0` are one value). A row whose cut form
differs is a new revision, with the key paths that changed; one that only
moves back and forth makes a run of revisions, each keeping its first raw
JSON. A later version of the cut form may only merge runs, never split them,
since a run keeps only its first raw JSON.

**Gone.** A transaction that a complete read did not return is gone: a read
of every type and no symbol, answered with fewer than 3000 rows, made after
the last read that returned it, whose window holds both its time and its
trade date with a second to spare on each side (the window's end taken as
no later than when the request was sent). A gone transaction is kept,
flagged `gone` and not counted unless you say; one that a later read
returns again counts again by itself. What each complete read did not return
is kept as evidence, so a narrower rule, once the probes settle how Schwab
bounds a window, is a rebuild. A transaction whose time changes in a later
revision keeps the absences recorded under its old time; the next complete
read records them under the new one.

## Units and times

- **Money** is integer cents (`*_cents` in the views) and **quantities**
  integer millionths (`*_micros`): a transaction's net amount and each leg's
  cost, a cash leg's amount, a security leg's quantity, positions'
  quantities, the liquidation values and cash of a snapshot, and every money
  and quantity column of a file. A file's numbers are read exactly from their
  text. Schwab's are doubles, converted when exact: a double whose value
  times 100 (or a million) is a whole number below 2^51.
- A value within a ten-thousandth of a step of that grid, binary noise from
  Schwab's own arithmetic such as `0.30000000000000004` or
  `-236.999999999999`, is stored rounded and flagged `rounded` (info), with
  the field and the literal.
- Anything else, such as `81.29223999999999` in cents, is left empty and
  flagged `inexact`, with the field and the literal; the raw JSON keeps it.
  `status -review` lists counted entries whose amount is empty.
- Prices, positions' averages, profit and loss, market values and strikes
  stay floating point (`REAL`): they carry fractions of a cent.
- **Signs** are Schwab's: cash into the account is positive, a quantity
  bought is positive. A History export writes trades' quantities unsigned;
  the store signs them by the action.
- **Instants** (`*_at`, `time`, `fetched_at`) are UTC, written
  `2026-11-03T02:05:00.000Z` in the store and `2026-11-03T02:05:00Z` in an
  export when the milliseconds are 0. **Days** (`day`, `*_date`, `*_day`)
  are New York calendar dates, `2026-11-02`. An API transaction's day is the
  New York date of its trade date; a file transaction's is its "as of" date
  when the row has one, else its posted date.

## Counting each transaction once

Several sources can hold one transaction: the API's rows, a History export,
its CSV or JSON twin, a later export that overlaps it, a `-json` file. The
store counts each transaction once, by three rules.

**Within History files, the most that one file holds.** Rows of one account
with the same posted date, as-of date, action, fees and amount are one
group. A file's rows of a group are each a transaction, so two identical
trades in one file stay two. Across files, the group holds as many
transactions as the most rows of it that any one file holds; each
transaction takes one row from each file that has it, the best fitting by
symbol, quantity, price and description. So a re-import, an overlapping
export or a JSON and CSV of one export add nothing, and a later export that
restates a ticker or, after a split, a quantity adds nothing either: it
joins the transaction it restates, flagged `restated` (or
`restated_quantity`). The count is raised only where rows are known to be of
different securities: different Schwab ids that existed at once, different
option contracts, different CUSIPs, or your `-differ`. Two tickers that
nothing in the store resolves are counted once and flagged `symbol_unsure`,
with the command that splits them if they are two securities; a count raised
because securities differ is flagged `split_by_security`, with the command
that joins them back.

**Where the API covers, it is the record.** A file transaction that is an
API transaction (by family, day, amount and symbol, or by your `-link`) is
linked to it, and only the API's record counts. One that several API
transactions could be is uncertain, flagged `uncertain_match`, and not
counted until you say which. One inside the API's coverage that no API
transaction is, is held out (`held_out`): not counted until you say, so that
a miss of the matcher can never count one transaction twice. Outside the
API's coverage, a file transaction counts: it is history the API does not
serve. One exception: a row of TD Ameritrade's history (its description
starts `TDA TRAN`) inside the coverage that matches nothing counts, flagged
`tda_history`, since the API probably does not serve that history.

**Your decisions come last.** `edit C… -link T…` links, `-link none` counts
from the file, `-reject T…` keeps a pair from forming, `-count yes|no`
overrides the rest, and `edit -action LABEL -policy count` counts every
held-out row of an action, such as Bank Interest, that the API turns out not
to show. `-link none`, and `-count yes`, also say that the row is none of the
API rows the store holds then: one that may be it and comes later, as a row
the API serves late does, flags the file row `late_api_row`, and both count
until you say which it is.

An entry's `counted_why` says which rule decided:

| `counted_why` | Counted | Why |
| --- | --- | --- |
| `api` | yes | an API transaction |
| `gone` | no | a complete read left it out |
| `invalid` | no | Schwab's status says INVALID |
| `linked` | no | a file transaction linked to API transactions, which count in its place |
| `uncertain` | no | several API transactions could be it |
| `held_out` | no | inside the API's coverage, and no API transaction is it |
| `tda` | yes | of TD Ameritrade's history, inside the coverage, unmatched |
| `file` | yes | outside the API's coverage: history the API does not serve |
| `user` | as you said | your `-count`, or your `-link none`, which counts it from the file |

Before the sync, the API's coverage is the `-json` files' `api_file` days. On
their end days, and outside them, a row that the matcher misses counts beside
the API row it missed; the sync's coverage will close that gap.

`store check` verifies that no counted file transaction is in a match and
that every match has both sides in one account.

### Coverage

The `coverages` view, `status` and every export's summary say which days
each source covers, per account:

| Source | Days |
| --- | --- |
| `api` | the days that complete reads of the API covered, from the account's oldest API row on (an empty window does not show that history has ended) |
| `api_file` | the days strictly inside the span of a `-json` file's rows of the account |
| `file` | the coverage of History files established complete |
| `file_unknown` | the coverage of History files whose completeness nothing established |
| `file_filtered` | the coverage of History files filtered by type or symbol |
| `realized` | the ranges of Realized Gain/Loss files |
| `snapshot` | the first to the last market date of the account's snapshots |

Only `api` and `file` are days whose history is whole.

A History file's coverage is its range, clipped by what its warnings show:
the range it states, else what you gave with `-from` and `-to`, else its
rows' oldest to newest date (`coverage_from_rows`); widened by rows outside
it (`rows_outside_coverage`). Then: an empty file covers nothing (`empty`),
since an export over a limit is reported to come back empty, until you say
the range had no activity (`edit F… -completeness complete`); a file at a
row cap, or whose oldest row is long after its start (`at_cap`,
`late_start`, `two_years`), starts the day after its oldest row, since rows
come newest first and a cut loses the oldest; and a range that ends on the
day of the export ends the day before, which was not over. Each clipping is
in the `coverage_clipped` flag's detail.

### Completeness

A filtered export looks like any other: nothing in it records the filter. So
a file's completeness is established, never assumed:

1. Your word: `-all-types` or `-filtered` at import, or `edit F…
   -completeness complete|filtered`.
2. Else, if the file lacks rows that another source of the account holds in
   its dates, and the lack has a filter's shape (it holds no row of that
   family at all, or every row it holds is of one other symbol), it is
   `filtered`, with `lacks_rows` saying what it lacks.
3. Else, if every one of more than 20 rows over more than 60 days is of one
   family or one symbol, it stays `unknown`, flagged `looks_filtered`.
4. Else, if its dates share days with the API's coverage, or with a file
   already established complete, and it lacks nothing that source holds on
   those days, it is `complete`, by `api` or by `file`. A `-json` file can
   show a file filtered but never complete, since it may have been filtered
   by `-types` the same way.
5. Else `unknown` (`completeness_unknown`), with the command that says it is
   complete.

A file that lacks one row while holding the rows of its family around it
looks like a correction, not a filter: it stays complete, and the
transaction is flagged `missing_from_later` (or `counts_differ`). In the
walk-through, the JSON was made complete by your word, and its CSV twin then
by agreeing with it (`complete_by` `file`).

Completeness changes flags and coverage, never a count.

### Settling what the store flags

`status -review` lists everything of severity `attention`, grouped, each
with the command that settles it where an edit does (a file's missing dates
need another export), and `-all` adds the `info` flags. The usual ones:

| Flag | What to do |
| --- | --- |
| `uncertain_match` | `edit C… -link T…` the one it is, or `-link none` |
| `held_out` | `edit C… -link T…` if an API row is it, else `-link none`; or `edit -action LABEL -policy count` for every such row of an action |
| `linked_api_uncounted` | the API row it is linked to does not count; `edit C… -link none` counts it from the file |
| `late_api_row` | `edit C… -link T…` if the API row that came is it (`-count auto`, if you gave `-count yes`), else `edit C… -reject T…`, one edit for each it names; only an API row first seen after your decision is flagged, since one the store held then is one you said it is not |
| `symbol_unsure` | nothing if one security changed its ticker; else `edit K:… -differ K:…` |
| `split_by_security` | nothing if they are different securities; else `edit K:… -same K:…` |
| `near_duplicate` | `edit C… -count no` on the one Schwab corrected |
| `missing_from_later`, `counts_differ` | `edit C… -count no` if the later export is right |
| `gone` | nothing if it was cancelled; `edit T… -count yes` if it stands |
| `absent_earlier` | nothing if it came late; `edit T… -count no` if it was cancelled |
| `awaits_probe`, `unknown_type`, `unknown_action`, `zero_amount` | `edit REF -category C` |
| `unpaired_journal`, `ambiguous_journal` | `edit REF -pair REF2`, or `-category` |
| `completeness_unknown`, `looks_filtered`, `lacks_rows` | `edit F… -completeness complete` or `filtered` |
| `stale_decision` | decide again (the same command), or withdraw it with `auto` |
| `empty`, `at_cap`, `late_start`, `two_years` | export the missing dates again, as a shorter range; a late start of a file filtered by type or symbol is no gap: `edit F… -completeness filtered`, and it waits no more |
| snapshot gaps | none: a missed session cannot be taken later; keep the scheduled snapshot running |

## Your decisions

Your decisions are a log that is never changed: a new decision on the same
thing replaces the old one for the derivation, and `auto` withdraws it, but
both stay in the log. Each names what it is about by a subject that does not
depend on the derivation: an API transaction by its account and activity ID
(`a:1:73508261941`), a file transaction by a row of a file
(`c:<sha256>:<row>`), a file by its SHA-256 (`f:<sha256>`), a key
(`k:i:1973757747`), an account (`acct:1`) or an action (`action:bank
interest`). Every decision takes `-note`.

| Kind | Subject | Values | What it does | `schwabtx edit` |
| --- | --- | --- | --- | --- |
| `category` | an entry | a category, `auto` | books the entry in that category, after every rule and pair | `REF -category C` |
| `count` | an entry | `yes`, `no`, `auto` | counts it or not, whatever the rules say | `REF -count yes\|no` |
| `pair` | an entry | another entry, `none`, `auto` | pairs it with another as a journal, sweep or reversal, or forbids a pair | `REF -pair REF2\|none` |
| `entry_security` | an entry | a key, `auto` | its security, in place of what its symbol gave | `REF -security K:…` |
| `match` | a file transaction | API transactions, `none`, `auto` | links it to those API transactions, or says no API row is it | `C… -link T…[,T…]\|none` |
| `reject` | a file transaction | an API transaction, `auto` | keeps that link from forming | `C… -reject T…` |
| `action` | an action | `count`, `hold`, `auto` | whether held-out rows of that action count | `-action LABEL -policy count\|hold` |
| `security` | a key | `same` or `differ` another key, `auto` | that two keys are one security, or two | `K:… -same K:…`, `-differ K:…`, `-auto` |
| `account` | an account | its kind, `auto` | the account's kind (taxable, ira, roth, ...) | `A… -kind TEXT` |
| `completeness` | a file | `complete`, `filtered`, `auto` | the file's completeness; `filtered` on a `-json` file withdraws its `api_file` coverage; refused for a Realized export, of which it is not said | `F… -completeness complete\|filtered` |
| `file_account` | a file | an account, `auto` | the account the file is for, in place of the one at import; both accounts are derived again | `F… -account ACCOUNT` |
| `file_range` | a file | a range, `auto` | the dates the file covers, for a file that states none (refused for one that states its own) | `F… -from DATE -to DATE`, `F… -range auto` |
| `forget` | a file | `yes`, `no` | sets the file aside, or brings it back | `schwabtx import -forget F… [-undo]` |

**A category you set is applied last**, after every rule and automatic pair,
and nothing after it changes it (`category_rule` `user`). A journal you
booked as a deposit (to a spouse's account, say) stays a deposit and takes
no part in automatic pairing.

**A decision follows its transaction.** A decision on a file transaction
names one of its rows, and applies to whatever transaction holds that row
now, whichever file opened it. When that row's file is set aside, the
decision applies to the transaction that holds the row's twins in other
files; only when no other file has the transaction does it wait, never
applied elsewhere, until the file comes back. `import -forget` says how many
wait.

**Stale decisions.** A decision on an entry records what it was made
against. When what the rules read of the transaction changes (its type,
status, wording, amount, the signs and kinds of its legs; for a file
transaction its action, quantity and amount), the decision still applies,
and the entry is flagged `stale_decision`. A key that Schwab adds to every
row, a moved `closingPrice` or a later export's restated ticker stales
nothing. Deciding again confirms the decision against what is there now.

## Categories

Each entry is booked as one of: `deposit`, `withdrawal`, `transfer_in`,
`transfer_out`, `journal`, `buy`, `sell`, `dividend`, `interest`, `fee`,
`tax`, `other`. The rule that booked it is its `category_rule`: for an API
transaction by its type, description and legs (`trade.buy`,
`doi.dividend`, `journal.sweep`, `cash.in`, ...), for a file transaction by
its action's family (`file.buy`, `file.income`, `file.cash`, ...), and
`user` for yours. A trade's fees are all its cash legs, named or not; a
dividend's or withholding's security is the symbol after `~` in its
description. Sweeps between your brokerage cash and the bank are pairs
within an account, booked `other`, never deposits. A journal between two of
your accounts is paired with its counterpart and booked `journal`, which is
external to each account and internal to the whole; one without a
counterpart is booked by its sign, and flagged. A cancel (Cancel Buy, Cancel
Sell) is paired with the trade it cancels. Kinds of row that the probes have
not settled, such as RECEIVE_AND_DELIVER, are `other`, flagged
`awaits_probe`, until you set a category.

## Flags

Each flag has a code, what it can be on (`entry`, `snapshot`, `file` or
`lot`), a severity (`attention` for what needs you, `info` for the rest) and
a meaning; the `flags` view adds a detail, composed of masked text and
identifiers: the literal, the paths that changed, the files that disagree, a
command to copy. Every code:

| Code | On | Severity | Meaning |
| --- | --- | --- | --- |
| `problems` | entry, snapshot | attention | Schwab sent something the records could not read |
| `inexact` | entry, lot, snapshot | attention | a money or quantity value off the cent or millionth grid; its column is empty |
| `rounded` | entry, snapshot, lot | info | a value within a ten-thousandth of a step of the grid (binary noise), stored rounded |
| `no_activity_id` | entry | attention | Schwab sent no activity ID: the row is known by its time and content |
| `activity_id_text` | entry | info | Schwab sent the activity ID as text |
| `account_mismatch` | entry | attention | the row names another account than the one read |
| `duplicate_in_answer` | entry | info | one answer held the row twice |
| `unknown_type` | entry | attention | a transaction type outside Schwab's 15 |
| `unknown_wording` | entry | info | a description not seen in Schwab's answers before |
| `awaits_probe` | entry | attention | a kind the probes have not settled; other until you set a category |
| `unexpected_sign` | entry | attention | a deposit or withdrawal type with the other sign |
| `mixed_legs` | entry | info | a trade with legs bought and sold |
| `no_security_leg` | entry | attention | a trade without a security |
| `zero_quantity` | entry | attention | a trade whose security leg moved nothing |
| `zero_amount` | entry | attention | a cash movement or journal of 0.00: booked as other |
| `legs_mismatch` | entry | attention | the legs' cash does not add up to the net amount |
| `no_legs` | entry | attention | the row has no legs, so its cash cannot be checked |
| `status` | entry | attention | a status other than VALID; INVALID is not counted |
| `revised` | entry | info | Schwab's record changed since first seen |
| `gone` | entry | attention | a complete re-read left it out; not counted unless you say |
| `absent_earlier` | entry | attention | first met in a -json file, and a complete read made before you imported it did not return it: it came late, or it was cancelled |
| `unpaired_sweep` | entry | info | a sweep without its opposite |
| `unpaired_journal` | entry | attention | a journal without a counterpart in your other accounts; booked by sign |
| `ambiguous_journal` | entry | attention | a journal with several possible counterparts in your other accounts; booked by sign |
| `unresolved_security` | entry, lot | info | the symbol names no one security in the store near the row's date |
| `security_by_name` | entry | info | the symbol was attached to the one security that has had the name, seen within a year but not within a week of the row |
| `stale_decision` | entry, file | attention | your decision was made when what the rules read of the row was different, or the transaction it named now holds other rows |
| `category_disagrees` | entry | info | the linked file row's action suggests another category |
| `not_in_files` | entry | info | inside a complete History file's dates, with no file row |
| `unknown_action` | entry | attention | an Action not known |
| `other_action` | entry | info | an Action booked as other |
| `return_of_capital` | entry | info | a return of capital, booked as a dividend |
| `two_row_event` | entry | info | one of the two rows that a file writes for one event |
| `tda_history` | entry | info | a row from TD Ameritrade's history, which the API probably does not serve |
| `uncertain_match` | entry | attention | several API rows could be it; not counted until you say |
| `held_out` | entry | attention | no API row although the API covers its dates; not counted until you say |
| `linked_api_uncounted` | entry | attention | the API row it is linked to is gone, INVALID or not counted by you, so neither counts |
| `late_api_row` | entry | attention | you said it counts from the file, and an API row that may be it has come since: both count until you say |
| `merged_fills` | entry | info | linked to several fills of one order |
| `merged_rows` | entry | info | one of several file rows linked to one API transaction |
| `differs_from_api` | entry | info | linked, with another quantity, price, fees or day |
| `restated` | entry, lot | info | another export shows it with another symbol, description or price |
| `restated_quantity` | entry | attention | another export shows a row that moves holdings with another quantity (a split restatement?) |
| `symbol_unsure` | entry, lot | attention | rows in different exports of one day, action and amount name different symbols that nothing in the store resolves |
| `split_by_security` | entry, lot | attention | counted apart from a row of another file because their securities differ |
| `near_duplicate` | entry | attention | another transaction of the same day, action and symbol, never in the same file, differs in amount |
| `missing_from_later` | entry | attention | a later complete export of its day does not have it, though it holds the rows around it |
| `counts_differ` | entry | attention | complete exports of its day hold different numbers of it |
| `unpaired_reversal` | entry | attention | a Cancel Buy or Cancel Sell without its original |
| `reversal_signs` | entry | attention | a Cancel Buy or Cancel Sell whose Amount has the original's sign, so its cash counts twice |
| `no_account_number` | snapshot | attention | no readable account number |
| `snapshot_conflict` | snapshot | attention | two different snapshots of one account at one time |
| `in_session` | snapshot | info | taken during a session |
| `before_calendar` | snapshot | info | taken before 2000, where the calendar starts |
| `calendar_unverified` | snapshot | info | past the years the calendar was checked against NYSE's published dates |
| `empty` | file | attention | the file holds no transactions |
| `at_cap` | file | attention | the file holds as many rows as an export can, so its oldest days may be missing |
| `two_years` | file | attention | a range of more than two years whose oldest row is near its two-year limit |
| `late_start` | file | attention | the oldest row is more than a week after the range's start |
| `total_mismatch` | file | attention | the stated total is not the rows' sum |
| `lacks_rows` | file | attention | it lacks, in a filter's shape, rows that other sources of the account hold in its dates |
| `looks_filtered` | file | attention | every row is of one family or one symbol over a long span: an export filtered by type or symbol? |
| `completeness_unknown` | file | info | nothing yet shows that the file holds every type and symbol |
| `coverage_clipped` | file | info | its coverage is narrower than its stated range |
| `encoding_fallback` | file | info | the file is not UTF-8 and was read as Windows-1252 |
| `lazy_quotes` | file | info | the file's quotes were read leniently |
| `rows_after_total` | file | info | rows after the file's total, which were not read |
| `coverage_from_rows` | file | info | the file states no range, so its rows' dates give it |
| `rows_outside_coverage` | file | info | rows lie outside the stated range, which they widen |
| `undated_rows` | file | attention | rows without a date, which make no transaction |
| `unknown_actions` | file | info | rows with actions schwabtx does not know |
| `name_mismatch` | file | attention | the digits in the file's name are not those of its account |
| `columns_disagree` | lot | info | the gain or loss is not proceeds less cost basis, with or without the disallowed loss |
| `placeholder` | lot | info | a money column holds a placeholder such as -- |
| `lot_revised` | lot | info | exports show the lot in more than one version |

The flags on files are the import's warnings: `import` prints each with its
detail, as `warning: code: detail`.

## Exports

```
schwabtx export [-store PATH] [-what WHAT] [-format csv|jsonl] [-account ACCOUNT]
                [-from DATE] [-to DATE] [-all] [-raw] [-show-accounts] [-o FILE | -dir DIR]
```

`-what` names one of these tables, a comma-separated list of them, or
`all`; a list and `all` need `-dir`:

| Name | View | What it holds | Takes `-raw` |
| --- | --- | --- | --- |
| `ledger` | `ledger` | counted entries, from the API and from files: what a calculator reads | yes |
| `entries` | `entries` | every entry, with whether it is counted and why | yes |
| `transactions` | `transactions` | Schwab's API transactions, one row each | yes |
| `legs` | `legs` | one row per leg, the API's and those made for file transactions; of counted entries unless -all |  |
| `file-transactions` | `file_transactions` | the transactions of History files, each once | yes |
| `imports` | `files` | the files imported from schwab.com and schwabtx |  |
| `snapshots` | `snapshots` | snapshots of the accounts, with the market date each stands for | yes |
| `positions` | `positions` | the positions of each snapshot |  |
| `balances` | `balances` | every balance figure of each snapshot |  |
| `lots` | `realized_lots` | closed lots from Realized Gain/Loss files, each once | yes |
| `gaps` | `snapshot_gaps` | market sessions that no snapshot of an account stands for |  |
| `flags` | `flags` | what the store flagged, with why |  |
| `coverage` | `coverages` | the days each source covers, per account |  |
| `securities` | `securities` | the securities, with their latest symbol, CUSIP and name |  |

With `-all`, `ledger` is `entries`, and `legs` holds the legs of every
entry, counted or not.

- **Columns** are the view's, in its order (see [The views](#the-views)). A
  `*_cents` column is written as dollars with two places and named without
  the suffix (`amount_cents` is `amount`, `-605.46`), a `*_micros` column as
  a decimal without trailing zeros (`quantity_micros` is `quantity`,
  `0.0378`, `3`), both formatted from the store's integers, so exactly.
  Other numbers are written as briefly as they read back
  (`71.42857`), dates as they are, and instants in UTC in RFC 3339, with
  milliseconds only when there are any. An empty value is an empty CSV cell
  and JSON `null`.
- **CSV** is UTF-8 without a byte order mark, with a header row, quoted as
  RFC 4180 says, lines ending in LF. **JSON Lines** (`-format jsonl`) is one
  object per row, its keys the CSV header, amounts as JSON numbers written
  from the decimal text.
- **Order.** The ledger and entries by day, time, account and ref;
  transactions by time and activity ID; legs by day, ref and leg; file
  transactions by posted date and ref; imports by when they were imported;
  snapshots, positions and balances by when they were taken, account and
  snapshot; lots by closed date and ref. An export of a store that has not
  changed is the same bytes.
- **Filters.** `-account` keeps one account's rows; `-from` and `-to` keep
  rows whose day (a transaction's trade day, a file transaction's day, a
  snapshot's market date, a lot's closed date) falls inside, and coverage
  that meets them. Imports, flags and securities have no day.
- **`-raw`** adds a column `raw`, and for API rows `first_raw`: Schwab's
  JSON as it sent it, a History JSON element or a CSV row as the file had
  it, a snapshot element as the snapshot file held it; inserted as it is in
  JSON Lines, never re-encoded, and as text in CSV. **`-show-accounts`** adds
  `account_number`. Both hold full account numbers, and `export` then says
  so on stderr (`warning: this export holds full account numbers`).
  Without them no exported cell holds one.
- **Where.** stdout by default. `-o FILE`, and each file of `-dir DIR` (made
  readable only by you), is written to a temporary file beside it, readable
  only by you, synced, and then renamed over it, so an export that fails
  leaves no half file and never one that others can read. `-dir` also writes
  `manifest.json`: when, the store's versions, the filters, each table's file
  and rows, the units, and each account's coverage by source.
- **The summary** on stderr gives the rows written and each account's
  coverage by source, and says what an export of counted rows (the ledger
  and the legs, without `-all`) leaves out: file transactions that wait for
  you to resolve them.
- **One read.** An export reads every table it writes in one read
  transaction, so they agree with one another, into memory, and only then
  writes them, so that a slow reader of its output (`| less`) keeps no
  transaction open.

Portfolio Performance and Ghostfolio formats are not written yet. The ledger
holds what both need (the type from the category, the security by symbol or
CUSIP, shares, gross, fees, net, date; journal pairs mark moves between your
accounts), so they will be `-format pp` and `-format ghostfolio` over the
same rows.

## Backup and restore

`schwabtx store backup FILE` copies the store, as of its last commit, to a
new file readable only by you, while other processes go on reading and
writing it. FILE must not exist, and must not be one of the store's files.

`schwabtx store restore BACKUP` replaces what the store holds with what the
backup holds:

```
$ schwabtx store restore -dry-run history-backup.db
the store held 3 accounts, 14 API transactions, 18 snapshots, 4 imported files, 7 decisions
the backup holds 3 accounts, 14 API transactions, 18 snapshots, 4 imported files, 5 decisions
nothing was changed (-dry-run)
$ schwabtx store restore history-backup.db
warning: restored the store from /home/you/history-backup.db (taken 2026-11-12T12:40:43.143Z); what was recorded since is gone
the store held 3 accounts, 14 API transactions, 18 snapshots, 4 imported files, 7 decisions
it now holds 3 accounts, 14 API transactions, 18 snapshots, 4 imported files, 5 decisions
```

The backup is read first: it must be a schwabtx store, sound, and not
written by a newer `schwabtx`; one of an older schema or rules is brought up
to date in a private copy first. Then SQLite's backup interface copies it
into the store in one write: SQLite first takes in whatever the store's own
`-wal` holds, then replaces the content, and other processes' connections
stay valid and see the restored content. `restore` makes the store when it
is missing.

**A damaged store.** A store that SQLite cannot read is refused with what to
do:

```
error: PATH is damaged or not an SQLite file: move history.db, history.db-wal and history.db-shm aside together (never the first alone: the others may hold its last changes), then schwabtx store restore BACKUP (PATH.schema*.bak, or one from schwabtx store backup), or start a new store
```

Move the three files aside together, into a folder of their own, then
`schwabtx store restore BACKUP`, which makes a new store and fills it.

## Privacy

The store holds full account numbers and your whole history: it is made
readable only by you, and so is every export and backup. What it shows is
masked:

- **Account numbers** show as `...` and their last three digits, as Schwab
  shows them (`...135`), in every command's output, every view and every
  export, and the full number only with `-show-accounts` (`status`,
  `export`) or `-raw`.
- **Free text** that Schwab, a file or you wrote (descriptions, nicknames,
  security names from files and positions, a file's name, notes, Schwab's
  error text) is masked by pattern when it is stored: every run of 8 or more
  digits, with up to three spaces or one dash between groups of them,
  becomes `...` and its last three digits. `JOURNAL TO 12345678` reads
  `JOURNAL TO ...678`, and a check number of six digits stays. A date
  written with dashes holds 8 digits and is masked too: Bank Interest's
  period `021524-031424` reads `...424`, as in the walk-through; the
  transaction's own day, and its raw JSON, keep it.
- **Identifiers** are never masked, and none can be an account number:
  refs, Schwab's activity, order, position and instrument IDs, CUSIPs,
  `ItemIssueId`s, hashes, dates, amounts, and numbers from Schwab's JSON.
- **No view** reads an account number, raw JSON, a file's name or its bytes.
  Every view column is free text or an identifier ([The views](#the-views)
  says which). The tests hold every view to it: SQLite's authorizer shows
  that no view reads those columns, and a store filled from every source,
  with numbers planted in every kind of free text, holds none of them in
  any cell of any view.

Schwab's account numbers have 8 digits. `store check` and `status` warn of
one that does not (`A3's number has 7 digits: text that holds it is not
masked; please report it`), since the pattern would miss it.

## The views

The views are what milestones 3 and 4 read, and what `export` writes. They
use the units above. `account` is the masked number; join accounts on
`account_id`, never on a label, since nicknames and last digits need not be
unique. Entries are named by `ref` and snapshots by `snapshot_id`, which
derivations and rebuilds keep. A security is named by its `security_key`,
its least key, which stays the same while the evidence does but changes
when new evidence joins two securities (a position's CUSIP key meets a
transaction's instrument ID); join on keys through `security_keys`, which
maps every key ever seen to today's `security_key`.

Each column is `free`, text masked when it was stored, or `id`, which cannot
hold an account number.

### `accounts`

One row per account.

| Column | Kind | What it holds |
| --- | --- | --- |
| `account_id` | id | the account's number in the store |
| `ref` | id | `A` and `account_id` |
| `account` | id | the masked number |
| `nickname` | free | its nickname on schwab.com, from your preferences |
| `account_type` | id | CASH or MARGIN, from its latest snapshot |
| `kind` | free | your `-kind` (taxable, ira, roth, ...) |
| `primary_account` | id | 1 for the primary account of your preferences |
| `lot_method` | id | its lot selection method, from your preferences |
| `hash` | id | its latest account hash |
| `first_seen_at` | id | when the store first met it |
| `last_seen_at` | id | when the store last met it |
| `not_shared_since` | id | when Schwab stopped listing it |
| `used` | id | 1 when anything of it is in the store (transactions, snapshots, a file not set aside, hashes) |

### `securities`

One row per security.

| Column | Kind | What it holds |
| --- | --- | --- |
| `security_key` | id | its least key |
| `definite` | id | 1 when one of its keys is Schwab's id (`i:`, `c:` or `x:`) |
| `asset_type` | id | EQUITY, OPTION, COLLECTIVE_INVESTMENT, ... |
| `type` | id | Schwab's finer type (COMMON_STOCK, VANILLA, ...) |
| `symbol` | id | its latest symbol |
| `cusip` | id | its latest CUSIP |
| `description` | free | its latest name |
| `put_call` | id | an option's PUT or CALL |
| `strike` | id | an option's strike |
| `expiration_day` | id | an option's expiration |
| `multiplier` | id | an option's multiplier |
| `underlying_symbol` | id | an option's underlying |
| `underlying_key` | id | the underlying's `security_key` |
| `first_seen_at` | id | when one of its keys was first seen |
| `last_seen_at` | id | when one of its keys was last seen |

### `security_names`

One row per name a key of a security has had: a symbol, a CUSIP or a
description.

| Column | Kind | What it holds |
| --- | --- | --- |
| `security_key` | id | the security's key |
| `key` | id | the key that had the name |
| `kind` | id | `symbol`, `cusip` or `description` |
| `value` | free | the name: free text when `kind` is `description` |
| `seen_in` | id | where it was seen: an API transaction, a position or a file |
| `first_seen_at` | id | when it was first seen |
| `last_seen_at` | id | when it was last seen |

### `security_keys`

One row per key ever seen.

| Column | Kind | What it holds |
| --- | --- | --- |
| `key` | id | the key |
| `security_key` | id | the security that holds it now |

### `entries`

One row per entry: each API transaction, and each transaction of History
files, counted or not.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | `T…` or `C…` |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `source` | id | `api` or `file` |
| `day` | id | the day it happened on |
| `posted_date` | id | a file transaction's posted date |
| `time` | id | an API transaction's time |
| `kind` | free | Schwab's type, or the file's Action: free text when `source` is `file` |
| `description` | free | Schwab's description, or the file's |
| `amount_cents` | id | the net amount: cash in positive |
| `fees_cents` | id | the fees, positive |
| `gross_cents` | id | the amount before fees (amount plus fees) |
| `quantity_micros` | id | shares or contracts, positive when bought |
| `quantity_as_of` | id | the export date of the row whose quantity is shown, for a file transaction |
| `price` | id | the price per share |
| `symbol` | id | its symbol (a dividend's is after `~`) |
| `security_key` | id | its security |
| `category` | id | its category |
| `category_rule` | id | the rule that booked it, or `user` |
| `pair_kind` | id | `journal`, `sweep` or `reversal` when paired |
| `pair_ref` | id | the entry it is paired with |
| `pair_account_id` | id | that entry's account |
| `counted` | id | 1 when it counts |
| `counted_why` | id | why (see [Counting](#counting-each-transaction-once)) |
| `flags` | id | its flags' codes, comma-separated |
| `matched_with` | id | the refs it is linked to, comma-separated |

### `ledger`

The counted entries, with the columns a calculator reads.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | as in `entries` |
| `account_id` | id | as in `entries` |
| `account` | id | as in `entries` |
| `source` | id | as in `entries` |
| `day` | id | as in `entries` |
| `time` | id | as in `entries` |
| `kind` | free | as in `entries`: free text when `source` is `file` |
| `description` | free | as in `entries` |
| `amount_cents` | id | as in `entries` |
| `fees_cents` | id | as in `entries` |
| `gross_cents` | id | as in `entries` |
| `quantity_micros` | id | as in `entries` |
| `quantity_as_of` | id | as in `entries` |
| `price` | id | as in `entries` |
| `symbol` | id | as in `entries` |
| `security_key` | id | as in `entries` |
| `category` | id | as in `entries` |
| `pair_kind` | id | as in `entries` |
| `pair_ref` | id | as in `entries` |
| `pair_account_id` | id | as in `entries` |
| `flags` | id | as in `entries` |

### `transactions`

One row per API transaction.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | `T…` |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `activity_id` | id | Schwab's activity ID |
| `time` | id | Schwab's `time` |
| `trade_day` | id | the New York date of its trade date |
| `settlement_day` | id | its settlement date |
| `type` | id | Schwab's type |
| `status` | id | Schwab's status |
| `sub_account` | id | CASH or MARGIN |
| `description` | free | Schwab's description |
| `net_cents` | id | its net amount |
| `order_id` | id | Schwab's order ID |
| `position_id` | id | Schwab's position ID |
| `qualified_dividend` | id | Schwab's `qualifiedDividend` |
| `category` | id | its category |
| `category_rule` | id | the rule that booked it |
| `symbol` | id | its symbol |
| `security_key` | id | its security |
| `quantity_micros` | id | its security legs' quantity |
| `price` | id | its price |
| `gross_cents` | id | its amount before fees |
| `fees_cents` | id | its fees |
| `revisions` | id | how many revisions it has had |
| `gone_since` | id | when a complete read first left it out, while it is gone |
| `first_seen_at` | id | when the store first met it |
| `first_seen_by` | id | `api`, or `file` for a `-json` file, whose `first_seen_at` is when you imported it |
| `latest_seen_at` | id | when a read last returned it |
| `counted` | id | 1 when it counts |
| `counted_why` | id | why |
| `flags` | id | its flags' codes |

### `revisions`

One row per revision of an API transaction.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | the transaction's ref |
| `account_id` | id | its account |
| `seq` | id | the revision's number, from 1 |
| `first_seen_at` | id | when it was first seen |
| `last_seen_at` | id | when it was last seen |
| `first_seen_by` | id | `api` or `file` |
| `changed` | id | the key paths that changed from the revision before, as JSON |

### `legs`

One row per leg: each API leg, and the legs made for file transactions. A
file transaction with a quantity has its security as leg 0 and its fees, if
any, as leg 1; one without has one cash leg holding its amount, as an API
cash row does.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | the transaction's ref |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `day` | id | the transaction's day |
| `time` | id | the transaction's time |
| `source` | id | `api` or `file` |
| `leg` | id | the leg's number |
| `asset_type` | id | its instrument's asset type; CURRENCY for cash |
| `symbol` | id | its instrument's symbol |
| `security_key` | id | its security |
| `instrument_id` | id | Schwab's instrument ID |
| `item_issue_id` | id | a History JSON row's `ItemIssueId` |
| `quantity_micros` | id | a security leg's shares or contracts |
| `cash_cents` | id | a cash leg's amount |
| `cost_cents` | id | the leg's cash effect: the legs' costs sum to the net amount |
| `price` | id | a security leg's price |
| `fee_type` | id | a fee leg's type |
| `position_effect` | id | OPENING or CLOSING |
| `put_call` | id | an option's PUT or CALL |
| `strike` | id | an option's strike |
| `expiration_day` | id | an option's expiration |
| `multiplier` | id | an option's multiplier |
| `underlying_symbol` | id | an option's underlying |
| `txn_symbol` | id | the transaction's symbol, which no cash leg carries |
| `txn_security_key` | id | the transaction's security |
| `quantity_as_of` | id | as in `entries` |
| `category` | id | the transaction's category |
| `counted` | id | 1 when the transaction counts |

### `file_transactions`

One row per transaction of History files, shown as the newest export that
holds it writes it.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | `C…` |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `posted_date` | id | the date the row was posted |
| `asof_date` | id | its "as of" date |
| `event_date` | id | the as-of date, else the posted date |
| `action` | free | the Action |
| `symbol` | id | the Symbol |
| `description` | free | the Description |
| `quantity_micros` | id | the Quantity, signed |
| `quantity_as_of` | id | the export date of the row shown |
| `price` | id | the Price |
| `fees_cents` | id | Fees & Comm |
| `amount_cents` | id | the Amount |
| `item_issue_id` | id | a JSON row's `ItemIssueId` |
| `acctg_rule_cd` | id | a JSON row's `AcctgRuleCd` |
| `files` | id | how many files hold it |
| `security_key` | id | its security |
| `category` | id | its category |
| `category_rule` | id | the rule that booked it |
| `counted` | id | 1 when it counts |
| `counted_why` | id | why |
| `flags` | id | its flags' codes |

### `matches`

One row per file transaction and API transaction linked as one.

| Column | Kind | What it holds |
| --- | --- | --- |
| `file_ref` | id | the file transaction |
| `api_ref` | id | the API transaction |
| `account_id` | id | their account |
| `account` | id | the account's masked number |
| `method` | id | how: `exact`, `unique`, `fills`, `legs` or `user` |
| `score` | id | the score of a `unique` match |

### `snapshots`

One row per snapshot of an account.

| Column | Kind | What it holds |
| --- | --- | --- |
| `snapshot_id` | id | the snapshot's number |
| `ref` | id | `S` and `snapshot_id` |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `fetched_at` | id | when Schwab's answer arrived |
| `market_date` | id | the session it stands for: the last closed when it was taken |
| `in_session_date` | id | the session it was taken during, if any |
| `source` | id | `file` (a snapshot file) or `sync` |
| `account_type` | id | CASH or MARGIN |
| `current_liquidation_cents` | id | the current liquidation value |
| `initial_liquidation_cents` | id | the initial liquidation value: the session's start |
| `aggregated_liquidation_cents` | id | `aggregatedBalance`'s liquidation value |
| `cash_cents` | id | the current cash balance |
| `positions` | id | how many positions |
| `flags` | id | its flags' codes |

### `balances`

One row per balance figure of a snapshot.

| Column | Kind | What it holds |
| --- | --- | --- |
| `snapshot_id` | id | the snapshot |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `fetched_at` | id | when it was taken |
| `market_date` | id | the session it stands for |
| `in_session_date` | id | the session it was taken during, if any |
| `balance_set` | id | `initial`, `current`, `projected` or `aggregated` |
| `figure` | id | Schwab's name of the figure |
| `value_as_sent` | id | the value as Schwab sent it |
| `value_cents` | id | the value in cents, when exact or rounded |

### `positions`

One row per position of a snapshot.

| Column | Kind | What it holds |
| --- | --- | --- |
| `snapshot_id` | id | the snapshot |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `fetched_at` | id | when it was taken |
| `market_date` | id | the session it stands for |
| `in_session_date` | id | the session it was taken during, if any |
| `position` | id | the position's place in Schwab's answer, from 0 |
| `security_key` | id | its security |
| `asset_type` | id | its asset type |
| `symbol` | id | its symbol |
| `cusip` | id | its CUSIP |
| `description` | free | its description |
| `long_micros` | id | the long quantity |
| `short_micros` | id | the short quantity |
| `settled_long_micros` | id | the settled long quantity |
| `settled_short_micros` | id | the settled short quantity |
| `average_price` | id | Schwab's average price |
| `market_value` | id | its market value |
| `long_open_pl` | id | Schwab's open profit or loss of a long position |
| `short_open_pl` | id | Schwab's open profit or loss of a short position |
| `cost_basis` | id | its cost: market value less open profit, or for a short position its proceeds |
| `maintenance` | id | Schwab's maintenance requirement |
| `day_pl` | id | Schwab's profit or loss of the day, which is not to be trusted (see [the API notes](schwab-api.md#positions-and-balances)) |

### `sessions`

One row per NYSE session in the span of the snapshots.

| Column | Kind | What it holds |
| --- | --- | --- |
| `date` | id | the session's date |
| `open_at` | id | when it opened |
| `close_at` | id | when it closed |
| `early` | id | 1 for an early close |
| `verified` | id | 1 when its year has been checked against NYSE's published calendar |

### `snapshot_gaps`

One row per session between an account's first and last snapshot that no
snapshot taken after its close stands for.

| Column | Kind | What it holds |
| --- | --- | --- |
| `account_id` | id | the account |
| `account` | id | its masked number |
| `market_date` | id | the session |

### `coverages`

One row per run of days that a source covers for an account (see
[Coverage](#coverage)).

| Column | Kind | What it holds |
| --- | --- | --- |
| `account_id` | id | the account |
| `account` | id | its masked number |
| `source` | id | `api`, `api_file`, `file`, `file_unknown`, `file_filtered`, `realized` or `snapshot` |
| `from_date` | id | the first day |
| `to_date` | id | the last day |

### `files`

One row per imported file.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | `F…` |
| `account_id` | id | the account it is for; none for a `-json` file, whose rows name theirs |
| `account` | id | the account's masked number |
| `kind` | id | `history_csv`, `history_json`, `realized_csv` or `api_json` |
| `label` | free | its name, masked |
| `layout` | id | the layout it was read as |
| `imported_at` | id | when it was imported |
| `exported_at` | id | when schwab.com exported it, from its content or its name |
| `covers_from` | id | the first day it covers |
| `covers_to` | id | the last day it covers |
| `coverage_source` | id | where its range came from: `stated`, `user`, `rows` or, for a `-json` file, `api_rows` |
| `completeness` | id | `complete`, `filtered` or `unknown` |
| `complete_by` | id | what established it: `you`, `api` or `file` |
| `lacks_rows` | id | how many rows it lacks in a filter's shape |
| `row_count` | id | how many rows it holds |
| `forgotten` | id | 1 when it is set aside |
| `warnings` | id | its flags' codes |

### `realized_lots`

One row per closed lot of Realized Gain/Loss files, shown as the newest
export writes it.

| Column | Kind | What it holds |
| --- | --- | --- |
| `ref` | id | `L…` |
| `account_id` | id | its account |
| `account` | id | the account's masked number |
| `symbol` | id | Symbol |
| `name` | free | Name |
| `security_key` | id | its security |
| `closed_date` | id | Closed Date |
| `opened_date` | id | Opened Date, when it is a date |
| `opened_text` | id | Opened Date as written |
| `quantity_micros` | id | Quantity |
| `proceeds_cents` | id | Proceeds |
| `cost_basis_cents` | id | Cost Basis (CB) |
| `gain_cents` | id | Gain/Loss ($) |
| `lt_gain_cents` | id | Long Term Gain/Loss |
| `st_gain_cents` | id | Short Term Gain/Loss |
| `term` | id | Term |
| `wash_sale` | id | Wash Sale?, as 1 or 0 |
| `disallowed_loss_cents` | id | Disallowed Loss |
| `unadjusted_cost_cents` | id | Unadjusted Cost Basis |
| `txn_cost_basis_cents` | id | Transaction Cost Basis |
| `txn_gain_cents` | id | Total Transaction Gain/Loss ($) |
| `versions` | id | how many versions the exports show |
| `files` | id | how many files hold it |
| `flags` | id | its flags' codes |

### `flags`

One row per flag raised.

| Column | Kind | What it holds |
| --- | --- | --- |
| `code` | id | its code (see [Flags](#flags)) |
| `severity` | id | `attention` or `info` |
| `meaning` | id | what the code means |
| `detail` | free | what in particular |
| `subject_ref` | id | what it is on |
| `account_id` | id | that thing's account |

### `reads`

One row per read of Schwab's API, or of a `-json` file, without its error
text.

| Column | Kind | What it holds |
| --- | --- | --- |
| `read_id` | id | the read's number |
| `endpoint` | id | what it read, or `file` |
| `account_id` | id | the account read |
| `window_start` | id | the window's start, as sent |
| `window_end` | id | the window's end, as sent |
| `all_types` | id | 1 when it asked for every type |
| `symbol` | id | the symbol it asked for |
| `sent_at` | id | when the request was sent |
| `answered_at` | id | when the answer came |
| `outcome` | id | how it ended |
| `http_status` | id | the HTTP status |
| `row_count` | id | how many rows came back |
| `capped` | id | 1 when it came back with 3000 rows |
| `complete` | id | 1 when it can show a row gone |

## How the open questions stay open

The [open questions](ROADMAP.md#open-questions) do not block the store:
whichever way each falls, what it holds now stays right, and an answer is a
new version of the rules and a rebuild.

| Question | Why nothing is lost or wrong now |
| --- | --- |
| how far back the API serves; old windows refused or empty | every read and its outcome is kept; the API's coverage starts at the oldest row, not at an empty window |
| evening and overnight fills' trade date | time and trade date are kept as sent; matching allows a day either way |
| whether `symbol=` keeps dividends | a read with a symbol is never complete; a dividend's symbol is taken from after its `~` |
| whether account hashes are stable | the account number is the key; hashes are kept with their times |
| how expirations, assignments and corporate actions show up | such rows are `other`, flagged `awaits_probe`, with overrides; file actions are hints |
| whether rows change, go or come late | revisions with their raw JSON and changed paths; absences kept as evidence; lateness is first seen less time, over rows an API read saw first |
| how windows' bounds work | windows are stored as sent; the gone rule's margins are wide and versioned |
| cancellations: a status, a gone row or a reversing row | all three are kept apart: a revision, `gone` (not counted), a `reversal` pair |
| whether an export merges an order's fills | the matcher links one row to several fills (`fills`) or one to one |
| which date the API gives rows "as of" in an export | matching allows days either way |
| whether `ItemIssueId` is the API's instrument ID | each is a key of its own; they join only through symbols |
| export caps and empty files | warnings that clip coverage; counts unaffected |
| filtered exports | counts unaffected; completeness established, never assumed |
| TD Ameritrade-era rows in the API | they link if there; else they count from the file, flagged `tda_history` |
| quantities restated after splits | linked on amount and day, flagged `differs_from_api` |

## Rules for future migrations

- The schema's version is SQLite's `user_version`; migrations are numbered
  SQL files, each with an optional Go step, applied in one write after a
  backup beside the store.
- Evidence tables change only by adding a column, or by a copy that keeps
  every row. Derived tables may be dropped and made again, which makes the
  next open rebuild.
- A table that must be rebuilt follows SQLite's twelve steps, with foreign
  keys off on the writer connection outside the transaction and a
  `foreign_key_check` before the commit.
- A new version of the cut form may only merge runs of revisions, never
  split them: a run keeps only its first raw JSON.
- No downgrades: restore the backup made before the migration.

## Tests and their time

The store's tests run on copies of a store made once per test binary, each
in its own folder, in parallel; most of the derivation is a pure function
and is tested without SQLite. SQLite is about 20 to 40 times slower under
the race detector, so the tests are held to a budget on CI's 4-CPU runners,
under `-race`:

| Package | Budget | Measured (4 vCPU, `-race`, go1.26.0 and go1.27.3) |
| --- | --- | --- |
| `internal/history` | 45 s of wall time | 35 to 43 s |
| `cmd/schwabtx`, the store's commands' tests | 15 s, run one after another | about 15 s, of the package's 43 to 49 s |
| `internal/nytime`, `internal/nyse`, `internal/mask`, `internal/history/schwabfile` | 5 s together | 1.0 to 2.8 s each, 6.7 to 7.0 s together, run side by side |
| the whole `go test -race ./...` | no more than 20 s longer than before | (`internal/schwab`, the longest, takes 93 to 113 s) |

A test that grows past 3 s under `-race` is cut down, but for
`TestIncrementalEqualsRebuild`, the only check that each write derives what a
rebuild does, which runs in CI with 4 seeds of 8 operations under `-race`
(30 without). `make volume` runs it with 50 seeds of 30 operations, by
hand, and `TestVolume` (behind `-history.volume`): a year of History exports
of 3,000 rows a quarter and five years of snapshots, imported, rebuilt,
checked and exported (about 50 s, without `-race`).
