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
// each of the local history's categories and each reason of counted_why
// that the store's schema takes, says that Schwab may send others, and is
// served as Markdown, private, for a day, in both eras.
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
		for _, why := range countedWhys(t) {
			if !regexp.MustCompile(`[ (]` + why + `[,:;)]`).MatchString(text) {
				t.Errorf("the page does not name the counted_why %s", why)
			}
		}
	}
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
