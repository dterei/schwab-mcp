# schwab-mcp

[![ci](https://github.com/dterei/schwab-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/dterei/schwab-mcp/actions/workflows/ci.yml)

Go tools for getting your own data out of a Charles Schwab brokerage account
through Schwab's official Trader API.

The first goal is your complete transaction history, positions and balances
in a local store and plain files that your own return calculators and
performance dashboards can read. The longer-term goal is a full
[MCP](https://modelcontextprotocol.io) server for Schwab retail accounts that
lets an AI assistant look up your accounts, positions, transactions and market
data. Today it is a small CLI that logs in, prints your recent transactions
and saves snapshots of your positions and balances.

This project is not affiliated with, endorsed by or supported by Charles
Schwab & Co.

## Status

| Piece | State |
| --- | --- |
| [`schwabtx`](cmd/schwabtx): log in and print recent transactions across all accounts | Works |
| [`schwabtx snapshot`](cmd/schwabtx/README.md#snapshots): save your current positions and balances; [run it daily](cmd/schwabtx/schedule) with launchd, cron or Task Scheduler | Works |
| Go client for Schwab's API (milestone 1): the login, retries under a shared rate limit, and the account records | In progress, internal for now |
| Full transaction history, stored locally and exported for your calculators (milestone 2) | Planned |
| MCP server, read-only first (milestone 3) | Planned |

The [roadmap](docs/ROADMAP.md) has the plan and the full feature list,
[docs/schwab-api.md](docs/schwab-api.md) has notes on the API itself, and
[docs/prior-art.md](docs/prior-art.md) surveys existing Schwab MCP servers and
client libraries.

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

1. Enable thinkorswim on every brokerage account you want to read (on
   schwab.com: Trade > Trading Platforms). The request can take about a day,
   so start it first. Schwab's API team, as relayed in one report
   ([regress#125](https://github.com/ssandy33/regress/issues/125)), says an
   account without it has no transaction or order history through the API,
   and that once it is enabled, transaction data can be retrieved "from that
   day forward". Whether older history then appears is unconfirmed;
   schwab.com's CSV exports can fill the gap. Without thinkorswim nothing
   fails: login and positions work, but transactions come back empty.
2. Go to <https://developer.schwab.com>, click Register and create a developer
   account (a separate login from your brokerage one; using the same email is
   the safe choice). Pick the **Individual Developer** profile.
3. On the API Products page open **Trader API - Individual** and click Request
   Access if it asks.
4. Dashboard > Apps > Create App. Add both API products, **Accounts and
   Trading Production** and **Market Data Production**: the CLI needs the
   first, the planned MCP server both, and calls to a product the app lacks
   fail with "Client not authorized". Set the callback URLs to
   `https://127.0.0.1,https://127.0.0.1:8182`: two URLs, comma-separated,
   with no trailing slashes. The CLI uses the first. The second is for an
   automatic login planned for later, which needs a port that does not
   require root. Changing the app later sends it back to "Approved - Pending"
   for a few days, so set both now.
5. Wait for the app status to change from "Approved - Pending" to
   **"Ready For Use"**. Usually a few days, sometimes longer; login fails until
   then.
6. Copy `.env.example` to `.env` and fill in the App Key and Secret. `.env` is
   git-ignored; never commit it or paste the values anywhere.

## Quick start

```sh
git clone https://github.com/dterei/schwab-mcp
cd schwab-mcp
cp .env.example .env          # then fill in your app key and secret
chmod 600 .env                # and keep them to yourself
set -a; . ./.env; set +a      # load it into the environment

go run ./cmd/schwabtx login      # follow the printed steps
go run ./cmd/schwabtx            # your 100 most recent transactions
go run ./cmd/schwabtx snapshot   # save today's positions and balances
```

On Windows, where `chmod` does not apply, keep the clone inside your user
profile, which other users cannot read, and load `.env` in PowerShell
instead:

```powershell
Get-Content -Encoding UTF8 .env | ForEach-Object { if ($_ -match '^\s*([A-Za-z_]\w*)=(.*)$') { Set-Item "Env:$($Matches[1])" ($Matches[2] -replace '^(["''])(.*)\1$', '$2') } }
```

See [cmd/schwabtx](cmd/schwabtx/README.md) for flags and troubleshooting.

## Privacy and security

- The tools talk only to Schwab (`api.schwabapi.com`). No third party sees your
  credentials or data.
- The saved login is a file readable only by you, in your user config
  directory. Treat it like a password: the login is not read-only, so the file
  lets whoever has it act on your accounts, trading included.
- `schwabtx -json` prints Schwab's raw data, which includes full account
  numbers. `schwabtx snapshot` saves the same kind of data, in a file
  readable only by you.
- Once the MCP server exists, what its tools return also goes to your AI
  assistant's model, which for most assistants runs on the provider's
  servers rather than your machine. The server will mask account numbers in
  everything it returns.

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## Development

You need Go 1.26 or newer.

```sh
make check   # everything CI runs: go.mod tidiness, lint, tests, govulncheck
make test    # tests with the race detector
make fuzz    # fuzz each target in internal/schwab in turn (needs Go 1.27)
make lint    # golangci-lint, with go vet and the format check, and ShellCheck
make fmt     # format the code
make help    # all targets
```

`make lint` and `make fmt` run the pinned golangci-lint version through
`go run`, so there is nothing to install; `make lint` skips ShellCheck if it
is not installed. The race detector in `make test` and `make check` needs
`gcc` on Linux and Windows (or `CC` set to another C compiler); without one,
run `make test RACE=`.

To get the same formatting on save, set up gopls in your editor with
`"formatting.gofumpt": true` and
`"formatting.local": "github.com/dterei/schwab-mcp"`.

```
cmd/schwabtx/   the CLI and its tests, which run it against a fake Schwab server
cmd/schwabtx/schedule/
                scripts and steps for running snapshots daily
internal/schwab/
                the Schwab client: the login, its refresh and the token file
                that every process using the login shares, reads that are
                tried again after a failure, under a rate limit that every
                process using the app key shares, and the accounts,
                positions, transactions and preferences they return, with
                Schwab's JSON kept
internal/schwab/schwabtest/
                a fake Schwab for the tests, kept out of the programs: its
                token endpoint, authorize page and account endpoints, with
                Schwab's rules for tokens and its error answers, and fixtures
                in the shape Schwab sends them
internal/filelock/
                file locks across processes, which keep two processes from
                refreshing the login at once, or from going over the rate
                limit together
docs/           roadmap, notes on the Schwab API, the live mode and a survey
                of prior art
.github/        CI and Dependabot
```

What runs where:

- **Linting** ([`.golangci.yml`](.golangci.yml)): golangci-lint v2 with its
  standard linters (go vet, staticcheck, errcheck, ineffassign, unused) plus
  ones for error wrapping, HTTP bodies and contexts, security (gosec) and
  modern Go idioms, and a rule that keeps the fake Schwab out of the
  programs. Code is formatted with gofumpt and goimports. ShellCheck checks
  the shell scripts.
- **Tests** run against the fake Schwab, `internal/schwab/schwabtest`, a
  server that each test starts for itself; only the live mode, below,
  reaches Schwab. They include each fuzz target's seeds, and the live mode's
  harness run against the fake, every probe included.
- **CI** ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)), on pull
  requests, pushes to `main` and weekly: tests with the race detector on
  Linux, macOS and Windows with the latest Go, and on Linux with the oldest
  supported Go; golangci-lint, ShellCheck and a `go mod tidy` check;
  govulncheck.
- **Dependabot** ([`.github/dependabot.yml`](.github/dependabot.yml)): weekly
  grouped updates for Go modules and GitHub Actions, each release left to age
  a week first. Actions are referenced by major version (`@v7`), so their
  minor and patch releases arrive without Dependabot and without the wait.
- **Fuzzing**, by hand: `make fuzz` fuzzes each target in `internal/schwab`
  in turn, 30 seconds each unless `FUZZTIME` says otherwise, and stops at
  the first that fails. It needs Go 1.27. A failing input is saved under
  `internal/schwab/testdata/fuzz`, to commit with the fix.
- **The live mode** ([docs/live.md](docs/live.md)), by hand and never in CI:
  `go test ./internal/schwab -run '^TestLive$' -live -v` probes Schwab's real
  API with your saved login, checks each answer against the records, and
  runs the default probes of the roadmap's
  [open questions](docs/ROADMAP.md#open-questions); the watches and the rate
  probe each run alone, with a flag of their own and `-timeout 0`. It only
  reads, and its report leaves out account numbers, hashes, IDs and balances,
  each line checked for what the run has seen before it is written, so that
  you can share it once you have read it.

## License

[MIT](LICENSE)
