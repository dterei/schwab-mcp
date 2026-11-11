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
package history

import (
	// The driver registers itself with database/sql as "sqlite". It is pure
	// Go, so the commands build without cgo for every system.
	_ "modernc.org/sqlite"
)
