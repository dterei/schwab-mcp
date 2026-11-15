# The MCP server

`schwab-mcp` is an [MCP](https://modelcontextprotocol.io) server that lets
a model read your Charles Schwab accounts: their balances, positions and
transactions, from Schwab's Trader API and from the local history that
`schwabtx` keeps. A client starts it on your computer and speaks the
protocol on its stdin and stdout: a desktop client, a command-line agent
or an editor extension.

It only reads. It never places, changes or cancels an order, never writes
the local history, and never logs in: you log in with `schwabtx login` in
your own terminal, and the server reads the login that saved. When the
login is missing or has lapsed, every tool that reads Schwab says so and
asks the model to ask you to run `schwabtx login`; the model must not run
it itself, since it needs your browser.

This page covers setting it up for each kind of client, the tools, the
resources and the prompt, the errors and warnings that answers carry, what
reaches the model, the limits, and what to do when something fails. The
command's flags, config file, exit codes and logs are in its
[README](../cmd/schwab-mcp/README.md). Orders and market data come later
(see [Later](#later)).

## Set up

Install both commands from one checkout, the same version, so that they
agree on the store:

```sh
go install ./cmd/schwab-mcp ./cmd/schwabtx
```

Then, once, in the terminal where `schwabtx` works, with `SCHWAB_APP_KEY`
and `SCHWAB_APP_SECRET` loaded as the [quick start](../README.md#quick-start)
loads them:

```sh
schwabtx setup     # add -store PATH if your store commands take one
schwabtx login
```

`schwabtx setup` saves `config.json`, readable only by you, with the app
key and secret and the absolute paths of the token file, the store and the
rate-limit file as `schwabtx` finds them in that terminal. The server reads
everything from there, so a client's configuration holds no secret, and the
server uses the files that `schwabtx` uses whatever folder and environment
the client starts it in. Setup then prints, for each kind of client, the
server's command and arguments; the [CLI's
README](../cmd/schwabtx/README.md#schwabtx-setup) shows all of its output.

Run `schwabtx setup` again in that terminal whenever you move the token
file or the store, with the same `-store` the store commands take.

## Add it to a client

Give the client the command and arguments that `schwabtx setup` printed,
in the form it takes for your kind of client. The examples here are of
macOS, whose user config directory, `~/Library/Application Support`, holds
a space; on Windows, setup writes each path's backslashes escaped in JSON
and the path in double quotes in the add command.

- Use the absolute paths that setup prints: a client may start the server
  in any folder, `/` included, and a desktop app's `PATH` often lacks
  `~/go/bin`.
- Name the server `schwab`, or another name under 20 characters: some
  clients join the server's name and a tool's into one name of limited
  length.
- Never put the app key or secret in a client's configuration, which
  clients keep in plain text, nor `SCHWAB_BASE_URL`, which the server
  ignores: it takes the API's address from `config.json` alone.
- Never add the server to a project's configuration that version control
  shares: add it for your user.

### A desktop client

A desktop client takes the server in its JSON configuration, under
`"mcpServers"`, with each path one element of `args`. Restart the client
after you change it.

```json
{
  "mcpServers": {
    "schwab": {
      "command": "/Users/you/go/bin/schwab-mcp",
      "args": ["--config", "/Users/you/Library/Application Support/schwabtx/config.json"]
    }
  }
}
```

### A command-line agent

A command-line agent takes the server through its add command. Put the
agent's own command in place of `<agent>`, and keep each path in quotes,
as setup prints it: unquoted, the path splits at its space into a
`--config` that names no file and an argument the server does not take.
If the agent asks for a scope, choose one that version control does not
share.

```sh
<agent> mcp add schwab -- '/Users/you/go/bin/schwab-mcp' --config '/Users/you/Library/Application Support/schwabtx/config.json'
```

### An editor extension

An editor extension takes the server under `"servers"` in its MCP
configuration, with `"type": "stdio"`. Use your user's configuration, not
a workspace's.

```json
{
  "servers": {
    "schwab": {
      "type": "stdio",
      "command": "/Users/you/go/bin/schwab-mcp",
      "args": ["--config", "/Users/you/Library/Application Support/schwabtx/config.json"]
    }
  }
}
```

### Check it

`schwab-mcp --check` prints what the `auth_status` tool answers, for a
person, and sends nothing to Schwab:

```
$ schwab-mcp --config '~/Library/Application Support/schwabtx/config.json' --check
schwab-mcp (devel)
Reading ~/Library/Application Support/schwabtx/config.json:
  app key      set (config file)
  API          api.schwabapi.com
  token file   ~/Library/Application Support/schwabtx/token.json (config file)
  rate file    ~/Library/Caches/schwabtx/rate-47c1c724e6b8353a (config file)
  store        ~/Library/Application Support/schwabtx/history.db (config file), usable; 9 waiting for you in schwabtx status
  login        made 2026-11-14 19:05 ET, lapses 2026-11-21 19:05 ET, in 168 hours
Nothing to do: the server can read Schwab.
```

`--config` takes a leading `~/` as your home folder, quoted or not.

Then ask the model to call `auth_status`, which answers the same in the
client. Every tool is read-only and declares a closed world: it reads only
Schwab's API for your accounts, and the local files. So an editor client
does not ask you to review each result before the model may use it, though
a client may still ask before each call until you allow the server's tools.

Schwab ends a login after 7 days. Within 48 hours of the end, every answer
that reads Schwab carries the warning `login_expires_soon`; run
`schwabtx login` again in your terminal. The server reads the new login at
its next call, with no restart.

## The tools

| Tool | What it answers | Ask it |
| --- | --- | --- |
| `auth_status` | Whether the server can read Schwab: the login and when it lapses, the config, token, rate-limit and store files it uses, and how many items of the local history wait for you. It sends nothing to Schwab. | "Is my Schwab login still good?" |
| `list_accounts` | The accounts the login shares: masked number, account hash, nickname, CASH or MARGIN, whether it is the primary account, the kind you gave it in the local history, and its value and cash, with totals. | "Which accounts do I have, and what are they worth?" |
| `get_balances` | Each account's value, cash and buying power, the figures of `schwabtx balances`, with the totals of value and cash; `detail: "full"` adds each current balance figure that Schwab sent and `schwabtx` reads, by Schwab's name. | "How much cash can I trade with in my IRA?" |
| `get_positions` | What each account holds, the figures of `schwabtx positions`: quantity, average price, market value, cost, open profit or loss, and each position's share of its account and of every account, with totals. `symbol` keeps one symbol and its options. | "What do I hold, and what is my largest position?" |
| `get_transactions` | The transactions between two New York dates, the last 90 days unless asked, newest first, with totals per category over every page, and a coverage row per account that says where its rows came from and which days they cover. Filters: account, categories, Schwab's types, symbol. | "How much did I get in dividends this year?" |
| `get_transaction` | One transaction in full: every leg, the settlement date, the order ID, what the local history says of it (counted or not and why, its pair, its matches and its flags), and with `include_raw` Schwab's JSON of it. | "What were the fees on that trade?" |

`get_transactions` reads each account from the local history when the
history holds every asked day of it whole, and else from Schwab, but never
one account's days from both: only the history's matcher can tell a
History row from Schwab's row of the same trade a day apart, and a mix
would count it twice. Until `schwabtx sync` exists, the whole days are
those of schwab.com's History exports that you said hold every
transaction, with the `schwabtx edit F... -completeness complete` that
`schwabtx import` prints for each; so a call for an account's older years
can read them from the history, while one for its recent days reads
Schwab. `source` can ask for `store` or `schwab` instead of `auto`. One
call reads at most 724 days of an account from Schwab; for older years,
import schwab.com's History exports with `schwabtx import` and ask with
`source: "store"`.

## Arguments and answers

- **Accounts.** `account` takes an account's `account_hash`, from
  `list_accounts`, or 12 or more of its first characters; the last 3 or
  more digits of its number (`135` or `...135`); its store ref (`A1`); or
  its nickname. Left out, a tool reads every account. An account is never
  guessed: an argument that matches none is `account_not_found`, which
  lists the accounts, and one that matches several is
  `account_ambiguous`.
- **Detail.** Answers are compact; `detail: "full"` adds fields.
- **Pages.** `get_positions` and `get_transactions` answer a page at a
  time: pass `next_cursor` back as `cursor`, with the same other arguments,
  until it is null. `limit` caps a page's rows. A page also stops at about
  20,000 bytes. When the call's time runs out before Schwab answered for
  every asked day, the page holds what came, `complete` is false, the
  totals are null, the warning `partial` says so, and `next_cursor` goes
  on from there.
- **Cuts.** An answer that would pass the 24,000-byte cap on its own has
  its longest lists cut, halved until it fits: `get_balances`' figures,
  and the legs of `get_transactions` and `get_transaction`. `cut` names
  each list, with how many items it shows of how many, and the warning
  `list_cut` says so. The accounts, the coverage and the totals are never
  cut.
- **Missing figures.** A figure Schwab did not send is null, never 0, and
  a total with a missing part is null; a warning says which.
- **Times and money.** Times are New York time with their offset; dates
  are New York dates. Money is in dollars, positive into the account.
- **Warnings.** Every answer has a list of warnings, empty when there are
  none (see [Warnings](#warnings)).

## Resources

| URI | What it holds |
| --- | --- |
| `schwab://accounts` | What `list_accounts` answers. |
| `schwab://accounts/{account}/positions` | The first page of `get_positions` for one account, where `{account}` is its `account_hash` or 12 or more of its first characters, its store ref (`A1`), or the last 3 or more digits of its number; not a nickname. `get_positions` gives the rest. |
| `schwab://reference/transaction-types` | Schwab's transaction types, what a transaction row holds, and the local history's categories, as Markdown. |

The answers of the account resources are private to you, and are not to
be kept; the reference page is fixed text, which a client may keep for a
day. Nothing a model needs exists only as a resource, since some clients
show resources only when you attach them.

## The prompt

### `portfolio_review`

A review of your accounts: their values and cash, the largest positions
and their share of the portfolio, open profit or loss, and the last 90
days' deposits, withdrawals, dividends, interest and fees, with the tools
to call for each. Both arguments are one word, and may be left empty:

| Argument | What it takes |
| --- | --- |
| `account` | One account: the last 3 or more digits of its number, its store ref (`A1`) or its `account_hash`; every account when empty. |
| `focus` | One of `allocation`, `risk`, `income`, `activity` or `cash`. |

## Errors

A tool that fails answers an error result, never a protocol error: one
text block that starts with its code in brackets, then what happened and
what to do, as in `[busy] 4 calls are already running; try again in a
moment.`, and Schwab's own words after `Detail:` when it sent some. The
codes are fixed; their sentences may change.

| Code | What it means, and what to do |
| --- | --- |
| `not_configured` | The server has no app key and secret: run `schwabtx setup`, and start the server with `--config` and the path it prints. |
| `config_unreadable` | The server cannot use its config: the file is missing, cannot be read, is not a config file, or was written by a newer `schwabtx`; it names a path the server may not use, or a token file's path that it cannot follow, as when a link leads back to itself; or it names an API address that the server may not send the login to. Or the client started the server with an argument or a flag's value it does not take, usually a path with a space that was not quoted. Run `schwabtx setup` again, mend the path, or quote it. |
| `not_logged_in` | No login is saved at the token file the server reads. Run `schwabtx login` in your terminal; if `schwabtx` works there already, run `schwabtx setup` there too, and restart the server. |
| `login_expired` | The login has ended, after its 7 days, or because a login elsewhere replaced it. Run `schwabtx login` in your terminal. |
| `app_rejected` | Schwab refused the app key or secret. Check them, run `schwabtx setup` again, and check that the app says Ready For Use on developer.schwab.com. |
| `app_not_ready` | Schwab answers so until the app says Ready For Use, and for an account type its API does not serve. |
| `token_refused` | Schwab refused a token it had just issued. |
| `rate_limited` | Schwab is limiting the app key's requests, which every program that uses the key shares. Try again in a minute. |
| `edge_refused` | Schwab's network turned the request away; that usually passes within hours. |
| `timed_out` | The call's time, 45 seconds unless `--call-timeout` says otherwise, ran out. Try again, or ask for less. |
| `busy` | As many calls are running as the server takes at once, 4 unless `--max-calls` says otherwise. Try again in a moment. |
| `schwab_unavailable` | Schwab could not be reached, or did not answer properly. Try again later. |
| `schwab_refused` | Schwab refused the request; `Detail:` says what Schwab said. |
| `too_big` | The answer is too big even cut, or the login shares more accounts than the 50 that `list_accounts` lists. Ask for less: one account, or a shorter period. |
| `account_not_found` | No account matches the `account` argument; the error lists the accounts. |
| `account_ambiguous` | The `account` argument matches several accounts; give more of the last digits, or the `account_hash`. |
| `account_not_shared` | Schwab no longer shares the account, or a tool that reads Schwab was given one that only the local history knows: run `schwabtx login` and tick it; `get_transactions` with `source` `store` reads its local history. |
| `not_found` | Schwab or the local history has no transaction of that ID. |
| `bad_argument` | An argument is not one the tool takes; the error says what it takes. |
| `bad_cursor` | The cursor belongs to other arguments or another tool; call again without it. |
| `no_store` | There is no local history yet; `schwabtx import` makes one. |
| `store_newer` | A newer `schwabtx` wrote the local history: install `schwab-mcp` of the same version, and restart it. |
| `store_needs_update` | An older `schwabtx` last wrote the local history: run `schwabtx status` once in your terminal, which brings it up to date; the server never changes it. |
| `store_busy` | Another `schwabtx` kept the local history locked. Try again shortly. |
| `store_unusable` | The local history cannot be read: it is damaged, on a network drive, or not a `schwabtx` store; `schwabtx status` says what to do. |
| `internal` | A bug of the server's; it is still running. |

When `get_transactions` reads several accounts and some of them fail, it
answers with the others: each that failed has the error's code in its
coverage row, and the warning `account_failed` says why.

## Warnings

| Code | When |
| --- | --- |
| `login_expires_soon` | The login lapses within 48 hours: run `schwabtx login` before then. |
| `nicknames_missing` | Schwab's preferences, which hold the nicknames, could not be read, so every nickname is null. |
| `extended_hours` | The values are as of now, outside the regular session, at extended-hours prices. |
| `figures_missing` | Schwab sent no value of a figure, so it is null, and so is any total that needs it. |
| `cash_differs` | Schwab's cash for an account is not its value less its holdings, so it is null. |
| `problems` | Records differed from what Schwab has sent before; a figure that could not be read is null. |
| `api_unavailable` | Schwab could not be read, so the answer comes from the local history; the reason follows. |
| `store_unavailable` | The local history could not be read, so what the answer would take from it is left out, or read from Schwab; the reason follows. |
| `store_newer_rules` | A newer `schwabtx` derived the local history: install `schwab-mcp` of the same version. |
| `store_waiting` | Rows of the local history in these days wait for your decision in `schwabtx status`, and are not counted. |
| `store_coverage_partial` | The local history does not hold every asked day of an account whole, or does not know the account at all, so rows of those days may be missing. |
| `store_covers_some` | An account was read from Schwab: the local history holds some of the asked days, but not every one whole. |
| `before_decisions` | Rows read from Schwab carry the rules' category, before your decisions and the local history's pairing. |
| `empty_not_proof` | Schwab refused an older window as too old, or sent nothing for it, which does not show that there is nothing older. |
| `partial` | The call's time ran out before every asked day was read, and `next_cursor` goes on from there; or Schwab sent its most rows for part of a window, so some may be missing. Totals are null. |
| `account_failed` | One account could not be read; its row has the error's code. |
| `types_filter` | `types` leaves out the local history's rows of History exports, which have no Schwab type. |
| `raw_omitted` | Schwab's JSON was left out to stay under the size cap. |
| `list_cut` | Lists were cut to stay under the size cap; `cut` names each. |
| `value_withheld` | The server withheld a value it should not show: a bug of the server's. |

## Privacy

What reaches the model: account numbers masked as `...` and the last three
digits, Schwab's account hashes, your nicknames and the kinds you gave
accounts, figures, symbols, Schwab's descriptions of transactions, dates,
and paths, with your home folder written `~`, each run of 8 or more digits
and each account number the server has seen in a path's names masked, and
a path outside your home folder shown by its file name alone; the name
that `schwabtx setup` gives the rate-limit file, `rate-` and 16 hex digits
of a hash of the app key, is shown whole. Descriptions and nicknames are
data, which the server's instructions tell the model not to take as
instructions.

What never does: a full account number, the `user` details that Schwab's
spec puts on a transaction (a person's name and login), the app key and
secret, and the login's tokens. `include_raw` withholds a value of
Schwab's JSON under a key the server does not know, unless it is a time,
a code, a number that cannot be an account number or a number already
masked. A last pass over every string the server sends replaces any
account number it has seen, the key, the secret and the tokens, and the
tests fail if it ever finds one.

The server reads the local history with a reader that cannot write: it
never creates, changes or upgrades the store.

It logs to stderr, never to stdout, which carries the protocol alone. Some
desktop clients keep each server's stderr in a file. Every line has the
account numbers the server has seen, the key, the secret and the tokens
replaced, and your home folder written `~`; text from outside, such as an
error's message, has every number of 8 or more digits masked too. A line
can still hold symbols, dates, masked numbers and paths outside your home
folder: read a log before you attach it to a bug report.

## Limits

- A page of `get_positions` or `get_transactions` stops at about 20,000
  bytes, and no answer passes 24,000 bytes; one that would is cut (see
  [Cuts](#arguments-and-answers)) or, with nothing left to cut, is
  `too_big`.
- Each call has 45 seconds (`--call-timeout`, 5 s to 240 s); a walk over
  Schwab's windows that runs short answers with what it has.
- 4 calls run at once (`--max-calls`, 1 to 16); a call waits at most 10
  seconds for its turn, then answers `busy`. `auth_status` never waits.
- Schwab ends a login after 7 days.
- One call reads at most 724 days of an account from Schwab.

## Troubleshooting

- **"There is no Schwab login" though `schwabtx` works in your
  terminal.** The server reads the token file that `config.json` names,
  and `schwabtx` there uses another, from `SCHWAB_TOKEN_FILE` or the
  default. Run `schwabtx setup` in that terminal, which saves the paths it
  uses, and restart the server.
- **The client shows only "failed", or the server has no tools.** The
  client could not start the command: check that its path is absolute and
  right, and run it with `--check` in a terminal.
- **`config_unreadable`, saying the server was started with an argument
  it does not take.** A path with a space was not quoted, so it split into
  a `--config` of its first part and a stray argument. Quote it in a shell
  line, or give it as one element of `args`, as `schwabtx setup` prints
  it; `auth_status` lists every problem:

  ```
  [config_unreadable] The server was started with an argument it does not take; a path that holds a space must be quoted in a shell line, or be one element of a JSON `args` list, as `schwabtx setup` prints it.
  ```

- **The store "was last written by an older schwabtx"
  (`store_needs_update`).** Run `schwabtx status` once in your terminal;
  the server never changes the store.
- **The store "was written by a newer schwabtx" (`store_newer`).** Install
  `schwab-mcp` from the same checkout as `schwabtx`, with the `go install`
  line above, and restart it.
- **Where the logs are.** On the server's stderr, which the client keeps:
  a desktop client usually in a log file of each server's, which its
  documentation names. `--log-level info` adds a line as the server starts
  and one per call.

## Running the server

The client runs the command with these flags; no flag takes a secret, and
only `--config` takes a path. The [command's
README](../cmd/schwab-mcp/README.md) has the config file, the exit codes
and the logs.

```
--config PATH      the config file that schwabtx setup saved
--call-timeout D   each call's budget, from 5s to 240s (default 45s)
--max-calls N      how many calls may run at once, from 1 to 16 (default 4)
--log-level L      debug, info, warn or error (default warn)
--check            print what auth_status would say, for a person, and exit
--version          print the version and exit
```

## Later

These come with later stages of milestone 3 and are not served yet; their
names are fixed now.

- **Orders**: `get_orders` and `get_order`, and the reference pages
  `schwab://reference/order-types` and `schwab://reference/order-statuses`.
  An order's tag will be shown only as its origin.
- **Market data**: `get_quotes`, `get_price_history`, `get_option_chain`,
  `get_option_expirations`, `search_instruments`, `get_market_hours` and
  `get_movers`, which need the Market Data product on the Schwab app; an
  app without it will get `market_data_not_enabled`.
- **Updating the local history**: `sync_history`, once `schwabtx sync`
  exists.
