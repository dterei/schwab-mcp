# Roadmap

Where this project is going and in what order. It draws on
[notes on the Schwab API](schwab-api.md) and a
[survey of existing Schwab clients and MCP servers](prior-art.md), both
current as of October 2026.

## Goals

1. **Own your data.** Pull your complete transaction history, positions and
   balances out of Schwab into a local store and plain files that your own
   return calculators and performance dashboards can read.
2. **A complete MCP server for Schwab retail accounts.** Let an AI assistant
   answer questions about your accounts, holdings, history, performance and
   the market, and, only when you switch it on, place orders.

## Non-goals, for now

- **Other brokers and banks.** Schwab Bank checking, Plaid and other
  aggregators are a different project.
- **A hosted, multi-user service.** Schwab's Individual tier is for your own
  accounts, and brokerage tokens should not live on someone else's server.
  Remote access, if it comes, is for your own devices.
- **Automating the weekly browser login, or schwab.com.** Schwab requires a
  human login every 7 days, and it blocks agents that sign in with a client's
  password. The tools make the login quick rather than remove it, and they
  never sign in to or scrape schwab.com: CSV exports are files you download
  yourself.
- **An algorithmic trading engine.** Trading support is for orders you ask
  for, one at a time.

## Design principles

- **Read-only by default.** Order tools do not exist until trading is switched
  on. Every order is previewed first and needs your confirmation.
- **Your data stays local.** The only network peer is Schwab. Tokens are
  stored owner-only, and secrets never appear in logs, errors or tool output.
- **Private by default.** Account numbers are masked in output. Tools refer to
  accounts by nickname or Schwab's account hash.
- **Lossless and lenient.** Schwab's raw JSON is kept next to every typed or
  normalized form, so a calculation can always be checked against the source.
  Live responses differ from Schwab's spec, so decoding tolerates missing and
  unknown fields and keeps enums as open strings.
- **Orders are never retried automatically.** A retried order request can be
  a second order.
- **Few dependencies.** The standard library, an MCP SDK and, from milestone
  2, SQLite. Each new dependency has to earn its place.
- **Tested against a fake Schwab.** Every endpoint the code uses is imitated by
  an in-process fake server that enforces Schwab's auth rules, as the CLI's
  tests already do. Nothing in CI talks to the real API.

## Where this fits

- **Schwab's own MCP servers.** Schwab already runs a read-only MCP connector
  for advisors. In October 2026 it started a pilot that lets retail traders
  connect an LLM, Claude first, to its own MCP layer, for balances and
  research but not money movement. At the same time it is blocking outside
  agents that sign in with a client's password. A general official server
  would overlap with milestone 3. It is unlikely to keep your full history
  locally, compute your returns, or let you script against your own data,
  which are the core of this project. This project uses the route Schwab
  sanctions: its public API, your own developer app and your own login.
- **Existing community servers** (see [prior art](prior-art.md)) mostly map
  API endpoints to tools one for one, in Python or TypeScript. One is in Go,
  itsmostafa/schwab-mcp, but it has no reusable client. None of them keeps a
  persistent local history or computes portfolio returns. The best,
  jkoelker/schwab-mcp, has an order-safety design that milestone 5 borrows.
- **No mature, maintained Go client library** for the Trader API exists, so
  milestone 1 builds one.

## Architecture

```
cmd/schwabtx/            CLI: login, sync, export, quick lookups
cmd/schwab-mcp/          MCP server (stdio; streamable HTTP later)

internal/schwab/         Trader API client: OAuth, token store, HTTP with
                         retries and rate limiting, typed models + raw JSON
internal/schwab/schwabtest/
                         the fake Schwab server used by every test
internal/history/        local SQLite store, sync, CSV import, export
internal/perf/           cash-flow classification, TWR, MWR/IRR, income
internal/mcpserver/      MCP tools, resources and prompts over the above
```

`internal/schwab` becomes a public package once its API settles, since Go
has no maintained Schwab client today.

The CLI and the server share one token file. A file lock keeps them from
refreshing at the same time, and each re-reads the file before refreshing,
because a refresh by one process can invalidate the other's copy.

## Milestones

Milestones 1 and 2 serve the immediate goal of getting all your transactions
into your own calculators, so they come first. Milestone 3 needs only
milestone 1 and can start in parallel with milestone 2.

### M0. Project foundation (done)

- [x] Single Go module, CLI under `cmd/schwabtx`
- [x] golangci-lint with gofumpt and goimports, `make check`
- [x] CI on Linux, macOS and Windows, govulncheck, Dependabot
- [x] READMEs, security policy, `.env.example`

### M1. A reusable Schwab client

Move the CLI's auth and HTTP code into `internal/schwab` and cover the API.

- [ ] OAuth login, refresh and token store extracted from the CLI, with
      a file lock and refresh coordination between processes
- [ ] Optional automatic login: a local HTTPS listener, bound to
      `127.0.0.1` only, with a self-signed certificate catches the redirect,
      so there is nothing to paste. Pasting stays as the fallback.
- [ ] Optional OS keychain storage for the token (macOS Keychain, Windows
      Credential Manager, Secret Service)
- [ ] Client-side rate limiting (Schwab publishes 120 requests a minute, plus
      the order limit set for the app); backoff on 429 and 5xx for reads
      only; errors that understand each of Schwab's error formats and carry
      its correlation ID for support tickets
- [ ] Typed models with the raw JSON kept, for: account numbers, accounts and
      balances, positions, transactions, orders, user preferences, quotes,
      price history, option chains and expirations, instruments, market
      hours, movers. Written by hand, since the spec is wrong in places (see
      [the API notes](schwab-api.md#where-the-spec-is-wrong))
- [ ] `internal/schwab/schwabtest`: the fake Schwab server, grown to every
      endpoint, plus scrubbed recordings of real responses as fixtures
- [ ] Fuzz tests for the parsers (Schwab timestamps, pasted redirect
      addresses, transaction JSON)
- [ ] A manual `-live` test mode for probing the real API, never run in CI
- [ ] Answer the open questions below that a probe can settle, starting with
      how far back transactions go

### M2. Full transaction history, stored locally

The data your return calculators need, kept complete and current.

- [ ] SQLite store (pure-Go driver, so cross-compiling stays easy) holding
      transactions with their raw JSON, transfer items, sync state per
      account, and daily snapshots of positions and balances
- [ ] `schwabtx sync`: first a backfill that walks back in windows of at most
      a year, splitting any window that returns 3000 transactions and
      recording the oldest date Schwab serves; afterwards incremental syncs
      that re-read a trailing overlap to catch transactions that change status
- [ ] Idempotent upserts by Schwab's activity ID, with status changes and
      cancellations recorded rather than lost
- [ ] Daily position and balance snapshots taken on every sync, the only
      exact source of past valuations the API offers
- [ ] CSV import of the History and Realized Gain/Loss exports you download
      from schwab.com, to reach further back than the API (schwab.com shows
      four years) and to get lot-level cost basis, with duplicates against
      API data detected and flagged
- [ ] `schwabtx export`: normalized CSV and JSON Lines (one row per
      transaction or per leg), with raw JSON optional
- [ ] Export formats for existing tools: Portfolio Performance and Ghostfolio
- [ ] `schwabtx accounts`, `positions` and `balances` for quick lookups

### M3. MCP server, read-only

- [ ] `cmd/schwab-mcp` on the official Go MCP SDK, stdio transport first:
      over stdio every MCP protocol version works, including the elicitation
      that milestone 5 uses for confirmations
- [ ] The read-only tools in the [tool catalog](#mcp-tool-catalog), with
      input and output schemas, structured results and `readOnlyHint`
- [ ] Compact results by default with a `detail` option; pagination for
      transactions; option chains always bounded; masked account numbers
- [ ] Transactions served from the local store when it is synced, from the
      API otherwise
- [ ] Clear tool errors when the login has expired or is about to, telling
      you to run `schwabtx login`; an `auth_status` tool
- [ ] Resources: account list, positions per account, and reference pages
      for Schwab's enums (transaction types, order types and statuses)
- [ ] Prompts: portfolio review, income report, performance report
- [ ] Setup docs for Claude Code, Claude Desktop and other MCP clients
- [ ] Tests through the SDK's in-memory transport against the fake Schwab

### M4. Returns and performance

- [ ] Cash-flow classification: deposits, withdrawals and transfers that are
      external, versus dividends, interest, fees and trades that are not,
      with per-account and whole-portfolio views (a journal between two of
      your accounts is external to each but internal to the total), and
      overrides you can edit
- [ ] Valuation series: exact from snapshots where they exist, otherwise
      rebuilt from transactions and daily closes from price history, with
      gaps (mutual funds, options, bonds) reported rather than guessed
- [ ] Time-weighted return (daily chain-linked, Modified Dietz where daily
      values are missing) and money-weighted return (XIRR), per account and
      in total, over any period
- [ ] Benchmarks from price history, such as an S&P 500 ETF, with dividends
      accounted for so the comparison is total return
- [ ] Income (dividends and interest by month and symbol), fees and
      commissions, realized gains from imported lot data, unrealized gains
      from positions
- [ ] `schwabtx perf` and MCP tools for all of the above; CSV and JSON output
      for external dashboards
- [ ] Read-only SQL over the local store, for the CLI and as an MCP tool, so
      the assistant can answer questions the fixed tools do not cover.
      Enforced by a read-only database connection with attaching other
      databases and loading extensions turned off, not by inspecting the query

### M5. Trading, opt-in

Copies the safety design of jkoelker/schwab-mcp and adds limits.

- [ ] Off unless the server starts with trading enabled; order tools are not
      registered otherwise
- [ ] Every order is previewed first. The server keeps the exact order under
      a single-use preview ID that expires after a few minutes and is bound
      to the account, and placing an order takes only that ID, so what gets
      placed is exactly what was previewed. The preview includes Schwab's
      own `previewOrder` checks and fees where that endpoint works for the
      account (one report says it fails on live accounts)
- [ ] Confirmation by you, not the model: an MCP elicitation where the client
      supports it, otherwise a confirmation step outside the chat. A flag the
      model can set is not a confirmation
- [ ] Limits you set: which accounts may trade, maximum order value, allowed
      asset types (equities and options are all Schwab's API supports)
- [ ] Order builders for equities (market, limit, stop, stop-limit, trailing
      stop) and options (single legs, verticals and other spreads), plus OCO,
      trigger and bracket orders; replace and cancel
- [ ] `destructiveHint` on order tools, and an append-only local audit log of
      every preview, order, replacement and cancellation

### M6. Streaming

- [ ] A client for Schwab's WebSocket streamer (one connection per user, so
      one per process, shared by every subscriber)
- [ ] Account activity events triggering incremental syncs, so the store stays
      current without polling
- [ ] Live quotes for an MCP resource or a dashboard

### M7. Packaging and releases

Release automation can come forward as soon as there is an MCP binary worth
installing.

- [ ] GoReleaser builds for macOS, Linux and Windows on amd64 and arm64, with
      checksums, an SBOM and GitHub artifact attestations
- [ ] Homebrew tap, and a container image for the HTTP mode
- [ ] An entry in the official MCP Registry (with "schwab" in the name, since
      the registry's search matches names only), and an MCP Bundle for
      one-click install in Claude Desktop
- [ ] Streamable HTTP transport bound to localhost, for clients that cannot
      launch a subprocess
- [ ] GitHub Actions pinned to commit SHAs once a workflow can write to the
      repository or holds secrets, as a release workflow will (CI today has a
      read-only token and no secrets)

### Later

- Notifications when the weekly login is about to expire
- A small local dashboard, if exporting to existing tools turns out not to be
  enough
- Tax-year reports (realized gains, income, fees) for checking a 1099
- Beancount or Ledger export

## MCP tool catalog

The tools planned for the MCP server. They are designed for an assistant to
use rather than mirroring Schwab's endpoints one for one: accounts are
addressed by nickname or hash, results are compact, and the local store is
used where it is better than the API.

| Tool | What it returns | Schwab API | Milestone |
| --- | --- | --- | --- |
| `list_accounts` | Accounts with nickname, type, masked number, hash and headline balances | accountNumbers, userPreference, accounts | M3 |
| `get_balances` | Cash, buying power, market value, per account and in total | accounts | M3 |
| `get_positions` | Holdings with quantity, value, cost, open P/L and portfolio weight | accounts?fields=positions | M3 |
| `get_transactions` | Transactions filtered by account, dates, type and symbol, paged | transactions, or the store | M3 |
| `get_transaction` | One transaction with every leg and the raw JSON | transactions/{id} | M3 |
| `get_orders`, `get_order` | Orders by status and date, or one order | orders | M3 |
| `get_quotes` | Quotes and fundamentals for up to 500 symbols | quotes | M3 |
| `get_price_history` | Candles for a symbol and period | pricehistory | M3 |
| `get_option_chain`, `get_option_expirations` | Option chains with filters, expiration dates | chains, expirationchain | M3 |
| `search_instruments` | Symbol or description search, fundamentals, CUSIP lookup | instruments | M3 |
| `get_market_hours`, `get_movers` | Market hours by date, top movers by index | markets, movers | M3 |
| `auth_status` | Whether you are logged in and when the login expires | none | M3 |
| `sync_history` | Brings the local store up to date | transactions, accounts | M3 |
| `get_performance` | TWR and MWR per account and in total, against a benchmark | the store, pricehistory | M4 |
| `get_cash_flows` | Deposits and withdrawals, classified | the store | M4 |
| `get_income` | Dividends and interest by period and symbol | the store | M4 |
| `get_realized_gains` | Realized gains from imported lot data | the store | M4 |
| `query_history` | Read-only SQL over the local store | the store | M4 |
| `preview_order` | The order as it will be placed, with Schwab's checks and fees where available, and a preview ID | previewOrder | M5 |
| `place_order` | Places a previewed order after your confirmation | orders (POST) | M5 |
| `replace_order`, `cancel_order` | Changes or cancels an open order after your confirmation | orders/{id} (PUT, DELETE) | M5 |

## Feature list

Everything planned, by area.

| Area | Feature | Milestone |
| --- | --- | --- |
| Login | Browser login with paste-back of the redirect | done |
| Login | Expiry warning a day before the weekly login lapses | done |
| Login | Automatic redirect capture on `127.0.0.1` | M1 |
| Login | Token shared safely between the CLI and the server | M1 |
| Login | Token in the OS keychain | M1 |
| Client | Every Trader API and Market Data endpoint, typed, raw JSON kept | M1 |
| Client | Rate limiting, retries, correlation IDs in errors | M1 |
| History | Recent transactions across all accounts (`schwabtx`) | done |
| History | Full backfill as far as Schwab serves, then incremental sync | M2 |
| History | Daily position and balance snapshots | M2 |
| History | Import of schwab.com History and Realized Gain/Loss CSVs | M2 |
| History | CSV, JSON Lines, Portfolio Performance and Ghostfolio export | M2 |
| MCP | Read-only tools for accounts, positions, history, orders, market data | M3 |
| MCP | Resources, prompts, structured output, tool annotations | M3 |
| MCP | Streamable HTTP on localhost | M7 |
| Returns | External cash-flow classification with overrides | M4 |
| Returns | TWR, MWR/XIRR, Modified Dietz, per account and in total | M4 |
| Returns | Benchmarks, income, fees, realized and unrealized gains | M4 |
| Returns | Read-only SQL over the store | M4 |
| Trading | Preview-then-place with single-use preview IDs | M5 |
| Trading | Your confirmation for every order; account, value and asset limits | M5 |
| Trading | Equity, option, spread, OCO, trigger and bracket orders; replace, cancel | M5 |
| Trading | Audit log | M5 |
| Streaming | Account activity triggering syncs; live quotes | M6 |
| Releases | Binaries, Homebrew, container, MCP Registry, MCP Bundle | M7 |

## Open questions

Most of these can be settled with a short probe against a real account in
milestone 1.

- **How far back do transactions go?** Schwab documents at most a year per
  request but not how far back history reaches. One report says it starts
  when the account was enabled for thinkorswim.
- **Does the per-account orders endpoint serve a year or only 60 days?** The
  spec and community clients disagree.
- **Does `scope=readonly` give a token that cannot trade?** It appears in
  Schwab's own spec but community clients leave it out. If it works, the
  read-only server should log in with it.
- **Does `previewOrder` work on live accounts?** One community server says it
  returns 404 there; others use it.
- **Are daily price bars adjusted for dividends**, or only for splits? This
  decides how benchmarks get to a total return.
- **Are account hashes stable** across logins and across developer apps?
- **How do corporate actions, option expirations and assignments, money
  market sweeps and transfers show up** in transactions? This decides the
  cash-flow rules in milestone 4.
- **Do transactions change after they post** (pending to valid, cancellations),
  and how long after? This decides the overlap for incremental syncs.
- **What are the real rate limits** for this app, and how does Schwab signal
  them?

## Risks

- **Schwab changes the rules.** It could restrict API access for AI agents or
  change the Individual tier. The project stays on documented endpoints and
  your own developer app, which is the route Schwab endorses.
- **Schwab's retail MCP pilot** may grow into a general official server that
  makes parts of milestone 3 redundant. The local history and the return
  calculations are the parts it is least likely to cover, which is why they
  come early, and why the client and the store do not depend on the MCP layer.
- **Trading mistakes are costly.** Hence opt-in, preview-then-place, your
  confirmation, limits and an audit log, and why trading waits until the
  read-only server has been in use for a while.
- **The weekly login** is a fixed cost of Schwab's API. The tools warn early
  and make the login fast.
