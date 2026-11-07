package schwabtest

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// files holds the fixtures, embedded so that a test in any package reads
// the same bytes wherever it runs.
//
//go:embed testdata
var files embed.FS

// Fixture returns a copy of the fixture with the given name, such as
// "accounts.json" or "errors/akamai_403.html", failing the test if there is
// none.
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := files.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("schwabtest: no fixture %s: %v", name, err)
	}
	return b
}

// Fixtures returns the names of every fixture, in lexical order, as
// Fixture takes them.
func Fixtures() []string {
	var names []string
	err := fs.WalkDir(files, "testdata", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, strings.TrimPrefix(path, "testdata/"))
		}
		return err
	})
	if err != nil {
		panic("schwabtest: reading the embedded fixtures: " + err.Error())
	}
	slices.Sort(names)
	return names
}

// HashFor returns the hash the fake gives an account number: upper-case
// hex of the SHA-256 of "schwabtest:" and the number. Schwab's hashes are
// opaque; the fake derives its own so that it can serve any account, and so
// that a test can check every hash in the fixtures.
func HashFor(number string) string {
	sum := sha256.Sum256([]byte("schwabtest:" + number))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// A FixtureAccount is one of the accounts the fixtures describe.
type FixtureAccount struct {
	Number    string // accountNumber
	Hash      string // hashValue, HashFor(Number)
	Nickname  string // nickName in the user preferences
	DisplayID string // displayAcctId: "..." and the last three digits
}

// FixtureAccounts are the fixtures' accounts, in the order the account
// numbers fixture lists them. No other value in the fixtures contains one
// of their numbers.
var FixtureAccounts = []FixtureAccount{
	{Number: "29470135", Hash: HashFor("29470135"), Nickname: "Individual", DisplayID: "...135"},
	{Number: "61803398", Hash: HashFor("61803398"), Nickname: "Options & Margin", DisplayID: "...398"},
	{Number: "40221783", Hash: HashFor("40221783"), Nickname: "Contributory", DisplayID: "...783"},
}
