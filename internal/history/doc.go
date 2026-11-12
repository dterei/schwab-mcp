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
// not at all. Beside them are lock files, the one a change of schema takes
// and the one a sync takes (LockSync), and the backups made before a schema
// changed (Companions). Open refuses a -wal or -shm without its
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
//
// # Files from schwab.com
//
// Import takes files by their contents, all of them in one write: snapshot
// files, History exports in CSV and JSON, Realized Gain/Loss exports'
// Lot Details, and `schwabtx transactions -json` output, whose rows are
// Schwab's own and join the API's transactions without changing one the
// store holds. A file's bytes are kept, so a better reader is a rebuild; its
// rows are read once, when it comes. Across files each transaction counts
// once: rows of one date, action, fees and amount are one transaction,
// as many as the most that one file holds, and more only where rows are
// known to be of different securities. A decision on a file transaction
// names one of its rows and follows the transaction that holds it, whichever
// file opened it, and survives that file being set aside (Forget). What
// a file covers is its range clipped by what its warnings show, and its
// completeness is established, by your word or by its agreeing with the
// API's rows or a complete file's, never assumed; until the sync exists, a
// -json file's rows cover the days strictly inside their span.
//
// # Matching and counting
//
// Where the API covers, it is the record. A file transaction that is an
// API transaction, by rules of families, days, amounts and symbols or by
// your decision, is linked to it, and the API's record counts in its
// place. Inside the API's coverage, a file transaction that no API
// transaction is, or that several could be, is held out and flagged until
// you say, so that nothing counts twice whichever way the matching errs;
// outside it, the files are the record. The matching is a function of the
// account's transactions and your decisions, never of what an earlier
// derivation linked.
//
// # What the views show
//
// The views are what other code reads of the store, and none of them reads
// an account number, raw JSON, or an imported file's name or bytes. Each
// of their columns is either free text, masked by pattern when it was
// stored, or an identifier, an amount, a date or a code, which cannot hold
// an account number (viewColumns).
//
// # Upkeep
//
// Rebuild derives everything again from the evidence alone, which gives
// what the derivations at the end of each write gave. Check runs SQLite's
// integrity and foreign key checks and checks the invariants of the
// derived data. Backup copies the store, as of a commit, to a new file
// readable only by you, while other processes go on using it. Restore
// replaces the store's content with a backup's through SQLite's backup
// API, never by copying files: a copy over a database whose killed writer
// left a -wal would be read with that -wal, and other processes' open
// connections would read a file replaced under them. A backup of an older
// schema or rules is brought up to this schwabtx's first, in a copy.
//
// # Exports
//
// Export reads the views that milestones 3 and 4 read, in one read
// transaction, into memory, and only then writes them, in CSV or JSON
// Lines, so that a slow reader of the output holds no transaction open:
// money in dollars and quantities in whole units, formatted from the
// store's integers, in an order that makes an export of a store that has
// not changed the same bytes. A file is written readable only by you,
// through a temporary file renamed over it, and never over one of the
// store's own files. Only when asked does an export hold full account
// numbers: Schwab's raw JSON, a file's rows and a snapshot's elements, or
// each row's number.
package history

import (
	// The driver registers itself with database/sql as "sqlite". It is pure
	// Go, so the commands build without cgo for every system.
	_ "modernc.org/sqlite"
)
