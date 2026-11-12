// Package schwabfile reads the files that the store imports, taking their
// contents, never their paths. It knows nothing of SQLite or of the store's
// tables. So far it reads the snapshot files that `schwabtx snapshot`
// writes, and writes their lines.
package schwabfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// SnapshotVersion is the version of the line that SnapshotLine writes and
// the only one that ReadSnapshotLines reads. It changes if the fields of a
// line ever do.
const SnapshotVersion = 1

// A Snapshot is one line of a snapshot file: the accounts that GET
// /accounts?fields=positions returned at one time.
type Snapshot struct {
	// Line is where the line is in the file, counting from 1.
	Line int
	// FetchedAt is when the answer came, in UTC: to the second, as
	// SnapshotLine writes it, though a fraction is read.
	FetchedAt time.Time
	// Accounts are the elements of the line's accounts, each as the line
	// holds it with any white space around and inside it taken out
	// (json.Compact), and in their order. An element may be anything that
	// JSON holds, such as an account whose number Schwab sent as a number.
	Accounts []json.RawMessage
}

// SnapshotLine returns the line of a snapshot file for accounts, Schwab's
// answer as it came, fetched at fetchedAt: one JSON object on one line,
// ending in a newline, with the time in UTC to the second and the
// accounts' JSON joined into one list, white space taken out and nothing
// escaped. json.Marshal would escape "&" and the like, and a json.Encoder
// on Go 1.27 still escapes U+2028 and U+2029; json.Compact escapes nothing,
// so the line is the same whatever Go built the tool, and so is what a
// store keeps of it. It fails when an account is not JSON.
func SnapshotLine(fetchedAt time.Time, accounts []json.RawMessage) ([]byte, error) {
	var list bytes.Buffer
	list.WriteByte('[')
	for i, a := range accounts {
		if i > 0 {
			list.WriteByte(',')
		}
		list.Write(a)
	}
	list.WriteByte(']')
	var line bytes.Buffer
	fmt.Fprintf(&line, `{"version":%d,"fetched_at":"%s","accounts":`, SnapshotVersion,
		fetchedAt.UTC().Truncate(time.Second).Format(time.RFC3339))
	if err := json.Compact(&line, list.Bytes()); err != nil {
		return nil, fmt.Errorf("writing the snapshot's accounts: %w", err)
	}
	line.WriteString("}\n")
	return line.Bytes(), nil
}

// Why ReadSnapshotLines skips a line, as the keys of the counts it
// returns. A version other than 1 is counted as "version" and what the
// line has, such as "version 2".
const (
	SkipNotJSON     = "not JSON"
	SkipNotObject   = "not an object"
	SkipNoVersion   = "no version"
	SkipNoFetchedAt = "no fetched_at"
	SkipBadTime     = "fetched_at not a time"
	SkipNotList     = "accounts not a list"
	SkipPartial     = "partial last line"
)

// ReadSnapshotLines reads the lines of a snapshot file, and counts the
// lines it skips by why (the Skip constants). Lines are read whole and only
// lines that end in a newline: the last line without one may be a write
// still in progress, and is skipped. A blank line is passed over and not
// counted. A line is read only when it is a JSON object whose version is
// the number 1, whose fetched_at is an RFC 3339 time and whose accounts is
// a list; any other line, such as one that a newer schwabtx wrote with
// another version, or the start of a write that was cut short, is skipped
// and counted, never guessed at. A UTF-8 byte order mark before the first
// line is passed over.
func ReadSnapshotLines(content []byte) ([]Snapshot, map[string]int) {
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	var out []Snapshot
	skipped := map[string]int{}
	for n := 1; len(content) > 0; n++ {
		line, rest, complete := bytes.Cut(content, []byte("\n"))
		content = rest
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !complete {
			skipped[SkipPartial]++
			break
		}
		s, why := readSnapshotLine(line)
		if why != "" {
			skipped[why]++
			continue
		}
		s.Line = n
		out = append(out, s)
	}
	return out, skipped
}

// readSnapshotLine reads one line, or says why it is skipped.
func readSnapshotLine(line []byte) (Snapshot, string) {
	var s Snapshot
	if !json.Valid(line) {
		return s, SkipNotJSON
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil || fields == nil {
		return s, SkipNotObject
	}
	version, ok := fields["version"]
	if !ok {
		return s, SkipNoVersion
	}
	if v := string(version); v != "1" {
		if len(v) > 20 {
			v = v[:20] + "..."
		}
		return s, "version " + v
	}
	var at string
	if json.Unmarshal(fields["fetched_at"], &at) != nil || at == "" {
		return s, SkipNoFetchedAt
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return s, SkipBadTime
	}
	var accounts []json.RawMessage
	if json.Unmarshal(fields["accounts"], &accounts) != nil || accounts == nil {
		return s, SkipNotList
	}
	s.FetchedAt = t.UTC()
	s.Accounts = make([]json.RawMessage, len(accounts))
	for i, a := range accounts {
		var b bytes.Buffer
		_ = json.Compact(&b, a) // valid: it came from a line that is
		s.Accounts[i] = b.Bytes()
	}
	return s, ""
}

// IsSnapshotFile reports whether content is a snapshot file: whether one
// of its lines starts as SnapshotLine starts a line, with {"version":.
func IsSnapshotFile(content []byte) bool {
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	for line := range bytes.Lines(content) {
		if bytes.HasPrefix(line, []byte(`{"version":`)) {
			return true
		}
	}
	return false
}
