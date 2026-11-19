package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	configfile "github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/mcpserver"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// lookupWorlds are the worlds of the lookups' tests: the fixtures' three
// accounts, one of which sends no value and one of which is short an
// option; balances that are odd, as TestLookupsSayWhyAFigureIsNone has
// them, beside a margin account whose cash is in its margin and short
// balances; and positions that are short or cannot be read, as
// TestPositionsTable and TestPositionsWarnOfUnreadablePositions have them.
var lookupWorlds = []struct {
	name  string
	setup func(t *testing.T, f *fakeSchwab)
}{
	{"the fixtures", func(_ *testing.T, f *fakeSchwab) { f.LoadFixtures() }},
	{"odd balances", func(t *testing.T, f *fakeSchwab) {
		for _, n := range []string{"11112222", "33334444", "55556666"} {
			f.addAccount(n, 1000, 0, testNow, time.Hour)
		}
		// No balances at all.
		setDetailsOf(t, f, "11112222", func(a map[string]any) {
			a["securitiesAccount"].(map[string]any)["currentBalances"] = map[string]any{}
			delete(a, "aggregatedBalance")
		})
		// Savings beside the cash, which is then not the value less the
		// holdings.
		setDetailsOf(t, f, "33334444", func(a map[string]any) {
			a["securitiesAccount"].(map[string]any)["currentBalances"] = map[string]any{
				"liquidationValue": 1500, "cashBalance": 1000, "buyingPower": 2000, "savings": 500, "longMarketValue": 0,
				"shortMarketValue": 0, "longOptionMarketValue": 0, "shortOptionMarketValue": 0, "mutualFundValue": 0, "bondValue": 0,
			}
		})
		// A margin account's cash in its margin and short balances, beside a
		// cashBalance of 0, and no buying power.
		setDetailsOf(t, f, "55556666", func(a map[string]any) {
			a["securitiesAccount"].(map[string]any)["currentBalances"] = map[string]any{
				"liquidationValue": 13346.57, "cashBalance": 0, "marginBalance": -12345.67, "shortBalance": 12846.12,
			}
		})
	}},
	{"odd positions", func(t *testing.T, f *fakeSchwab) {
		for _, n := range []string{"11112222", "33334444", "55556666"} {
			f.addAccount(n, 1000, 0, testNow, time.Hour)
		}
		// A long stock and a short call.
		setDetailsOf(t, f, "11112222", func(a map[string]any) {
			a["securitiesAccount"].(map[string]any)["positions"] = decodeJSON(t, `[`+
				`{"longQuantity":1,"shortQuantity":0,"averagePrice":1,"marketValue":4,"longOpenProfitLoss":0,`+
				`"instrument":{"assetType":"EQUITY","symbol":"AAPL","description":"APPLE INC"}},`+
				`{"longQuantity":0,"shortQuantity":1,"averagePrice":2.0533,"marketValue":-272.5,"shortOpenProfitLoss":-67.17,`+
				`"instrument":{"assetType":"OPTION","symbol":"MRVL  250321C00155000","description":"MARVELL TECHNOLOGY INC 03/21/2025 $155 Call"}}]`)
		})
		// A value that cannot be read, an open profit or loss missing, and
		// a position that is not an object.
		setDetailsOf(t, f, "33334444", func(a map[string]any) {
			ps := decodeJSON(t, "["+fakePositions+",null]").([]any)
			ps[0].(map[string]any)["marketValue"] = "1300.5"
			delete(ps[1].(map[string]any), "longOpenProfitLoss")
			a["securitiesAccount"].(map[string]any)["positions"] = ps
		})
		// Positions that are not a list.
		setDetailsOf(t, f, "55556666", func(a map[string]any) {
			a["securitiesAccount"].(map[string]any)["positions"] = map[string]any{"longQuantity": 1}
		})
	}},
}

// The MCP server works its figures out with internal/lookup, as schwabtx's
// lookups do, and the two agree on the same answers (design 15.8): in each
// world of the lookups' own tests, schwabtx accounts, balances and
// positions, and the server's list_accounts, get_balances and
// get_positions on the same fake, give the same cells: each table is
// parsed, and every cell compared with the server's answer, a - with null,
// every other figure as the same digits, and a text as the table shows
// the server's.
func TestLookupsAgreeWithMCP(t *testing.T) {
	for _, w := range lookupWorlds {
		t.Run(w.name, func(t *testing.T) {
			f, tokenFile := setup(t)
			w.setup(t, f)
			doLogin(t)
			cs := mcpClient(t, f, tokenFile)
			for _, l := range []struct {
				command, tool string
				args          map[string]any
				compare       func(t *testing.T, rows [][]string, answer map[string]any)
			}{
				{"accounts", "list_accounts", nil, compareAccounts},
				{"balances", "get_balances", nil, compareAccounts},
				{"positions", "get_positions", map[string]any{"limit": 200}, comparePositions},
			} {
				stdout, _, err := runTx(t, l.command)
				if err != nil {
					t.Fatalf("schwabtx %s: %v", l.command, err)
				}
				cols, _, _ := lookups[l.command].table(nil, false)
				rows := parseTable(t, stdout, cols)
				answer := callTool(t, cs, l.tool, l.args)
				t.Run(l.command, func(t *testing.T) { l.compare(t, rows, answer) })
			}
		})
	}
}

// setDetailsOf changes the details of f's account numbered number by
// change, which gets them decoded, numbers as json.Number.
func setDetailsOf(t *testing.T, f *fakeSchwab, number string, change func(a map[string]any)) {
	t.Helper()
	a := decodeJSON(t, f.account(number).details).(map[string]any)
	change(a)
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	f.account(number).details = string(b)
	f.SetDetails(f.hash(number), string(b))
}

// decodeJSON decodes js, numbers as json.Number, which keep the digits
// they were written with.
func decodeJSON(t *testing.T, js string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decoding %.200s: %v", js, err)
	}
	return v
}

// mcpClient starts the MCP server on the fake f, with the login in
// tokenFile and no store, and returns a client of it over in-memory
// transports; the end of the test closes both.
func mcpClient(t *testing.T, f *fakeSchwab, tokenFile string) *mcp.ClientSession {
	t.Helper()
	srv, err := mcpserver.New(mcpserver.Options{
		Config: configfile.Config{
			AppKey:    configfile.Setting{Value: testKey, From: configfile.FromEnv},
			AppSecret: configfile.Setting{Value: testSecret, From: configfile.FromEnv},
			BaseURL:   configfile.Setting{Value: f.URL(), From: configfile.FromFile},
			TokenFile: configfile.Setting{Value: tokenFile, From: configfile.FromEnv},
			RateFile:  configfile.Setting{Value: filepath.Join(t.TempDir(), "rate"), From: configfile.FromFile},
		},
		Version:    "test",
		HTTPClient: f.Client(),
		Client:     schwab.ClientConfig{RatePerMinute: 60_000, Burst: 100, Backoff: time.Millisecond},
		Now:        func() time.Time { return testNow },
		Home:       t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.MCP().Connect(t.Context(), serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		ss.Close()
		if err := srv.Close(); err != nil {
			t.Error("closing the server:", err)
		}
	})
	return cs
}

// callTool calls the tool with args and returns its answer, decoded as
// decodeJSON decodes.
func callTool(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", tool, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		t.Fatalf("%s: %s", tool, text)
	}
	return decodeJSON(t, text).(map[string]any)
}

// parseTable returns the rows of table, as writeTable writes it with the
// columns cols, each cut into its cells by where the heads stand in its
// first line: a left-aligned column's cells start where its head starts,
// and a right-aligned one's end where its head ends. A right-aligned cell,
// a figure, is the run with no space in it that ends there, or nothing
// when the line ends before it; a left-aligned one is what lies between
// its start and the next cell, trimmed.
func parseTable(t *testing.T, table string, cols []column) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n")
	head := []rune(lines[0])
	starts, ends := make([]int, len(cols)), make([]int, len(cols))
	at := 0
	for i, c := range cols {
		rest := string(head[at:])
		j := strings.Index(rest, c.head)
		if j < 0 {
			t.Fatalf("no head %q in %q", c.head, lines[0])
		}
		starts[i] = at + utf8.RuneCountInString(rest[:j])
		ends[i] = starts[i] + utf8.RuneCountInString(c.head)
		at = ends[i]
	}
	var rows [][]string
	for _, line := range lines[1:] {
		rs := []rune(line)
		cells := make([]string, len(cols))
		limit := len(rs) // where the cell to the right starts
		for i := len(cols) - 1; i >= 0; i-- {
			switch {
			case cols[i].right && ends[i] > len(rs):
				// The line ends before the cell: it is empty.
			case cols[i].right:
				start := ends[i]
				for start > 0 && rs[start-1] != ' ' {
					start--
				}
				cells[i], limit = string(rs[start:ends[i]]), start
			case starts[i] < limit:
				cells[i], limit = strings.TrimSpace(string(rs[starts[i]:limit])), starts[i]
			}
		}
		rows = append(rows, cells)
	}
	return rows
}

// withoutTotal returns rows without their last, TOTAL, row, and that row,
// or nil when there is none.
func withoutTotal(rows [][]string) ([][]string, []string) {
	if len(rows) > 0 && rows[len(rows)-1][0] == "TOTAL" {
		return rows[:len(rows)-1], rows[len(rows)-1]
	}
	return rows, nil
}

// sameFigure reports whether cell, a table's, shows v, a figure of the
// server's: - for null, else the same digits.
func sameFigure(cell string, v any) bool {
	if v == nil {
		return cell == "-"
	}
	n, ok := v.(json.Number)
	return ok && cell == n.String()
}

// text returns v, a text of the server's, or "" for null.
func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// compareAccounts compares the rows of schwabtx accounts or balances with
// list_accounts' or get_balances' answer: each account's number, nickname,
// type, value and, for balances, cash and buying power; and the totals.
func compareAccounts(t *testing.T, rows [][]string, answer map[string]any) {
	t.Helper()
	accounts, _ := answer["accounts"].([]any)
	rows, total := withoutTotal(rows)
	switch {
	case total == nil && len(accounts) > 1:
		t.Error("no TOTAL row")
	case total != nil && (!sameFigure(total[3], answer["total_value"]) || len(total) > 4 && !sameFigure(total[4], answer["total_cash"])):
		t.Errorf("the totals %q, the server's %v and %v", total, answer["total_value"], answer["total_cash"])
	}
	if len(rows) != len(accounts) {
		t.Fatalf("%d rows, and %d accounts in the server's answer", len(rows), len(accounts))
	}
	for i, row := range rows {
		a := accounts[i].(map[string]any)
		ok := row[0] == text(a["account"]) && row[1] == textCell(text(a["nickname"]), 30) && row[2] == text(a["type"]) &&
			sameFigure(row[3], a["value"])
		if len(row) > 4 {
			ok = ok && sameFigure(row[4], a["cash"]) && sameFigure(row[5], a["buying_power"])
		}
		if !ok {
			t.Errorf("the row %q, the server's account %v", row, a)
		}
	}
}

// comparePositions compares the rows of schwabtx positions with
// get_positions' answer: each position's account, symbol, quantity,
// average price, value, cost, open profit or loss and description, and
// the totals. The table's row of an account whose positions could not be
// read, ? with no figures, is the server's account with positions null.
func comparePositions(t *testing.T, rows [][]string, answer map[string]any) {
	t.Helper()
	positions, _ := answer["positions"].([]any)
	unread := map[string]bool{}
	for _, a := range answer["accounts"].([]any) {
		if a := a.(map[string]any); a["positions"] == nil {
			unread[text(a["account"])] = true
		}
	}
	rows, total := withoutTotal(rows)
	tot := answer["total"].(map[string]any)
	switch {
	case total == nil && len(rows) > 1:
		t.Error("no TOTAL row")
	case total != nil && (!sameFigure(total[4], tot["positions_value"]) || !sameFigure(total[5], tot["positions_cost"]) ||
		!sameFigure(total[6], tot["open_pl"])):
		t.Errorf("the totals %q, the server's %v", total, tot)
	}
	for _, row := range rows {
		if unread[row[0]] {
			if row[1] != "?" || slices.ContainsFunc(row[2:7], func(c string) bool { return c != "-" }) {
				t.Errorf("the row %q of an account whose positions could not be read", row)
			}
			delete(unread, row[0])
			continue
		}
		if len(positions) == 0 {
			t.Fatalf("the row %q, and no position left in the server's answer", row)
		}
		p := positions[0].(map[string]any)
		positions = positions[1:]
		symbol := plainCell(text(p["symbol"]))
		if symbol == "" {
			symbol = "?"
		}
		description := textCell(text(p["description"]), 40)
		if p["asset_type"] == string(schwab.AssetOption) {
			description = tailCell(text(p["description"]), 40)
		}
		if row[0] != text(p["account"]) || row[1] != symbol || !sameFigure(row[2], p["quantity"]) || !sameFigure(row[3], p["average_price"]) ||
			!sameFigure(row[4], p["value"]) || !sameFigure(row[5], p["cost"]) || !sameFigure(row[6], p["open_pl"]) || row[7] != description {
			t.Errorf("the row %q, the server's position %v", row, p)
		}
	}
	if len(positions) > 0 || len(unread) > 0 {
		t.Errorf("%d positions and %d accounts of the server's answer have no row", len(positions), len(unread))
	}
}
