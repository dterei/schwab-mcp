package mcpserver

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// testMasker returns a masker that knows the fixtures' numbers and a home
// folder, home, with the fake's app key and secret.
func testMasker(home string) *masker {
	m := newMasker(home, schwabtest.AppKey, schwabtest.AppSecret)
	for _, a := range schwabtest.FixtureAccounts {
		m.known.Add(a.Number)
	}
	return m
}

// free masks text from outside: runs of 8 or more digits by pattern, the
// known numbers of any length, written whole or in groups, and the
// secrets, before the digits, so that a secret holding a run of digits is
// still found whole. stored masks only the known numbers and the secrets,
// since the store masked its text.
func TestMaskerFreeAndStored(t *testing.T) {
	t.Parallel()
	m := newMasker("", "key12345678", "made-up-secret")
	m.known.Add("1234567")
	for _, tc := range []struct{ in, free, stored string }{
		{"JOURNAL TO 87654321", "JOURNAL TO ...321", "JOURNAL TO 87654321"},
		{"to 1234567 and back", "to ...567 and back", "to ...567 and back"},
		{"Kids 123-4567 gift", "Kids ...567 gift", "Kids ...567 gift"},
		{"to 123 4567, 1 234 567", "to ...567, ...567", "to ...567, ...567"},
		{"the key key12345678 and made-up-secret", "the key [app key] and [app secret]", "the key [app key] and [app secret]"},
		{"2026-09-14", "...914", "2026-09-14"},
		{"AT&T <x>", "AT&T <x>", "AT&T <x>"},
	} {
		if got := m.free(tc.in); string(got) != tc.free {
			t.Errorf("free(%q) = %q, want %q", tc.in, got, tc.free)
		}
		if got := m.stored(tc.in); string(got) != tc.stored {
			t.Errorf("stored(%q) = %q, want %q", tc.in, got, tc.stored)
		}
	}
	m.scrub = func(s string) string { return strings.ReplaceAll(s, "tok-99999999", "[token]") }
	if got := m.free("Bearer tok-99999999"); got != "Bearer [token]" {
		t.Errorf("free with a token = %q, want it scrubbed before its digits are masked", got)
	}
}

// masked learns the number, so that it is masked wherever it appears from
// then on, and returns it as Schwab masks it; an account number already
// masked stays as it is.
func TestMaskerMasked(t *testing.T) {
	t.Parallel()
	m := newMasker("", "", "")
	if got := m.masked("1234567"); got != "...567" {
		t.Errorf("masked(1234567) = %q", got)
	}
	if got := m.free("to 1234567"); got != "to ...567" {
		t.Errorf("after masked, free(to 1234567) = %q", got)
	}
	if got := m.masked("...135"); got != "...135" {
		t.Errorf("masked(...135) = %q", got)
	}
}

// path shows a path under the home folder as ~/ and its components, each
// masked as a file name, with the known numbers replaced, in its given form
// and with its links resolved; any other path only as its base name; and
// the name of the app key's rate-limit file whole.
func TestMaskerPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	home := filepath.Join(dir, "home-61803398")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	m := testMasker(home)
	m.known.Add("1234567")
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{home, "~"},
		{filepath.Join(home, ".config", "schwabtx", "token.json"), "~/.config/schwabtx/token.json"},
		{filepath.Join(home, "accounts-61803398", "history.db"), "~/accounts-...398/history.db"},
		{filepath.Join(home, "token-1234567.json"), "~/token-...567.json"},
		{filepath.Join(home, "Individual_XXX398_Transactions_20261111-120000.csv"), "~/Individual_XXX398_Transactions_20261111-120000.csv"},
		{filepath.Join(dir, "elsewhere", "x29470135", "history.db"), "history.db"},
		{filepath.Join(dir, "elsewhere", "store-87654321.db"), "store-...321.db"},
		{filepath.Join(home+"x", "token.json"), "token.json"},
	} {
		if got := m.path(tc.in); string(got) != tc.want {
			t.Errorf("path(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The rate-limit file that schwabtx setup names for an app key whose
	// digest holds 8 decimal digits in a row is shown whole, in the home
	// folder or not; a name of its shape that is not the key's digest is
	// masked as any other.
	const appKey = "appkey00025XXXXXXXXXXXXXXXXXXXXXXX"
	if name := filepath.Base(schwab.DefaultRateFile(appKey, "")); name != "rate-8d2a81279577ea15" {
		t.Fatalf("the app key's rate-limit file is %s", name)
	}
	m = newMasker(home, appKey, "")
	for _, tc := range []struct{ in, want string }{
		{filepath.Join(home, ".cache", "schwabtx", "rate-8d2a81279577ea15"), "~/.cache/schwabtx/rate-8d2a81279577ea15"},
		{filepath.Join(dir, "elsewhere", "rate-8d2a81279577ea15"), "rate-8d2a81279577ea15"},
		{filepath.Join(home, ".cache", "schwabtx", "rate-8d2a81279577ea16"), "~/.cache/schwabtx/rate-8d2a...577ea16"},
	} {
		if got := m.path(tc.in); string(got) != tc.want {
			t.Errorf("path(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	m = testMasker(link)
	if got := m.path(filepath.Join(home, "x.json")); got != "~/x.json" {
		t.Errorf("a home given by a link: path of its target's file = %q, want ~/x.json", got)
	}
}

// A home folder that is a volume's root is none, so that a path outside a
// user's folder shows by its base name and a log line keeps its
// separators; and a log line's home is the home only where it starts a
// path, not inside a longer folder's name or under another folder.
func TestMaskerHomeBounds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.VolumeName(dir) + string(filepath.Separator)
	m := newMasker(root, "", "")
	if len(m.homes) != 0 {
		t.Errorf("a home of %s is kept: %q", root, m.homes)
	}
	if got := m.path(filepath.Join(root, "etc", "schwabtx", "config.json")); got != "config.json" {
		t.Errorf("with a home of %s, path = %q, want config.json", root, got)
	}
	line := "open " + filepath.Join(root, "var", "lib", "schwabtx", "history.db") + ": permission denied"
	if got := m.logString(line, true); got != line {
		t.Errorf("with a home of %s, the log line %q became %q", root, line, got)
	}

	home := filepath.Join(dir, "al")
	m = newMasker(home, "", "")
	sep := string(filepath.Separator)
	for _, tc := range []struct{ in, want string }{
		{"open " + filepath.Join(home, "x", "token.json"), "open ~" + sep + filepath.Join("x", "token.json")},
		{"open " + filepath.Join(dir, "alice", "x", "token.json"), "open " + filepath.Join(dir, "alice", "x", "token.json")},
		{"open " + filepath.Join(dir, "mnt") + home, "open " + filepath.Join(dir, "mnt") + home},
		{"home is " + strings.ToUpper(home), "home is ~"},
		{`file="` + home + `"`, `file="~"`},
	} {
		if got := m.logString(tc.in, false); got != tc.want {
			t.Errorf("logString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := m.path(filepath.Join(dir, "alice", "token.json")); got != "token.json" {
		t.Errorf("path of a folder whose name begins with the home's = %q", got)
	}
}

// compose writes the server's own words with pieces already safe as they
// are, identifiers and dates whole, and masks any string it is given as
// free text; it never masks the text whole.
func TestMaskerCompose(t *testing.T) {
	t.Parallel()
	m := testMasker("")
	got := m.compose("%s on %s, order %s, %d rows, in %s; Schwab said: %s",
		Ident("742C51B544655B85F41224019A82CD7AB3C6BABDB67BF80531E82B306CB5F691"), Date("2026-09-14"),
		Ident("1004517688099"), 3, MaskedNumber("...135"), "Invalid account number 29470135")
	want := "742C51B544655B85F41224019A82CD7AB3C6BABDB67BF80531E82B306CB5F691 on 2026-09-14, order 1004517688099, 3 rows, " +
		"in ...135; Schwab said: Invalid account number ...135"
	if string(got) != want {
		t.Errorf("compose =\n%s\nwant\n%s", got, want)
	}
	if got := m.compose("%s", errors.New("account 61803398")); got != "account ...398" {
		t.Errorf("compose of an error = %q", got)
	}
}

// schwabDetail shows Schwab's words masked as free text, and the status,
// the tries, the correlation ID and the reference as sent, cut at 300
// characters. A code comes as sent when it is one of Schwab's codes, and
// masked as free text when Schwab's "error" or "errorcode" holds a
// sentence instead: an account no one knows, of 8 or more digits, shows
// as its last three, as one the server knows does.
func TestSchwabDetail(t *testing.T) {
	t.Parallel()
	m := testMasker("")
	e := &schwab.Error{
		Status: http.StatusBadRequest, Tries: 2, Message: "Invalid account number 29470135 on 2026-09-14",
		CorrelID: "5b1d2c3e-1234-5678-9abc-def012345678", Reference: "18.6a2f1c17.1700000000.1a2b3c4d",
	}
	want := "HTTP 400 from Schwab after 2 tries: Invalid account number ...135 on ...914 " +
		"(correlation ID 5b1d2c3e-1234-5678-9abc-def012345678) (reference #18.6a2f1c17.1700000000.1a2b3c4d)"
	if got := m.schwabDetail(e); string(got) != want {
		t.Errorf("schwabDetail =\n%s\nwant\n%s", got, want)
	}
	for _, tc := range []struct {
		e    *schwab.Error
		want string
	}{
		{&schwab.Error{Status: 400, Code: "invalid_grant", Detail: "expired"}, "HTTP 400 from Schwab: invalid_grant (expired)"},
		{&schwab.Error{Status: 400, Code: "unsupported_token_type", Detail: "invalid_grant"}, "HTTP 400 from Schwab: unsupported_token_type (invalid_grant)"},
		{&schwab.Error{Status: 502, Code: "protocol.http.TooBigBody", Message: "Body buffer overflow"}, "HTTP 502 from Schwab: Body buffer overflow (protocol.http.TooBigBody)"},
		{&schwab.Error{Status: 401, Code: "oauth.v2.InvalidApiKey"}, "HTTP 401 from Schwab: oauth.v2.InvalidApiKey"},
		{&schwab.Error{Status: 504, Code: "messaging.adaptors.http.flow.GatewayTimeout"}, "HTTP 504 from Schwab: messaging.adaptors.http.flow.GatewayTimeout"},
		{
			&schwab.Error{Status: 403, Code: "Account 31415926 and 29470135 restricted", Detail: "acct 27182818284"},
			"HTTP 403 from Schwab: Account ...926 and ...135 restricted (acct ...284)",
		},
		{&schwab.Error{Status: 502, Message: "Bad 27182818284", Code: "acct.27182818284"}, "HTTP 502 from Schwab: Bad ...284 (acct....284)"},
	} {
		if got := m.schwabDetail(tc.e); string(got) != tc.want {
			t.Errorf("schwabDetail(%+v) = %q, want %q", *tc.e, got, tc.want)
		}
	}
	long := m.schwabDetail(&schwab.Error{Status: 500, Message: strings.Repeat("x", 400)})
	if n := len([]rune(string(long))); n != maxDetail+3 || !strings.HasSuffix(string(long), "...") {
		t.Errorf("a long detail has %d characters: %q", n, long)
	}
}

// The backstop rewrites only what holds a known number or a secret, in a
// string, whole or in groups, a key or a number of any form, and leaves
// every other byte as it was: key order, number literals, escapes, a hash
// that holds a known number, and a date or a time whose date's digits are
// those of a known number.
func TestBackstop(t *testing.T) {
	t.Parallel()
	m := testMasker("")
	m.known.Add("20261113")
	for _, tc := range []struct {
		in, want string
		n        int
	}{
		{`{"a":"x","b":[1,2.50,"S&P <500>"],"c":null}`, `{"a":"x","b":[1,2.50,"S&P <500>"],"c":null}`, 0},
		{`{"hash":"742C51B544655B85F41224019A82CD7AB3C6BABDB67BF80531E82B306CB5F691"}`, `{"hash":"742C51B544655B85F41224019A82CD7AB3C6BABDB67BF80531E82B306CB5F691"}`, 0},
		{`{"d":"to 29470135","n":61803398,"f":61803398.0,"e":4.0221783E7,"k":1004517688099}`, `{"d":"to ...135","n":null,"f":null,"e":null,"k":1004517688099}`, 4},
		{`{"29470135":1}`, `{"...135":1}`, 1},
		{`{"d":"Joint 2947-0135","e":"to 6180 3398 x"}`, `{"d":"Joint ...135","e":"to ...398 x"}`, 2},
		{`{"s":"` + schwabtest.AppSecret + `","k":"key ` + schwabtest.AppKey + `"}`, `{"s":"[app secret]","k":"key [app key]"}`, 2},
		{`["\u2028",  "29470135"  ]`, `["\u2028",  "...135"  ]`, 1},
		{
			`{"as_of":"2026-11-13T16:40:00-05:00","date":"2026-11-13","asked":{"from":"2026-11-01","to":"2026-11-13"},"read":"2026-11-13T21:40:00.000Z"}`,
			`{"as_of":"2026-11-13T16:40:00-05:00","date":"2026-11-13","asked":{"from":"2026-11-01","to":"2026-11-13"},"read":"2026-11-13T21:40:00.000Z"}`, 0,
		},
		{`{"d":"to 2026 1113","n":20261113}`, `{"d":"to ...113","n":null}`, 2},
	} {
		got, n, err := m.backstop([]byte(tc.in))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want || n != tc.n {
			t.Errorf("backstop(%s) = %s, %d; want %s, %d", tc.in, got, n, tc.want, tc.n)
		}
	}
	if _, _, err := m.backstop([]byte(`{"a":`)); err == nil {
		t.Error("the backstop took JSON cut short")
	}
}

// json masks Schwab's raw JSON by key, with the known numbers, and hides
// the secrets that it might hold.
func TestMaskerJSON(t *testing.T) {
	t.Parallel()
	m := testMasker("")
	raw := `{"activityId":73508261946,"accountNumber":"29470135","description":"JOURNAL TO 61803398 & co",` +
		`"time":"2024-04-04T15:56:07+0000","user":{"login":"jdoe1234"},"newKey":"made-up text"}`
	got, report, err := m.json([]byte(raw), schwab.TransactionKeyClass)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"activityId":73508261946,"accountNumber":"...135","description":"JOURNAL TO ...398 & co",` +
		`"time":"2024-04-04T15:56:07+0000","user":{"login":"[withheld]"},"newKey":"[withheld]"}`
	if string(got) != want {
		t.Errorf("json =\n%s\nwant\n%s", got, want)
	}
	if report.AccountNumbers != 1 || report.Withheld != 2 { // the description's number is masked by pattern, which is not counted
		t.Errorf("report %+v", report)
	}
	if _, _, err := m.json([]byte(`{`), schwab.TransactionKeyClass); err == nil {
		t.Error("json took what is not JSON")
	}
	m.scrub = func(s string) string { return strings.ReplaceAll(s, "made-up", "[token]") }
	if got, _, _ := m.json([]byte(`{"description":"made-up"}`), schwab.TransactionKeyClass); string(got) != `{"description":"[token]"}` {
		t.Errorf("json kept a secret: %s", got)
	}
	// A secret that holds a run of 8 digits is replaced before free text is
	// masked by pattern, which would cut it so that it is no longer found.
	m = newMasker("", "Key12345678abc", "Sec87654321xyz")
	m.scrub = func(s string) string { return strings.ReplaceAll(s, "tok-99999999", "[token]") }
	raw = `{"description":"app Key12345678abc secret Sec87654321xyz token tok-99999999","memo":"Key12345678abc"}`
	got, _, err = m.json([]byte(raw), schwab.TransactionKeyClass)
	if want := `{"description":"app [app key] secret [app secret] token [token]","memo":"[withheld]"}`; err != nil || string(got) != want {
		t.Errorf("json of secrets with digits =\n%s, %v\nwant\n%s", got, err, want)
	}
}

// The log handler masks every value as text: an error's by pattern too,
// then the secrets, the known numbers and the home folder, in its given
// form, with its links resolved and in any case, in the message, in
// attributes of every kind and in groups, and in attributes given to With.
func TestLogHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	home := filepath.Join(dir, "Home-61803398")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	m := testMasker(home)
	m.known.Add("1234567")
	var b bytes.Buffer
	log := slog.New(&logHandler{inner: slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug}), m: m})
	log = log.With("component", "store", "file", filepath.Join(home, "with.db"))
	log.Warn("reading "+filepath.Join(home, "a.db"),
		"error", fmt.Errorf("open %s: account 87654321 and 1234567, key %s", filepath.Join(strings.ToUpper(home), "b.db"), schwabtest.AppKey),
		"text", "plain 87654321 stays, 1234567 does not",
		"number", 61803398,
		"group", slog.GroupValue(slog.String("inner", "secret "+schwabtest.AppSecret)),
		"date", "2026-09-14",
	)
	got := b.String()
	for _, want := range []string{
		"file=~/with.db", `msg="reading ~/a.db"`, "~/b.db", "account ...321 and ...567", "key [app key]",
		`text="plain 87654321 stays, ...567 does not"`, "number=...398", `group.inner="secret [app secret]"`, "date=2026-09-14",
	} {
		if !strings.Contains(got, strings.ReplaceAll(want, "/", string(filepath.Separator))) && !strings.Contains(got, want) {
			t.Errorf("the log lacks %s:\n%s", want, got)
		}
	}
	for _, leak := range []string{home, schwabtest.AppKey, schwabtest.AppSecret, "61803398", "1234567 "} {
		if strings.Contains(got, leak) {
			t.Errorf("the log holds %q:\n%s", leak, got)
		}
	}
}

// Text the server composes keeps its identifiers (design 7.8, 15.4): a
// date, a 13-digit order ID, a correlation ID, an edge reference and an
// account hash holding a run of 8 digits come out as they went in, through
// compose, schwabDetail, the log handler and the store's own details
// through stored, while a full account number in Schwab's own words, the
// one piece of free text, comes out masked. Dates and times stay whole
// though their dates' digits are those of known account numbers.
func TestComposedTextKeepsIdentifiers(t *testing.T) {
	t.Parallel()
	m := testMasker("")
	m.known.Add("20260914", "20260105")
	hash := schwabtest.HashFor(individual)
	ids := []string{"2026-09-14", "1004517688099", "5b1d2c3e-1234-5678-9abc-def012345678", "18.6a2f1c17.1700000000.1a2b3c4d", hash}
	composed := []string{
		string(m.compose("%s %s %s %s %s", Date(ids[0]), Ident(ids[1]), Ident(ids[2]), Ident(ids[3]), Ident(ids[4]))),
		string(m.schwabDetail(&schwab.Error{Status: 400, Message: "no account 29470135", CorrelID: ids[2], Reference: ids[3]})),
	}
	var b bytes.Buffer
	log := slog.New(&logHandler{inner: slog.NewTextHandler(&b, nil), m: m})
	log.Warn("composed", "day", ids[0], "order", ids[1], "correl_id", ids[2], "reference", ids[3], "account_hash", ids[4])
	composed = append(composed, b.String())
	for _, text := range composed {
		for _, id := range ids[:2] {
			if strings.Contains(text, "day") && !strings.Contains(text, id) {
				t.Errorf("%s lost %s", text, id)
			}
		}
	}
	all := strings.Join(composed, "\n")
	for _, id := range ids {
		if !strings.Contains(all, id) {
			t.Errorf("the composed texts lost %s:\n%s", id, all)
		}
	}
	if strings.Contains(all, individual) || !strings.Contains(all, "no account ...135") {
		t.Errorf("Schwab's own words were not masked:\n%s", all)
	}
	for _, detail := range []string{
		"left out by the complete read sent 2026-01-05T19:12:00.000Z, ...: schwabtx edit T73508261946 -count no",
		"3 fills of order 1004517688020",
		"F0869ae0a (Individual_XXX398_Transactions_20261111-120000.csv) does not hold it",
	} {
		if got := m.stored(detail); string(got) != detail {
			t.Errorf("stored(%q) = %q", detail, got)
		}
	}
	if got := m.free("no account 29470135"); strings.Contains(string(got), individual) || !strings.Contains(string(got), "...135") {
		t.Errorf("free kept the number: %q", got)
	}
	if mask.Text(ids[1]) == ids[1] {
		t.Error("mask.Text keeps a 13-digit ID: the test shows nothing")
	}
}
