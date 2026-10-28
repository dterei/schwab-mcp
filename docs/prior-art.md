# Prior art

A survey of what already exists for Schwab's API: Schwab's own AI plans,
community MCP servers, client libraries, and tools for computing returns.
Checked in October 2026; star counts and activity change quickly.

## Schwab's own MCP servers

- **Advisors.** Schwab publishes a read-only MCP connector for Schwab Advisor
  Services, at `agent.schwabinstitutional.com/mcp`, listed in Claude's
  connector directory in October 2026. It has 29 read-only tools, such as
  `schwab-as-list-balances`, `schwab-as-get-transaction-detail` and
  `schwab-as-list-rgl-transactions`, and is only for advisors who custody
  with Schwab.
- **Retail.** On its 2026-10-15 earnings call Schwab said a pilot had started
  that lets traders connect the LLM of their choice to its "MCP layer",
  beginning with Claude and widening "in the coming months". The next day
  its CEO said on Bloomberg TV that outside AI tools will check balances and
  do research through Schwab's MCP servers, but not move money. Some reports
  say some traders can also trade through it.
- **Blocking.** In the same remarks Schwab said it blocks agents that log in on
  a client's behalf with the client's password, naming Meta's Muse.

What this means here:

- Use only OAuth and the documented API. Never ask for a Schwab password and
  never automate or scrape schwab.com. CSV exports are files you download
  yourself.
- Keep the Go client and the history store independent of the MCP layer, so
  they stay useful next to Schwab's own server.
- Local history and return calculations are the parts an official
  balances-and-research server is least likely to offer.

## Community MCP servers

| Project | Stack | Trading | Notes |
| --- | --- | --- | --- |
| [jkoelker/schwab-mcp](https://github.com/jkoelker/schwab-mcp) | Python, schwab-py | Gated | The most active (commits in October 2026) and the best safety design, below. 24 read tools, 3 write tools, 11 optional technical-analysis tools, 4 reference resources. stdio or streamable HTTP; the HTTP endpoint has no authentication. |
| [itsmostafa/schwab-mcp](https://github.com/itsmostafa/schwab-mcp) | Go, official go-sdk | Off unless `SCHWAB_ALLOW_TRADING=true` | The only active Go MCP server found. MIT. Fetches transactions in 364-day windows with all 15 types and warns at 3000 rows. All code is in `package main`, so there is no client library to reuse. |
| [sudowealth/schwab-mcp](https://github.com/sudowealth/schwab-mcp) | TypeScript, Cloudflare Workers | Ungated | A remote server with its own OAuth, tokens in KV. 23 tools, account identifiers scrubbed from output. `getTransaction` is a stub. Legacy SSE transport. Inactive since June 2025. |
| [vsoneji/schwab-mcp](https://github.com/vsoneji/schwab-mcp) | TypeScript, stdio | Ungated | sudowealth's code run locally. Its OAuth callback listens on every network interface. |
| [Tapojit/schwab-mcp](https://github.com/Tapojit/schwab-mcp) | TypeScript | None | A rewrite of jkoelker's server without trading, about 35 tools, refreshing the token every 25 minutes. |
| [k1xme/schwab-mcp](https://github.com/k1xme/schwab-mcp) | Python, Schwabdev | Dry run, then `confirmed=true` | About 22 tools, streamable HTTP, a Claude Code plugin with skills, credential redaction. The confirmation is a boolean the model can set, not tied to the dry run. Says `previewOrder` returns 404 on live accounts. |
| [acidsolution/schwab-mcp-server](https://github.com/acidsolution/schwab-mcp-server) | Python, httpx | None | 9 tools. Loads price history and option chains into DuckDB for SQL. Its SELECT-only check parses the query, which `SELECT * FROM read_csv('/etc/passwd')` passes. Reads a cost-basis field that is not in Schwab's position schema. No transactions tool. |
| [Open-Agent-Tools/open-stocks-mcp](https://github.com/Open-Agent-Tools/open-stocks-mcp) | Python, schwab-py | Not checked | Robinhood and Schwab; 64 Schwab tools on main (21 in the last PyPI release), including transactions by date. |
| [lonniev/schwab-mcp](https://github.com/lonniev/schwab-mcp) | Python, hosted | | A paid, multi-tenant hosted server where users sign in through the operator's Schwab app: the Commercial-tier pattern. Listed in the official MCP Registry. |

jkoelker/schwab-mcp's design is the one to learn from:

- Order tools are previews. Each preview calls Schwab and caches the exact
  order under a random ID that is single-use, expires after 10 minutes and is
  bound to the account. Placing takes only that ID, so the model cannot alter
  the order between preview and placement.
- Write tools exist only when a human approver is configured, on Discord or
  Signal, or when the operator passes `--jesus-take-the-wheel`.
- Every tool carries `readOnlyHint`; write tools carry `destructiveHint`.
- An asset-type check rejects mutual fund and bond orders before preview. It
  lets an order through when the lookup fails.
- Some results are compact by default, with a `verbose` option, and results
  are TOON-encoded to save tokens.
- Unrealized profit/loss comes from `longOpenProfitLoss` and
  `shortOpenProfitLoss`, avoiding a Schwab quirk in `currentDayProfitLoss`.
- Its weak spot for this project: transactions assume a 60-day limit.

## Client libraries

**Python.**

- [schwab-py](https://github.com/alexgolec/schwab-py): full REST and
  streaming coverage with order builders. Last released June 2025, and
  activity has slowed.
- [Schwabdev](https://github.com/tylerebowers/Schwabdev): sync and async
  clients, streaming with automatic reconnect, and an SQLite token store
  that coordinates refreshes between processes. v4.0.0 in August 2026;
  active.

**Go.** No library is both mature and maintained.

| Project | License | State | Notes |
| --- | --- | --- | --- |
| [major/schwab-go](https://github.com/major/schwab-go) | Apache-2.0 | Archived May 2026 | Every REST operation with typed models and an auth package; no streaming, rate limiting or 429 retry. Its `docs/` has the copies of Schwab's OpenAPI specs these notes use. |
| [Nightsuki/goschwab](https://github.com/Nightsuki/goschwab) | MIT | Alpha, 2026 | A port of Schwabdev with a streamer; thin transaction model; 429s are returned, not retried. |
| [citizenadam/go-schwabapi](https://github.com/citizenadam/go-schwabapi) | None | 2026 | Schwabdev-based, with a streamer. No license, so not reusable. |
| [go-schwab/trader](https://github.com/go-schwab/trader) | GPL-2.0 | Stale since early 2025 | Transaction listing, preview and user preferences are commented out. |
| [rayli09/schwab-cli](https://github.com/rayli09/schwab-cli) | MIT | October 2026 | A CLI port of schwab-py; writes are dry runs unless `--execute`. Internal packages only. |
| [Chalupa-Tech/go-schwab-api-individual](https://github.com/Chalupa-Tech/go-schwab-api-individual) | MIT | 2026 | Generated from the spec; no OAuth flow. |

Beware look-alikes: the Go module proxy flags ten copies of go-schwab/trader
under random account names as malicious. Keep the default `GOPROXY` and
`GOSUMDB`, and check a module's real home before adding it.

**TypeScript.** [sudowealth/schwab-api](https://github.com/sudowealth/schwab-api)
validates responses strictly with Zod, which makes calls fail on real data
that differs from the spec.
[slimandslam/schwab-client-js](https://github.com/slimandslam/schwab-client-js)
includes streaming.

## Tools for computing returns

- [Portfolio Performance](https://www.portfolio-performance.info) computes
  true time-weighted return and IRR. It can read Schwab's History CSV through
  [rlan/convert-csv-schwab2pp](https://github.com/rlan/convert-csv-schwab2pp),
  which handles buys, dividends, interest and wires but not yet ACH or sells.
- [Ghostfolio](https://ghostfol.io) imports Schwab CSV through
  [dickwolff/Export-To-Ghostfolio](https://github.com/dickwolff/Export-To-Ghostfolio).
- [hoostus/portfolio-returns](https://github.com/hoostus/portfolio-returns)
  computes XIRR for Beancount ledgers, with a choice of which accounts count
  as external cash flows.

## What this project takes, and avoids

Takes:

- Preview-then-place with single-use, server-side preview IDs, and a human
  confirmation that the model cannot give itself.
- Read-only unless trading is switched on.
- `readOnlyHint` and `destructiveHint` on every tool.
- Compact results, and option chains that are always bounded.
- Transaction windows of at most a year, split whenever a response hits 3000.
- Lenient decoding that keeps the raw JSON.

Avoids:

- Ungated order tools, or a confirmation flag the model can set.
- Automatic retries of order requests.
- Network listeners on anything but `127.0.0.1`, and HTTP endpoints without
  authentication.
- "Read-only" SQL enforced by parsing the query instead of a read-only
  database connection.
- Strict validation that fails a whole call on one unexpected field.
- The 60-day assumption about transaction history.
- Code under licenses that cannot be copied into this MIT project (GPL) or
  that have no license at all.
