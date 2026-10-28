# Notes on the Schwab Trader API

Working notes on the parts of Schwab's API this project uses or plans to use.
The authority is <https://developer.schwab.com>. These notes come from:

- Schwab's OpenAPI specs, as captured from the developer portal in 2024
  (Trader API of 2024-05-11, Market Data of 2024-03-21), with copies in
  [major/schwab-go](https://github.com/major/schwab-go/tree/main/docs);
- Schwab's Streamer Guide of 2024-05-15, as redistributed by
  [Schwabdev](https://github.com/tylerebowers/Schwabdev/blob/main/docs/pages/pdfs/Schwab_Trader_API_Streamer_Guide.pdf);
- community clients, mainly [schwab-py](https://github.com/alexgolec/schwab-py)
  and [Schwabdev](https://github.com/tylerebowers/Schwabdev), for live
  behavior the specs get wrong.

Checked in October 2026. Where live behavior is known to differ from the
spec, it says so. Things nobody has pinned down are collected under
[open questions](ROADMAP.md#open-questions).

## Access and app setup

- **Individual tier** means an app you build to reach your own self-directed
  Schwab accounts. Giving an app to other account holders, free or paid,
  needs the Commercial tier.
- **Two API products**: Accounts and Trading Production, and Market Data
  Production. Add both; a call to a product the app lacks fails with 401
  "Client not authorized".
- The app must say **Ready For Use**; "Approved - Pending" does not work yet.
- **thinkorswim** must be enabled on the brokerage account for orders and
  some other calls, according to Schwabdev's setup guide.
- The **order limit** (0 to 120 order requests a minute) is set when you
  register the app. schwab-py recommends 120.
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
  Schwab's own spec also sends `scope=readonly`, which community clients leave
  out; whether it limits the token is unknown.
- **The code** comes back URL-encoded in the redirect, must be decoded, and
  expires in about 30 seconds.
- **Token requests** are `POST /v1/oauth/token` with HTTP Basic auth (app key
  and secret) and a form body of either
  `grant_type=authorization_code&code=...&redirect_uri=...` or
  `grant_type=refresh_token&refresh_token=...`. The response has
  `access_token`, `refresh_token`, `id_token`, `expires_in` (1800) and
  `token_type`.
- **Lifetimes**: access tokens last 30 minutes. A refresh token works for 7
  days from the browser login, and refreshing does not extend that. It can
  stop sooner, for example after a login somewhere else. schwab-py forces a
  new login at 6.5 days and jkoelker/schwab-mcp at 5.
- **Callback URLs** must be `https`, match the registered value exactly
  (a trailing slash difference gives an "Access Denied" page) and use
  `127.0.0.1`, not `localhost`. An app may register several, comma-separated.
- **One refresher at a time.** schwab-py warns that when two applications
  share a token file, "one of them will beat the other to refreshing, locking
  the slower one out". Schwabdev serializes refreshes with a database lock.
  `unsupported_token_type` means the refresh token is no longer valid,
  possibly replaced by a login elsewhere.
- **Revocation**: an unreleased schwab-py branch posts to `/v1/oauth/revoke`.
  Not documented by Schwab as far as we know.

## Accounts and Trading

Every account-scoped path takes the account's **hash** from
`/accounts/accountNumbers`, although the spec names the parameter
`accountNumber`.

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/accounts/accountNumbers` | `[{accountNumber, hashValue}]` |
| GET | `/accounts` | Every linked account. Add `fields=positions` for holdings (the only documented `fields` value). Live responses also carry an `aggregatedBalance` the spec does not mention. |
| GET | `/accounts/{hash}` | One account, same shape |
| GET | `/accounts/{hash}/orders` | `fromEnteredTime` and `toEnteredTime` required, `maxResults` (default 3000), `status`. The spec says at most a year; schwab-py says 60 days. |
| POST | `/accounts/{hash}/orders` | 201 with an empty body; the new order's URL, ending in its ID, is in the `Location` header |
| GET, PUT, DELETE | `/accounts/{hash}/orders/{orderId}` | Get, replace or cancel. A replace cancels the order and creates a new one, with a new `Location`. |
| GET | `/orders` | Orders across all accounts; `fromEnteredTime` must be within 60 days |
| POST | `/accounts/{hash}/previewOrder` | `orderStrategy`, `orderValidationResult` (`accepts`, `alerts`, `rejects`, `reviews`, `warns`) and `commissionAndFee`. One community server reports a 404 on live accounts; others use it on live accounts. |
| GET | `/accounts/{hash}/transactions` | See [transactions](#transactions) |
| GET | `/accounts/{hash}/transactions/{id}` | One transaction by its `activityId` |
| GET | `/userPreference` | `accounts[]` (with `nickName`, `primaryAccount`, `type`), `streamerInfo[]` and `offers[]` (market data permissions) |

## Transactions

The data behind the return calculators, so in more detail.

**Request.**

- `startDate`, `endDate` and `types` are required; `symbol` is optional.
- Dates are instants in UTC, formatted `yyyy-MM-dd'T'HH:mm:ss.SSSZ`.
- The spec types `types` as a single value, but a comma-separated list
  works: schwab-py and this project's CLI send all 15 types that way.

**Limits.**

- At most **3000 transactions per response**, with **no paging**.
- At most **one year per request**.
- A window that comes back with 3000 rows may have been cut short, so split
  it and ask again.

**How far back.** Not documented. The one-year limit is per request, not a
retention period.

- schwab-py's docs, jkoelker/schwab-mcp and TradeZella say 60 days. That
  probably comes from the wording for orders.
- LumiBot and other clients fetch history in 365-day windows.
- A 2026 report relays Schwab saying API history starts when the account was
  enabled for thinkorswim.
- schwab.com shows up to four years of history under Accounts > History, and
  ten years of statements and trade confirmations.

**Boundaries.**

- Send exact instants. A date-only end date sent as midnight UTC drops that
  day's trades.
- A window whose start equals its end returns nothing.
- Keep each window at or under 365 days, measured in time rather than
  calendar dates.

**Response.**

- A `Transaction` has `activityId`, `time`, `tradeDate`, `settlementDate`,
  `type`, `status` (VALID, INVALID, PENDING, UNKNOWN), `subAccount`,
  `activityType`, `description`, `accountNumber`, `positionId`, `orderId`,
  `netAmount` and `transferItems`.
- Each transfer item has `instrument`, `amount`, `cost`, `price`, `feeType`
  and `positionEffect` (OPENING, CLOSING, AUTOMATIC, UNKNOWN).
- Fees are separate transfer items with instrument `CURRENCY_USD` and a
  `feeType`: COMMISSION, SEC_FEE, STR_FEE, R_FEE, CDSC_FEE, OPT_REG_FEE,
  TAF_FEE, INDEX_OPTION_FEE and others.
- The 15 types are TRADE, RECEIVE_AND_DELIVER, DIVIDEND_OR_INTEREST,
  ACH_RECEIPT, ACH_DISBURSEMENT, CASH_RECEIPT, CASH_DISBURSEMENT,
  ELECTRONIC_FUND, WIRE_OUT, WIRE_IN, JOURNAL, MEMORANDUM, MARGIN_CALL,
  MONEY_MARKET and SMA_ADJUSTMENT.
- Instrument asset types are EQUITY, OPTION, INDEX, MUTUAL_FUND,
  CASH_EQUIVALENT, FIXED_INCOME, CURRENCY, COLLECTIVE_INVESTMENT (ETFs),
  FOREX, FUTURE and PRODUCT.
- Timestamps use offsets without a colon (`2025-12-04T01:21:21+0000`), which
  is not RFC 3339, so Go's default `time.Time` decoding rejects them.

## Positions and balances

- An account's `securitiesAccount` has `type` (CASH or MARGIN),
  `initialBalances`, `currentBalances`, `projectedBalances` and, with
  `fields=positions`, `positions`.
- A position has quantities (long, short, settled, previous session, aged),
  `averagePrice` and long/short averages, tax-lot average prices,
  `marketValue`, `maintenanceRequirement`, `longOpenProfitLoss`,
  `shortOpenProfitLoss`, and current-day cost and profit/loss.
- **No lots.** There are no lot IDs, no acquisition dates and no realized
  gains anywhere in the API. Lot-level cost basis and realized gains are only
  on schwab.com, whose Realized Gain/Loss view exports a CSV with cost basis,
  short- and long-term gain, a wash-sale flag and the disallowed loss.
- For unrealized profit/loss use `longOpenProfitLoss` and
  `shortOpenProfitLoss`. `currentDayProfitLoss` covers today only, and for
  some positions (seen on ETFs) Schwab subtracts the full cost basis from it.

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
- **What can be ordered**: only equities and options, and no fractional shares,
  per Schwabdev (one 2026 report disagrees on fractions).
- **The order ID** comes from the `Location` header, which is missing when the
  order filled instantly.
- **Never retry** a POST, PUT or DELETE automatically: a retried order is a
  second order.
- **Option symbols** follow the OCC format: the root padded to six characters,
  YYMMDD, C or P, and the strike times 1000 padded to eight digits
  (`AAPL  240517P00190000`). Indexes take a `$` prefix (`$SPX`).
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

## Rate limits

- **Neither spec states a limit.** Schwab's Trader API page, seen through
  search excerpts, gives 120 requests a minute for everything except orders,
  plus the order limit you chose for the app.
- **Schwabdev** adds 4000 order-related calls a day and 500 streamed symbols.
  Going over returns HTTP 429.
- **Bursts can be refused** even under the per-minute rate, so limit on the
  client side and back off on 429, honoring `Retry-After`.

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

- **Error bodies vary by API**:
  - the Trader API sends `{message, errors: [string]}`;
  - Market Data sends `{errors: [{id, status, title, detail, source}]}`;
  - OAuth sends `{error, error_description}`;
  - the gateway sometimes sends `{fault: {...}}`.

  `status` is sometimes a number and sometimes a string. Keep the raw body.
- **Correlation IDs**: responses carry a `Schwab-Client-CorrelId` header.
  Include it in errors, so a failed request can be traced with Schwab's
  Trader API team (traderapi@schwab.com).
- **Rate limiting**: the spec documents 400, 401, 403, 404, 500 and 503 for
  transactions. 429 is not documented, but it happens.

## Where the spec is wrong

Do not generate a client from the spec; decode leniently and keep the raw
JSON.

- **Shape mismatches.** `GET /userPreference` and
  `GET /transactions/{id}` are documented as arrays but return objects.
- **Field mismatches.** Fields marked required are sometimes missing, and
  live responses carry fields the spec lacks (`aggregatedBalance`,
  `expirationDate`, extra instrument fields).
- **Enum mismatches.** Enum lists disagree between schemas (16 fee types in
  one place, 25 in another) and miss values seen live (the position
  instrument type has no COLLECTIVE_INVESTMENT, yet ETFs use it). Keep enums
  as open strings.
- **Request bodies.** The documented request body for `previewOrder` is a
  wrapper; clients post the plain order.

## Sources

- Schwab OpenAPI specs (2024 captures):
  [Trader API](https://github.com/major/schwab-go/blob/main/docs/trader_api.openapi.json),
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
  [GeneBO98/tradetally#274](https://github.com/GeneBO98/tradetally/issues/274)
