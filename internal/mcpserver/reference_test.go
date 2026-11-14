package mcpserver

import (
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The reference page of transaction types names each of Schwab's types
// and each of the local history's categories, says that Schwab may send
// others, and is served as Markdown, private, for a day, in both eras.
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
	}
}
