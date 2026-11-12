package history

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// readAt returns a read sent at at and answered a second later.
func readAt(at time.Time) Read {
	return Read{SentAt: at, AnsweredAt: at.Add(time.Second)}
}

// The accounts shared at login are added with their hashes, each hash
// kept with when it was first and last seen, so that one that comes back
// is the account's latest again. The preferences give the accounts the
// store holds their nicknames, masked as free text is, and add no account.
// A refused read is recorded and changes nothing. An account recorded as
// not shared says since when, until it is shared again.
func TestAccounts(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ctx := t.Context()
	var numbers []schwab.AccountNumber
	if err := json.Unmarshal(schwabtest.Fixture(t, "account_numbers.json"), &numbers); err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := s.RecordAccountNumbers(ctx, readAt(t1), numbers); err != nil {
		t.Fatal(err)
	}
	moved := slices.Clone(numbers)
	moved[0].HashValue = "ANOTHERHASH"
	if err := s.RecordAccountNumbers(ctx, readAt(t1.Add(time.Hour)), moved); err != nil {
		t.Fatal(err)
	}
	refused := Read{
		SentAt: t1.Add(90 * time.Minute), AnsweredAt: t1.Add(90 * time.Minute),
		Err: &schwab.Error{Status: 401, CorrelID: "c-1", Message: "Unauthorized"},
	}
	if err := s.RecordAccountNumbers(ctx, refused, []schwab.AccountNumber{{AccountNumber: "11111111", HashValue: "X"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAccountNumbers(ctx, readAt(t1.Add(2*time.Hour)), numbers[:1]); err != nil {
		t.Fatal(err)
	}
	var prefs schwab.UserPreference
	if err := json.Unmarshal(schwabtest.Fixture(t, "user_preference.json"), &prefs); err != nil {
		t.Fatal(err)
	}
	prefs.Accounts[1].NickName = "Joint 1234-5678"
	prefs.Accounts = append(prefs.Accounts, schwab.PreferenceAccount{AccountNumber: "22222222", NickName: "Unknown"})
	if err := s.RecordPreferences(ctx, readAt(t1.Add(3*time.Hour)), prefs); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNotShared(ctx, numbers[2].AccountNumber, t1.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNotShared(ctx, "33333333", t1); err == nil || !strings.Contains(err.Error(), "...333") {
		t.Errorf("SetNotShared of an account the store does not hold: %v", err)
	}

	got := storeStrings(t, s, `SELECT ref || ' ' || account || ' ' || coalesce(nickname, '-') || ' ' || hash || ' ' ||
		first_seen_at || ' ' || last_seen_at || ' ' || coalesce(not_shared_since, '-') || ' ' || primary_account || ' ' ||
		coalesce(lot_method, '-') || ' ' || used FROM accounts ORDER BY account_id`)
	want := []string{
		"A1 ...135 Individual " + string(numbers[0].HashValue) + " " + instant(t1.Add(time.Second)) + " " + instant(t1.Add(2*time.Hour+time.Second)) + " - 1 FIFO 1",
		"A2 ...398 Joint ...678 " + string(numbers[1].HashValue) + " " + instant(t1.Add(time.Second)) + " " + instant(t1.Add(time.Hour+time.Second)) + " - 0 FIFO 1",
		"A3 ...783 Contributory " + string(numbers[2].HashValue) + " " + instant(t1.Add(time.Second)) + " " + instant(t1.Add(time.Hour+time.Second)) + " " + instant(t1.Add(4*time.Hour)) + " 0 - 1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the accounts view:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	hashes := storeStrings(t, s, "SELECT hash || ' ' || first_seen_at || ' ' || last_seen_at FROM account_hash WHERE account_id = 1 ORDER BY hash")
	if want := []string{
		string(numbers[0].HashValue) + " " + instant(t1.Add(time.Second)) + " " + instant(t1.Add(2*time.Hour+time.Second)),
		"ANOTHERHASH " + instant(t1.Add(time.Hour+time.Second)) + " " + instant(t1.Add(time.Hour+time.Second)),
	}; !slices.Equal(hashes, want) {
		t.Errorf("account 1's hashes: %q, want %q", hashes, want)
	}
	if got := storeStrings(t, s, "SELECT outcome || ' ' || http_status || ' ' || correlation_id || ' ' || row_count FROM api_read WHERE outcome <> 'answered'"); !slices.Equal(got, []string{"refused 401 c-1 0"}) {
		t.Errorf("the refused read is recorded as %q", got)
	}
	if err := s.SetNotShared(ctx, numbers[2].AccountNumber, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM account WHERE not_shared_since IS NOT NULL"); n != 0 {
		t.Errorf("%d accounts are still not shared, want none", n)
	}
	if err := s.RecordPreferences(ctx, Read{SentAt: t1, AnsweredAt: t1.Add(-time.Second)}, prefs); err == nil {
		t.Error("a read answered before it was sent was recorded")
	}
	failed := Read{SentAt: t1, AnsweredAt: t1, Err: errors.New("dial tcp: connection refused to 12345678")}
	if err := s.RecordPreferences(ctx, failed, prefs); err != nil {
		t.Fatal(err)
	}
	if got := storeStrings(t, s, "SELECT outcome || ': ' || error FROM api_read WHERE outcome = 'failed'"); !slices.Equal(got,
		[]string{"failed: dial tcp: connection refused to ...678"}) {
		t.Errorf("the failed read is recorded as %q, want its error masked", got)
	}
}
