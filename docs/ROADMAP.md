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
- **Few dependencies.** The standard library and golang.org/x/sys, for file
  locks on Windows, an MCP SDK and, from milestone 2, SQLite, whose driver
  needs x/sys too. Each new dependency has to earn its place.
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

internal/filelock/       file locks across processes, for the token file
                         and the rate limit
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
because another process may have refreshed meanwhile, or logged in again,
which ends the earlier login's refresh token. They share the rate limit too,
which Schwab counts by app key, through a small file that each request locks,
reads and rewrites. Schwab's single streamer connection (milestone 6) is
shared the same way.

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

- [x] `schwabtx snapshot`, built on the CLI's current code so it does not
      wait for the rest: it appends the raw `GET /accounts?fields=positions`
      response and its fetch time to an owner-only JSON Lines file. Schwab
      reports positions and balances only as of now. Milestone 4 can rebuild
      past holdings from transactions, but not the values of holdings that
      price history does not cover (mutual funds, money-market funds,
      options, bonds) or Schwab's own balance figures, so those are lost for
      every day without a snapshot
- [x] A daily scheduled run of it after the market close
      ([`cmd/schwabtx/schedule`](../cmd/schwabtx/schedule)): scripts that set
      up a launchd agent on macOS and a Task Scheduler task on Windows, and a
      cron line, each running a script that tries a failed run twice more, a
      minute apart, and passes on any warning or error, as a notification on
      macOS and Windows and through cron's mail elsewhere. The expiry warning
      now starts two days before the weekly login lapses, so that a daily run
      gives at least a day's notice
- [x] OAuth login, refresh and token store extracted from the CLI into
      `internal/schwab`, with a file lock (`internal/filelock`) and refresh
      coordination between processes. A process re-reads the token file
      under the lock before it refreshes, and takes a token that another
      process saved meanwhile, after a 401 too. A token request runs to its
      end even if the caller gives up, so that a grant is not lost unsaved.
      The token file keeps its format, now with a version, and keeps fields
      that a newer version adds; `Status` reads it without a request, for
      the M3 `auth_status` tool
- [x] A client-side rate limit, retries, and Schwab's errors read in full.
      Schwab counts 120 requests a minute for each app key, orders apart, so
      every process on the computer that uses the key keeps one limit
      together, 100 a minute with bursts of 5, through a small file in the
      user cache directory that each request locks, reads and rewrites. A
      429 holds all of them back. Reads are tried again after a 429, a 5xx
      or a failure of the network, up to four times, and so is a token
      refresh, which lets go of the token file's lock while it waits and
      re-reads the file after, since another process may have refreshed in
      between; goroutines that refresh at once share one refresh. Errors read
      each of Schwab's error formats, the gateway's faults and the edge
      network's pages included, and carry the correlation ID, or the edge's
      reference number, for support requests
- [x] Typed records with the raw JSON kept, for account numbers, accounts
      with balances and positions, transactions and user preferences, and
      the Client methods that fetch them. Written by hand from real answers
      that other projects keep, since the spec is wrong in places (see
      [the API notes](schwab-api.md#where-the-spec-is-wrong)). They decode
      leniently and never fail: a value of the wrong type is named in the
      record's Problems, at its key path, and costs only its field, and a
      balance or flag that Schwab sends only sometimes is nil when absent.
      Transactions give their trade day as a New York date, through the
      system's time zone or an embedded copy of New York's
- [ ] `internal/schwab/schwabtest`: the fake Schwab server for those
      endpoints, plus scrubbed recordings of real responses as fixtures
- [ ] Fuzz tests for the parsers (Schwab timestamps, pasted redirect
      addresses, transaction JSON)
- [ ] A manual `-live` test mode for probing the real API, never run in CI,
      that also checks each real answer against the records: every key a
      field takes, every key that a field marked as always present needs,
      and no key repeated in an object
- [ ] Answer the open questions below that a probe can settle, starting with
      how far back transactions go and what Schwab returns past that point

### M2. Full transaction history, stored locally

The data your return calculators need, kept complete and current.

- [ ] SQLite store (pure-Go driver, so cross-compiling stays easy) holding
      transactions with their raw JSON, transfer items, sync state per
      account, and snapshots of positions and balances. Money as integer
      cents and quantities in millionths, from the records' float64s, which
      is exact while the value times its scale stays under 2^51; prices and
      averages can stay REAL. Accounts keyed by account number behind a local
      id, never by hash, which may change between logins: the hashes are
      looked up again on each run, and a 400 "Invalid account number"
      (`schwab.ErrInvalidAccount`) means a stale hash, to look up again and
      retry once, never Schwab refusing a window
- [ ] `schwabtx sync`: first a backfill that walks back in windows of at most
      365 days (see [the API notes](schwab-api.md#transactions)), splitting
      any window that returns 3000 transactions. It stops when Schwab refuses
      a window, at a start date you set (such as when the account opened), or
      after several empty years in a row, and records the range it covered
      and why it stopped: an empty window does not prove there is no older
      history. Afterwards incremental syncs re-read a trailing overlap to
      catch transactions that change status
- [ ] Idempotent upserts by account and Schwab's activity ID, with status
      changes and cancellations recorded rather than lost. An instrument in
      a transaction is Schwab's reference data as of the request, not the
      trade's: `closingPrice` is the latest close, at the leg and in an
      option's deliverable, and a delisted equity comes back under its
      CUSIP. So a re-read row is a revision only if it differs once each
      instrument is cut to its asset type and `instrumentId`; the first raw
      JSON seen is kept beside the latest, and instruments go in a table of
      securities with the symbols and CUSIPs each has had. A row without an
      activity ID is kept and flagged, never keyed by its bytes, and a row
      missing from a later complete re-read (every type, no symbol, fewer
      than 3000 rows) is recorded as gone, never deleted
- [ ] A snapshot on every sync, and the files from `schwabtx snapshot`
      imported, each kept with its time and the market date it stands for;
      days without one are recorded as gaps. A snapshot taken after a close
      stands for the last session that closed at or before its time, from a
      trading calendar with the early closes, not for the New York date of
      its time: the scheduled 9:05 PM run is past midnight in New York for a
      user in Pacific time. Its initial balances are the start of the
      session, while its current ones keep moving in the evening and on
      Sunday, so both liquidation values are kept. Snapshots are Schwab's own
      record of each day's holdings and values: a check on what milestone 4
      rebuilds from transactions, and for mutual funds, options and bonds,
      which price history does not cover, the only source of past values.
      The daily scheduled run becomes `schwabtx sync`
- [ ] Every transaction classified (deposit or withdrawal, in-kind transfer in
      or out, journal between your own accounts, buy, sell, dividend,
      interest, fee, tax, other), with overrides you can edit, by the
      conventions in [the API notes](schwab-api.md#transactions): a trade's
      fees are all its CURRENCY legs, named or not; a cash row carries its
      cash in the leg's amount; a dividend's or withholding's security is
      after the `~` in its description; and sweep journals are not
      deposits. A row whose legs' cash does not add up to `netAmount`, or
      that has problems, is flagged
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
      so those types get schema overrides or hand-written schemas. The
      records of `internal/schwab` infer cleanly as they are: no custom
      marshalers, maps, enum lists or recursive types, `Raw` left out, and
      `Problems` carried along. Build output types around a record with a
      named field, never by embedding it, since its UnmarshalJSON would come
      along
- [ ] Compact results by default with a `detail` option; pagination for
      transactions; option chains always bounded
- [ ] Account numbers masked everywhere a tool or resource returns data,
      Schwab's JSON included: inside each handler, before the result is
      returned, since the SDK checks results and quotes a failing value in
      its error. Mask deep copies of the records, and Schwab's JSON decoded
      as `any`; replace only string values, whole or in part, and never
      change a value's type or delete a key, which would break the listed
      schema. Masked numbers take Schwab's own form, `...` and the last
      three digits. `StreamerInfo` is never returned. Encode Schwab's JSON
      with HTML escaping off, since `json.Marshal` escapes `&`
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
- [ ] An order goes out only with an access token that has at least five
      minutes left, refreshed first if need be, since a 401 would call for a
      retry, which orders never get
- [ ] Order requests wait their turn under the order limit set for the app,
      which Schwab counts for each account and only for PUT, POST and DELETE;
      reading orders shares the limit for reads. An order refused with 429
      never reached the order engine, so it is reported as not placed. The
      request is built so that Go's HTTP/2 transport cannot send it again
      by itself either: without a GetBody

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
| Login | Expiry warning two days before the weekly login lapses | done |
| Login | Token shared safely between the CLI and the server | done |
| Login | Automatic redirect capture on `127.0.0.1` | Later |
| Login | Token in the OS keychain | Later |
| Client | Accounts, balances, positions, transactions and preferences, typed, raw JSON kept | done |
| Client | Rate limiting, retries for reads, correlation IDs in errors | done |
| Client | Orders and every Market Data endpoint, typed, raw JSON kept | M3 |
| History | Recent transactions across all accounts (`schwabtx`) | done |
| History | Position and balance snapshots (`schwabtx snapshot`) | done |
| History | Snapshots on a daily schedule, with sample scheduler entries | done |
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
  Another, in 2026, got empty lists for windows past an account's history
  ([schwab-cli](https://github.com/piekstra/schwab-cli/blob/main/docs/api.md)),
  so an empty window does not show that the history has ended.
- **Does an account's hash change with a new login?** Schwabdev's maintainer
  believes so; other clients treat it as stable. The store keeps data by
  account number either way.
- **What do expirations, assignments, transfers in kind and corporate
  actions look like?** No public sample has one; they are probably
  RECEIVE_AND_DELIVER rows.
- **Does a fill in the overnight or futures sessions carry its instant in
  `tradeDate`, or the next day's midnight?** It decides which day such a fill
  is booked on.
- **When do initial balances move to the new session**, and do they then
  hold the close? A snapshot taken after that could stand for the close.
- **Does the transactions `symbol` filter keep dividends and withholdings**,
  which name their security only in their description?
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
- **How far under 120 a minute does Schwab refuse requests, and are bursts
  what it refuses?** It answers 429 with no Retry-After, and has done so to a
  client sending 19 a minute (see [the API notes](schwab-api.md#rate-limits)).
  This decides the limit's rate and burst.
- **Does a refresh end the older access tokens, and does it ever change the
  refresh token?** Community clients suggest that it does not change the
  refresh token. The login code copes either way, but each answer saves
  requests.

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
  the scheduled snapshot tells you at least a day before the login lapses
  and again once it has, and the login itself takes a minute.
