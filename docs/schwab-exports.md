# Schwab's exports

Notes on the files you can download from schwab.com that `schwabtx import`
reads into [the store](store.md): the History export, in CSV and JSON, and
the Realized Gain/Loss export's Lot Details. Schwab documents none of them.
What follows comes from real exports that other projects keep, read for
their structure only, from samples that keep Schwab's layout, and from the
parsers of tools that import them, listed under [sources](#sources). Checked
in November 2026. The project never signs in to schwab.com or downloads
these files itself (see the [roadmap's non-goals](ROADMAP.md#non-goals-for-now)):
you export them, and `schwabtx` reads them.

The API serves transactions only from when an account was enabled for
thinkorswim (see [the API notes](schwab-api.md#transactions)), so these
files are how older history gets into the store, and the Realized Gain/Loss
export is the only source of each closed lot's cost basis: the API has no
lots.

## How to export

**History.** On schwab.com, Accounts > History, the Transactions tab: pick
the account and a date range, leave the types and the symbol alone, and
Export. Choose JSON, which states its range and a total; CSV works too, and
the store counts a JSON and a CSV of one export once.

- **Every type, no symbol.** The page can filter the export by type (16 of
  them, from Trades to Visa Debit Card) and by symbol, and nothing in the
  file records a filter: a filtered export looks like a complete one. One
  real 2025 export held only a year's 34 dividend and reinvestment rows,
  where the full export of the same span had 220 more. Say so at import
  (`-all-types`, or `-filtered` for an export you did filter), else the
  store establishes it as it can ([completeness](store.md#completeness)).
- **Short ranges.** No Schwab source states a limit per export. Guides by
  third parties give 1,500 rows, with a file that comes back empty over it,
  or 10,000 rows, and at most two years per export; one author who looked
  for the 1,500-row limit in 2026 did not find it. Export ranges of under a
  year, under 1,500 rows, and check that no file comes back empty for a
  range that had activity. The store warns of an empty file, one of exactly
  1,500 or 10,000 rows, and one whose oldest row is long after its start,
  and does not take such a file's oldest days as covered.
- **How far back.** schwab.com is said to show four years of history; the
  only Schwab pages found that say so are about the history moved over from
  TD Ameritrade. The range drop-down offers "All".
- **One file per account.** The account is in the file's name only (its
  last three digits), not in its content.

**Realized Gain/Loss.** Accounts > History, the Realized Gain / Loss tab
(one guide gives Accounts > Cost Basis > Realized Gain/Loss): pick the
account and the range, show the lot details and Export. The file is
`XXXX1234_GainLoss_Realized_Details_20260706-104711.csv`: the account's last
four digits, and when it was exported.

**XML** is offered as a third format of the History export, but no sample of
it was found, so `schwabtx` refuses it; export JSON or CSV.

## The History export

### File names

`Individual_XXX123_Transactions_20250309-115444.json`: the account's
nickname, `XXX` and the last three digits of its number, and the time of the
export, `YYYYMMDD-HHMMSS`; `.csv` for the CSV. In 2020 the name held the
nickname only (`Brokerage_Transactions_20201115-180021.CSV`, with an upper
case extension). `schwabtx` takes the account's digits and the time of the
export from the name, keeps the name as Schwab wrote it, and masks only free
text in it that holds 8 digits or more.

### Layouts

| Years | First line | Rows | Last line |
| --- | --- | --- | --- |
| to 2021 | a banner, `"Transactions  for account XXXX-1234 as of 12/20/2014 21:16:49 ET"`: two spaces after "Transactions", maybe a nickname, a 24-hour time, and maybe ` From 01/01/2020 to 11/15/2020` | the header and each row end with a comma, an empty ninth cell | `Transactions Total,"","","","","","",-$1234.56`, its first cell and total not quoted |
| 2022 to mid 2023 | the same banner with a 12-hour time and the account as `...123` | eight cells | `"Transactions Total","","","","","","","-$1,234.56",` |
| late 2023 on | the header: `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` | eight cells, all quoted | none: the footer went in December 2023, a few months after the banner |
| JSON, 2024 on | `{"FromDate":"01/01/2026","ToDate":"03/31/2026","TotalTransactionsAmount":"$1,234.56","BrokerageTransactions":[...]}` | objects with the eight names as keys | |

- **Columns by name.** In January 2024 one user's export had Price before
  Quantity; every later sample has Quantity first. `schwabtx` binds columns
  by name and refuses a file that lacks one of the eight, naming it.
- **An empty export** in the 2020 layout was its banner, its header and one
  empty cell. What the current CSV gives for an empty range is not known;
  `schwabtx` reads a header alone as an empty export, and warns.
- **The current CSV states no range and no total.** Its coverage is what you
  give with `-from` and `-to`, else its rows' oldest to newest date, with a
  warning. A JSON export states its range, `FromDate` to `ToDate`, which
  bounded every row's posted date in the real files seen, and a total, which
  equalled the sum of the rows' Amounts in every one of them.
- **A JSON export and the CSV of the same export hold the same rows** in the
  same order: three real pairs, exported seconds apart, differed only in the
  CSV's Amounts lacking thousands separators. `schwabtx` counts them once.

### Bytes

- Every cell is quoted, and descriptions hold commas (`"Tfr BANK NAME, N,
  HOLDER NAME"`), so `schwabtx` reads the file as CSV, by RFC 4180, and again
  leniently, with a warning, when its quotes break the rules, as a file
  re-saved by a spreadsheet's can.
- The real exports of 2025 and 2026 are plain ASCII with LF line ends and no
  byte order mark. Samples from 2020 and 2022 end lines in CR LF, and one
  CSV of 2022 held a byte that is not UTF-8 (0xC9, É in Windows-1252).
  `schwabtx` passes over a byte order mark, takes either line end, and reads a
  file that is not UTF-8 as Windows-1252, with a warning.
- Actions were once padded with spaces to 20 characters (2014): every cell is
  trimmed.
- Rows come newest first by posted date. Rows of one date come in no stated
  order; a row's place in its file is kept only to tell identical rows apart.

### Dates

A row's Date is the day Schwab posted it, `MM/DD/YYYY`, or that and the day
it took effect, `MM/DD/YYYY as of MM/DD/YYYY`. In real exports 15 to 30 rows
a file carried an "as of", from 1 to 3 days before the posted date. It is
the event's day: the end of an interest period (Bank Interest always has
one), an option's expiration or assignment, a split's or a spin-off's
effective date, an equity award's vest, some dividends' payment. The API
books such an event on that day (an expiration's row has its time on the
expiration day), so `schwabtx` takes a row's day as its "as of" date when it
has one, else its posted date, and keeps both. Tools disagree on which date
to use: cgt-calc and schwab2pp take the posted date, cgt-tool and meridian
the "as of".

### Numbers

- **Amount** is net of fees and signed as the account's cash moves: `$1.23`
  in, `-$1234.56` out, the minus before the dollar sign. It compares with
  the API's `netAmount` as it is: on 66 trade rows of three real exports,
  Amount was minus quantity times price (times 100 for an option) less fees
  for a purchase, and that plus for a sale, within a cent.
- **Thousands separators**: none in the CSV's Amounts, on every Amount of
  $1,000 or more in the JSON, and on quantities of 1,000 or more in both
  (`"1,000"`). `schwabtx` strips `$` and `,`.
- **Parentheses** for a debit, `(100.65)`, are what one tool says Schwab
  writes for options; no file seen has one, but `schwabtx` reads them.
- **Blank** is an empty string, in the CSV and in the JSON alike.
- **Price** has up to 4 decimals. An option's price is per share, and its
  Amount uses 100 times it. A bond's price is per 100 of face value, and its
  Amount can differ from quantity times price over 100 by accrued interest:
  by 1.9% on one Treasury sale.
- **Fees & Comm** is one number, blank when there are none, already taken out
  of Amount.
- **Quantity** is unsigned on trades (a sale's is positive), signed on rows
  that move shares (a journal of shares out is negative), and fractional to 4
  decimals on reinvestments. `schwabtx` signs trades' quantities by the
  action. Whether a later export restates the quantities of rows before a
  split is not settled: sources disagree. `schwabtx` never matches on
  quantity alone, and flags a quantity that two exports give differently.

`schwabtx` reads every number exactly from its text: money to the cent and
quantities to the millionth.

### Symbols and descriptions

- **Symbols** are tickers, empty on cash rows, or 9-character CUSIPs for
  bonds, CDs and the old side of a corporate action. Options are written
  `XYZ 09/04/2026 17.50 P` (root, expiration, strike, put or call); the API
  writes the padded OCC form, `XYZ   260918C00050000`, so `schwabtx` compares
  options by their contract, not by text. One tool reads the OCC form from
  Schwab's files too; no file seen has it.
- **A ticker is as of the export**, not as of the row: a later export can
  name an old row by a renamed ticker. `schwabtx` does not count a
  transaction twice for it.
- **Option descriptions** read `PUT XYZ CORP $17.5 EXP 09/04/26`, the name cut
  to about 20 characters. **Security names** match the API's instrument
  descriptions in the samples seen. **Corporate actions' old rows** read
  `<OLD NAME> XXXREVERSE SPLIT EFF: MM/DD/YY` (or `XXXMANDATORY MERGER`,
  `XXXNAME CHANGE`), and matured bonds `...XXX**MATURED**`.
- **TD Ameritrade's history**, moved to Schwab, shows as rows such as
  `TDA TRAN - Bought to Cover 100 (XYZ) @12.3400` and `TDA TO CS&CO
  TRANSFER`, under generic actions.
- **Descriptions hold personal data**: a linked bank's name and the account
  holder's (`Tfr BANK NAME, N, HOLDER NAME`), a payer's name, the last digits
  of another account (`JOURNAL TO ...123`), check numbers (`SCHWAB ONE CHECK
  123456`). The store masks every run of 8 digits or more in them, and keeps
  the rest as written (see [privacy](store.md#privacy)).

### The JSON export's extra keys

- **`ItemIssueId`**, on every row of the real 2025 exports: `"0"` on rows
  without a symbol, otherwise an id of 8 to 10 digits, the same for one
  security across files, an option's its own. It follows a security across a
  change of symbol (a spin-off's row under a CUSIP and a later purchase under
  the ticker share one). The web site's own requests use the name for
  Schwab's security id, so it is a better key than the symbol. Whether it
  equals the API's `instrumentId` is not known: their lengths overlap. The
  store keeps it as a key of its own (`x:`) and reports, in `status`, how
  often the two agree where a security has both.
- **`AcctgRuleCd`**, `"1"` or `"2"`: in one margin account "1" was on bank
  interest, transfers, journals and some option trades, "2" on most trades
  and dividends. Perhaps the cash or margin sub-account; not known. Kept,
  unread.
- **`TotalFeesAndCommAmount`** is in samples written by hand in 2026, not in
  the real 2025 files. The 2024 documentation of the export has neither of
  the two row keys. Every key beyond the eight is optional.

## The Realized Gain/Loss export

The Lot Details layout, stable since at least 2018:

- **A title line**, its first cell holding everything and 24 empty cells
  after it: `"Realized Gain/Loss - Lot Details for Individual XXXX-1234 as of
  Mon Jul 06  10:47:11 EDT 2026 from 01/01/2026 to 07/06/2026"`. The time is
  Java's format, with one space or two before it. The range is the file's
  coverage.
- **A header of 25 columns**: Symbol, Name, Closed Date, Opened Date,
  Quantity, Proceeds Per Share, Cost Per Share, Proceeds, Cost Basis (CB),
  Gain/Loss ($), Gain/Loss (%), Long Term Gain/Loss, Short Term Gain/Loss,
  Term, Unadjusted Cost Basis, Wash Sale?, Disallowed Loss, Transaction
  Closed Date, Transaction Cost Basis, Total Transaction Gain/Loss ($), Total
  Transaction Gain/Loss (%), LT Transaction Gain/Loss ($), LT Transaction
  Gain/Loss (%), ST Transaction Gain/Loss ($), ST Transaction Gain/Loss (%).
  Until at least 2020 `Cost Basis (CB)` was `Cost Basis`; `schwabtx` reads
  both.
- **One row per closed lot**, and no total row in a real file of 2026. An
  empty export is the title and the header.
- **Values**: dates `MM/DD/YYYY`, though Opened Date can be something else
  for a lot without history, which `schwabtx` keeps as text; percentages
  with up to 17 decimals, which `schwabtx` keeps as written; Term `Short
  Term` or `Long Term`; Wash Sale? `Yes`, `No` or blank; money cells can
  hold `--`, `N/A` or `Missing` for a lot without history, such as a gift or
  a transfer in, which `schwabtx` keeps and flags.
- **Relations** on a real file's 44 lots: Gain/Loss ($) was Proceeds less
  Cost Basis on lots without a wash sale, and that plus the Disallowed Loss
  (so 0) on the wash sales; Cost Basis equalled Unadjusted Cost Basis; the
  Transaction columns were filled on 17 lots only, where Total Transaction
  Gain/Loss was Proceeds less Transaction Cost Basis. Another tool's sample
  has a wash sale whose gain is Proceeds less Cost Basis, so `schwabtx`
  stores each column as written, derives none, and flags a lot whose gain
  fits neither.
- **Fees.** In one sample a lot's Proceeds equalled its sale's Amount and its
  Cost Basis its purchase's: net of fees, and with them. To be checked on
  your own files.
- **Versions.** Wash-sale processing changes a lot's cost basis and gain
  after a later purchase, so two exports can show one lot two ways. The store
  knows a lot by what that does not change (its closed date, opened date,
  quantity and proceeds), counts it once, and shows the newest export's
  version, flagged `lot_revised`.
- **The summary layout**, an older export with 22 columns and a Total row,
  and a current one whose header is not known, have no lots; `schwabtx`
  refuses the one it knows and asks for Lot Details.

## Other files

`schwabtx` refuses Schwab's other exports by what they are, so that a wrong
file is never taken for history: the Positions export, the Lot Details of an
open position (the only export seen that gives an open lot's time of
purchase), the Realized Gain/Loss summary, Equity Awards exports in CSV and
JSON, Schwab Bank's checking exports in CSV and JSON, and the XML History
export. An empty file is refused too, with a note that an export over a
limit is reported to come back empty.

## The actions

No list of History's actions exists. These are the ones that the research
found, in real exports, in samples or in other tools' parsers, as
`schwabtx` reads them: each action's family (what it does to cash or
holdings, which the matching and counting use) and the category it is booked
as. Actions are compared without case, spaces or punctuation, so `Non-Qual
Div` and `Non Qual Div` are one. A row of an action not here is kept, booked
`other` and flagged `unknown_action`; please report its name.

| Family | Category | Actions |
| --- | --- | --- |
| `buy` | `buy` | `Buy`, `Buy to Open`, `Buy to Close`, `Buy to Cover`, `Reinvest Shares` |
| `sell` | `sell` | `Sell`, `Sell Short`, `Sell to Open`, `Sell to Close` |
| `proceeds` | `sell` | `Cash In Lieu`, `Cash Merger`, `Full Redemption Adj` |
| `income` | `dividend` | `Qualified Dividend`, `Cash Dividend`, `Non-Qualified Div`, `Special Dividend`, `Special Qual Div`, `Special Non Qual Div`, `Pr Yr Cash Div`, `Pr Yr Special Div`, `Pr Yr Non-Qual Div`, `Reinvest Dividend`, `Qual Div Reinvest`, `Pr Yr Div Reinvest`, `Long Term Cap Gain`, `Short Term Cap Gain`, `Long Term Cap Gain Reinvest`, `Short Term Cap Gain Reinvest`, `Div Adjustment`, `Qual Div Reinvest Adj`, `Reinvestment Adj`, `Return Of Capital` |
| `income` | `interest` | `Credit Interest`, `Bank Interest`, `Bond Interest`, `Margin Interest`, `Interest Adj` |
| `tax` | `tax` | `NRA Tax Adj`, `NRA Withholding`, `NRA Withhold`, `Pr Yr NRA Tax`, `Foreign Tax Paid`, `Foreign Tax Reclaim`, `Foreign Tax Reclaim Adj`, `IRS Withhold Adj` |
| `fee` | `fee` | `ADR Mgmt Fee`, `Service Fee`, `Advisor Fee`, `Wire Fee`, `Auto S1 Fee` |
| `cash` | by its sign | `MoneyLink Transfer`, `MoneyLink Deposit`, `MoneyLink Adj`, `Wire Sent`, `Wire Funds`, `Wire Received`, `Wire Funds Received`, `Wire Funds Adj`, `Funds Received`, `Funds Paid`, `Returned Check`, `Auto S1 Debit`, `Auto S1 Credit`, `Visa Purchase`, `Bank Transfer` |
| `journal` | by its sign | `Journal`, `Internal Transfer` |
| `sweep` | `other` | `Futures MM Sweep` |
| `shares` | by its sign | `Journaled Shares`, `Security Transfer`, `Stock Plan Activity`, `Received` |
| `holding` | `other` | `Stock Split`, `Reverse Split`, `Stock Merger`, `Name Change`, `Conversion`, `Spin-off`, `Stock Div Dist`, `Expired`, `Assigned`, `Exchange or Exercise`, `Exercised`, `Options Frwd Split`, `Options Frwd Split Adj`, `Cash Merger Adj`, `Full Redemption` |
| `reversal` | by its sign | `Cancel Buy`, `Cancel Sell` |
| `other` | `other` | `Misc Cash Entry`, `Adjustment`, `Misc Credits`, `Promotional Award`, `Schwab Sub Inc Cr` |

- **By its sign**: cash and journals are a deposit when the Amount is
  positive and a withdrawal when it is negative (`other` at 0.00, flagged); a
  journal between two of your accounts pairs with its counterpart and is
  booked `journal`. Shares in are `transfer_in` and out `transfer_out`. A
  Journal, Internal Transfer or Security Transfer with a quantity moves
  shares, and without one moves cash. Cancel Buy is booked `buy` and Cancel
  Sell `sell`, each with the opposite sign to the trade it cancels, which it
  pairs with.
- **Two rows, one event.** A cash merger is two rows of one day, symbol and
  description: Cash Merger with the cash, then Cash Merger Adj with the
  shares; a bond's maturity likewise, Full Redemption Adj then Full
  Redemption. A reverse split, a merger, a name change and a conversion are a
  row of new shares under the ticker and a row of old ones under the old
  CUSIP. An assignment is the option's row and a separate Buy or Sell of the
  stock, posted at settlement. Such rows are flagged `two_row_event`, and the
  matcher can link several rows of a day to one API transaction.
- **Reinvestments.** A reinvested dividend is two rows (the dividend and
  Reinvest Shares), and three for a nonresident (with NRA Tax Adj), on both
  sides. The History export can date Reinvest Shares up to two trading days
  after its dividend, where the API dates both on the dividend's day: the
  matcher allows five days.
- `Return Of Capital` is booked as a dividend, flagged; `Expired` gives a
  long position a negative quantity and a short one a positive quantity.

## What the import checks

Each check is a flag on the file, which the import prints as a warning
(`warning: code: detail`), and `status F…` shows; [the store's flags](store.md#flags)
lists them all. None stops an import, but a header without one of the
columns and a JSON export that cannot be read:

- the rows' total against the stated total (`total_mismatch`), rows after a
  CSV's total (`rows_after_total`);
- an empty file (`empty`), exactly 1,500 or 10,000 rows (`at_cap`), an
  oldest row more than a week after the stated start (`late_start`), a range
  of more than two years whose oldest row is near the two-year mark
  (`two_years`); each narrows what the file is taken to cover
  (`coverage_clipped`);
- a range taken from the rows (`coverage_from_rows`), rows outside the stated
  range (`rows_outside_coverage`), rows without a date (`undated_rows`);
- Windows-1252 (`encoding_fallback`), quotes read leniently (`lazy_quotes`),
  actions not known (`unknown_actions`);
- digits in the name that are not the account's (`name_mismatch`): an error
  without `-force`;
- every row of one family or one symbol over a long span (`looks_filtered`),
  rows lacking that other sources hold, in a filter's shape (`lacks_rows`),
  and a completeness that nothing has established yet
  (`completeness_unknown`);
- on Realized Gain/Loss files, a gain that fits neither relation
  (`columns_disagree`), placeholders (`placeholder`), and a lot that exports
  show two ways (`lot_revised`).

## What is not known

- The XML export's shape, and what the current CSV gives for an empty range.
- Each export's limits, and what a cut export looks like: empty, or the
  newest rows.
- Whether an export merges an order's partial fills into one row.
- Which day the API gives rows that are "as of" in an export, and how an
  export's reinvestment dates relate to the API's.
- How the API shows assignments, exercises, corporate actions, transfers in
  kind, equity awards, cash in lieu, cash mergers, redemptions,
  cancellations, sweeps, bank and margin interest and fees; expirations have
  one example, from another project.
- Whether an export restates quantities after a split.
- What `ItemIssueId` is to the API's `instrumentId`, and what `AcctgRuleCd`
  means.
- The meanings of Unadjusted Cost Basis, of the Transaction columns, of a
  Stock Split row's Price, of "Schwab Sub Inc Cr", "Auto S1" and "Received",
  and the signs that Cancel Buy and Cancel Sell rows are written with.
- Whether real exports ever write options in the OCC form, or amounts in
  parentheses.

The store's rules hold whichever way each falls (see [the
store](store.md#how-the-open-questions-stay-open)). One export of yours
would settle most of them: export one window as JSON and as CSV, every type
and no symbol, for dates that `schwabtx transactions -json` also covers, and
import all three. The import's report says how many rows link to API rows
and how many are held out, and `status -review` and `export -what entries`
then show, without account numbers, which actions and which kinds of API
row stay apart, and on which days.

## Sources

Real exports, read for their structure only (no value from them is in this
project):

- [RobbyMo81/th-analysis](https://github.com/RobbyMo81/th-analysis): three
  JSON and four CSV History exports of one account, of September 2025
- [redbean87/expo-trading-journal](https://github.com/redbean87/expo-trading-journal):
  a Realized Gain/Loss Lot Details export of July 2026
- [webclinic017/small_construct](https://github.com/webclinic017/small_construct):
  Realized Gain/Loss exports of 2020, re-saved

Samples that keep Schwab's layout, and the parsers of tools that read the
files:

- [cgt-calc](https://github.com/KapJI/capital-gains-calculator)'s Schwab
  parser and its history: the layouts' changes (commits 5c18196, 828c83a,
  d1864b5; issues #407, #451, #464; PRs #459, #1012), identical rows,
  parentheses, Cancel Buy, assignments, bonds' prices
- [convert-csv-schwab2pp](https://github.com/rlan/convert-csv-schwab2pp): the
  2022 layout, a file that is not UTF-8 (#11), the export without its banner
  (#16)
- [beancount_reds_importers](https://github.com/redstreet/beancount_reds_importers)
  and [beancount-import](https://github.com/jbms/beancount-import): the
  layouts of 2014 to 2023, an empty export, Schwab Bank's exports
- [Export-To-Ghostfolio](https://github.com/dickwolff/Export-To-Ghostfolio):
  a screenshot of the History page, corporate actions, TD Ameritrade's rows
- [opensteuerauszug](https://github.com/vroonhof/opensteuerauszug): the JSON
  export's documentation, file names, Positions and Equity Awards exports
- [stonks](https://github.com/leedenison/stonks): the current CSV header, the
  JSON and CSV of one export, split restatements
- [offshiftoptions](https://github.com/matthewtmonk-dot/offshiftoptions): the
  current CSV checked against a real export in 2026, the API's expiration row
- [pacioli](https://github.com/PierreRochard/pacioli): the Realized
  Gain/Loss title's range, Positions exports
- [net_alpha](https://github.com/chen-star/net_alpha): Realized Gain/Loss
  placeholders and wash sales
- [laramiel/golang_txf](https://github.com/laramiel/golang_txf): the Realized
  Gain/Loss columns of 2018
- [capital-gains-uk-101](https://github.com/coffee-cpu/capital-gains-uk-101):
  an options lifecycle with "as of" dates
- [itsjafer/schwab-api](https://github.com/itsjafer/schwab-api) and
  [finance-dl](https://github.com/jbms/finance-dl): the web site's export
  requests, its type filters and `ItemIssueId`
- [ofxstatement-schwab-json](https://github.com/edwagner/ofxstatement-schwab-json),
  [cgt-tool](https://github.com/velikodniy/cgt-tool),
  [decaf](https://github.com/vjt/decaf),
  [Schwab2ofx](https://github.com/ewmailing/Schwab2ofx),
  [nvst](https://github.com/fengb/nvst) and
  [tradetally](https://github.com/GeneBO98/tradetally): actions and their
  signs
- [z-Wind/schwab_api](https://github.com/z-Wind/schwab_api): the API's side
  of reinvested dividends and security names
- Guides known only through a search engine's summary, since their sites
  could not be reached: TraderFyles and a tax preparer (1,500 rows, an empty
  file over it), TradeLog and TickerScribe (10,000 rows), JournalPlus (two
  years per export), and Schwab's TD Ameritrade transition pages (four
  years of history)
