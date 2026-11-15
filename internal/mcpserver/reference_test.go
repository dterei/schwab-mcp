package mcpserver

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The reference page of transaction types names each of Schwab's types,
// each of the local history's categories and, in its sentence on
// counted_why, each reason that the store's schema takes, says that
// Schwab may send others, and is served as Markdown, private, for a day, in
// both eras.
func TestTransactionTypesPage(t *testing.T) {
	t.Parallel()
	for _, protocol := range caseProtocols {
		e := newEnv(t, withProtocol(protocol))
		list, err := e.cs.ListResources(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		uri := referencePrefix + "transaction-types"
		if !slices.ContainsFunc(list.Resources, func(r *mcp.Resource) bool { return r.URI == uri && r.MIMEType == "text/markdown" }) {
			t.Errorf("resources/list lacks %s", uri)
		}
		r, err := e.readResource(uri)
		if err != nil {
			t.Fatal(err)
		}
		text := r.Contents[0].Text
		for _, typ := range schwab.AllTransactionTypes() {
			if !strings.Contains(text, "\n| "+string(typ)+" | ") {
				t.Errorf("the page does not describe %s", typ)
			}
		}
		for _, c := range history.Categories() {
			if !strings.Contains(text, "\n| "+c+" | ") {
				t.Errorf("the page does not describe the category %s", c)
			}
		}
		if !strings.Contains(text, "as Schwab's spec lists them; Schwab may send others") || r.Contents[0].URI != uri ||
			r.Contents[0].MIMEType != "text/markdown" || r.CacheScope != "private" || r.TTLMs != 86_400_000 {
			t.Errorf("the page: %+v", r)
		}
		if n := e.fake.APICalls() + e.fake.TokenRequests(); n != 1 {
			t.Errorf("reading the page sent %d requests to Schwab", n-1)
		}
		// The reasons that count, and those that do not, as the store gives
		// them (internal/history's matching, docs/store.md's table of
		// counted_why); user is the user's own decision, either way.
		counts, not, rest := countedWhyLists(t, text)
		named := slices.Concat(counts, not)
		if regexp.MustCompile(`\buser\b`).MatchString(rest) && strings.Contains(rest, "either way") {
			named = append(named, "user")
		}
		slices.Sort(named)
		whys := countedWhys(t)
		slices.Sort(whys)
		if !slices.Equal(counts, []string{"api", "file", "tda", "policy"}) ||
			!slices.Equal(not, []string{"gone", "invalid", "linked", "uncertain", "held_out"}) || !slices.Equal(named, whys) {
			t.Errorf("the page says a row counts for %q, does not for %q, and then %q; the schema takes %q", counts, not, rest, whys)
		}
	}
}

// countedWhyLists returns what the page's sentence on counted_why says: the
// reasons why a row counts, the first word of each item in the parentheses
// after "counts and why"; those why it does not, after "why not"; and the
// rest of the sentence after them.
func countedWhyLists(t *testing.T, page string) (counts, not []string, rest string) {
	t.Helper()
	_, sentence, ok := strings.Cut(strings.Join(strings.Fields(page), " "), "counted and counted_why say ")
	sentence, _, _ = strings.Cut(sentence, ". ")
	m := regexp.MustCompile(`^whether a row counts and why \(([^)]*)\), or why not \(([^)]*)\)(.*)$`).FindStringSubmatch(sentence)
	if !ok || m == nil {
		t.Fatalf("the page's sentence on counted_why is not as the test reads it: %q", sentence)
	}
	words := func(list string) []string {
		var out []string
		for item := range strings.SplitSeq(list, ",") {
			item = strings.TrimSpace(item)
			if w := regexp.MustCompile(`^(?:or )?([a-z_]+)`).FindStringSubmatch(item); w != nil {
				item = w[1]
			}
			out = append(out, item)
		}
		return out
	}
	return words(m[1]), words(m[2]), m[3]
}

// countedWhys returns the values of counted_why that the store's schema
// takes, from its CHECK.
func countedWhys(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "history", "schema", "001.sql"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`counted_why\s+TEXT NOT NULL CHECK \(counted_why IN \(([^)]*)\)\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("the schema's CHECK of counted_why is not where the test looks")
	}
	var whys []string
	for v := range strings.SplitSeq(string(m[1]), ",") {
		whys = append(whys, strings.Trim(strings.TrimSpace(v), "'"))
	}
	if len(whys) < 10 {
		t.Fatalf("counted_why takes %q", whys)
	}
	return whys
}
