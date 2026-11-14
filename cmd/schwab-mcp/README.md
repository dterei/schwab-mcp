# schwab-mcp

An MCP server that reads your Charles Schwab accounts for an MCP client:
a desktop client, a command-line agent or an editor extension starts it
and speaks the protocol on its stdin and stdout. It reads balances,
positions and transactions from the Schwab Trader API, and the local
history that `schwabtx` keeps. It only reads: it never places, changes or
cancels an order, never writes the history, and never logs in.

[docs/mcp.md](../../docs/mcp.md) describes the tools, how to add the
server to each kind of client, and what reaches the model. This page
covers the command itself.

## Set up

Install both commands from one checkout, the same version, so that they
agree on the store:

```sh
go install ./cmd/schwab-mcp ./cmd/schwabtx
```

Then, once, in your own terminal, where `schwabtx` works:

```sh
schwabtx setup     # saves config.json, and prints the command and arguments for a client
schwabtx login     # the browser login, which Schwab ends after 7 days
schwab-mcp --config /abs/path/to/config.json --check
```

The server cannot log in: when the login is missing or has lapsed, every
tool says so and asks you to run `schwabtx login` in your terminal.

## Flags

```
--config PATH      the config file that schwabtx setup saved (default: schwabtx/config.json
                   in your user config directory); a leading ~/ is your home folder
--call-timeout D   each call's budget, from 5s to 240s (default 45s)
--max-calls N      how many calls may run at once, from 1 to 16 (default 4)
--log-level L      debug, info, warn or error (default warn); info adds a line per call
--check            print what auth_status would say, for a person, and exit
--version          print the version and exit
```

No flag takes a secret, and only `--config` takes a path. Give the
absolute path that `schwabtx setup` prints: a client may start the server
in any folder, `/` included. A path that holds a space, as the default on
macOS does (`~/Library/Application Support`), must be quoted in a shell
line, or be one element of a JSON `args` list.

A command typed wrong in a terminal gets what is wrong and the usage on
stderr, and exit code 2. A client that starts the server with a flag or
an argument it cannot use does not stop it, since a client shows only
"failed" for a server that exits: the server starts with the flags it
could read, `auth_status` lists the problem, and every tool answers
`[config_unreadable]` with how to fix it.

`--check` prints, for a person, what the `auth_status` tool answers: the
config file, where the app key and secret came from, the API's host, the
token, rate-limit and store files with where each setting came from, the
store's state, the login and when it lapses, and what the model would be
asked to tell you. It sends nothing to Schwab and prints no secret.

## The config file

`schwabtx setup` saves `config.json`, readable only by you, in
`schwabtx` in your user config directory: the app key and secret, and the
token file, the store and the rate-limit file as `schwabtx` finds them in
that terminal, each an absolute path. The server reads it once, as it
starts, and never writes it.

The key and secret, the token file and the store come from their
`SCHWAB_*` variable when the client's environment sets it (a path only
when it is absolute), then from `config.json`, then from the default. The
rate-limit file and the API's address come from `config.json` alone,
never from a variable: `base_url` is there only to run against the tests'
fake, and plain `http` is refused but to the loopback address. `--check`
and `auth_status` say where each setting came from, and name the API's
host when it is not Schwab's.

A problem with the file does not stop the server either: a file that
`--config` names and that does not exist, one that is not a config file,
one that a newer `schwabtx` wrote, a relative path, or one that others
could read, which the server makes readable only by you. `auth_status`
lists each, and the tools say what to do.

## Exit codes

| Code | When |
| --- | --- |
| 0 | stdin ended, or a SIGINT or SIGTERM came; and after `--check`, `--version` and `-h` |
| 1 | the server could not start, or the session failed, as on a line that is not JSON; one line on stderr says why |
| 2 | a command line typed wrong in a terminal; the usage is on stderr |

A signal ends the running calls at once: the server exits as soon as
they have, and 250 ms after the signal whatever still runs.

## Logs

The server logs to stderr, never to stdout, which carries the protocol
alone. At the default level, `warn`, it logs what went wrong and what it
could not use; `info` adds a line as it starts, with its version and where
each setting came from, and a line per call, with the tool, how long it
took, its outcome and its size. No line holds a call's arguments or
answer, Schwab's bodies, a token, or the app key or secret.

Every line has the account numbers that the server has seen, the key, the
secret and the tokens replaced, and your home folder written `~`. Text
from outside, such as an error's message, is masked by pattern too, so a
number of 8 or more digits shows as its last three. A line can still hold
symbols, dates, masked numbers and paths outside your home folder: read a
log before you attach it to a bug report. Some desktop clients keep each
server's stderr in a file.

Whatever else writes to stdout, in the server or in a library it links,
reaches stderr instead: before it serves, the server points the process's
stdout at stderr, and keeps the original for the protocol alone.
