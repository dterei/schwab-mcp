# Notes on the Schwab Trader API

Working notes on the parts of Schwab's API this project uses or plans to use.
The authority is <https://developer.schwab.com>. These notes come from:

- Schwab's OpenAPI specs, as captured from the developer portal in 2024
  (Accounts and Trading of 2024-05-11, Market Data of 2024-03-21), with copies in
  [major/schwab-go](https://github.com/major/schwab-go/tree/main/docs);
- Schwab's Streamer Guide of 2024-05-15, as redistributed by
  [Schwabdev](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages/pdfs/Schwab_Trader_API_Streamer_Guide.pdf);
- community clients, mainly [schwab-py](https://github.com/alexgolec/schwab-py)
  and [Schwabdev](https://github.com/tylerebowers/Schwabdev), for live
  behavior the specs get wrong;
- real answers that other projects keep, anonymized or scrubbed, and fixes
  they made after live failures (listed under [sources](#sources)), for the
  shapes of accounts, positions, balances, transactions and preferences.

Checked in October 2026, and for the account endpoints again in November
2026. Where live behavior is known to differ from the spec, it says so.
Things nobody has pinned down are collected under
[open questions](ROADMAP.md#open-questions); most of them have a probe in
[the live mode](live.md#the-probes), named below where a note awaits one.

## Access and app setup

- **Individual tier** means an app you build to reach your own self-directed
  Schwab accounts. Giving an app to other account holders, free or paid,
  needs the Commercial tier.
- **Two API products**: Accounts and Trading Production, and Market Data
  Production. Add both; a call to a product the app lacks fails with 401
  "Client not authorized".
- The app must say **Ready For Use**; "Approved - Pending" does not work yet.
  Changing an approved app, even only its callbacks or products, sends it
  back to "Approved - Pending", typically for a few days (schwab-py's auth
  docs), so set up everything you will need at the start.
- **thinkorswim** must be enabled on each brokerage account. Schwabdev's
  setup guide says it is needed for orders and other calls. Schwab's Trader
  API team, as relayed in a July 2026 comment on
  [ssandy33/regress#125](https://github.com/ssandy33/regress/issues/125),
  says that without it an account has no transaction or order history
  through the API. The reported symptom is not an error: transactions come
  back empty with HTTP 200 while positions still load. See
  [transactions](#transactions).
- The **order limit**, 0 to 120 order requests a minute for each account, is
  set when you register the app (see [rate limits](#rate-limits)). schwab-py
  recommends 120.
- **No money movement.** The API has no transfer endpoints. Orders are the
  only writes.
- App key and secret lengths have changed without notice before, so do not
  validate them by length.

## Base URLs

| What | URL |
| --- | --- |
| OAuth | `https://api.schwabapi.com/v1/oauth/authorize`, `.../v1/oauth/token` |
| Accounts and Trading | `https://api.schwabapi.com/trader/v1` |
| Market Data | `https://api.schwabapi.com/marketdata/v1` |
| Streamer | `streamerInfo[0].streamerSocketUrl` from `GET /userPreference`, e.g. `wss://streamer-api.schwab.com/ws` |

## OAuth

- **Login** is the authorization code flow:
  `authorize?response_type=code&client_id=<app key>&redirect_uri=<callback>`.
  The authorize URL in Schwab's own spec also carries `scope=readonly`.
  schwab-py, Schwabdev and this project's CLI send no scope;
  sudowealth/schwab-api sends `api offline_access`; ssandy33/regress has sent
  `scope=readonly` since May 2026 without reporting any effect. Whether
  `readonly` limits the token is unknown: [probe Q6](live.md#the-probes)
  reports the scope Schwab grants, but only an order could show that a
  token cannot trade.
- **The code** comes back URL-encoded in the redirect, must be decoded, and
  expires in about 30 seconds.
- **Token requests** are `POST /v1/oauth/token` with HTTP Basic auth (app key
  and secret) and a form body of either
  `grant_type=authorization_code&code=...&redirect_uri=...` or
  `grant_type=refresh_token&refresh_token=...`. The response has
  `access_token`, `refresh_token`, `id_token`, `expires_in` (1800),
  `token_type` and `scope` (`api` in the samples community clients test
  against).
- **Lifetimes**: access tokens last 30 minutes (`expires_in` has been 1800 in
  every response reported). A refresh token works for 7 days from the browser
  login, and refreshing does not extend that. A refresh gives back the same
  refresh token, or at least never ends the old one: community clients send
  the login's original refresh token all week, though none has compared the
  token that comes back with the one sent ([probe Q15](live.md#the-probes)).
  It can stop sooner, after a later login (below). schwab-py forces a new
  login at 6.5 days and jkoelker/schwab-mcp at 5.
- **Callback URLs** must be `https`, match the registered value exactly
  (a trailing slash difference gives an "Access Denied" page) and use
  `127.0.0.1`, not `localhost`. An app may register several, comma-separated.
  A callback without a port means port 443, which a local listener normally
  cannot use on Linux without root, so catching the redirect automatically
  needs a registered callback with a port above 1023; schwab-py uses
  `https://127.0.0.1:8182`.
- **One login at a time.** A new browser login with the same app ends the
  refresh token of every earlier login ("There can only be one valid refresh
  token at a time", Schwabdev's maintainer in
  [Schwabdev#40](https://github.com/tylerebowers/Schwabdev/issues/40)). So
  processes that use one login share its token file and re-read it before they
  refresh. Whether a refresh ends older access tokens is not known
  ([probe Q15](live.md#the-probes)). schwab-py's warning that two programs
  sharing a token file lock each other out was copied from its TD Ameritrade
  predecessor, whose refresh tokens did change. Schwabdev serializes
  refreshes with a database lock.
- **A refused refresh token** gets HTTP 400 with `unsupported_token_type`. The
  real reason is nested, as JSON, in `error_description`: `invalid_grant`,
  "Refresh token is invalid, expired or revoked", in 2026 reports, and
  `refresh_token_authentication_error`, with a digest of the token, in 2024
  ones. An older schwab-py document shows `invalid_client`, "refresh token
  invalid". A refused token stays refused until a new login, so sending it
  again only adds load: one daemon sent two token requests a second for 16
  hours. A wrong app key or secret gets `invalid_client`, "Unauthorized".
- **Revocation**: an unreleased schwab-py branch posts to `/v1/oauth/revoke`.
  Not documented by Schwab as far as we know.

## Accounts and Trading

Every account-scoped path takes the account's **hash** from
`/accounts/accountNumbers`, although the spec names the parameter
`accountNumber`. A plain account number there gets 400
([ffainvestments](https://github.com/archiecrawford/ffainvestments/blob/main/docs/FFA_RECONSTRUCTION_GUIDE.md))
with `{"message": "Invalid account number"}`
([schwab-py#203](https://github.com/alexgolec/schwab-py/issues/203)).

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/accounts/accountNumbers` | `[{accountNumber, hashValue}]`; see [account numbers](#user-preferences-and-account-numbers) |
| GET | `/accounts` | Every linked account. Add `fields=positions` for holdings (the only documented `fields` value). See [positions and balances](#positions-and-balances). |
| GET | `/accounts/{hash}` | One account, same shape |
| GET | `/accounts/{hash}/orders` | `fromEnteredTime` and `toEnteredTime` required, `maxResults` (default 3000), `status`. The spec says at most a year; schwab-py says 60 days ([probe Q5](live.md#the-probes)). |
| POST | `/accounts/{hash}/orders` | 201 with an empty body; the new order's URL, ending in its ID, is in the `Location` header |
| GET, PUT, DELETE | `/accounts/{hash}/orders/{orderId}` | Get, replace or cancel. A replace cancels the order and creates a new one, with a new `Location`. |
| GET | `/orders` | Orders across all accounts; `fromEnteredTime` must be within 60 days |
| POST | `/accounts/{hash}/previewOrder` | `orderStrategy`, `orderValidationResult` (`accepts`, `alerts`, `rejects`, `reviews`, `warns`) and `commissionAndFee`. [k1xme/schwab-mcp](https://github.com/k1xme/schwab-mcp) says it returns 404 on live accounts; other servers use it on live accounts. |
| GET | `/accounts/{hash}/transactions` | See [transactions](#transactions) |
| GET | `/accounts/{hash}/transactions/{id}` | One transaction by its `activityId`: an object, the same as its row in the list, though the spec says an array |
| GET | `/userPreference` | One object, though the spec says an array; see [user preferences](#user-preferences-and-account-numbers) |

## Transactions

The data behind the return calculators, so in more detail. The live findings
come from real answers that other projects keep (see [sources](#sources)):
one account's 2023-24 history, scrubbed captures of 2025 and 2026, and fixes
that followed live failures.

**Request.**

- `startDate`, `endDate` and `types` are required by the spec; `symbol` is
  optional. Live, the two dates must come together, and omitting `types`
  returned every type in August 2026
  ([piekstra/schwab-cli](https://github.com/piekstra/schwab-cli/blob/main/docs/api.md)),
  though in December 2025 a client fixed failures that it put down to a
  missing required parameter, and it now insists on `types`
  ([SchwabPy 2.0.2](https://github.com/jaycollett/schwabpy/releases/tag/2.0.2)).
  This project's client, and so its CLI, always sends `types`; only the live
  mode's extras ask Schwab for a window without it
  ([probes](live.md#the-probes)).
- Dates are instants in UTC, formatted `yyyy-MM-dd'T'HH:mm:ss.SSSZ`.
- The spec types `types` as a single value, but a comma-separated list
  works: schwab-py and this project's CLI send all 15 types that way.
- A `symbol` filter may leave out dividends and withholdings, which name
  their security only in their description (untested;
  [probe Q4](live.md#the-probes)).

**Limits.**

- At most **3000 transactions per response**, with **no paging**. A window
  with more came back with the newest 3000 and the oldest day cut part way
  (schwab-cli, August 2026, most likely asking for TRADE alone); how the cap
  meets a request for several types, as this project sends, is not known.
- At most **one year per request**.
- A window that comes back with 3000 rows may have been cut short, so split
  it and ask again.

**How far back.** Not documented ([probe Q1](live.md#the-probes)). The
one-year limit is per request, not a retention period.

- schwab-py's docs, jkoelker/schwab-mcp and TradeZella say 60 days. That
  probably comes from the wording for orders; schwab-cli got 121-day-old rows
  in full in 2026.
- LumiBot and other clients fetch history in 365-day windows.
- A July 2026 comment on
  [ssandy33/regress#125](https://github.com/ssandy33/regress/issues/125)
  quotes Schwab's Trader API team: once an account is thinkorswim enabled,
  transaction data can be retrieved "from that day forward".
- Windows reaching 13 months back, past an account's history, were answered
  with an empty list, not refused (schwab-cli, 2026). An empty window
  therefore does not prove there is no older history.
- schwab.com shows up to four years of history under Accounts > History, and
  ten years of statements and trade confirmations.
- Exports from that page are reportedly limited. Third-party guides give
  about 1,500 rows per file, with an empty file when a range has more, or
  10,000 rows, and at most two years per export. No Schwab source we found
  states a limit, so export short ranges and check that no file comes back
  empty.

**Boundaries.**

- Send exact instants. A date-only end date sent as midnight UTC drops that
  day's trades.
- Whether Schwab takes a window that ends days after the request is not
  known. The live mode's Q12 and Q13 send one, to take in evening and
  overnight fills dated a later trading day
  ([probes](live.md#the-probes)).
- What Schwab answers to a window whose start equals its end is not known.
  The one report that it returns nothing
  ([GeneBO98/tradetally#274](https://github.com/GeneBO98/tradetally/issues/274))
  reasoned from code that never sent such a request.
- Keep each window at most 365 × 24 hours from start to end. The spec says
  only "1 year", but one client says, without having tried it, that the cap
  counts hours, so a window from local midnight a year ago up to now would
  be a few hours too long
  ([Akiko0210/PersonalAssistant#25](https://github.com/Akiko0210/PersonalAssistant/pull/25)).
  z-Wind's live test of May 2024, to judge by its code, got rows for a
  window of 366 days, from 2023-05-01 to 2024-05-01
  ([z-Wind/schwab_api](https://github.com/z-Wind/schwab_api/commit/2c760ad)).
- The live mode's extras ask Schwab for a window whose start equals its
  end, one of 365 days and a second, and one of 366 days
  ([probes](live.md#the-probes)).

**Response.**

- Every row has `activityId`, `time`, `accountNumber` (the plain number, as
  a string), `type`, `status`, `subAccount`, `tradeDate`, `netAmount` and
  `transferItems`. The rest depends on the kind of row:
  - an order execution (a TRADE with an `orderId`) has `orderId` and
    `positionId`, but no `description` and no `settlementDate`;
  - every other row has a `description` and a `settlementDate`; TRADE rows
    without an `orderId` (dividend reinvestments, "Sell Trade") have a
    `positionId`, and cash rows have neither id;
  - dividends gained a boolean `qualifiedDividend` by 2026, which 2024 rows
    lack, perhaps only in some accounts;
  - the spec's `activityType` and `user` have not been seen.
- `activityId` identifies the record. Every fill of an order shares its
  `orderId`, so a store keyed on `orderId` loses partial fills, as
  [strat-journal](https://github.com/alynnyree/strat-journal-backend) did.
  Ids are JSON integers of about 11 digits (activities) and 13 (orders).
- **Times.** `time` is an execution's fill, or when a cash row was posted.
  `tradeDate` is the fill again on an execution, and midnight in New York,
  written in UTC (`T04:00:00+0000` in summer, `T05:00:00+0000` in winter), on
  every other row; some executions carry the midnight as well. So a row's day
  is the New York date of its `tradeDate` on every row seen, a
  futures-option fill at 00:15 in New York included. A fill between 6 PM and
  midnight there (CME's evening session, or Schwab's overnight session from
  8 PM), which no sample shows, may be booked on the next trading day (see
  the roadmap's open questions and [probe Q2](live.md#the-probes)). Order
  rows by `time`: ordering by `tradeDate` puts sales before the purchases
  they close. Monthly interest is posted around 9 PM in New York on the last
  business day of the month, which is the next day in UTC. `settlementDate`
  is midnight in New York, and absent on executions. All are
  `2024-04-04T15:56:07+0000`: no fraction and an offset without a colon (see
  [encodings](#encodings)).
- **Legs.** Each transfer item has `instrument`, `amount` and `cost`, and
  some have `price`, `feeType` and `positionEffect` (OPENING or CLOSING).
  - On a trade, a security leg's `amount` is the signed quantity in shares
    or contracts (positive for a buy), `cost` the signed cash, and `price`
    the price per share; an option's price is its premium per share, to be
    multiplied by `optionPremiumMultiplier` (100, or 50 on options on E-mini
    S&P 500 futures).
  - A trade's other legs are CURRENCY_USD charges, with `amount` the charge,
    `cost` minus it, and no `price`. Order executions carry COMMISSION,
    SEC_FEE, OPT_REG_FEE and TAF_FEE legs even at 0. A futures-option fill
    carries eight, of which only COMMISSION and FUTURES_EXCHANGE_FEE name a
    `feeType`, and an unnamed one can hold a charge
    ([jwplatta/schwab_rb](https://github.com/jwplatta/schwab_rb)'s January
    2025 bodies). So the fees of a trade are minus the `cost` of its
    CURRENCY legs, named or not.
  - On every trade seen, `netAmount` is the sum of the legs' `cost`: the
    account's cash effect, positive in and negative out.
  - On other rows the CURRENCY leg carries the cash in `amount`, with `cost`
    and `price` 0.
  - Never add up `amount` across legs.
- **Instruments are reference data as of the request**, not the trade's.
  `closingPrice` is a price as of the request, not of the trade (whether a
  close or a live price is not known), so the same row reads differently on
  a later day, and a delisted equity comes back with its CUSIP as its symbol
  ([tradetally](https://github.com/GeneBO98/tradetally)). Compare a re-read
  row with a stored one with its instruments cut to `assetType` and
  `instrumentId`, never by its raw JSON. Transaction instruments carry
  `instrumentId` (1 for cash) and, beyond the spec, `closingPrice` and
  `status` (only ACTIVE seen), and no `cusip`. Options carry `expirationDate`
  (midnight in New York), `strikePrice`, `putCall`, `underlyingSymbol` and
  `optionDeliverables`, and, in a capture of August 2026 (not in fills of
  January 2025), also `uniformSymbol`, `underlyingCusip` and, in each
  deliverable, the delivered instrument.
- **Kinds of row**, as seen:
  - an ACH deposit came as ELECTRONIC_FUND "ACH Credit Received", not
    ACH_RECEIPT, and a wire as WIRE_IN "Wire Incoming";
  - monthly interest on cash comes as DIVIDEND_OR_INTEREST "Free Balance
    Interest Adjustment";
  - in an account with a W-8BEN (a nonresident), a reinvested dividend is
    three rows on one trade date: DIVIDEND_OR_INTEREST "Ordinary
    Dividend~SYM", JOURNAL "W-8 Withholding~SYM", negative, and TRADE
    "Dividend Reinvest". Refunds of withholding come back positive under the
    same description, and a withholding can come without its dividend in the
    same answer;
  - a dividend's or withholding's security is named only after the `~`,
    since its only leg is cash;
  - TRADE "Sell Trade" rows without an `orderId` seem to sell fractional
    remainders, priced at an execution's price rounded to cents;
  - sweep journals ("BANK SWEEP TO BROKERAGE" and the like) and "TRF FDS
    TO/FRM TYPE n" come in opposite-signed pairs within one account and are
    not deposits ([LumiBot](https://github.com/Lumiwealth/lumibot)).

  No public sample shows expirations, assignments, transfers in kind or
  corporate actions; they are probably RECEIVE_AND_DELIVER
  ([probe Q11](live.md#the-probes)).
- Rows are not in time order, even within one type: jwplatta's twelve TRADE
  fills of January 2025 come in no order, and z-Wind's history, grouped by
  type and newest first within each, may be one answer per type joined.
  Sort by `time`, then `activityId`.
- Fees are separate transfer items with instrument `CURRENCY_USD`. The spec
  lists COMMISSION, SEC_FEE, STR_FEE, R_FEE, CDSC_FEE, OPT_REG_FEE, TAF_FEE,
  INDEX_OPTION_FEE and others; COMMISSION, SEC_FEE, OPT_REG_FEE, TAF_FEE and
  FUTURES_EXCHANGE_FEE have been seen.
- The 15 types are TRADE, RECEIVE_AND_DELIVER, DIVIDEND_OR_INTEREST,
  ACH_RECEIPT, ACH_DISBURSEMENT, CASH_RECEIPT, CASH_DISBURSEMENT,
  ELECTRONIC_FUND, WIRE_OUT, WIRE_IN, JOURNAL, MEMORANDUM, MARGIN_CALL,
  MONEY_MARKET and SMA_ADJUSTMENT.
- Instrument asset types are EQUITY, OPTION, INDEX, MUTUAL_FUND,
  CASH_EQUIVALENT, FIXED_INCOME, CURRENCY, COLLECTIVE_INVESTMENT (ETFs),
  FOREX, FUTURE and PRODUCT.

## Positions and balances

- Each account is `securitiesAccount` and, beside it, `aggregatedBalance`:
  `currentLiquidationValue` and `liquidationValue` for that account, not a
  total, and in captures of August 2026 `intradayBuyingPowerAmount` and
  `currentIntradayBuyingPowerAmount`. Some captures from 2024 lack it.
- `securitiesAccount` has `type`, `accountNumber`, `roundTrips`,
  `isDayTrader`, `isClosingOnlyRestricted`, `pfcbFlag`, in captures of
  August 2026 `isPortfolioMargin` and, on margin accounts,
  `isIntradayMargin`, the balances `initialBalances`, `currentBalances` and
  `projectedBalances`, and, with `fields=positions`, `positions`. An account
  that holds nothing has no `positions` key, even then.
- `type` is only ever CASH or MARGIN. A retirement account is one of the two,
  and nothing in the API says which accounts are retirement accounts.

**Balances.**

- Which figures Schwab sends depends on the account's type and on the set,
  and matches none of the spec's four balance schemas. A cash account's
  current balances carry 13 figures that the spec's CashBalance lacks
  (`cashBalance`, `liquidationValue`, `longMarketValue`, `savings` and
  more). Projected balances hold 2 figures on a cash account and about 10 on
  a margin account, none of them totals. In captures of August 2026, margin
  accounts add `intradayBuyingPowerAmount`. `isInCall` is a boolean, though
  the spec says a number. Zero is common and real, so tell an absent figure
  from 0.
- **The account's value** is `currentBalances.liquidationValue`, as of the
  request: in the evening it follows extended-hours prices, and it moves
  again on Sunday evening, while `initialBalances.liquidationValue` (equal to
  `accountValue`) stays at the start of the session. A daily snapshot taken
  in the evening therefore does not hold the close. When initial balances
  move to the next session, and whether they then hold the close, is not
  known ([probe Q3](live.md#the-probes)). In current balances,
  `liquidationValue` equals `cashBalance + longMarketValue + mutualFundValue
  + longOptionMarketValue + shortOptionMarketValue + shortMarketValue +
  bondValue + moneyMarketFund + savings` to the cent in every sample, and
  `aggregatedBalance.currentLiquidationValue` in all but one.
- **Figures that are not what their names say:** a margin account's
  `equity` leaves options out; `initialBalances.mutualFundValue` repeats a
  cash figure; a margin account's `initialBalances.bondValue` is non-zero
  without bonds; and a cash account's `longNonMarginableMarketValue` is its
  cash. With short positions or a debit, `cashBalance` can read 0, with the
  cash in `marginBalance` and `shortBalance`
  ([jkoelker/schwab-mcp#222](https://github.com/jkoelker/schwab-mcp/pull/222)).
- **Money market funds** are positions of asset type MUTUAL_FUND (type
  NO_LOAD_TAXABLE), priced at 1 and counted in `mutualFundValue`.
  `moneyMarketFund` has been 0 in every sample, and CASH_EQUIVALENT has not
  been seen.

**Positions.**

- A position has quantities (long, short, settled, previous session, and on
  money market funds aged), `averagePrice` and long/short averages, tax-lot
  average prices, `marketValue`, `maintenanceRequirement`,
  `longOpenProfitLoss`, `shortOpenProfitLoss`, and current-day cost and
  profit/loss. It carries only its own side's figures: a long position lacks
  the short averages, `shortOpenProfitLoss` and
  `previousSessionShortQuantity`, and a short one the long ones.
  Quantities are fractional, and whole ones come as `1` or `1.0`.
- **Signs:** `shortQuantity` is positive, `settledShortQuantity` negative, a
  short position's `marketValue` negative, and both open profit/loss figures
  positive for a gain.
- `averagePrice` is the tax-lot average, fees and adjustments included;
  `averageLongPrice` and `averageShortPrice` are trade prices. The cost basis
  is `marketValue - longOpenProfitLoss` for a long position and
  `shortOpenProfitLoss - marketValue` for a short one, with no need for an
  option's multiplier, which positions do not carry.
- Position instruments carry `cusip` but no `instrumentId`. Equities have no
  `description`. Options carry `putCall`, `underlyingSymbol`, `type` and
  `netChange`, but no strike, expiration or multiplier. An option on a
  stock, fund or index has an OCC symbol, which gives the strike and
  expiration, as does its description. In transactions, an option on a
  future has Schwab's own form, such as `/ESH25_P5050:XCME`, which gives the
  strike and the future's contract month but not the day: that fill's
  `expirationDate` and description say 2025-03-21. No futures-option
  position has been seen. The multiplier, the cost basis over
  `averagePrice` times the quantity, is 100 in every sample.
- Positions' `maintenanceRequirement` adds up to the account's; a spread's
  is booked on one of its legs.
- **No lots.** There are no lot IDs, no acquisition dates and no realized
  gains anywhere in the API. Lot-level cost basis and realized gains are only
  on schwab.com, whose Realized Gain/Loss view exports a CSV with cost basis,
  short- and long-term gain, a wash-sale flag and the disallowed loss.
- For unrealized profit/loss use `longOpenProfitLoss` and
  `shortOpenProfitLoss`. `currentDayProfitLoss` covers today only. For some
  equities and ETFs Schwab subtracts the full cost basis from it
  ([jkoelker/schwab-mcp#223](https://github.com/jkoelker/schwab-mcp/pull/223)),
  and a position sold to open that day reports its whole value as the day's
  profit or loss.

## User preferences and account numbers

- `GET /userPreference` returns one object: `accounts`, `streamerInfo` and
  `offers`, the last two with one element each. Each account has
  `accountNumber`, `primaryAccount` (which can be false for every account),
  `type` (BROKERAGE, retirement accounts included), `nickName` (the name set
  on schwab.com, which need not be unique), `displayAcctId` (`...` and the
  last three digits), `autoPositionEffect`, `accountColor` and, in a capture
  of 2025 (not in 2024 ones), `lotSelectionMethod`. `streamerInfo`'s
  customer and correlation IDs identify the user and the session; they are
  not credentials, but keep them out of logs.
- In `GET /accounts/accountNumbers`, `accountNumber` is an 8-digit string and
  `hashValue` 64 characters. Schwabdev's maintainer believes the hash changes
  with the refresh token
  ([Schwabdev#51](https://github.com/tylerebowers/Schwabdev/issues/51)); other
  clients treat it as stable, with no evidence either way
  ([probe Q10](live.md#the-probes)). So keep data by
  account number, and look the hashes up again for each run. The account
  number is the one key present in all four endpoints.

## Orders

- **A simple order** is `{orderType, session, duration, orderStrategyType:
  "SINGLE", price, orderLegCollection: [{instruction, quantity, instrument:
  {symbol, assetType}}]}`.
- **Option spreads** use `orderType` NET_DEBIT, NET_CREDIT or NET_ZERO, several
  legs and a `complexOrderStrategyType` such as VERTICAL or IRON_CONDOR.
- **Conditional orders**: TRIGGER orders carry `childOrderStrategies`; OCO
  holds two children; a bracket is a TRIGGER wrapping an OCO.
- **Common values**:
  - sessions: NORMAL, AM, PM, SEAMLESS;
  - durations: DAY, GOOD_TILL_CANCEL, FILL_OR_KILL, IMMEDIATE_OR_CANCEL and
    a few more;
  - instructions: BUY, SELL, BUY_TO_COVER, SELL_SHORT, BUY_TO_OPEN,
    BUY_TO_CLOSE, SELL_TO_OPEN, SELL_TO_CLOSE and others.

  The spec lists 21 order statuses.
- **What can be ordered**: only equities and options, and no fractional
  shares, per Schwabdev's docs. A September 2026 comment on
  [Schwabdev#110](https://github.com/tylerebowers/Schwabdev/issues/110)
  relays Schwab support saying decimal quantities work above one share;
  unconfirmed.
- **The order ID** comes from the `Location` header, which is missing when the
  order filled instantly.
- **Never retry** a POST, PUT or DELETE automatically: a retried order is a
  second order.
- **Option symbols** follow the OCC format: the root padded to six characters,
  YYMMDD, C or P, and the strike times 1000 padded to eight digits
  (`AAPL  240517P00190000`). Index options have no `$`, and their root is not
  always the index's name (`SPXW  251208C06880000` on `$SPX`), so take option
  symbols from the chain rather than building them from the index symbol.
- To learn an order's JSON, place it in thinkorswim and read it back with
  `GET`.

## Market Data

| Path | Notes |
| --- | --- |
| `GET /quotes` | `symbols` (up to 500), `fields` (quote, fundamental, extended, reference, regular; default all), `indicative` |
| `GET /{symbol}/quotes` | One symbol. Use `/quotes` for futures symbols like `/ES`. |
| `GET /chains` | Symbol plus filters: contract type, strike count and range, strategy (SINGLE by default, or ANALYTICAL, VERTICAL, CALENDAR and others), date range, expiration month and more. An unbounded chain such as `$SPX` fails with 502 "Body buffer overflow" at about 10 MB, so always bound it. |
| `GET /expirationchain` | Expiration dates. Live responses name the field `expirationDate`; the spec says `expiration`. |
| `GET /pricehistory` | Equities and ETFs only. `periodType` and `period`, `frequencyType` and `frequency` (minutes: 1, 5, 10, 15 or 30), `startDate`/`endDate` in epoch milliseconds, extended hours, previous close. Observed depth: 1-minute bars about 48 days, 5 to 30 minutes about nine months, daily bars back to 1985 for AAPL. |
| `GET /movers/{index}` | `$DJI`, `$COMPX`, `$SPX`, NYSE, NASDAQ, OTCBB, INDEX_ALL, EQUITY_ALL, OPTION_ALL, OPTION_PUT, OPTION_CALL; sort by volume, trades or percent change; frequency 0, 1, 5, 10, 30 or 60 |
| `GET /markets`, `GET /markets/{market}` | Hours for equity, option, bond, future and forex markets, from today up to a year ahead; send `markets` comma-separated |
| `GET /instruments`, `GET /instruments/{cusip}` | Search by symbol or description (exact or regex), or fundamentals |

Indexes take a `$` prefix wherever the index itself is named: quotes
(`$SPX`, `$DJI`), option chains, movers and streamed quotes.

## Rate limits

- **120 requests a minute for each app**, for everything but orders. Neither
  spec states it. TD Ameritrade's
  [Schwab integration guide](https://raw.githubusercontent.com/kaizhu256/tdameritrade-dev-mirror/gh-pages/developer.tdameritrade.com/content/trader-api-schwab-integration-guide-june-2023-update.html)
  (June 2023) says "Non-order related throttles are set to 120 requests per
  minute on an application level". So the limit belongs to the app key, not
  to a login: every program that uses the key draws from it, on any computer.
- **Orders are limited for each account**: "the number of PUT/POST/DELETE
  order requests per minute per account", from 0 to 120 as set at
  registration, and "Get order requests are unthrottled", in Schwab's
  Accounts and Trading documentation as
  [captured](https://github.com/DistortionPoint/dp-exchange-schwab/tree/main/docs/reference/schwab)
  in August 2026. Schwabdev adds 4000 order calls a day, which no Schwab
  document states.
- **A 429 has no Retry-After.** In 24 rate-limit answers that
  [marketlake](https://github.com/l3a0/marketlake/blob/main/docs/design.md)
  recorded in October 2026, the body was whitespace, and the headers had no
  Retry-After or rate-limit field, only the correlation IDs.
- **429s come well under the limit**: marketlake got them while it sent 19
  requests a minute, one with a single request in flight
  ([marketlake#533](https://github.com/l3a0/marketlake/issues/533)). So no
  client-side limit prevents every 429; back off and try again. Whether
  bursts are what Schwab refuses is not known
  ([probe Q14](live.md#the-probes)).
- **The stream** has its own limits (see [streaming](#streaming)).

## Streaming

- **Connection details** come from `GET /userPreference`: `streamerInfo` gives
  the socket URL, customer ID, correlation ID, channel and function ID.
- **Log in** with an `ADMIN`/`LOGIN` request carrying the access token
  without a `Bearer` prefix. Wait for the response before subscribing:
  subscribing too early fails with code 20.
- **Commands**: LOGIN, SUBS, ADD, UNSUBS, VIEW, LOGOUT.
- **Services**: LEVELONE_EQUITIES, LEVELONE_OPTIONS, LEVELONE_FUTURES,
  LEVELONE_FUTURES_OPTIONS, LEVELONE_FOREX, NYSE_BOOK, NASDAQ_BOOK,
  OPTIONS_BOOK, CHART_EQUITY, CHART_FUTURES, SCREENER_EQUITY, SCREENER_OPTION,
  ACCT_ACTIVITY.
- **Limits**:
  - One streamer connection per user at a time; a second one is closed with
    code 12.
  - There is a symbol limit, code 19, which Schwabdev puts at 500.
  - With no subscriptions the stream stops (code 30).
- **ACCT_ACTIVITY** sends order and fill events (OrderCreated, OrderAccepted,
  OrderFillCompleted, CancelAccepted and more). The event's data is a JSON
  string inside the JSON message, with unusual number and time encodings, so
  treat an event as a signal to re-sync, not as a record.

## Errors

- **Error bodies vary**, and not only by API:
  - Accounts and Trading documents `{message, errors: [string]}`. It also
    sends a message alone, such as `{"message": "Invalid account number"}`
    (one report shows it inside a list of one, perhaps as the reporter's
    own code printed it:
    [schwab-py#203](https://github.com/alexgolec/schwab-py/issues/203)),
    and 401s in the Market Data format, as the real ones that
    [z-Wind/schwab_api](https://github.com/z-Wind/schwab_api/tree/main/tests/model/Trader)
    keeps show.
  - Market Data sends `{errors: [{id, status, title, detail, source}]}`.
    `status` is sometimes a number and sometimes a string, and `source` names
    the `parameter`, `header` or `pointer` at fault.
  - OAuth sends `{error, error_description}`, with the real reason of a
    refusal nested in the description (see [OAuth](#oauth)).
  - The gateway, Apigee, sends `{fault: {faultstring, detail: {errorcode}}}`.
    An answer too big for it gets 502 "Body buffer overflow" with
    `protocol.http.TooBigBody`, the same on every try.
  - Schwab's edge network, Akamai, sends an "Access Denied" HTML page with 403.
    Its text is written in character references, and it carries a
    "Reference #" number and no correlation ID. It has come from the token
    endpoint ([Schwabdev#61](https://github.com/tylerebowers/Schwabdev/issues/61),
    a "blip" that cleared the same day) and from API requests during a
    re-import of a year of history
    ([strat-journal-app#51](https://github.com/alynnyree/strat-journal-app/pull/51),
    which advises waiting about fifteen minutes).
  - An app that is not yet "Ready For Use", or an account of a type the API
    does not serve, gets Spring's "Whitelabel Error Page" with 500
    (Schwabdev's
    [troubleshooting](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages-raw/troubleshooting.md)).
  - A 429 has a body of whitespace (see [rate limits](#rate-limits)).

  So read a body by its shape, not by which API sent it, and keep it raw.
- **Correlation IDs**: API responses carry a `Schwab-Client-CorrelId` header,
  which parts of the spec spell `Schwab-Client-CorrelID`, the same name to
  HTTP. Include it in errors, so a failed request can be traced with Schwab's
  Trader API team (traderapi@schwab.com). Akamai's pages give their reference
  number instead.
- **Statuses**: the spec documents 400, 401, 403, 404, 500 and 503 for
  transactions. 429 is not documented, but it happens.

## Encodings

- **Timestamps** are in UTC, written `2024-04-04T15:56:07+0000` in
  transactions and orders: no fraction, and an offset without a colon, which
  RFC 3339, and so Go's `time.Time` decoding, refuses. previewOrder answers
  with `Z`, and Market Data with `.000+00:00`. Accounts, positions and
  preferences carry none.
- **Numbers** have never been strings, null or NaN in the captures known to
  be byte-exact. Whole values come as `0` or as `0.0`, even within one
  endpoint. Prices carry up to five decimals, and position figures binary
  noise (`-236.999999999999`): they are doubles, which a float64 holds
  exactly.
- **Duplicate keys.** One captured instrument named `assetType` twice. Go's
  `encoding/json` takes the last value; strict parsers and Go's
  `encoding/json/v2` (built by default from Go 1.27, behind
  `GOEXPERIMENT=jsonv2` in 1.26) refuse the object unless given
  `jsontext.AllowDuplicateNames(true)`.
- **Keep the raw bytes.** Re-encoding Schwab's JSON changes it: Go's
  `json.Marshal` escapes `&`, `<` and `>`, a `json.Encoder` on Go 1.27
  escapes U+2028 and U+2029 even with HTML escaping off, and decoding into a
  map reorders keys, rewrites numbers and drops duplicates. `json.Compact`
  and `json.Indent` change only the white space.

## Where the spec is wrong

Do not generate a client from the spec; decode leniently and keep the raw
JSON. A capture of the spec from August 2026
([dp-exchange-schwab](https://github.com/DistortionPoint/dp-exchange-schwab/tree/main/docs/reference/schwab))
is identical to the 2024 one, so none of this has been fixed.

- **Shape mismatches.** `GET /userPreference` and
  `GET /transactions/{id}` are documented as arrays but return objects.
- **Field mismatches.** The only fields the spec marks required are an
  instrument's `assetType` and a `name` that does not exist. Fields appear
  that the spec lacks: `aggregatedBalance` beside each account, balance
  figures such as `savings` and `longMarketValue`, instruments'
  `closingPrice`, `status` and `uniformSymbol`, `qualifiedDividend`,
  `lotSelectionMethod`, and the expiration chain's `expirationDate`, which
  the spec names `expiration`. More appear over time. The spec's
  `activityType` and `user` have not been seen, transaction instruments have
  no `cusip`, and position instruments no `instrumentId`.
- **Type mismatches.** `isInCall` is a boolean, not a number. A position
  option's deliverable `symbol` is declared a string of format int64.
- **Enum mismatches.** Enum lists disagree between schemas (16 fee types in
  one place, 25 in another) and miss values seen live. The position
  instrument's `assetType` lists COLLECTIVE_INVESTMENT, INDEX and CURRENCY,
  but its oneOf has no branch for them, though ETFs are
  COLLECTIVE_INVESTMENT. Keep enums as open strings.
- **Schema defects.** The base instruments require a property, `name`, that
  does not exist. Future and Index are built from the transaction
  instrument's own oneOf, so a generated type recurses. TransferItem and
  UserDetails declare a discriminator, `activityType`, that they lack.
- **Request bodies.** The documented request body for `previewOrder` is a
  wrapper; clients post the plain order.

## Sources

- Schwab OpenAPI specs (2024 captures):
  [Accounts and Trading](https://github.com/major/schwab-go/blob/main/docs/trader_api.openapi.json),
  [Market Data](https://github.com/major/schwab-go/blob/main/docs/market_data.openapi.json)
- [Streamer Guide](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages/pdfs/Schwab_Trader_API_Streamer_Guide.pdf)
  (2024-05-15)
- schwab-py: [client](https://github.com/alexgolec/schwab-py/blob/main/schwab/client/base.py),
  [auth docs](https://github.com/alexgolec/schwab-py/blob/main/docs/auth.rst),
  [getting started](https://github.com/alexgolec/schwab-py/blob/main/docs/getting-started.rst)
- Schwabdev: [API](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages-raw/api.md),
  [orders](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages-raw/orders.md),
  [setup guide](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages-raw/setupguide.md),
  [troubleshooting](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages-raw/troubleshooting.md)
- [jkoelker/schwab-mcp](https://github.com/jkoelker/schwab-mcp) (position
  profit/loss, order handling)
- [LumiBot's Schwab notes](https://github.com/Lumiwealth/lumibot/blob/dev/docsrc/brokers.schwab.rst)
  (transaction windows, rate limits)
- Transaction history depth and boundaries:
  [ssandy33/regress#125](https://github.com/ssandy33/regress/issues/125),
  [Akiko0210/PersonalAssistant#25](https://github.com/Akiko0210/PersonalAssistant/pull/25),
  [GeneBO98/tradetally#274](https://github.com/GeneBO98/tradetally/issues/274),
  and [z-Wind/schwab_api](https://github.com/z-Wind/schwab_api/commit/2c760ad)'s
  live test of May 2024 (a window of 366 days)
- Orders: [k1xme/schwab-mcp](https://github.com/k1xme/schwab-mcp)
  (`previewOrder` on live accounts),
  [Schwabdev#110](https://github.com/tylerebowers/Schwabdev/issues/110)
  (fractional shares)
- Real answers of the account endpoints:
  [z-Wind/schwab_api](https://github.com/z-Wind/schwab_api/tree/main/tests/model/Trader)
  (one account's transactions of 2023-24, accounts and preferences,
  anonymized),
  [piekstra/schwab-cli](https://github.com/piekstra/schwab-cli/tree/main/tests/fixtures)
  (accounts, positions and transactions of August 2026, scrubbed, with
  [findings](https://github.com/piekstra/schwab-cli/blob/main/docs/api.md)),
  [jwplatta/schwab_rb](https://github.com/jwplatta/schwab_rb) (accounts,
  preferences and futures-option fills of 2025),
  [owen-gordon/schwab-api-wrapper](https://github.com/owen-gordon/schwab-api-wrapper)
  (sanitized samples), and the account snapshots kept by
  [gregtee2/wheelhouse](https://github.com/gregtee2/wheelhouse) and
  [archiecrawford/ffainvestments](https://github.com/archiecrawford/ffainvestments)
  (2025-26)
- Fixes after live failures:
  [strat-journal](https://github.com/alynnyree/strat-journal-backend)
  (identity by `activityId`, executions booked at midnight, option
  expirations), [tradetally](https://github.com/GeneBO98/tradetally)
  (delisted equities by CUSIP), [LumiBot](https://github.com/Lumiwealth/lumibot)
  (journal labels), [jkoelker/schwab-mcp#222](https://github.com/jkoelker/schwab-mcp/pull/222)
  and [#223](https://github.com/jkoelker/schwab-mcp/pull/223) (cash in margin
  accounts, day profit/loss), [sudowealth/schwab-api#19](https://github.com/sudowealth/schwab-api/pull/19)
  (fields its schemas required that live answers lack),
  [schwab-sdk#6](https://github.com/jonathan-g-davis/schwab-sdk/pull/6)
  (the shape of `/userPreference`, and `isInCall` as a boolean),
  [z-Wind/schwab_api](https://github.com/z-Wind/schwab_api/commit/2c760ad)'s
  change of May 2024 (the shape of `/userPreference`),
  [SchwabPy 2.0.2](https://github.com/jaycollett/schwabpy/releases/tag/2.0.2)
  (transactions without `types`, December 2025), and
  [major/schwab-go](https://github.com/major/schwab-go)'s history (the shape
  of `/transactions/{id}`)
- Account hashes: [Schwabdev#51](https://github.com/tylerebowers/Schwabdev/issues/51),
  [schwab-py#203](https://github.com/alexgolec/schwab-py/issues/203),
  [ffainvestments](https://github.com/archiecrawford/ffainvestments/blob/main/docs/FFA_RECONSTRUCTION_GUIDE.md)
- Rate limits and errors: TD Ameritrade's
  [Schwab integration guide](https://raw.githubusercontent.com/kaizhu256/tdameritrade-dev-mirror/gh-pages/developer.tdameritrade.com/content/trader-api-schwab-integration-guide-june-2023-update.html)
  (June 2023),
  [dp-exchange-schwab](https://github.com/DistortionPoint/dp-exchange-schwab/tree/main/docs/reference/schwab)
  (Schwab's documentation as captured in August 2026),
  [marketlake](https://github.com/l3a0/marketlake/blob/main/docs/design.md)
  (429s and timeouts measured in 2026),
  [z-Wind/schwab_api](https://github.com/z-Wind/schwab_api/tree/main/tests/model)
  (real error bodies)
