package history

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"hash"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// rulesDigests holds, for each DeriveVersion, the digest of what
// TestRulesVersion prints of the rules.
var rulesDigests = map[int]string{
	1: "9822f933727a4f3211e684056fb1ea442c41b268b6c3f31b4ec8d8e86410c6b5",
}

// printRules writes to h what the rules make of a grid of inputs, and their
// tables: the classification of API transactions and of file actions, the
// signs of file quantities, the matching's families and windows, the flag
// definitions, the cut form of every fixture transaction, masking and the
// units of a few values, and the golden output of schwabfile's readers.
// The calendar has its own version (nyse.Version), which this leaves out.
func printRules(t *testing.T, h hash.Hash) {
	t.Helper()
	nets := []sql.NullInt64{{}, {Int64: -100, Valid: true}, {Int64: 0, Valid: true}, {Int64: 100, Valid: true}}
	legSets := [][]legClass{
		nil,
		{leg("CURRENCY", positive)},
		{leg("EQUITY", positive)},
		{leg("EQUITY", negative)},
		{leg("EQUITY", zero)},
		{leg("EQUITY", unknownSign)},
		{leg("OPTION", positive), leg("OPTION", negative)},
		{leg("CURRENCY", negative), leg("EQUITY", positive)},
	}
	wordings := append(slices.Clone(knownWordings), "", "Foreign Tax Paid~SYM", "ADR Fee~SYM", "SCHWAB1 INT 01/30-02/27",
		"Special Payout~SYM", "Removed due to Expiration", "W-8 Withholding", "BANK SWEEP", "Qualified Dividend~SYM")
	types := append(schwab.AllTransactionTypes(), "SOMETHING_NEW")
	for _, typ := range types {
		for _, status := range []string{"VALID", "INVALID"} {
			for _, w := range wordings {
				for _, net := range nets {
					for _, legs := range legSets {
						c := classifyAPI(apiClass{typ: string(typ), status: status, wording: w, net: net, legs: legs})
						fmt.Fprintf(h, "api %s %s %q %v %v: %s %s %s %v\n", typ, status, w, net, legs, c.rule, c.category, c.family, c.flags)
					}
				}
			}
		}
	}
	actions := append(slices.Sorted(maps.Keys(fileActions)), "somethingnew")
	for _, a := range actions {
		for _, q := range []sign{zero, positive, negative} {
			for _, net := range nets {
				c := classifyFile(fileClass{action: a, hasQuantity: q != zero, quantity: q, amount: net})
				fmt.Fprintf(h, "file %s %v %v: %s %s %s %v\n", a, q, net, c.rule, c.category, c.family, c.flags)
			}
		}
	}
	for _, f := range fileFamilies {
		for _, a := range f.actions {
			fmt.Fprintf(h, "quantity %s %d %d\n", a, signedQuantity(a, 5), signedQuantity(a, -5))
		}
	}
	for _, family := range slices.Sorted(maps.Keys(matchRules)) {
		fmt.Fprintf(h, "match %s %v\n", family, matchRules[family])
	}
	fmt.Fprintf(h, "reinvest %v legs %v %d candidates %d holds %v categories %v\n",
		reinvestRule, legsFamilies, maxLegsRows, maxCandidates, holdsFamilies, categories)
	for _, f := range flagDefs {
		fmt.Fprintf(h, "flag %v\n", f)
	}
	raws := [][]byte{schwabtest.Fixture(t, "transaction.json")}
	for _, number := range []string{individual, options} {
		for _, row := range fixtureRows(t, number) {
			raws = append(raws, row.Raw)
		}
	}
	fmt.Fprintf(h, "cut version %d\n", CutVersion)
	for _, raw := range raws {
		cut, digest := mustCut(t, raw)
		fmt.Fprintf(h, "cut %s %x\n", cut, digest)
	}
	for _, s := range []string{"JOURNAL TO 12345678 100 SHS", "TRANSFER FROM 2947-0135 1 OF 2", "1000 2947-0135", "Ref 12345678-1"} {
		fmt.Fprintf(h, "mask %q %q\n", s, mask.Text(s))
	}
	fmt.Fprintf(h, "mask %q\n", mask.FileName("Joint 12345678_XXX398_Transactions_20261111-120000.csv"))
	for _, f := range []float64{0.30000000000000004, -236.999999999999, 81.29223999999999, 71.42857, 1e13} {
		c, how := cents(f)
		m, mhow := micros(f)
		fmt.Fprintf(h, "units %v %d %v %d %v\n", f, c, how, m, mhow)
	}
	goldens, err := filepath.Glob(filepath.Join("schwabfile", "testdata", "golden", "*"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("schwabfile's golden files: %q, %v", goldens, err)
	}
	for _, g := range goldens {
		b, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "golden %s %x\n", filepath.Base(g), sha256.Sum256(b))
	}
}

// A change to the rules comes with a new DeriveVersion, so that stores
// derive their data again when they are opened (8.3 of the design): what
// the rules make of a grid of inputs and their tables, printed and hashed,
// is what was recorded for DeriveVersion.
func TestRulesVersion(t *testing.T) {
	t.Parallel()
	h := sha256.New()
	printRules(t, h)
	got := hex.EncodeToString(h.Sum(nil))
	want, ok := rulesDigests[DeriveVersion]
	switch {
	case !ok:
		t.Fatalf("no digest is recorded for DeriveVersion %d: record these rules', %s", DeriveVersion, got)
	case got != want && slices.Contains(slices.Collect(maps.Values(rulesDigests)), got):
		t.Fatalf("the rules are those of an older DeriveVersion, not of DeriveVersion %d", DeriveVersion)
	case got != want:
		t.Fatalf("the rules changed: set DeriveVersion to %d and record its digest, %s, so that stores derive again",
			slices.Max(slices.Collect(maps.Keys(rulesDigests)))+1, got)
	}
}
