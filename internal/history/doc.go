// Package history is the store: one SQLite file on the local disk. It keeps
// the evidence, which is what Schwab's API returned for each account, the
// snapshots that schwabtx took, the files exported from schwab.com and the
// decisions made about them, and deletes none of it. Beside the evidence, in
// tables of their own, it keeps what versioned rules derive from it, such as
// each transaction's category and which transactions are counted, so that
// new rules can derive it all again.
//
// It reaches nothing over the network and never takes a schwab.Client: a
// sync reads from Schwab and hands the store the answers.
//
// # The store's files
//
// The store is its database and the database's -wal and -shm, which are one
// thing: a store whose last writer was killed may keep committed changes
// only in its -wal, so the three are moved, copied or removed together or
// not at all. Beside them are lock files and the backups made before a
// schema changed (Companions). Open refuses a -wal or -shm without its
// database, or beside an empty one, rather than adopt another store's
// journal.
//
// A process that has the store open never opens any of the three by path:
// on Linux and macOS, closing any descriptor of a file drops every lock that
// SQLite holds on it in that process, and another process can then take
// itself for the last one and remove the journal under it. So the store
// takes the contents of the files it imports, never their paths; a command
// checks its inputs and outputs with IsCompanion and reads every input
// before it opens the store.
//
// # Writes
//
// Each write is one transaction on the one writer connection, and takes the
// write lock at BEGIN, so that no transaction reads and then fails to
// write. SQLite's own wait for a lock polls rather than queues, so a write
// that SQLite reports busy is rolled back and run again after a jittered
// pause, until it lands, its context ends or Options.LockWait has passed
// (ErrBusy). Since a write may run more than once, what it does has no
// effect outside its transaction. Each write first reads the store's
// versions, of its schema, its rules and its calendar, and stops with
// ErrNewer when a newer schwabtx has changed the store meanwhile. Reads use
// a pool of their own, whose connections cannot write.
//
// # Derivation
//
// What one piece of evidence gives by itself, such as a transaction's
// columns and legs from its latest raw JSON, or a snapshot's balances,
// positions and the market date it stands for, is derived when the
// evidence arrives. What relates the evidence of an account, such as which
// transactions a complete read shows gone and which days the API and the
// snapshots cover, is derived again from scratch, for each account whose
// evidence a write changed, at the end of that write; so is which
// instruments' keys name one security, when a write added or changed a key
// or a name, and which market sessions the snapshots span, when a write
// added a snapshot. Only rows that change are written. No derived value
// shares a row with raw JSON or a file's bytes, so deriving never rewrites
// them. A rebuild derives everything again from the evidence alone, and a
// newer NYSE calendar every market date and what depends on it.
//
// # The ledger and your decisions
//
// Each transaction is an entry of the ledger, which versioned rules
// classify (a category and a family), flag for what needs a look, pair
// (sweeps within an account, journals across your accounts, a cancel with
// the trade it cancels) and count. Your decisions (Decide) are a log that is
// never changed, keyed by stable subjects rather than by derived rows; the
// latest of a kind on a subject holds, and the derivation applies it after
// the rules, your category last. A decision on an entry records the
// revision it was made against, and the entry is flagged stale_decision
// when what the rules read of the transaction has changed since. Snapshots
// are flagged too: for what the record could not read, numbers rounded or
// off the grid, an account number that could not be read, two of one
// account at one time that differ, and where they stand in the calendar.
package history

import (
	// The driver registers itself with database/sql as "sqlite". It is pure
	// Go, so the commands build without cgo for every system.
	_ "modernc.org/sqlite"
)
