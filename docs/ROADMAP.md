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

- **Read-only by default.** Tools that preview, place, replace or cancel
  orders do not exist until trading is switched on. Every order is previewed
  first and needs your confirmation.
- **Schwab is the only network peer.** The CLI and the server connect to
  Schwab and nothing else, and your history is stored on your machine. Tokens
  are stored owner-only, and secrets never appear in logs, errors or tool
  output.
- **Private by default.** What an MCP tool returns goes to the model your
  client uses. For most clients that model runs on the provider's servers;
  only a local model keeps the data on your machine.
  So account numbers are masked in all tool and resource output, Schwab's raw
  JSON and SQL results included, and tools refer to accounts by nickname or
  Schwab's account hash.
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
  connect an LLM to its own "MCP layer", for balances and
  research but not money movement. At the same time it is blocking outside
  agents that sign in with a client's password. A general official server
  would overlap with milestone 3. It is unlikely to keep your full history
  locally, compute your returns, or let you script against your own data,
  which are the core of this project. This project uses the route Schwab
  provides for your own accounts: its public API, your own developer app and
  your own login.
- **Existing community servers** (see [prior art](prior-art.md)) mostly map
  API endpoints to tools one for one, in Python or TypeScript. One is in Go,
  itsmostafa/schwab-mcp, but it has no reusable client. None of them keeps a
  persistent local history or computes portfolio returns. The best,
  jkoelker/schwab-mcp, has an order-safety design that milestone 5 borrows.
- **No mature, maintained Go client library** for Schwab's API exists, so
  milestones 1 and 3 build one.

## Architecture

```
cmd/schwabtx/            CLI: login, snapshot, sync, export, quick lookups
cmd/schwab-mcp/          MCP server (stdio; streamable HTTP later)

internal/schwab/         Schwab API client: OAuth, token store, HTTP with
                         retries and rate limiting, typed models + raw JSON
internal/schwab/schwabtest/
                         the fake Schwab server used by every test
internal/history/        local SQLite store, sync, cash-flow classification,
                         CSV import, export
internal/perf/           valuations, TWR, MWR/IRR, income
internal/mcpserver/      MCP tools, resources and prompts over the above
```

`internal/schwab` becomes a public package once its API settles, since Go
has no mature, maintained Schwab client library today.

The CLI and the server share one token file. A file lock keeps them from
refreshing at the same time, and each re-reads the file before refreshing,
because a refresh by one process can invalidate the other's copy. Schwab's
single streamer connection (milestone 6) is shared the same way.

## Milestones

Milestones 1 and 2 serve the immediate goal of getting all your transactions
into your own calculators, so they come first. Most of milestone 3 needs only
milestone 1 and can be built in parallel with milestone 2; its store-backed
parts land once milestone 2 does. Milestone 4 needs milestones 1 and 2; only
its MCP tools need milestone 3.

### M0. Project foundation (done)

- [x] Single Go module, CLI under `cmd/schwabtx`
- [x] golangci-lint with gofumpt and goimports, `make check`
- [x] CI on Linux, macOS and Windows, govulncheck, Dependabot
- [x] READMEs, security policy, `.env.example`

### M1. Snapshots, and a Schwab client for history

Start recording positions and balances right away, then move the CLI's auth
and HTTP code into `internal/schwab` with what the history store needs.

- [ ] `schwabtx snapshot`, built on the CLI's current code so it does not
      wait for the rest: it appends the raw `GET /accounts?fields=positions`
      response and its fetch time to an owner-only JSON Lines file. Schwab
      reports positions and balances only as of now, so every day without a
      snapshot is lost for good
- [ ] A daily scheduled run of it after the market close, with sample cron,
      launchd and Task Scheduler entries. It exits non-zero with the message
      to run `schwabtx login` once the weekly login lapses, and prints the
      day-ahead expiry warning, so the scheduler can alert you
- [ ] OAuth login, refresh and token store extracted from the CLI, with a
      file lock and refresh coordination between processes
- [ ] Client-side rate limiting (Schwab publishes 120 requests a minute plus
      the order limit set for the app, shared by every process using the
      login); backoff on 429 and 5xx for reads only; errors that understand
      each of Schwab's error formats and carry its correlation ID for support
      tickets
- [ ] Typed models with the raw JSON kept, for account numbers, accounts with
      balances and positions, transactions and user preferences. Written by
      hand, since the spec is wrong in places (see
      [the API notes](schwab-api.md#where-the-spec-is-wrong))
- [ ] `internal/schwab/schwabtest`: the fake Schwab server for those
      endpoints, plus scrubbed recordings of real responses as fixtures
- [ ] Fuzz tests for the parsers (Schwab timestamps, pasted redirect
      addresses, transaction JSON)
- [ ] A manual `-live` test mode for probing the real API, never run in CI
- [ ] Answer the open questions below that a probe can settle, starting with
      how far back transactions go and what Schwab returns past that point

### M2. Full transaction history, stored locally

The data your return calculators need, kept complete and current.

- [ ] SQLite store (pure-Go driver, so cross-compiling stays easy) holding
      transactions with their raw JSON, transfer items, sync state per
      account, and snapshots of positions and balances
- [ ] `schwabtx sync`: first a backfill that walks back in windows of at most
      365 days (see [the API notes](schwab-api.md#transactions)), splitting
      any window that returns 3000 transactions. It stops when Schwab refuses
      a window, at a start date you set (such as when the account opened), or
      after several empty years in a row, and records the range it covered
      and why it stopped: an empty window does not prove there is no older
      history. Afterwards incremental syncs re-read a trailing overlap to
      catch transactions that change status
- [ ] Idempotent upserts by Schwab's activity ID, with status changes and
      cancellations recorded rather than lost
- [ ] A snapshot on every sync, and the files from `schwabtx snapshot`
      imported, each kept with its time and the market date it stands for;
      days without one are recorded as gaps. Snapshots are the only exact
      record of past valuations, and for mutual funds, options and bonds,
      which price history does not cover, the only record at all. The daily
      scheduled run becomes `schwabtx sync`
- [ ] Every transaction classified (deposit or withdrawal, in-kind transfer in
      or out, journal between your own accounts, buy, sell, dividend,
      interest, fee, tax, other), with overrides you can edit
- [ ] CSV import of the History and Realized Gain/Loss exports you download
      from schwab.com, for history the API does not serve (schwab.com shows
      four years) and for the cost basis of each closed lot. An account's
      history can take several files, since exports may be capped (third
      parties report 1,500 or 10,000 rows, or two years, per file; Schwab
      states no limit), so the import lists the dates each file covers and
      warns about a file that is empty or stops at exactly a reported cap
- [ ] Each transaction counted once. Re-importing a file, or importing exports
      whose dates overlap, adds nothing twice. A CSV row that matches an API
      transaction is linked to it, and calculations use the API's record.
      Rows pair one to one, so two identical trades on the same day stay two,
      and an uncertain match is flagged for you to resolve
- [ ] `schwabtx export`: normalized CSV and JSON Lines, one row per
      transaction or per leg with its category, plus the snapshots, with raw
      JSON optional
- [ ] Export formats for existing tools: Portfolio Performance and Ghostfolio
- [ ] `schwabtx accounts`, `positions` and `balances` for quick lookups

### M3. MCP server, read-only

- [ ] `cmd/schwab-mcp` on the official Go MCP SDK, stdio transport first:
      over stdio every MCP protocol version works, including the elicitation
      that milestone 5 uses for confirmations
- [ ] Typed models and fake-server endpoints for orders and market data:
      quotes, price history, option chains and expirations, instruments,
      market hours, movers
- [ ] The read-only tools in the [tool catalog](#mcp-tool-catalog), with
      input and output schemas, structured results and `readOnlyHint`. The
      SDK checks every result against its tool's output schema, and the
      schemas it infers describe `json.RawMessage` and `json.Number` wrongly,
      so those types get schema overrides or hand-written schemas
- [ ] Compact results by default with a `detail` option; pagination for
      transactions; option chains always bounded
- [ ] Account numbers masked everywhere a tool or resource returns data,
      Schwab's JSON included
- [ ] Transactions served from the local store once milestone 2 has synced
      it, from the API otherwise; store-backed results say which dates they
      cover, per account
- [ ] Clear tool errors when the login has expired or is about to, telling
      you to run `schwabtx login`; an `auth_status` tool
- [ ] Resources: account list, positions per account, and reference pages
      for Schwab's enums (transaction types, order types and statuses)
- [ ] A prompt for a portfolio review
- [ ] Setup docs for the common MCP clients, desktop and command-line
- [ ] Tests through the SDK's in-memory transport against the fake Schwab
      that call every tool with empty and with full responses, so the SDK's
      output check covers every schema, and that fail if a full account
      number appears in any result or error

### M4. Returns and performance

- [ ] Per-account and whole-portfolio views of the cash flows classified in
      milestone 2 (a journal between two of your accounts is external to each
      but internal to the total)
- [ ] Valuation series: exact from snapshots where they exist. Before the
      first snapshot, holdings and cash are rebuilt backwards from it by
      undoing each transaction, so positions bought or transferred in before
      history starts still count, and priced with daily closes from price
      history on the same split basis as the quantities. Where the rebuild
      disagrees with a later snapshot, or prices are missing (mutual funds,
      options, bonds), the gap is reported rather than guessed
- [ ] Time-weighted return (daily chain-linked, Modified Dietz where daily
      values are missing) and money-weighted return (XIRR), per account and
      in total, over any period
- [ ] Benchmarks from price history, such as an S&P 500 ETF. Total return
      needs the benchmark's dividends, which the API does not keep: price bars
      carry none and a quote shows only the latest one. Unless daily bars
      turn out to be dividend-adjusted, dividends come from quotes recorded at
      each sync or from a file you import, and a comparison without them is
      labeled price return
- [ ] Income (dividends and interest by month and symbol), fees and
      commissions, realized gains from imported lot data, unrealized gains
      from positions
- [ ] `schwabtx perf` and MCP tools for all of the above, with prompts for an
      income report and a performance report; CSV and JSON output for
      external dashboards
- [ ] Read-only SQL over the local store, for the CLI and as an MCP tool. The
      MCP tool sees only views that leave out the raw JSON and show accounts
      by nickname, hash or masked number. The database enforces this, not an
      inspection of the query: a read-only connection plus an SQLite
      authorizer that allows only reads of those views and refuses attaching
      databases and loading extensions (a read-only connection alone still
      allows temporary views)

### M5. Trading, opt-in

Copies the safety design of jkoelker/schwab-mcp and adds limits.

- [ ] Off unless the server starts with trading enabled; order tools are not
      registered otherwise, and over stdio only until confirmation over HTTP
      is designed (an elicitation goes to whichever client is connected)
- [ ] Every order, replacements included, is previewed first. The server keeps
      the exact order under a single-use preview ID that expires after a few
      minutes and is bound to the account and, for a replacement, to the
      order it replaces. Placing or replacing takes only that ID, so what gets
      placed is exactly what was previewed. The preview includes Schwab's own
      `previewOrder` checks and fees where that endpoint works for the
      account (one community server says it fails on live accounts)
- [ ] Confirmation by you, not the model, through an MCP elicitation. On the
      2026-07-28 protocol the SDK's older elicitation call fails, so the tool
      returns an input request and runs twice: the first call only asks, with
      a nonce tying the answer to the question; the second consumes the
      preview ID whatever the answer, and places the order only if you
      accepted. A client that cannot elicit needs an approval channel the
      assistant has no tool for, such as the Discord or Signal approvers of
      jkoelker/schwab-mcp; until one exists, order tools stay off for that
      client. A flag the model can set, or a command it could run in a shell,
      is not a confirmation
- [ ] Limits you set: which accounts may trade, maximum order value, allowed
      asset types (equities and options are all Schwab's API supports)
- [ ] Order builders for equities (market, limit, stop, stop-limit, trailing
      stop) and options (single legs, verticals and other spreads), plus OCO,
      trigger and bracket orders; replace and cancel
- [ ] `readOnlyHint` on `preview_order` and `destructiveHint` on
      `place_order`, `replace_order` and `cancel_order`; an append-only local
      audit log of every preview, order, replacement and cancellation

### M6. Streaming

- [ ] A client for Schwab's WebSocket streamer. Schwab allows one streamer
      connection per user, not per process, and closes a second one with
      code 12. The CLI and every server process (each MCP client starts its
      own) share your login, so one process at a time holds the stream, under
      a lock next to the token file like the refresh lock, and shares it among
      its subscribers; the others use the REST API. With the lock in place,
      code 12 means a program outside this project has the stream, so back
      off rather than reconnect in a loop
- [ ] Order and fill events from the account activity stream trigger an
      incremental sync, so new trades are picked up between regular syncs.
      The stream is only known to report orders, so dividends, interest,
      transfers, journals and fees still wait for the next sync
- [ ] Live quotes for an MCP resource or a dashboard, with REST quotes in the
      processes that do not hold the stream

### M7. Packaging and releases

Release automation can come forward as soon as there is an MCP binary worth
installing.

- [ ] GoReleaser builds for macOS, Linux and Windows on amd64 and arm64, with
      checksums, an SBOM and GitHub artifact attestations
- [ ] A Homebrew tap
- [ ] An entry in the official MCP Registry (with "schwab" in the name, since
      the registry's search matches names only), and an MCP Bundle for
      one-click install in desktop clients that support bundles
- [ ] Streamable HTTP transport for clients that cannot launch a subprocess,
      bound to `127.0.0.1`, with a bearer token required on every request and
      requests from a foreign `Origin` refused: binding to loopback alone
      still lets any local process or user call the tools. The server
      generates the token on first run and stores it owner-only next to the
      Schwab token
- [ ] GitHub Actions pinned to commit SHAs once a workflow can write to the
      repository or holds secrets, as a release workflow will (CI today has a
      read-only token and no secrets)

### Later

- Automatic login: a listener on the second, high-port callback URL
  (`https://127.0.0.1:8182`, registered during setup), bound to `127.0.0.1`
  with a self-signed certificate, catches the redirect so there is nothing to
  paste. The browser shows a certificate warning to click through. Pasting
  stays as the fallback, and is what an app with only a port-less callback
  gets
- OS keychain storage for the token (macOS Keychain, Windows Credential
  Manager, Secret Service)
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
| `get_transaction` | One transaction with every leg and Schwab's JSON, account numbers masked | transactions/{id} | M3 |
| `get_orders`, `get_order` | Orders by status and date, or one order | orders | M3 |
| `get_quotes` | Quotes and fundamentals for up to 500 symbols | quotes | M3 |
| `get_price_history` | Candles for a symbol and period | pricehistory | M3 |
| `get_option_chain`, `get_option_expirations` | Option chains with filters, expiration dates | chains, expirationchain | M3 |
| `search_instruments` | Symbol or description search, fundamentals, CUSIP lookup | instruments | M3 |
| `get_market_hours`, `get_movers` | Market hours by date, top movers by index | markets, movers | M3 |
| `auth_status` | Whether you are logged in and when the login expires | none | M3 |
| `sync_history` | Brings the local store up to date and says what it covers | transactions, accounts | M3, after M2 |
| `get_value_history` | Daily value per account and in total, each day marked exact, rebuilt or missing | the store, pricehistory | M4 |
| `get_performance` | TWR and MWR per account and in total, against a benchmark | the store, pricehistory | M4 |
| `get_cash_flows` | Deposits and withdrawals, classified | the store | M4 |
| `get_income` | Dividends, interest, fees and commissions by period and symbol | the store | M4 |
| `get_realized_gains` | Realized gains from imported lot data | the store | M4 |
| `query_history` | Read-only SQL over views of the local store | the store | M4 |
| `preview_order` | A new order, or a replacement for an open order, as it will be placed, with Schwab's checks and fees where available, and a preview ID | previewOrder | M5 |
| `place_order` | Places a previewed order after your confirmation | orders (POST) | M5 |
| `replace_order` | Replaces an open order with a previewed replacement after your confirmation | orders/{id} (PUT) | M5 |
| `cancel_order` | Cancels an open order after your confirmation | orders/{id} (DELETE) | M5 |

## Feature list

Everything planned, by area.

| Area | Feature | Milestone |
| --- | --- | --- |
| Login | Browser login with paste-back of the redirect | done |
| Login | Expiry warning a day before the weekly login lapses | done |
| Login | Token shared safely between the CLI and the server | M1 |
| Login | Automatic redirect capture on `127.0.0.1` | Later |
| Login | Token in the OS keychain | Later |
| Client | Accounts, balances, positions, transactions and preferences, typed, raw JSON kept | M1 |
| Client | Rate limiting, retries for reads, correlation IDs in errors | M1 |
| Client | Orders and every Market Data endpoint, typed, raw JSON kept | M3 |
| History | Recent transactions across all accounts (`schwabtx`) | done |
| History | Daily position and balance snapshots, on a schedule | M1 |
| History | Full backfill as far as Schwab serves, then incremental sync | M2 |
| History | Every transaction classified, with overrides | M2 |
| History | Import of schwab.com History and Realized Gain/Loss CSVs, each transaction counted once | M2 |
| History | CSV, JSON Lines, Portfolio Performance and Ghostfolio export | M2 |
| MCP | Read-only tools for accounts, positions, history, orders, market data | M3 |
| MCP | Resources, prompts, structured output, tool annotations, masked account numbers | M3 |
| MCP | Streamable HTTP on `127.0.0.1`, with a token and an `Origin` check | M7 |
| Returns | Per-account and whole-portfolio cash flows | M4 |
| Returns | Daily valuation series, exact or rebuilt, with gaps reported | M4 |
| Returns | TWR, MWR/XIRR, Modified Dietz, per account and in total | M4 |
| Returns | Benchmarks, income, fees, realized and unrealized gains | M4 |
| Returns | Read-only SQL over the store | M4 |
| Trading | Preview-then-place with single-use preview IDs, replacements included | M5 |
| Trading | Your confirmation for every order; account, value and asset limits | M5 |
| Trading | Equity, option, spread, OCO, trigger and bracket orders; replace, cancel | M5 |
| Trading | Audit log | M5 |
| Streaming | One shared stream, order events triggering syncs, live quotes | M6 |
| Releases | Binaries, Homebrew, MCP Registry, MCP Bundle | M7 |

## Open questions

Most of these can be settled with a short probe against a real account in
milestone 1.

- **How far back do transactions go, and what does Schwab return for a window
  older than that (an error or an empty list)?** Schwab documents at most a
  year per request but not how far back history reaches. One report
  ([regress#125](https://github.com/ssandy33/regress/issues/125)) quotes
  Schwab saying it starts when the account was enabled for thinkorswim.
- **Does the per-account orders endpoint serve a year or only 60 days?** The
  spec and community clients disagree.
- **Does `scope=readonly` give a token that cannot trade?** It appears in
  Schwab's own spec. Most community clients send no scope, and the one
  project known to send `readonly` has reported no effect. If it works,
  `schwabtx login` should request it unless asked for trading, and the token
  file should record which kind of login it holds, so the server registers
  order tools only when trading is enabled and the token allows it.
  Switching trading on then takes one new login.
- **Does `previewOrder` work on live accounts?** One community server says it
  returns 404 there; others use it.
- **Are daily price bars adjusted for dividends, or only for splits?** If only
  for splits, the API has no record of a benchmark's past dividends, so total
  return before the first recorded dividend needs a file you import.
- **How do you confirm an order in a client that cannot elicit?**
  jkoelker/schwab-mcp sends each order to Discord or Signal for approval. A
  command in your own terminal is simpler, but an assistant that can run
  shell commands, as coding agents can, could run it itself.
- **Are account hashes stable across logins and across developer apps?**
- **How do corporate actions, option expirations and assignments, money
  market sweeps and transfers show up in transactions?** This decides the
  classification rules in milestone 2.
- **Do transactions change after they post (pending to valid,
  cancellations), and how long after?** This decides the overlap for
  incremental syncs.
- **How soon after a fill does the trade appear in transactions?** This
  decides whether a sync triggered by a stream event needs a delayed
  re-check.
- **What are the real rate limits for this app, and how does Schwab signal
  them?**

## Risks

- **Schwab changes the rules.** It could restrict API access for AI agents or
  change the Individual tier. The project stays on documented endpoints and
  your own developer app, the route Schwab provides for reaching your own
  accounts.
- **Schwab's retail MCP pilot** may grow into a general official server that
  makes parts of milestone 3 redundant. The local history and the return
  calculations are the parts it is least likely to cover. That is why the
  history comes first (milestone 2), and why the client, the store and the
  calculations do not depend on this project's MCP server. If such a server
  appears before milestone 3 is done, the calculations and `schwabtx perf`
  from milestone 4 can go ahead of it; only their MCP tools need the server.
- **Trading mistakes are costly.** Hence opt-in, preview-then-place, your
  confirmation, limits and an audit log, and why trading waits until the
  read-only server has been in use for a while.
- **The weekly login** is a fixed cost of Schwab's API. The tools warn early,
  the scheduled snapshot fails loudly when the login lapses, and the login
  itself takes a minute.
