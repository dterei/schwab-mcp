package schwabfile

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"
)

// SnapshotLine writes one line: the version, the time in UTC to the
// second, and the accounts joined into one list with their white space
// taken out and nothing escaped. ReadSnapshotLines reads back what it
// wrote, each account as it was but for white space.
func TestSnapshotLineRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 7, 0, 5, 0, 987_654_321, time.FixedZone("EST", -5*3600))
	accounts := []json.RawMessage{
		json.RawMessage(`{ "securitiesAccount": {"accountNumber": "29470135", "type": "CASH"},
			"name": "A & B <c> ` + " " + `" }`),
		json.RawMessage(`{"securitiesAccount":{"accountNumber":"61803398"}}`),
	}
	line, err := SnapshotLine(at, accounts)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"fetched_at":"2026-11-07T05:05:00Z","accounts":[` +
		`{"securitiesAccount":{"accountNumber":"29470135","type":"CASH"},"name":"A & B <c> ` + " " + `"},` +
		`{"securitiesAccount":{"accountNumber":"61803398"}}]}` + "\n"
	if string(line) != want {
		t.Fatalf("SnapshotLine wrote\n%s\nwant\n%s", line, want)
	}
	got, skipped := ReadSnapshotLines(line)
	if len(skipped) != 0 || len(got) != 1 {
		t.Fatalf("read %d snapshots and skipped %v, want 1 and none", len(got), skipped)
	}
	if s := got[0]; s.Line != 1 || !s.FetchedAt.Equal(at.Truncate(time.Second)) || s.FetchedAt.Location() != time.UTC {
		t.Errorf("read line %d fetched at %v, want line 1 at %v in UTC", s.Line, s.FetchedAt, at.Truncate(time.Second).UTC())
	}
	for i, a := range got[0].Accounts {
		if w := compact(t, accounts[i]); string(a) != w {
			t.Errorf("account %d reads %s, want %s", i, a, w)
		}
	}
	if _, err := SnapshotLine(at, []json.RawMessage{json.RawMessage(`{"a":`)}); err == nil {
		t.Error("SnapshotLine wrote an account that is not JSON")
	}
	if line, err := SnapshotLine(at, nil); err != nil || !strings.HasSuffix(string(line), `"accounts":[]}`+"\n") {
		t.Errorf("SnapshotLine of no accounts wrote %q, %v", line, err)
	}
}

// compact returns raw without its white space.
func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The lines of every writer of version 1 are read: the json.Encoder of
// PR #4 to #8, which on Go 1.27 escapes U+2028, and json.Compact since,
// each element kept as the file has it, escapes included, but for white
// space. A line of
// another version, a broken line, a line that is not an object or lacks
// what a line has, and a last line without its newline are skipped and
// counted by why; a blank line is passed over. An account whose number is
// a number, or whose positions are a number, is an element like any other:
// the store decides what to make of it.
func TestReadSnapshotLines(t *testing.T) {
	encoder := `{"version":1,"fetched_at":"2026-11-05T05:05:00Z","accounts":[{"securitiesAccount":{"accountNumber":"29470135","type":"CASH"},"note":"a b"}]}`
	lines := []string{
		"\xef\xbb\xbf" + encoder,
		`{"version":1,"fetched_at":"2026-11-06T05:05:00Z","accounts":[{"securitiesAccount":{"accountNumber":29470135}},` +
			`{"securitiesAccount":{"accountNumber":"61803398","positions":[5]}}]}`,
		"",
		`{"version":2,"fetched_at":"2026-11-07T05:05:00Z","accounts":[]}`,
		`{"version":1,"fetched_at":"2026-11-0`,
		`{"version":1,"fetched_at":"2026-11-08T05:05:00Z","accou{"version":1,"fetched_at":"2026-11-08T05:05:01Z","accounts":[]}`,
		`[1, 2]`,
		`{"fetched_at":"2026-11-09T05:05:00Z","accounts":[]}`,
		`{"version":"1","fetched_at":"2026-11-09T05:05:00Z","accounts":[]}`,
		`{"version":1,"accounts":[]}`,
		`{"version":1,"fetched_at":"Nov 9","accounts":[]}`,
		`{"version":1,"fetched_at":"2026-11-09T05:05:00Z","accounts":{}}`,
		`{"version":1,"fetched_at":"2026-11-09T05:05:00Z"}`,
		`{"version":1,"fetched_at":"2026-11-09T05:05:00Z","accounts":null}`,
		`{"version":1.0,"fetched_at":"2026-11-09T05:05:00Z","accounts":[]}`,
		"  \r",
		`{"version":1,"fetched_at":"2026-11-10T05:05:00.5Z","accounts":[ {"a": [1, 2]} ]}` + "\r",
		`{"version":1,"fetched_at":"2026-11-11T05:05:00Z","accounts":[{"securitiesAccount":{}}]}`, // no newline
	}
	got, skipped := ReadSnapshotLines([]byte(strings.Join(lines, "\n")))
	wantSkipped := map[string]int{
		SkipNotJSON: 2, "version 2": 1, `version "1"`: 1, "version 1.0": 1, SkipNotObject: 1, SkipNoVersion: 1,
		SkipNoFetchedAt: 1, SkipBadTime: 1, SkipNotList: 3, SkipPartial: 1,
	}
	if !maps.Equal(skipped, wantSkipped) {
		t.Errorf("skipped %v, want %v", skipped, wantSkipped)
	}
	if len(got) != 3 {
		t.Fatalf("read %d lines, want 3: %+v", len(got), got)
	}
	if g := got[0]; g.Line != 1 || len(g.Accounts) != 1 || !strings.Contains(string(g.Accounts[0]), `"a b"`) {
		t.Errorf("the Encoder's line reads as line %d with %s", g.Line, g.Accounts)
	}
	if g := got[1]; g.Line != 2 || len(g.Accounts) != 2 || string(g.Accounts[0]) != `{"securitiesAccount":{"accountNumber":29470135}}` ||
		string(g.Accounts[1]) != `{"securitiesAccount":{"accountNumber":"61803398","positions":[5]}}` {
		t.Errorf("line 2 reads as line %d with %s", g.Line, g.Accounts)
	}
	if g := got[2]; g.Line != 17 || len(g.Accounts) != 1 || string(g.Accounts[0]) != `{"a":[1,2]}` ||
		!g.FetchedAt.Equal(time.Date(2026, 11, 10, 5, 5, 0, 5e8, time.UTC)) {
		t.Errorf("line 17 reads as line %d at %v with %s", g.Line, g.FetchedAt, g.Accounts)
	}
	long := `{"version":123456789012345678901234567890,"fetched_at":"2026-11-09T05:05:00Z","accounts":[]}` + "\n"
	if _, skipped := ReadSnapshotLines([]byte(long)); skipped["version 12345678901234567890..."] != 1 {
		t.Errorf("a long version is skipped as %v", skipped)
	}
	if got, skipped := ReadSnapshotLines(nil); got != nil || len(skipped) != 0 {
		t.Errorf("an empty file reads as %v, skipping %v", got, skipped)
	}
}

// A snapshot file is one with a line that starts as SnapshotLine starts
// one, wherever that line is, as after a write cut short; a History export
// in JSON or CSV is not.
func TestIsSnapshotFile(t *testing.T) {
	for content, want := range map[string]bool{
		`{"version":1,"fetched_at":"2026-11-05T05:05:00Z","accounts":[]}` + "\n":               true,
		"\xef\xbb\xbf" + `{"version":1,"fetched_at":"2026-11-05T05:05:00Z","accounts":[]}`:     true,
		`{"version":1,"fet` + "\n" + `{"version":1,"fetched_at":"2026-11-05T05:05:00Z"` + "\n": true,
		`{"vers` + "\n" + `{"version":1,"fetched_at":"2026-11-05T05:05:00Z"` + "\n":            true,
		`{"FromDate":"10/01/2026","BrokerageTransactions":[]}`:                                 false,
		`{ "version":1 }`: false,
		"Date,Action,Symbol,Description,Quantity,Price,Fees & Comm,Amount\n": false,
		"": false,
	} {
		if got := IsSnapshotFile([]byte(content)); got != want {
			t.Errorf("IsSnapshotFile(%q) = %v, want %v", content, got, want)
		}
	}
}
