package history

import (
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"
)

// walFrames returns how many frames the store's -wal holds, from its size.
func walFrames(t *testing.T, s *Store) int64 {
	t.Helper()
	fi, err := os.Stat(s.Path() + "-wal")
	if err != nil || fi.Size() == 0 {
		return 0
	}
	page := storeInt(t, s, "PRAGMA page_size")
	return (fi.Size() - 32) / (page + 24)
}

// truncateWAL checkpoints the store and empties its -wal, so that what
// the next write adds can be measured.
func truncateWAL(t *testing.T, s *Store) {
	t.Helper()
	var busy, frames, done int
	if err := s.w.QueryRowContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &done); err != nil || busy != 0 {
		t.Fatalf("checkpointing the store: %v (busy %d)", err, busy)
	}
	if n := walFrames(t, s); n != 0 {
		t.Fatalf("the -wal holds %d frames after a checkpoint that truncates it", n)
	}
}

// Deriving an account whose evidence did not change writes next to
// nothing: fewer than 20 frames to the -wal. Deriving one that holds a
// transaction of 2 MB of raw JSON (first and latest) and a 5 MB file, or
// rebuilding the whole store, rewrites no page of either: the -wal stays
// under 1 MB.
func TestDerivationWritesLittle(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ctx := t.Context()
	start := time.Date(2023, 1, 1, 5, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	big := strings.Replace(cashRaw(1, time.Date(2026, 8, 3, 15, 0, 0, 0, time.UTC)),
		`"subAccount": "CASH",`, `"subAccount": "CASH", "padding": "`+strings.Repeat("x", 1<<20)+`",`, 1)
	rows := append(fixtureRows(t, individual), decodeRow(t, big))
	record(t, s, listRead(individual, start, end, sent, rows...))
	record(t, s, listRead(options, start, end, sent, fixtureRows(t, options)...))
	content := []byte(strings.Repeat("Date,Action,Symbol,Description,Quantity,Price,Fees & Comm,Amount\n", 5<<20/64))
	sum := sha256.Sum256(content)
	if err := s.withWrite(ctx, "importing a file", func(w *wtx) error {
		_, err := w.exec(`INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at)
			VALUES (1, 'history_csv', 'big.csv', ?, ?, ?)`, content, sum[:], instant(w.now))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	touch := func(w *wtx) error {
		w.touch(1)
		w.touch(2)
		return nil
	}
	if err := s.withWrite(ctx, "deriving", touch); err != nil {
		t.Fatal(err)
	}
	truncateWAL(t, s)
	if err := s.withWrite(ctx, "deriving", touch); err != nil {
		t.Fatal(err)
	}
	if n, page := walFrames(t, s), storeInt(t, s, "PRAGMA page_size"); n >= 20 || n*page >= 1<<20 {
		t.Errorf("deriving two accounts that did not change wrote %d frames, want fewer than 20", n)
	}
	truncateWAL(t, s)
	if err := s.withWrite(ctx, "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if n, page := walFrames(t, s), storeInt(t, s, "PRAGMA page_size"); n*page >= 1<<20 {
		t.Errorf("rebuilding the store wrote %d frames of %d bytes, a page of evidence among them", n, page)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM txn"); n != 15 {
		t.Errorf("after the rebuild the store derives %d transactions, want 15", n)
	}
}
