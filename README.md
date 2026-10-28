# schwab-mcp

[![ci](https://github.com/dterei/schwab-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/dterei/schwab-mcp/actions/workflows/ci.yml)

Go tools for getting your own data out of a Charles Schwab brokerage account
through Schwab's official Trader API.

The goal is a full [MCP](https://modelcontextprotocol.io) server for Schwab
retail clients: one that lets an AI assistant look up your accounts,
positions, transactions and market data, and that feeds your own return
calculators and performance dashboards. Today it is a small CLI that logs in
and prints your recent transactions.

This project is not affiliated with, endorsed by or supported by Charles
Schwab & Co.

## Status

| Piece | State |
| --- | --- |
| [`schwabtx`](cmd/schwabtx): log in and print recent transactions across all accounts | Works |
| Reusable Go client for the Trader API | Planned |
| MCP server, read-only first | Planned |
| Full transaction history export for return calculations | Planned |

The [roadmap](docs/ROADMAP.md) has the plan and the full feature list, and
[docs/schwab-api.md](docs/schwab-api.md) has notes on the API itself.

## Why the Schwab Trader API

| Option | Verdict |
| --- | --- |
| **Schwab Trader API** (developer.schwab.com, "Individual Developer") | **Picked.** Free, first-party, nobody else holds your data, and it returns the most detail per transaction (each fee, the security leg, order ids). |
| Plaid | Works for personal use and would also cover Schwab Bank checking and other institutions, but it puts a third party in the middle, flattens the data, refreshes overnight, and needs a web page (Plaid Link) just to log in. Worth revisiting if you want non-Schwab accounts. |
| SnapTrade and other aggregators | Same trade-offs as Plaid. |
| CSV export from schwab.com | Zero setup and reaches further back, but manual. A good way to backfill history later. |

The API covers **brokerage accounts**, not Schwab Bank checking or savings,
and Schwab makes you log in through the browser again **every 7 days**.

## One-time setup

You need a Schwab self-directed brokerage account; the Individual tier is free
and only for accessing your own accounts. These steps come from community
guides, so menu names on the portal may differ slightly.

1. Go to <https://developer.schwab.com>, click Register and create a developer
   account (a separate login from your brokerage one; using the same email is
   the safe choice). Pick the **Individual Developer** profile.
2. On the API Products page open **Trader API - Individual** and click Request
   Access if it asks.
3. Dashboard > Apps > Create App. Add the **Accounts and Trading Production**
   API product (add **Market Data Production** too if you want quotes later).
   Set the callback URL to `https://127.0.0.1`.
4. Wait for the app status to change from "Approved - Pending" to
   **"Ready For Use"**. Usually a few days, sometimes longer; login fails until
   then.
5. Copy `.env.example` to `.env` and fill in the App Key and Secret. `.env` is
   git-ignored; never commit it or paste the values anywhere.

Some guides say the brokerage account must be thinkorswim-enabled for API calls
to work. If login succeeds but calls fail, enable thinkorswim on schwab.com.

## Quick start

```sh
git clone https://github.com/dterei/schwab-mcp
cd schwab-mcp
cp .env.example .env          # then fill in your app key and secret
set -a; source .env; set +a

go run ./cmd/schwabtx login   # follow the printed steps
go run ./cmd/schwabtx         # your 100 most recent transactions
```

See [cmd/schwabtx](cmd/schwabtx/README.md) for flags and troubleshooting.

## Privacy and security

- The tools talk only to Schwab (`api.schwabapi.com`). No third party sees your
  credentials or data.
- The saved login is a file readable only by you, in your user config
  directory. Treat it like a password: Schwab's tokens are not read-only, so
  whoever holds one can do anything your app can, trading included, until the
  login expires.
- `schwabtx -json` prints Schwab's raw data, which includes full account
  numbers.

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## Development

You need Go 1.26 or newer. With Go 1.21 or later installed, `go` downloads a
new enough toolchain by itself.

```sh
make check   # everything CI runs: go.mod tidiness, lint, tests, govulncheck
make test    # tests with the race detector
make lint    # golangci-lint, which includes go vet and the format check
make fmt     # format the code
make help    # all targets
```

`make lint` and `make fmt` run the pinned golangci-lint version through
`go run`, so there is nothing to install. To get the same formatting on save,
turn on gofumpt in your editor (for gopls: `"formatting.gofumpt": true`).

```
cmd/schwabtx/   the CLI and its tests, which run it against a fake Schwab server
docs/           roadmap and notes on the Schwab API
.github/        CI and Dependabot
```

What runs where:

- **Linting** ([`.golangci.yml`](.golangci.yml)): golangci-lint v2 with its
  standard linters (go vet, staticcheck, errcheck, ineffassign, unused) plus
  ones for error wrapping, HTTP bodies and contexts, security (gosec) and
  modern Go idioms. Code is formatted with gofumpt and goimports.
- **CI** ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)), on pull
  requests, pushes to `main` and weekly: tests with the race detector on
  Linux, macOS and Windows with the latest Go, and on Linux with the oldest
  supported Go; golangci-lint and a `go mod tidy` check; govulncheck.
- **Dependabot** ([`.github/dependabot.yml`](.github/dependabot.yml)): weekly
  grouped updates for Go modules and GitHub Actions, each release left to age
  a week first.

## License

[MIT](LICENSE)
