// Package schwab is a client for the Charles Schwab Trader API: the browser
// login, the refresh of the access token, the token file that every process
// using the login shares, a Client that reads from the API, trying again
// after a failure where that is safe, under a rate limit that every process
// using the app key shares, and the records that the account endpoints
// return.
//
// Processes share the login through the token file. A lock file next to it
// lets one process at a time decide whether to refresh, refresh, or replace
// the login, and a process re-reads the file under the lock first, because
// another process may have refreshed the token, or logged in again, since it
// last looked. The lock is held for a file read, at most one token request
// and a file write, and nothing else is locked while it is held.
//
// They share the rate limit through a small file of its own, which each
// request locks, reads and rewrites; see ClientConfig.
//
// # Records
//
// AccountNumber, Account, Position, Transaction and UserPreference are
// written by hand, since Schwab's answers differ from its spec, and they
// decode leniently, so that a change at Schwab costs a field rather than an
// answer:
//
//   - Keys that the types do not know are ignored. A record's Raw keeps the
//     exact bytes that Schwab sent for it, unknown keys included.
//   - A value of the wrong JSON type leaves its field zero, or nil, and is
//     named in the record's Problems, as is a key that every record of its
//     kind has and this one lacks. A record that is null, or not an object,
//     decodes to its zero value, with a problem. Decoding a record never
//     fails, so one odd record costs only itself.
//   - A field that Schwab sends on every record of its kind is a plain
//     value. A balance, or a flag that Schwab sends only on some records, is
//     a pointer, nil when Schwab did not send it, since 0 and false are real
//     values there. Any other field that Schwab sends only on some records
//     reads as its zero value when absent, and is left out of the record's
//     encoding, so that an encoded record holds only what Schwab sent.
//   - Times are in UTC.
//   - Where a key appears twice in an object, the last value wins for a
//     number, string or boolean, but a duplicated object or array is merged
//     into the first.
//
// A record's Problems cover its own JSON and the objects nested in it, but
// not the Positions inside an Account, which have their own.
//
// To build a type around a record, give it the record as a named field. A
// record embedded in a struct brings its UnmarshalJSON along, which would
// then decode the whole struct as the record.
package schwab
