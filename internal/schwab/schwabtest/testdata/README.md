# The fake Schwab's fixtures

These files are answers in the shape Schwab sends them. They are embedded in
package schwabtest, and `Fixture` reads one byte for byte. `.gitattributes`
keeps their bytes, line endings included, in every checkout.

## What each file is

`index.json` has one entry per file, but itself and this README: the request
the file answers, the answer's status and headers, and where the file came
from. `Index` reads it.

Every file here today is made by hand; none is a recording.

- `account_numbers.json`, `accounts.json`, `transactions.json`,
  `transaction.json` and `user_preference.json` are answers of the account
  endpoints, written for three accounts. They are modeled on the public
  captures that docs/schwab-api.md cites, with every key Schwab is known to
  send, so that tests of the records see each one. `transactions.json`
  holds rows of two accounts over three years, which no one request
  returns. `transaction.json` sends `assetType` twice in one instrument, as
  a capture of 2024 does.
- `errors/` holds Schwab's error bodies and pages, rebuilt from community
  logs and from the gateway's documented faults. Each entry's `source` says
  which, and whether Schwab has been seen sending it. The refusals of the
  token endpoint are among them.

Every identifier in them is a fake: account numbers are
`FixtureAccounts`' or `SpareAccountNumbers`, a hash is `HashFor` of an
account number, the streamer's IDs are `StreamerCustomerID` and
`StreamerCorrelID`, an ID in an error body or a correlation header is a UUID
of the form `00000000-0000-4000-8000-NNNNNNNNNNNN` or a canned answer's
fixed ID (`corr-<status>`, `req-429`), and the one Akamai Reference is
`AkamaiReference`.

## Rules

schwabtest's tests check that:

- the files on disk are the embedded ones, and no name starts with `.` or
  `_` (`go:embed` would leave such a file out without `all:`, though git
  commits it);
- every file but this README and `index.json` has one entry, and every entry
  a file and a source;
- each file holds as many carriage returns as its entry's `cr` says;
- an entry's `headers` hold only headers whose values `KeptHeader` keeps,
  but `Content-Length`, whose name goes in `otherHeaders` with the names of
  all the rest, and every correlation ID is a fake.

## Recordings

A recording of Schwab's real answers may join these files, but only once it
is scrubbed:

- Every identifier is replaced by a fake, consistently across the
  recording: account numbers by `FixtureAccounts`' and then the spares', in
  order, hashes by their `HashFor`, activity, order and position IDs by
  fakes of the same length, nicknames by `Account N`, the streamer's and the
  correlation IDs by the fakes above.
- Every value at every key path is classified: mapped to a fake, kept when
  it matches its grammar (an enum, a symbol, a time), or replaced by a
  placeholder (`DESCRIPTION N`, `MESSAGE N`) unless it is wording known to
  be Schwab's. A value nothing classifies refuses the file.
- Amounts are refused unless the owner chooses to keep them or to scale
  them; scaling hides little, so that choice is the owner's alone.
- The scrubbed bytes are spliced, never re-encoded, so that key order,
  repeated keys, number literals and escapes stay as Schwab sent them.

A scrubbed recording goes under `recorded/<date>/`, the day it was recorded,
beside the hand-made files and never over them: the hand-made set stays as
it is for the tests that pin its values. Each of its files
gets its own entry in `index.json`, with source `recorded`, its date in
`recorded` and the scrubber's version and amounts policy in `scrub`.

Nothing raw or partly scrubbed may be committed here, not even for a moment:
a file once pushed stays in the history and in every module download.
