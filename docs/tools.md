# Portfolio Performance and Ghostfolio

`schwabtx export -format pp` and `-format ghostfolio` write the store's
ledger, each transaction once, as the files that
[Portfolio Performance](https://www.portfolio-performance.info) (PP) and
[Ghostfolio](https://ghostfol.io) import, to a new folder, with a
`manifest.json` that says what the export wrote, what it left out and what
it asks of you. This page says how to import them, step by step; [the
store's reference](store.md#exports-for-portfolio-performance-and-ghostfolio)
says what each export holds and why.

The files are written for PP 0.90.0 and Ghostfolio 3.99.0 and tested against
those versions' import code, as read: no export has yet been imported into a
running PP or Ghostfolio (see the [roadmap](ROADMAP.md#m2-full-transaction-history-stored-locally)).
Menu items, options and messages are quoted from each tool's English
labels.

The commands shown after a `$` are run in this page's order on the store of
the store's [walk-through](store.md#a-walk-through), after every command of
that page, so that the decision of PP's example below holds for
Ghostfolio's first import; every account number, amount and file in them is
made up.

## In both tools

- **A full export first, then `-since`.** The first import is a full export,
  made without `-since`. Every later export names, with `-since`, the folder
  of the export you imported last, and writes only what the tool lacks, with
  what to delete there first. Import each export once, before you make the
  next. Each goes to a new folder: `export` refuses a folder that holds an
  export already, and a `-since` folder from which another export beside it
  has gone on.
- **Every row from each account's first.** These formats refuse `-from`: a
  tool that lacks the rows that opened a position takes its later sale for a
  short. `-to` ends the export, by default on the day before today in New
  York, so that a day still in progress is never split between two exports.
- **One store per Schwab account.** The tools know each account by its label
  in the store, `A1 ...135`: export each Schwab account from one store only,
  and after making a store again from scratch, whose accounts may be numbered
  otherwise, start over in the tool (below).
- **Do not rename** the accounts that you make for an export, or that it
  makes, and in Ghostfolio its assets: the tools find them again by name.
- **A key that the export cannot place.** A History file's rows name a
  security by a key of the file, which often knows no CUSIP. When the store
  linked one of its rows to an API row as one transaction, the export takes
  the two keys for one security, unless their links reach securities of two
  CUSIPs. Otherwise, when securities of two CUSIPs have had its ticker (a
  company that took a ticker another gave up, or a security that kept its
  ticker under a new CUSIP), the export writes its rows as the one that had
  the ticker first, by the days of its rows under it, since a file's rows
  come before the API's, or, when its tickers are each of another, on their
  own. A key whose ticker another security gave up before it had it,
  delisted, renamed or merged away, is not taken for that one, whatever
  CUSIPs they know: two companies of one ticker at different times stay two.
  The days of the API's rows say when a security went by a ticker, but a
  History file names every row by the ticker of its export, an old row too,
  so a file's rows go by the exports that named them, whatever key the store
  holds them under: a security renamed or delisted between two of a file's
  exports, with no API row under its new symbol, gave its ticker up at the
  later one, and another key that had the ticker in between is taken for it,
  as one security. stderr and the manifest's `keys_unsure` name each such
  key, with a `schwabtx edit K:… -same K:…` for each security it may be
  (`apart` for one that had the ticker at another time). Run the one that is
  right, and the store joins the two keys: if that is not how the export
  wrote it, export again before you import, in a new folder (delete the one
  not imported); once imported, the next `-since` export lists what changed,
  to delete.

## Portfolio Performance

PP takes CSV files through its CSV import wizard. Per Schwab account, the
export writes an account file (`account-transactions-A1-135.csv`), of the
rows that move only cash, and a portfolio file
(`portfolio-transactions-A1-135.csv`), of every row that moves shares, when
the account has such rows; one `securities.csv`, of each security and of
snapshot prices for those that PP has no price feed for; and three import
configurations, `pp-securities.json`, `pp-account-transactions.json` and
`pp-portfolio-transactions.json`, which tell PP the columns, their formats
and the types' names, whatever PP's language and locale.

### The first import

1. **New, empty accounts.** PP knows a transaction it holds only by its type,
   day, amount, shares and security, so rows of these Schwab accounts that
   it holds already, from PP's own Schwab PDF importer (in PP 0.89.0 and
   later) or typed by hand, are not recognized, and would count twice.
   Delete them first, or import into new accounts.
2. **Export in full** to a new folder:

   ```
   $ schwabtx export -format pp -dir pp-1
   wrote 17 rows for Portfolio Performance to pp-1, up to 2026-11-13: 2 account files, 2 portfolio files, 9 securities, 6 prices
     A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
     A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
     A3 ...783: snapshot 2026-11-02 to 2026-11-10
   11 holdings differ from the latest snapshot (pp-1/manifest.json): positions opened before the store's rows, or splits left out; add them by hand, less the rows left out that you book by hand (docs/tools.md)
   2 accounts' cash in Portfolio Performance differs from their latest snapshots' (pp-1/manifest.json): cash held before the store's first row of each, or rows left out; book each difference, less the rows left out that you book by hand, as an opening deposit or withdrawal (docs/tools.md)
   1 account has snapshots and nothing in Portfolio Performance (pp-1/manifest.json lists it under holdings.not_checked): A3 ...783 has no row that this export or its chain writes or names; import its History files (docs/store.md) to have it there
   in Portfolio Performance, make a USD deposit account and a securities account for each account, named:
     A1 ...135: Schwab A1 ...135
     A2 ...398: Schwab A2 ...398
   import in this order: securities.csv, account-transactions-A1-135.csv, portfolio-transactions-A1-135.csv, account-transactions-A2-398.csv, portfolio-transactions-A2-398.csv
   next time: schwabtx export -format pp -since pp-1 -dir <a new folder>
   ```

   In PP, make for each account that it names a deposit account in USD and
   a securities account, each named exactly as printed (`Schwab A1 ...135`),
   before you import, and never rename them. PP finds accounts by name: a
   Cash or Securities Account that it cannot find falls back to the one that
   the review page shows, and a transfer's Offset Account that it cannot
   find goes to the account that the review page shows under "transfer to",
   the first of that currency by name, even the transfer's own account,
   without a word: check it before you finish. A PP whose base currency is
   not USD converts at its own rates. An account that the export does not
   name, as one with snapshots and no rows (`A3 ...783` here), gets no PP
   account, and step 4's checks pass it over until an export writes or
   names it: stderr names it, and the manifest lists it under
   `holdings.not_checked`. Import its History files (`schwabtx import`) to
   have it in PP.
3. **Import**, through PP's CSV import:
   - Once, in the wizard's configuration menu, **Import configuration...**
     each of the three `pp-*.json`. PP keeps them among its user-specific
     configurations, for every later export: `schwabtx securities v1`,
     `schwabtx account transactions v1` and
     `schwabtx portfolio transactions v1`.
   - Then each file in the order printed (the manifest's `files`):
     `securities.csv` first, then each account's account file and portfolio
     file. Choose its configuration, and on the review page pick the file's
     account (`Schwab A1 ...135` for `account-transactions-A1-135.csv`),
     which PP takes for a row whose account it cannot find.
   - On each review page, leave "Convert buy/sell transactions into
     inbound/outbound deliveries" and "Generate an additional withdrawal for
     dividend transactions" unticked, and "Import notes (if available)"
     ticked: each row's note names it (`schwabtx A1 ...135 T73508261950`),
     which the later exports' lists of what to delete repeat.
   - PP marks a row "Transaction does already exist" when it holds one of the
     same type, day, amount, shares and security, and leaves it unticked.
     **Tick exactly the rows that stderr and the manifest's `tick` list for
     that file**, and leave every other such row unticked: those are the
     second halves of transfers between your accounts, which came in with
     their first, as PP books both sides of a transfer at once, and, in a
     later export, rows that PP holds already under another ref, which
     stderr counts and the manifest lists under `same_as` (a file row now
     linked to its API row). A row listed is new although PP takes it for
     one it holds: a withdrawal of the day and amount of a transfer's, or a
     trade equal to one that PP holds. What you booked by hand counts too:
     no list knows of it, so a row that a later export brings, equal to an
     entry that you booked for it, would be marked and left out. Change or
     delete what you booked before you import the export that lists its
     difference as changed or gone (step 4).
4. **Negative shares.** After an import, PP opens a dialog that names each
   security whose shares fall below zero ("Security '...' in portfolio
   '...' has negative shares"). While a short position is open, a written
   option or a short sale, that is expected. Otherwise a position was opened
   before the store's rows: stderr says that holdings differ from the latest
   snapshot, and the manifest's `holdings` lists each, with the snapshot's
   shares and those that the export wrote, once for a security that the
   store holds under two keys (a History file's and the API's), as PP holds
   it. Import the History files that go back to the purchase
   (`schwabtx import`), and the next export writes the rows they add; or
   add the opening position in PP, a Delivery (Inbound) dated before its
   first row. A row left out that moved shares, such as a split, is in the
   difference too: book it by hand as step 5 says, and only the rest as the
   opening position.

   **Opening cash.** PP's deposit account holds the sum of its records, from
   the account's first row in the store, so it lacks the cash that the
   account held before, and what a row left out moved: stderr says that an
   account's cash differs from its latest snapshot's, and the manifest's
   `holdings.cash` lists each, with the snapshot's cash and what the records
   leave of the rows that it holds: the API's rows by their time, a History
   file's by their posted date, which an evening snapshot of that day holds.
   Import the History files that go back to the account's first deposit, and
   the next export writes the rows they add; or book the difference in PP as
   a Deposit, or a Withdrawal when the records leave more, dated before the
   account's first row, less the rows left out that you book by hand (step
   5), whose cash it holds too. The cash check compares with the snapshot
   that the holdings check does, and does not check an account whose
   latest snapshot holds no cash figure (`no_cash`: take another), nor a
   margin account with a margin or short balance at its latest snapshot
   (`margin`), as Schwab can report its cash as 0 with a debit or short
   positions, its cash then in other figures: stderr names each, with why,
   and the manifest lists it under `holdings.cash_not_checked`. Compare the
   cash that Schwab shows for it with the balance of its PP deposit
   account, and book the difference as above. An account with rows and no
   snapshot is checked by neither check: stderr names it, and the manifest
   lists it under `holdings.not_checked`; take one with `schwabtx
   snapshot`.

   **Later exports.** The two ways exclude each other: what you add or book
   by hand stands for rows that the store lacks, and once a later export
   writes them, it is to go. So each later export compares every difference,
   of shares or cash, with the one before it. One listed alike is marked
   `before`, and not counted again; one listed otherwise keeps the earlier
   difference, the snapshot's less the records', as `was`; and one that it
   no longer finds is listed under `holdings.gone`, or `holdings.cash_gone`
   for cash, with `was`. A difference changes when what the export writes,
   gives or leaves out does: History files imported, a row left out,
   uncounted or revised since, or one that the store lacked. stderr lists
   each that changed or went with its change, signed as the manifest's
   amounts are: change what you added or booked by hand for it by that
   change, less the rows left out that you book by hand, or delete it,
   before you import the export, as PP marks a new row equal to one that it
   holds as existing (step 3): an entry that you booked as the rows were,
   their day, amount and shares, would otherwise keep out the rows that it
   stood for. If you booked nothing for it, take its difference as a new
   one. An account whose cash is not checked keeps, as its `was`, the
   difference listed before, for the export that checks it again.
5. **What the manifest asks of you**: the rows left out, each with its
   reason and what to do, and the rows written with a caveat
   ([below](#what-the-manifest-asks-of-you)). A row that PP refuses for its
   currency has reached a security that you keep in another currency under
   the same ticker, WKN or name: import it into a USD security by hand.

### Later imports

6. **With `-since`** naming the folder that you imported last, once you
   have imported it, and a new folder. Before importing, set each security
   that it lists under `symbols_changed` as step 8 says; delete in PP what
   it lists as changed or gone, each transfer once: deleting either half of
   a transfer in PP deletes both; and change or delete what you booked by
   hand for the differences that it lists (step 4). Each is listed with how
   its note begins; a transfer's note names its paying side first, in both
   accounts (`schwabtx A2 ...398 T73600000101 to A1 ...135 T73600000102`),
   and once you have paired transfers of one day again, perhaps another
   receiving side than the row's: delete the transfer whose note is quoted.
   A row listed as changed in the same form is one that PP holds under the
   note of a row that has come back since, such as a file row counted again
   beside the API row that took its place: delete it, and the export writes
   both, each under its own note. Then import in the order printed, and tick
   exactly the rows it lists to tick. Say that you book the interest
   adjustment of the walk-through as a fee:

   ```
   $ schwabtx edit T73508261949 -category fee
   T73508261949 -category fee: recorded
     T73508261949: interest -> fee (yours; the rule said doi.interest)
   $ schwabtx export -format pp -since pp-1 -dir pp-2
   wrote 1 row for Portfolio Performance to pp-2, since pp-1, up to 2026-11-13: 1 account file, 1 security, 6 prices
     this assumes Portfolio Performance holds pp-1 and every export before it, each imported once, with what it listed deleted and ticked
     A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
     A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
     A3 ...783: snapshot 2026-11-02 to 2026-11-10
   1 account has snapshots and nothing in Portfolio Performance (pp-2/manifest.json lists it under holdings.not_checked): A3 ...783 has no row that this export or its chain writes or names; import its History files (docs/store.md) to have it there
   before importing, delete in Portfolio Performance what changed or went (pp-2/manifest.json lists each):
     Schwab A1 ...135: 2024-04-30 Interest 0.42 (note schwabtx A1 ...135 T73508261949): changed; the new form is in this export
   import in this order: securities.csv, account-transactions-A1-135.csv
   next time: schwabtx export -format pp -since pp-2 -dir <a new folder>
   ```

   Here you delete in PP the Interest of 0.42 of 2024-04-30 whose note is
   `schwabtx A1 ...135 T73508261949`, then import the `securities.csv` of
   `pp-2`, which holds the snapshot prices again, and its account file,
   which holds the row as a Fees Refund, as PP books a fee that pays you.
   The export writes what changed whatever its day: a row that came late or
   that you decided since, a journal paired once its other side arrived, a
   revision, a ref that changed. A row that you uncount after it was
   exported comes back under gone, to delete, and so does one that the
   store no longer holds or that a revision dates after `-to`, which an
   export up to its new day writes again: both are "not in this export",
   as the export reads no row after `-to` to tell them apart; a file row
   that waits for you comes in a later export once you decide it; the
   left-out rows that an earlier export listed are marked `before`, so
   that you are not asked twice, and so are its differences from the
   snapshots that stayed as they were, while those that changed or went
   are listed with their change (step 4).
   Never name an export that you have not imported: the next one would
   take for granted what it gave PP.

   A second full export of days that PP holds already is not safe: PP takes
   every changed row for a new one beside the old, and marks new rows equal
   to old ones as existing.

### Starting over

7. **A redo**, to start over, or when a chain broke (a store made again, a
   manifest lost, an export not imported before the next was made): delete
   in PP every transaction of every account of the export, then import a
   full export as in step 3, of the chain's `-account` when it has one (a
   `-since` export of such a chain prints the command), ticking its `tick`
   list, and name it in the next `-since`. PP keeps its securities when you
   delete their transactions: before you import, set each that the full
   export lists under `symbols_changed` as step 8 says. Every account at
   once, not some: PP checks a transfer against the other account's side,
   so a row that an account kept could mark a new row that no list names.
   Neither a changed symbol nor a bond's unit needs a redo.

   A store made again may number its accounts otherwise, and an account's
   name in PP follows its ref (`Schwab A1 ...135`): `schwabtx status` lists
   the new store's. Name the chain's account as the new store holds it, by
   the last digits of its number (`-account ...135`), or by its new ref when
   two accounts end alike, not by the old chain's `A1`, which may be another
   account now; and make in PP the accounts that the full export names, as
   in step 2, then delete those that it no longer names, once they are
   empty.

### Securities, options and bonds

8. PP finds a security by its ticker, then its WKN, then its name. The export
   writes the ticker in Yahoo's form (`BRK-B` for `BRK.B`), the CUSIP as
   WKN and the description seen first as name, but no ISIN; a new security
   gets PP's Yahoo feed when it has a ticker. PP keeps the ticker and the
   feed of a security that it holds, and the rows of one that it finds by
   its CUSIP, so stderr and the manifest's `symbols_changed` name each
   security that PP may hold otherwise than written, or that may no longer
   trade, whether or not the export writes a row of it: with `why`, and
   with every name that it has had and the source that named it (`names`).
   On a first import into empty accounts PP makes each from what is
   written, and none asks anything. With `-since`, and on a redo (PP keeps
   its securities when you delete their transactions), set each that PP
   holds before you import; PP does not hold one marked `new`, which no
   export of the chain wrote:

   - `renamed`: it has had two tickers, and the ticker written is its
     latest. Give the PP security that holds its rows that ticker, and its
     CUSIP as WKN if it has none (PP made it before the store knew the
     CUSIP): PP finds it by them, and else makes a second security of the
     new ticker.
   - `no_ticker`: written without the ticker that it has had, `because` it
     is `delisted`: its latest symbol is its CUSIP. Clear its ticker and
     its feed, as its old ticker may be another company's now.
   - `changed` (with `-since`): what PP finds it by changed, its ticker and
     WKN, or its name when it has neither, as when an older History file
     brings an earlier description of a security that PP finds by its name
     alone: give the PP security the identifiers written in
     `securities.csv`. Beside a ticker or a WKN, a name changes nothing that
     PP finds a security by, and nothing is listed for it.

   An option's shares are its contracts times its multiplier, under its OCC
   symbol (`XYZ250221P00040000`); an option that expired, was assigned or
   exercised is closed by a delivery of value 0. A bond's shares are its
   face value in dollars, and its prices are per dollar of face, about 1: a
   price feed that quotes bonds in percent of face would value it 100 times
   too high, so keep its prices from your snapshots. Options, bonds, and
   securities without a ticker or delisted get their prices from your
   snapshots, in `securities.csv`.

## Ghostfolio

Ghostfolio takes one JSON file, `ghostfolio.json`, in its web app. It needs
Ghostfolio 3.44.0 or later, whose import takes an account without the
`balance` that earlier versions require of it, or 3.45.0 or later on a
shared instance such as ghostfol.io, where the import finds an account by
its name only from that version on; and the YAHOO data source enabled on
the instance. Its accounts carry the balances of your snapshots, which are
Ghostfolio's cash: deposits, withdrawals and journals are no activities
there. A security with a plain ticker is a YAHOO asset, which Yahoo prices;
options, bonds, securities without a ticker or delisted, tickers that are
not plain ones of Yahoo's, and those you name with `-manual` are MANUAL
assets, priced from your snapshots, each named from what identifies it,
with its description in its comment.

### The first import

1. **Export in full** to a new folder:

   ```
   $ schwabtx export -format ghostfolio -dir gf-1
   wrote 13 activities for Ghostfolio to gf-1, up to 2026-11-13: 3 accounts with 12 balances, 6 manual assets (1 more with prices only), 5 symbols for Yahoo
     A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
     A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
     A3 ...783: snapshot 2026-11-02 to 2026-11-10
   4 deposits, withdrawals and journals are in the accounts' balances
   11 holdings differ from the latest snapshot (gf-1/manifest.json): positions opened before the store's rows, or splits left out; add them by hand, less the rows left out that you book by hand (docs/tools.md)
   2 symbols for Yahoo have no row since 2025-11-13 and are not held (gf-1/manifest.json lists them): Yahoo may no longer know them; look them up, and name those it lacks in one -manual
   Ghostfolio takes an account's balances only in the import that makes the account; later imports leave its cash as it is
   next time: schwabtx export -format ghostfolio -since gf-1 -dir <a new folder>
   ```

   Ghostfolio's import asks Yahoo about every YAHOO symbol of the file, and
   refuses the whole file at the first that Yahoo cannot resolve, naming one
   at a time. So first look up on Yahoo the symbols that stderr counts and
   the manifest lists under `ghostfolio.yahoo_idle`: those with no row for a
   year that no account holds, often a company acquired, renamed or
   delisted since you sold. If Yahoo lacks any, export again, in full to
   another new folder, with `-manual` naming them all, comma-separated.
   `-manual` refuses a symbol that no security in the store has had. Say
   that Yahoo lacked BND:

   ```
   $ schwabtx export -format ghostfolio -manual BDN -dir gf-2
   error: -manual names BDN, which no security in the store has
   $ schwabtx export -format ghostfolio -manual BND -dir gf-2
   wrote 13 activities for Ghostfolio to gf-2, up to 2026-11-13: 3 accounts with 12 balances, 7 manual assets (1 more with prices only), 4 symbols for Yahoo
     A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
     A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
     A3 ...783: snapshot 2026-11-02 to 2026-11-10
   4 deposits, withdrawals and journals are in the accounts' balances
   11 holdings differ from the latest snapshot (gf-2/manifest.json): positions opened before the store's rows, or splits left out; add them by hand, less the rows left out that you book by hand (docs/tools.md)
   1 symbol for Yahoo has no row since 2025-11-13 and is not held (gf-2/manifest.json lists it): Yahoo may no longer know it; look it up, and if Yahoo lacks it, name it in -manual
   Ghostfolio takes an account's balances only in the import that makes the account; later imports leave its cash as it is
   next time: schwabtx export -format ghostfolio -since gf-2 -dir <a new folder>
   ```

   Then, on Ghostfolio's Activities page (Portfolio, Activities), **Import
   Activities...** the `ghostfolio.json` of the last folder, into a
   Ghostfolio user that holds none of these accounts, and review the
   preview, which shows an activity that Ghostfolio holds as "This activity
   already exists.". If the import says that `activities.N.symbol` "cannot
   be resolved by the data source", export again, in full to another new
   folder, with `-manual` naming that symbol and those you named before, and
   import that one. A MANUAL asset loses Yahoo's price history in
   Ghostfolio's charts; later exports with `-since` keep the chain's
   `-manual` list.
2. **Cash.** The import that makes an account sets its balances, one for
   each market date of your snapshots; later imports leave them as they
   are, and no import moves cash. So each `-since` export prints every
   account's latest snapshot cash: set it in Ghostfolio when it shows
   another. Ghostfolio counts the first balance as money invested on its
   day, which skews its investment and its percentages until daily balances
   exist (milestone 4). Schwab can report a margin account's cash balance as
   0, with its cash in other figures; Ghostfolio's cash is then off.
3. **A file with nothing new.** The import dialog imports a file only when it
   holds a new activity: an export with none (prices only, or an account
   with snapshots and no rows) cannot be imported there, and stderr says so.
   Its accounts come, with every balance, with the next export that has an
   activity, or by a direct `POST /api/v1/import` of the file. The prices of
   an asset that Ghostfolio does not hold yet reach it only with an activity
   of the asset.
4. **Names.** On a shared instance such as ghostfol.io, where the export's
   ids may be another user's, Ghostfolio gives the accounts and assets new
   ids and finds them again by their names: do not rename them. An asset's
   name is made from what identifies the security: its ticker (`Security
   (VBR)`), which the store's rows of it from every source share, under
   every key that the store holds it by; an option's symbol (`XYZ
   2025-02-21 40 put (XYZ250221P00040000)`); the CUSIP of a bond, of a
   delisted security, whose ticker another company may have taken, and of
   one whose ticker a security of another CUSIP has had too; else its key
   in the store (`Security (i:205187341)`), as for a futures option. Its
   description is in its comment.
5. **Fees and taxes** are FEE activities of the assets `Schwab fees` and
   `Schwab taxes`, which carry a price of 1 so that Ghostfolio takes them off
   net performance; their refunds are interest of the same asset, which
   Ghostfolio counts as interest and not in performance, so a fee later
   refunded stays counted there. Interest is interest of `Schwab interest`.
   Ghostfolio's own **Import Dividends...** books dividends of its own beside
   these: do not use both.
6. **Positions opened before the store's rows**: as for PP (step 4); in
   Ghostfolio a later sale shows as a negative holding, and a dividend of it
   sets the portfolio's dividend yield to 0. Add the opening position as a
   BUY dated before its first row, of the asset that the manifest lists the
   difference under (`ghostfolio_asset`, which Ghostfolio shows by its
   symbol, or a MANUAL asset by its name), or import the History files back
   to the purchase and export again. The two exclude each other, as in PP:
   once a later export writes the purchase, stderr lists the difference as
   gone, and you delete the BUY, or as changed, and you change its quantity
   by the change, once you have imported the export (Ghostfolio takes no
   activity of an export for one added by hand, whose comment is not the
   export's). Ghostfolio ties the BUY to its asset, and does not follow a
   change of symbol: once an export holds the security under another
   asset, after a rename, a delisting or `-manual`, it lists the difference
   as changed even when it is the same, with the asset that it was under
   (`was_asset`), and stderr says to delete the BUY there and add it under
   the new one, which the import makes when it is a new MANUAL asset,
   changed by the change, if any.

### Later imports

7. **With `-since`**, as for PP, each export imported before the next is
   made. Delete in Ghostfolio what it lists as changed or gone (the
   activities page filters by account and date, and each activity's comment
   names its row), import the file, change, move or delete what you added by
   hand for the differences that it lists (step 6), and set the cash it
   prints. Say you withdraw the decision of PP's example:

   ```
   $ schwabtx edit T73508261949 -category auto
   T73508261949 -category auto: recorded
     T73508261949: fee -> interest (the rule doi.interest)
   $ schwabtx export -format ghostfolio -since gf-2 -dir gf-3
   wrote 1 activity for Ghostfolio to gf-3, since gf-2, up to 2026-11-13: 3 accounts with 12 balances, 1 manual asset (1 more with prices only)
     this assumes Ghostfolio holds gf-2 and every export before it, each imported once, with what it listed deleted
     A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
     A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
     A3 ...783: snapshot 2026-11-02 to 2026-11-10
   4 deposits, withdrawals and journals are in the accounts' balances
   -manual names BND, which nothing in this export uses
   before importing, delete in Ghostfolio what changed or went (gf-3/manifest.json lists each):
     Schwab A1 ...135: 2024-04-30 INTEREST 0.42 (comment schwabtx A1 ...135 T73508261949): changed; the new form is in this export
   Ghostfolio takes an account's balances only in the import that makes the account; later imports leave its cash as it is
   set each account's cash in Ghostfolio to its latest snapshot, which imports do not change:
     Schwab A1 ...135: 1532.17 (2026-11-10)
     Schwab A2 ...398: 5210.44 (2026-11-10)
   a redo of every account (docs/tools.md) imports a full export of this chain's selection: schwabtx export -format ghostfolio -manual BND -dir <a new folder>
   next time: schwabtx export -format ghostfolio -since gf-3 -dir <a new folder>
   ```

   Here you delete the INTEREST of 0.42 of 2024-04-30 whose comment is
   `schwabtx A1 ...135 T73508261949`, import the `ghostfolio.json` of
   `gf-3`, and set each account's cash where Ghostfolio shows another. The
   line on `-manual` names the chain's symbols that nothing in this export
   uses, here BND, whose activities Ghostfolio holds already: nothing to do.
   A security renamed, delisted or named by `-manual` since is another
   asset to Ghostfolio, which ties activities to their asset: the export
   lists the activities that it gave of it as changed, to delete, writes
   them under the new asset, and lists each of its differences that the
   export before listed with the asset that it was under, from which to
   move what you added for it by hand (step 6).
   A full export imported again over what Ghostfolio holds is not a redo:
   it says nothing of what changed or went, which stays; an activity whose
   values you changed in Ghostfolio, as a delivery's that the export wrote
   at 0 (`value_unknown`), is another than the export's, which comes in
   again beside it; and it lists every difference from the snapshots anew,
   those you added BUYs for too. Redo an account as below.
8. **Limits.** On ghostfol.io, which data sources are enabled and how many
   activities one import may hold are the deployment's settings: a dry run,
   which the dialog makes first, tells. An import over that limit ("Too many
   activities"), or over the instance's 10 MB limit on a request, is split:
   a first full export with an earlier `-to`, imported, then `-since`
   exports with later ones, each imported before the next is made. Each
   file carries every balance, and the first makes the accounts.

### Redoing an account

**A redo of one account**, to start over with it or to have Ghostfolio take
every balance again: delete the account's activities (filter the
activities by the account, then Delete Activities), the BUYs you added for
it among them, then the account; then make the next `-since` export, as
ever from the export you imported last, with `-redo` naming the account:

```
$ schwabtx export -format ghostfolio -since gf-3 -redo ...135 -dir gf-4
wrote 8 activities for Ghostfolio to gf-4, since gf-3, up to 2026-11-13: 3 accounts with 12 balances, 3 manual assets (1 more with prices only), 2 symbols for Yahoo
  this assumes Ghostfolio holds gf-3 and every export before it, each imported once, with what it listed deleted
  but not A1 ...135, which you deleted with its activities to redo it: this export writes it whole, with every balance, and lists anew what to do for it by hand
  A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
  A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
  A3 ...783: snapshot 2026-11-02 to 2026-11-10
4 deposits, withdrawals and journals are in the accounts' balances
6 holdings differ from the latest snapshot (gf-4/manifest.json): positions opened before the store's rows, or splits left out; add them by hand, less the rows left out that you book by hand (docs/tools.md)
1 symbol for Yahoo has no row since 2025-11-13 and is not held (gf-4/manifest.json lists it): Yahoo may no longer know it; look it up, and if Yahoo lacks it, name it in -manual
Ghostfolio takes an account's balances only in the import that makes the account; later imports leave its cash as it is
set each account's cash in Ghostfolio to its latest snapshot, which imports do not change:
  Schwab A1 ...135: 1532.17 (2026-11-10)
  Schwab A2 ...398: 5210.44 (2026-11-10)
a redo of every account (docs/tools.md) imports a full export of this chain's selection: schwabtx export -format ghostfolio -manual BND -dir <a new folder>
next time: schwabtx export -format ghostfolio -since gf-4 -dir <a new folder>
```

It writes every row of the account, as a full export would, under the
chain's `-manual`, and lists its rows left out and its differences from
the snapshots anew, as what you did by hand for them went with it; for the
other accounts it goes on from the chain as ever, and their activities
stay as Ghostfolio holds them, values you changed included. Import it,
which makes the account again with every balance, do again what it asks
of the account (add its opening positions, book its rows left out), and
name it in the next `-since`. When the account has no activity, the dialog
cannot import the file: send it by `POST /api/v1/import`, or set the
account's cash by hand.

**A redo of every account** of the chain: delete each, with its
activities, and import a full export of the chain's selection, which a
`-since` export prints as the full export that a redo of every account
imports (`gf-3` above): it names the chain's `-account`, when it has one,
and every symbol of its `-manual` (`chain.manual` in the last manifest),
which Ghostfolio holds as MANUAL assets; without `-manual`, the full
export writes their activities for Yahoo, which may not know them. Name
it in the next `-since`.

**After a store is made again**, no export of it goes on from one of the old
store (`-since` refuses it as another store's, and `-redo` goes with
`-since`), and its accounts may be numbered otherwise, their names and
Ghostfolio ids with them: redo every account of the chain. Delete each,
with its activities; import one full export of the new store, with the old
chain's `-manual` and, when the chain has an `-account`, that account as the
new store holds it: by the last digits of its number (`-account ...135` for
the old `A1 ...135`), or by its new ref, which `schwabtx status` lists, when
two accounts end alike, never by the old `A1`, which may name another
account now. The import makes the export's accounts again, with their
balances: every account, or the chain's one. Name it in the next `-since`.

## What the manifest asks of you

Each left-out row of the manifest's `left_out` has an `advice`, and a
`command` when one helps:

| Advice | Portfolio Performance | Ghostfolio |
| --- | --- | --- |
| `check` | look at the row with `schwabtx status REF`: the store holds no day or no exact amount for it, its cash has its quantity's sign, or its legs do not add up to its cash; settle it with `schwabtx edit`, or book it in PP by hand. A bond whose rows do not follow face value and prices per 100 waits for the live probes, which settle the unit that Schwab's API uses | the same, in Ghostfolio |
| `split` | PP's split wizard, for the security, unless the rows before the split hold the new quantities already: a file row's quantity is as of its `quantity_as_of`, the day of the History export that brought it, and an export made after the split restates it | an admin's split of the asset, or a BUY of the new shares at 0 |
| `corporate_action` | a Delivery (Outbound) of the old security and a Delivery (Inbound) of the new one, at the cost that you carry | a SELL of the old asset and a BUY of the new one at one price, or an admin's merge of the two |
| `set_category` | book the row with `schwabtx edit REF -category C`; the next export writes it | the same |
| `by_hand` | book it in PP by hand: a futures contract, an option whose multiplier is not a whole number or that the store holds with two, or a trade or move without a quantity; a Cash In Lieu keeps its cash with `schwabtx edit REF -category dividend` | the same, in Ghostfolio |
| `set_security` | give the row its security with `schwabtx edit REF -security K:…`; the next export writes it | the same |
| `set_cash` | | no balance carries the row yet: a later snapshot will; set the account's cash in Ghostfolio to that snapshot's, or take snapshots (`schwabtx snapshot`) of an account that has none |

A row with no advice (`sweep_pair`, `zero_amount`) needs nothing. Of the
rows written with a caveat, under `caveats`: set the value of a delivery
written at 0 (`value_unknown`) in the tool; check one valued from a snapshot
(`value_from_snapshot`); lower the cost basis of a `return_of_capital` by
hand if you track it. The rows written, given, combined or in the balances
that carry a flag that needs you are listed under `flagged`, among them
those that the store may count twice until you decide them; `schwabtx
status -review` says why each is flagged. If you uncount one, the next
`-since` export lists it to delete. The file rows that wait for you, under
`waiting`, come in a later export once you decide them.
