package history

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// testFile returns a History file of id and SHA-256 hex sha (padded),
// exported on day, stating the range from..to.
func testFile(id int64, sha string, day nytime.Date, from, to nytime.Date) *fileInput {
	return &fileInput{
		id: id, sha: sha + strings.Repeat("0", 64-len(sha)), kind: "history_csv", ref: "F" + sha,
		exportedAt: midnight(day).Add(9 * time.Hour), span: schwabfile.Span{StatedFrom: from, StatedTo: to},
	}
}

// addRow adds to f a dated row of action and symbol with amount in cents
// and a quantity in shares (0 for none), its coarse identity worked out
// as a file's is.
func addRow(f *fileInput, posted nytime.Date, action, symbol string, amount, quantity int64) {
	r := &historyRowInput{
		file: f, ordinal: len(f.rows) + 1, id: f.id*100 + int64(len(f.rows)) + 1, posted: posted, action: action, symbol: symbol,
		amount: sql.NullInt64{Int64: amount, Valid: true}, fees: sql.NullInt64{}, feesBlank: true, fileKey: fileKey(symbol, f.exported()),
	}
	if quantity != 0 {
		r.quantity = sql.NullInt64{Int64: signedQuantity(action, quantity*1e6), Valid: true}
		r.quantityText = strconv.FormatInt(quantity, 10)
	}
	amt, _ := schwabfile.ParseDecimal(fmt.Sprintf("%d.%02d", amount/100, max(amount, -amount)%100))
	if amount < 0 {
		amt, _ = schwabfile.ParseDecimal(fmt.Sprintf("-%d.%02d", -amount/100, -amount%100))
	}
	r.coarse = coarseHistory(posted, "", action, schwabfile.Decimal{}, amt)
	f.rows = append(f.rows, r)
	if f.span.Oldest == "" || posted < f.span.Oldest {
		f.span.Oldest = posted
	}
	f.span.Newest = max(f.span.Newest, posted)
}

// clusterShape returns the clusters as the files and ordinals of their
// rows, "1.1+2.1", in order.
func clusterShape(cs []*cluster[*historyRowInput]) []string {
	var out []string
	for _, c := range cs {
		var parts []string
		for _, r := range c.rows {
			parts = append(parts, fmt.Sprintf("%d.%d", r.file.id, r.ordinal))
		}
		s := strings.Join(parts, "+")
		if c.split {
			s += " split"
		}
		out = append(out, s)
	}
	return out
}

// Rows are known to be different transactions only by what tells
// securities apart (14.5 of the design): two definite securities that
// existed at once, two option contracts, two CUSIPs, an option and a
// symbol that is not one, or your differ decision; not two tickers that
// nothing resolves, an empty symbol, two option roots of one contract, a
// definite security beside one that is not, or two contracts or CUSIPs
// that one security holds.
func TestKnownDifferent(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	secs := &securities{
		keys: map[string]secInfo{
			"x:1": {1, true}, "x:2": {2, true}, "x:3": {3, true}, "s:FILE:AAA@2026-09-20": {4, false}, "s:FILE:BBB@2026-09-20": {5, false},
			"s:FILE:CCC@2026-09-20": {6, false}, "s:FILE:DDD@2026-09-20": {7, false},
			"s:FILE:XYZ 09/18/2026 50.00 C@2026-09-20": {8, false}, "s:FILE:XYZ 09/18/2026 55.00 C@2026-09-20": {8, false},
			"s:FILE:037833100@2026-09-20": {9, false}, "s:FILE:594918104@2026-09-20": {9, false},
		},
		periods: map[int64][][2]time.Time{
			1: {{day, day}}, 2: {{day.AddDate(0, 0, 5), day.AddDate(0, 0, 6)}}, 3: {{day.AddDate(1, 0, 0), day.AddDate(1, 0, 0)}},
			4: {{day.AddDate(0, 0, 3), day.AddDate(0, 0, 3)}},
		},
		differ: map[[2]int64]bool{{6, 7}: true},
	}
	for _, c := range []struct {
		a, b rowSide
		want bool
	}{
		{rowSide{"AAA", "x:1", ""}, rowSide{"BBB", "x:2", ""}, true},
		{rowSide{"AAA", "x:1", ""}, rowSide{"AAA", "x:3", ""}, false}, // never at once: a change of id
		{rowSide{"AAA", "x:1", ""}, rowSide{"AAA", "x:1", ""}, false},
		{rowSide{"AAA", "", "s:FILE:AAA@2026-09-20"}, rowSide{"BBB", "", "s:FILE:BBB@2026-09-20"}, false},
		{rowSide{"CCC", "", "s:FILE:CCC@2026-09-20"}, rowSide{"DDD", "", "s:FILE:DDD@2026-09-20"}, true},
		{rowSide{"AAA", "", ""}, rowSide{"", "", ""}, false},
		{rowSide{"XYZ 09/18/2026 50.00 C", "", ""}, rowSide{"XYZ 09/18/2026 55.00 C", "", ""}, true},
		{rowSide{"XYZ 09/18/2026 50.00 C", "", ""}, rowSide{"XYZ   260918C00050000", "", ""}, false},
		{rowSide{"XYZ 09/18/2026 50.00 C", "", ""}, rowSide{"ABC 09/18/2026 50.00 C", "", ""}, false},
		{rowSide{"XYZ 09/18/2026 50.00 C", "", ""}, rowSide{"XYZ 09/18/2026 50.00 P", "", ""}, true},
		{rowSide{"XYZ 09/18/2026 50.00 C", "", ""}, rowSide{"XYZ", "", ""}, true},
		{rowSide{"037833100", "", ""}, rowSide{"594918104", "", ""}, true},
		{rowSide{"037833100", "", ""}, rowSide{"037833100", "", ""}, false},
		// One definite security and one that is not, at once: a restated
		// ticker, not two securities.
		{rowSide{"AAA", "x:1", ""}, rowSide{"OLD", "", "s:FILE:AAA@2026-09-20"}, false},
		// Two contracts, or two CUSIPs, that one security holds (your same).
		{
			rowSide{"XYZ 09/18/2026 50.00 C", "", "s:FILE:XYZ 09/18/2026 50.00 C@2026-09-20"},
			rowSide{"XYZ 09/18/2026 55.00 C", "", "s:FILE:XYZ 09/18/2026 55.00 C@2026-09-20"},
			false,
		},
		{rowSide{"037833100", "", "s:FILE:037833100@2026-09-20"}, rowSide{"594918104", "", "s:FILE:594918104@2026-09-20"}, false},
		{rowSide{"", "", ""}, rowSide{"XYZ 09/18/2026 50.00 C", "", ""}, false},
	} {
		if got := knownDifferent(c.a, c.b, secs); got != c.want {
			t.Errorf("knownDifferent(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := knownDifferent(c.b, c.a, secs); got != c.want {
			t.Errorf("knownDifferent(%v, %v) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

// Rows cluster into transactions (14.5 of the design): within a file each
// row is a transaction of its own, so twins stay two; across files a row
// joins the cluster of its coarse identity it fits best, so a JSON twin of
// a CSV adds nothing and overlapping files count each row once; a row
// known different from every cluster it could join opens its own,
// flagged split. The same files in another order of rows give clusters of
// the same shape.
func TestClusters(t *testing.T) {
	t.Parallel()
	a := testFile(1, "a", "2026-03-01", "", "")
	b := testFile(2, "b", "2026-03-02", "", "")
	c := testFile(3, "c", "2026-03-03", "", "")
	addRow(a, "2026-02-13", "Buy", "XYZ", -5000, 5)
	addRow(a, "2026-02-13", "Buy", "XYZ", -5000, 5)
	addRow(a, "2026-02-10", "Qualified Dividend", "ABCD", 321, 0)
	addRow(b, "2026-02-13", "Buy", "XYZ", -5000, 5)
	addRow(b, "2026-02-10", "Qualified Dividend", "ABCD", 321, 0)
	addRow(b, "2026-02-13", "Buy", "XYZ", -5000, 5)
	addRow(c, "2026-02-10", "Qualified Dividend", "XYZ 09/18/2026 50.00 C", 321, 0)
	secs := &securities{}
	var rows []*historyRowInput
	for _, f := range []*fileInput{a, b, c} {
		rows = append(rows, f.rows...)
	}
	got := clusterShape(clusterRows(rows, secs, historyScore))
	want := []string{"1.1+2.1", "1.2+2.3", "1.3+2.2", "3.1 split"}
	if !slices.Equal(got, want) {
		t.Errorf("the clusters are %q, want %q", got, want)
	}
	slices.Reverse(rows)
	if got := clusterShape(clusterRows(rows, secs, historyScore)); !slices.Equal(got, []string{"1.3+2.2", "3.1 split", "1.1+2.1", "1.2+2.3"}) {
		t.Errorf("in another order, the clusters are %q", got)
	}
	// A row left over beside a cluster that took a row of its own file
	// opens a cluster that is not split.
	g := testFile(6, "g", "2026-03-06", "", "")
	h := testFile(7, "h", "2026-03-07", "", "")
	addRow(g, "2026-09-18", "Expired", "XYZ 09/18/2026 50.00 C", 0, 1)
	addRow(h, "2026-09-18", "Expired", "XYZ 09/18/2026 50.00 C", 0, 1)
	addRow(h, "2026-09-18", "Expired", "XYZ 09/18/2026 55.00 C", 0, 1)
	if got := clusterShape(clusterRows(append(slices.Clone(g.rows), h.rows...), secs, historyScore)); !slices.Equal(got, []string{"6.1+7.1", "7.2"}) {
		t.Errorf("the expirations' clusters are %q", got)
	}
	// Two rows of one day, action and amount, as of different days, are
	// two transactions; as of one day, one.
	asOf := func(f *fileInput, day nytime.Date) {
		r := f.rows[len(f.rows)-1]
		r.asOf = day
		amount, _ := schwabfile.ParseDecimal("0.07")
		r.coarse = coarseHistory(r.posted, r.asOf, r.action, schwabfile.Decimal{}, amount)
	}
	m := testFile(8, "m", "2026-03-08", "", "")
	n := testFile(9, "n", "2026-03-09", "", "")
	addRow(m, "2026-03-02", "Bank Interest", "", 7, 0)
	asOf(m, "2026-02-27")
	addRow(n, "2026-03-02", "Bank Interest", "", 7, 0)
	asOf(n, "2026-02-26")
	addRow(m, "2026-03-03", "Bank Interest", "", 7, 0)
	asOf(m, "2026-02-27")
	addRow(n, "2026-03-03", "Bank Interest", "", 7, 0)
	asOf(n, "2026-02-27")
	if got := clusterShape(clusterRows(append(slices.Clone(m.rows), n.rows...), secs, historyScore)); !slices.Equal(got, []string{"8.1", "8.2+9.2", "9.1"}) {
		t.Errorf("the bank interest's clusters are %q", got)
	}
	// The best fit wins: a row that restates the symbol joins the cluster
	// whose row has its quantity.
	d := testFile(4, "d", "2026-03-04", "", "")
	e := testFile(5, "e", "2026-03-05", "", "")
	addRow(d, "2026-02-13", "Buy", "OLD", -5000, 5)
	addRow(d, "2026-02-13", "Buy", "OLD", -5000, 10)
	addRow(e, "2026-02-13", "Buy", "NEW", -5000, 10)
	if got := clusterShape(clusterRows(append(slices.Clone(d.rows), e.rows...), secs, historyScore)); !slices.Equal(got, []string{"4.1", "4.2+5.1"}) {
		t.Errorf("the restated row's clusters are %q", got)
	}
}

// The coarse identity of a History row (14.5 of the design) is its
// posted date, as-of date, action as letters and digits, and Fees & Comm
// and Amount by value; a placeholder is itself in any case, and not a
// blank. A lot's is its closed date, opened text, quantity and Proceeds,
// likewise. Changing any one part changes it.
func TestCoarseIdentity(t *testing.T) {
	t.Parallel()
	dec := func(s string) schwabfile.Decimal {
		d, _ := schwabfile.ParseDecimal(s)
		return d
	}
	type history struct {
		posted, asOf         nytime.Date
		action, fees, amount string
	}
	base := history{"2026-02-03", "2026-02-02", "Bank Interest", "$0.10", "$1.00"}
	h := func(c history) [32]byte {
		return coarseHistory(c.posted, c.asOf, c.action, dec(c.fees), dec(c.amount))
	}
	for _, c := range []struct {
		row  history
		same bool
	}{
		{history{"2026-02-03", "2026-02-02", "BANK  INTEREST", "0.1", "1"}, true},
		{history{"2026-02-04", "2026-02-02", "Bank Interest", "$0.10", "$1.00"}, false},
		{history{"2026-02-03", "2026-02-01", "Bank Interest", "$0.10", "$1.00"}, false},
		{history{"2026-02-03", "", "Bank Interest", "$0.10", "$1.00"}, false},
		{history{"2026-02-03", "2026-02-02", "Credit Interest", "$0.10", "$1.00"}, false},
		{history{"2026-02-03", "2026-02-02", "Bank Interest", "$0.11", "$1.00"}, false},
		{history{"2026-02-03", "2026-02-02", "Bank Interest", "", "$1.00"}, false},
		{history{"2026-02-03", "2026-02-02", "Bank Interest", "$0.10", "$1.01"}, false},
	} {
		if got := h(c.row) == h(base); got != c.same {
			t.Errorf("%+v beside %+v: the same coarse identity %v, want %v", c.row, base, got, c.same)
		}
	}
	placeholders := []struct {
		a, b string
		same bool
	}{{"--", "", false}, {"N/A", "n/a", true}, {"--", "N/A", false}, {"", "0", false}}
	for _, c := range placeholders {
		row := func(amount string) history { return history{"2026-02-03", "", "Expired", "", amount} }
		if got := h(row(c.a)) == h(row(c.b)); got != c.same {
			t.Errorf("an Amount of %q beside one of %q: the same %v, want %v", c.a, c.b, got, c.same)
		}
	}

	type lot struct{ closed, opened, quantity, proceeds string }
	l := func(c lot) [32]byte {
		return coarseLot(nytime.Date(c.closed), c.opened, dec(c.quantity), dec(c.proceeds))
	}
	baseLot := lot{"2026-07-01", "01/05/2026", "10", "$90.00"}
	for _, c := range []struct {
		lot  lot
		same bool
	}{
		{lot{"2026-07-01", "01/05/2026", "10.000", "90"}, true},
		{lot{"2026-07-02", "01/05/2026", "10", "$90.00"}, false},
		{lot{"2026-07-01", "Various", "10", "$90.00"}, false},
		{lot{"2026-07-01", "01/05/2026", "11", "$90.00"}, false},
		{lot{"2026-07-01", "01/05/2026", "10", "$90.01"}, false},
		{lot{"2026-07-01", "01/05/2026", "10", "--"}, false},
		{lot{"2026-07-01", "01/05/2026", "10", ""}, false},
	} {
		if got := l(c.lot) == l(baseLot); got != c.same {
			t.Errorf("lot %+v beside %+v: the same coarse identity %v, want %v", c.lot, baseLot, got, c.same)
		}
	}
	if l(lot{"2026-07-01", "01/05/2026", "10", "--"}) == l(lot{"2026-07-01", "01/05/2026", "10", ""}) {
		t.Error("a lot whose Proceeds is a placeholder is one whose Proceeds is blank")
	}
}

// A row joins the cluster it fits best (14.5 of the design): an equal
// symbol and description outweigh an equal quantity and price; an equal
// price outweighs an equal description; an equal description outweighs
// nothing. The shown row is the newest export's, and of two exported at
// once, the later file's.
func TestClusterScores(t *testing.T) {
	t.Parallel()
	row := func(f *fileInput, symbol, description, quantity, price string) {
		addRow(f, "2026-02-13", "Buy", symbol, -5000, 0)
		r := f.rows[len(f.rows)-1]
		r.description, r.quantityText, r.priceText = description, quantity, price
	}
	for _, c := range []struct {
		name string
		// first and second are the first file's rows, each opening a
		// cluster; later is the second file's row.
		first, second, later [4]string
		want                 []string
	}{
		{
			"symbol and description over quantity and price",
			[4]string{"OLD", "OLD CORP", "5", "10"},
			[4]string{"NEW", "NEW CORP", "1", "50"},
			[4]string{"NEW", "NEW CORP", "5", "10"},
			[]string{"1.1", "1.2+2.1"},
		},
		{
			"price over description",
			[4]string{"AAA", "SAME", "1", "1"},
			[4]string{"BBB", "OTHER", "1", "9"},
			[4]string{"CCC", "SAME", "2", "9"},
			[]string{"1.1", "1.2+2.1"},
		},
		{
			"description over nothing",
			[4]string{"AAA", "OTHER", "1", "1"},
			[4]string{"BBB", "SAME", "1", "1"},
			[4]string{"CCC", "SAME", "2", "2"},
			[]string{"1.1", "1.2+2.1"},
		},
	} {
		a := testFile(1, "a", "2026-03-01", "", "")
		b := testFile(2, "b", "2026-03-02", "", "")
		row(a, c.first[0], c.first[1], c.first[2], c.first[3])
		row(a, c.second[0], c.second[1], c.second[2], c.second[3])
		row(b, c.later[0], c.later[1], c.later[2], c.later[3])
		if got := clusterShape(clusterRows(append(slices.Clone(a.rows), b.rows...), &securities{}, historyScore)); !slices.Equal(got, c.want) {
			t.Errorf("%s: the clusters are %q, want %q", c.name, got, c.want)
		}
	}

	a := testFile(1, "a", "2026-03-01", "", "")
	b := testFile(2, "b", "2026-03-01", "", "")
	addRow(a, "2026-02-13", "Buy", "XYZ", -5000, 5)
	addRow(b, "2026-02-13", "Buy", "XYZ", -5000, 5)
	for _, rows := range [][]*historyRowInput{{a.rows[0], b.rows[0]}, {b.rows[0], a.rows[0]}} {
		if got := shown(&cluster[*historyRowInput]{rows: rows}); got.file != b {
			t.Errorf("of two exports made at once, the cluster shows file %d's row, not the later file's", got.file.id)
		}
	}
}

// What a file covers is its range clipped by its warnings (14.6 of the
// design): an empty file covers nothing until you say it is complete; one
// at the cap starts the day after its oldest row; one exported on its
// range's last day ends the day before; a range you give counts where it
// states none.
func TestFileCoverage(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		f            *fileInput
		from, to     nytime.Date
		completeness string
		want         string
	}{
		{"stated", testFile(1, "a", "2026-03-01", "2026-01-01", "2026-01-31"), "", "", "", "2026-01-01 2026-01-31 stated"},
		{"exported on its last day", testFile(1, "a", "2026-01-31", "2026-01-01", "2026-01-31"), "", "", "", "2026-01-01 2026-01-30 stated"},
		{"empty", withWarnings(testFile(1, "a", "2026-03-01", "2026-01-01", "2026-01-31"), "empty"), "", "", "", "  stated"},
		{"empty, complete", withWarnings(testFile(1, "a", "2026-03-01", "2026-01-01", "2026-01-31"), "empty"), "", "", "complete", "2026-01-01 2026-01-31 stated"},
		{"at the cap", withWarnings(withRows(testFile(1, "a", "2026-03-01", "2026-01-01", "2026-01-31"), "2026-01-10"), "at_cap"), "", "", "", "2026-01-11 2026-01-31 stated"},
		{"yours", withRows(testFile(1, "a", "2026-03-01", "", ""), "2026-01-10"), "2026-01-05", "2026-01-20", "", "2026-01-05 2026-01-20 user"},
	} {
		c.f.from, c.f.to, c.f.completeness = c.from, c.to, c.completeness
		cov := fileCoverage(c.f)
		if got := fmt.Sprintf("%s %s %s", cov.From, cov.To, cov.Source); got != c.want {
			t.Errorf("%s: covers %q, want %q", c.name, got, c.want)
		}
	}
}

// withWarnings gives f warnings by code, as its stored flags do.
func withWarnings(f *fileInput, codes ...string) *fileInput {
	f.span.Warnings = append(f.span.Warnings, codes...)
	return f
}

// withRows gives f a credit interest row on each of days.
func withRows(f *fileInput, days ...nytime.Date) *fileInput {
	for _, d := range days {
		addRow(f, d, "Credit Interest", "", 7, 0)
	}
	return f
}

// completenessOf derives in's files and returns each file's completeness
// and what established it, in the order of the files.
func completenessOf(in accountInputs, api []ledgerEntry, runs []coverageRun) []string {
	var out []string
	for _, f := range deriveFiles(in, api, runs).files {
		words := []string{f.completeness, f.completeBy}
		for _, fl := range f.flags.sorted() {
			if slices.Contains([]string{"lacks_rows", "looks_filtered"}, fl.code) {
				words = append(words, fl.code)
			}
		}
		out = append(out, strings.Join(slices.DeleteFunc(words, func(w string) bool { return w == "" }), " "))
	}
	return out
}

// A file's completeness is established, never assumed (14.6 of the
// design), and does not depend on the order of the files: a file alone is
// unknown; one of a single family over a long span looks filtered; one
// that lacks API rows of a family it has none of is filtered; one that
// agrees with the API's coverage is complete by the API, and one that
// agrees only with that file complete by file; a lack with a correction's
// shape does not demote; your decision overrides each.
func TestCompleteness(t *testing.T) {
	t.Parallel()
	full := func() *fileInput {
		f := testFile(1, "a", "2026-03-01", "2026-02-01", "2026-02-28")
		addRow(f, "2026-02-03", "Qualified Dividend", "ABCD", 100, 0)
		addRow(f, "2026-02-10", "Buy", "XYZ", -5000, 5)
		addRow(f, "2026-02-06", "Qualified Dividend", "ABCD", 200, 0)
		return f
	}
	api := []ledgerEntry{
		{entryRow: entryRow{day: nullString("2026-02-03"), amount: sql.NullInt64{Int64: 100, Valid: true}, family: "income", txn: sql.NullInt64{Int64: 1, Valid: true}}},
		{entryRow: entryRow{day: nullString("2026-02-10"), amount: sql.NullInt64{Int64: -5000, Valid: true}, family: "buy", txn: sql.NullInt64{Int64: 2, Valid: true}}},
	}
	apiRuns := []coverageRun{{source: "api", from: "2026-02-01", to: "2026-02-11"}}

	if got := completenessOf(accountInputs{files: []*fileInput{full()}}, nil, nil); !slices.Equal(got, []string{"unknown"}) {
		t.Errorf("alone: %q", got)
	}
	if got := completenessOf(accountInputs{files: []*fileInput{full()}}, api, apiRuns); !slices.Equal(got, []string{"complete api"}) {
		t.Errorf("beside the API: %q", got)
	}
	divs := testFile(2, "b", "2026-03-02", "2026-02-01", "2026-02-28")
	addRow(divs, "2026-02-03", "Qualified Dividend", "ABCD", 100, 0)
	addRow(divs, "2026-02-12", "Qualified Dividend", "ABCD", 200, 0)
	if got := completenessOf(accountInputs{files: []*fileInput{divs}}, api, apiRuns); !slices.Equal(got, []string{"filtered lacks_rows"}) {
		t.Errorf("a dividends-only file beside the API: %q", got)
	}
	many := testFile(3, "c", "2026-06-01", "", "")
	for i := range 21 {
		addRow(many, nytime.Date(time.Date(2026, 1, 1+4*i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)), "Qualified Dividend", "ABCD", int64(100+i), 0)
	}
	if got := completenessOf(accountInputs{files: []*fileInput{many}}, nil, nil); !slices.Equal(got, []string{"unknown looks_filtered"}) {
		t.Errorf("21 dividends over 80 days: %q", got)
	}
	// A later export that agrees with the complete one but lacks one
	// dividend among others of its symbol: complete by file, in either order.
	a := full()
	later := testFile(4, "d", "2026-03-05", "2026-02-01", "2026-02-28")
	addRow(later, "2026-02-03", "Qualified Dividend", "ABCD", 100, 0)
	addRow(later, "2026-02-10", "Buy", "XYZ", -5000, 5)
	a.completeness = "complete"
	if got := completenessOf(accountInputs{files: []*fileInput{a, later}}, nil, nil); !slices.Equal(got, []string{"complete you", "complete file"}) {
		t.Errorf("two exports: %q", got)
	}
	later.id, a.id = 0, 9
	if got := completenessOf(accountInputs{files: []*fileInput{later, a}}, nil, nil); !slices.Equal(got, []string{"complete file", "complete you"}) {
		t.Errorf("two exports in the other order: %q", got)
	}
	divs.completeness = "complete"
	if got := completenessOf(accountInputs{files: []*fileInput{divs}}, api, apiRuns); !slices.Equal(got, []string{"complete you"}) {
		t.Errorf("your word on the dividends-only file: %q", got)
	}
	// A -json file's coverage shows a file filtered, but never makes one
	// complete.
	if got := completenessOf(accountInputs{files: []*fileInput{full()}}, api, []coverageRun{{source: "api_file", from: "2026-02-01", to: "2026-02-11"}}); !slices.Equal(got, []string{"unknown"}) {
		t.Errorf("beside a -json file's rows: %q", got)
	}
	// Files are settled in rounds: the third agrees only with the second,
	// which agrees with the first you said is complete; in either order.
	first := testFile(1, "a", "2026-03-01", "2026-01-01", "2026-01-31")
	second := testFile(2, "b", "2026-03-02", "2026-01-15", "2026-02-28")
	third := testFile(3, "c", "2026-04-01", "2026-02-01", "2026-03-31")
	addRow(first, "2026-01-20", "Qualified Dividend", "ABCD", 100, 0)
	addRow(second, "2026-01-20", "Qualified Dividend", "ABCD", 100, 0)
	addRow(second, "2026-02-10", "Buy", "XYZ", -5000, 5)
	addRow(third, "2026-02-10", "Buy", "XYZ", -5000, 5)
	addRow(third, "2026-03-10", "Sell", "XYZ", 6000, 5)
	first.completeness = "complete"
	if got := completenessOf(accountInputs{files: []*fileInput{first, second, third}}, nil, nil); !slices.Equal(got, []string{"complete you", "complete file", "complete file"}) {
		t.Errorf("three exports in a chain: %q", got)
	}
	if got := completenessOf(accountInputs{files: []*fileInput{third, second, first}}, nil, nil); !slices.Equal(got, []string{"complete file", "complete file", "complete you"}) {
		t.Errorf("three exports in a chain, the other way: %q", got)
	}
}

// A file lacks only what Schwab still holds (14.6 of the design): an API
// row gone from a later complete read, or INVALID, which a complete
// export rightly leaves out, does not make it filtered or keep it from
// being complete by the API; one you decided not to count is still
// Schwab's, and a file without its family is filtered. Nor is a gone row
// a row of the days a file shares with the API, for it to agree on.
func TestCompletenessOfGone(t *testing.T) {
	t.Parallel()
	entry := func(id int64, day string, amount int64, family string) ledgerEntry {
		return ledgerEntry{entryRow: entryRow{
			day: nullString(day), amount: sql.NullInt64{Int64: amount, Valid: true}, family: family, txn: sql.NullInt64{Int64: id, Valid: true},
			counted: true, countedWhy: "api",
		}}
	}
	runs := []coverageRun{{source: "api", from: "2026-01-12", to: "2026-02-02"}}
	for _, c := range []struct {
		name string
		buy  func(e *ledgerEntry)
		want string
	}{
		{"gone", func(e *ledgerEntry) {
			e.counted, e.countedWhy = false, "gone"
			e.flags.add("gone", "left out by the complete read sent 2026-02-03T15:00:00.000Z")
		}, "complete api"},
		{"INVALID", func(e *ledgerEntry) {
			e.counted, e.countedWhy = false, "invalid"
			e.flags.add("status", "INVALID")
		}, "complete api"},
		{"not counted by you", func(e *ledgerEntry) { e.counted, e.countedWhy = false, "user" }, "filtered lacks_rows"},
		{"pending", func(e *ledgerEntry) { e.flags.add("status", "PENDING") }, "filtered lacks_rows"},
	} {
		f := testFile(1, "a", "2026-02-10", "2026-01-01", "2026-02-01")
		addRow(f, "2026-01-12", "Qualified Dividend", "ABCD", 321, 0)
		addRow(f, "2026-01-25", "Qualified Dividend", "ABCD", 100, 0)
		api := []ledgerEntry{entry(1001, "2026-01-12", 321, "income"), entry(1002, "2026-01-20", -5000, "buy"), entry(1007, "2026-01-25", 100, "income")}
		c.buy(&api[1])
		if got := completenessOf(accountInputs{files: []*fileInput{f}}, api, runs); !slices.Equal(got, []string{c.want}) {
			t.Errorf("beside a buy %s: %q, want %q", c.name, got, c.want)
		}
	}
	// Nor does a gone row give the days the file shares with the API a row
	// for it to agree on.
	f := testFile(1, "a", "2026-02-10", "2026-01-01", "2026-02-01")
	addRow(f, "2026-01-05", "Qualified Dividend", "ABCD", 321, 0)
	gone := entry(1002, "2026-01-20", -5000, "buy")
	gone.flags.add("gone", "")
	if got := completenessOf(accountInputs{files: []*fileInput{f}}, []ledgerEntry{gone}, runs); !slices.Equal(got, []string{"unknown"}) {
		t.Errorf("sharing with the API only days of a gone row: %q", got)
	}
}

// Your decision on a row of a file follows the transaction that holds the
// row (13.7 of the design): set the first file aside, and a decision made
// on its row applies to the transaction the second file's row now opens;
// a decision on a row no other file holds waits; the outputs do not
// depend on the order of the files' rows.
func TestDeriveAccountFiles(t *testing.T) {
	t.Parallel()
	a := testFile(1, "a", "2026-03-01", "2026-02-01", "2026-02-28")
	b := testFile(2, "b", "2026-03-02", "2026-02-01", "2026-02-28")
	addRow(a, "2026-02-03", "Wire Funds", "", -50000, 0)
	addRow(a, "2026-02-05", "Credit Interest", "", 7, 0)
	addRow(b, "2026-02-03", "Wire Funds", "", -50000, 0)
	in := accountInputs{
		account: 1, number: fileAccount, files: []*fileInput{a, b},
		fileDecisions: []decisionInput{
			{id: 1, kind: "category", subject: "c:" + a.sha + ":1", value: "transfer_out"},
			{id: 2, kind: "count", subject: "c:" + a.sha + ":2", value: "no"},
		},
	}
	entries := func(in accountInputs) []string {
		var out []string
		for _, e := range deriveAccount(in).entries {
			out = append(out, fmt.Sprintf("%s %s %s %v %s", e.ref, e.day.String, e.category, e.counted, e.countedWhy))
		}
		return out
	}
	if got := entries(in); !slices.Equal(got, []string{"Ca0000000.1 2026-02-03 transfer_out true file", "Ca0000000.2 2026-02-05 interest false user"}) {
		t.Errorf("with both files: %q", got)
	}
	a.forgotten = true
	if got := entries(in); !slices.Equal(got, []string{"Cb0000000.1 2026-02-03 transfer_out true file"}) {
		t.Errorf("with the first set aside: %q", got)
	}
	slices.Reverse(a.rows)
	slices.Reverse(in.fileDecisions)
	if got := entries(in); !slices.Equal(got, []string{"Cb0000000.1 2026-02-03 transfer_out true file"}) {
		t.Errorf("in another order: %q", got)
	}
}

// Of your decisions of a kind that reach one file transaction, on one row
// or on several of its rows, the latest holds, and auto withdraws the one
// before it (13.7 of the design); a decision on a row of a file set aside
// whose transaction other files split between two transactions reaches
// neither, and waits.
func TestFileDecisionsReach(t *testing.T) {
	t.Parallel()
	a := testFile(1, "a", "2026-03-01", "2026-02-01", "2026-02-28")
	b := testFile(2, "b", "2026-03-02", "2026-02-01", "2026-02-28")
	addRow(a, "2026-02-03", "Wire Funds", "", -50000, 0)
	addRow(a, "2026-02-05", "Credit Interest", "", 7, 0)
	addRow(b, "2026-02-03", "Wire Funds", "", -50000, 0)
	rowA, rowB, interest := "c:"+a.sha+":1", "c:"+b.sha+":1", "c:"+a.sha+":2"
	entries := func(ds ...decisionInput) []string {
		var out []string
		for _, e := range deriveAccount(accountInputs{account: 1, number: fileAccount, files: []*fileInput{a, b}, fileDecisions: ds}).entries {
			out = append(out, fmt.Sprintf("%s %s %s", e.category, e.rule, e.countedWhy))
		}
		return out
	}
	for _, c := range []struct {
		name string
		ds   []decisionInput
		want []string
	}{
		{
			"count no, then yes, on one row",
			[]decisionInput{{id: 1, kind: "count", subject: rowA, value: "no"}, {id: 2, kind: "count", subject: rowA, value: "yes"}},
			[]string{"withdrawal file.cash user", "interest file.income file"},
		},
		{
			"count no on one row, then yes on another of its transaction",
			[]decisionInput{{id: 1, kind: "count", subject: rowA, value: "no"}, {id: 2, kind: "count", subject: rowB, value: "yes"}},
			[]string{"withdrawal file.cash user", "interest file.income file"},
		},
		{
			"a category, then auto",
			[]decisionInput{{id: 1, kind: "category", subject: interest, value: "dividend"}, {id: 2, kind: "category", subject: interest, value: "auto"}},
			[]string{"withdrawal file.cash file", "interest file.income file"},
		},
	} {
		got := entries(c.ds...)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: the entries are %q, want %q", c.name, got, c.want)
		}
		slices.Reverse(c.ds)
		if got := entries(c.ds...); !slices.Equal(got, c.want) {
			t.Errorf("%s, in another order: the entries are %q, want %q", c.name, got, c.want)
		}
	}
	if got := entries(decisionInput{id: 1, kind: "count", subject: rowA, value: "no"}); got[0] != "withdrawal file.cash user" {
		t.Errorf("count no reads %q", got[0])
	}

	// Set aside, the first file's row P would join Q of the second file and
	// S of the third; without it, Q and S are in two transactions (R of the
	// third joins Q's). The decision on P waits.
	f := testFile(1, "f", "2026-03-01", "", "")
	g := testFile(2, "g", "2026-03-02", "", "")
	h := testFile(3, "h", "2026-03-03", "", "")
	addRow(f, "2026-02-13", "Buy", "P", -5000, 5)
	addRow(g, "2026-02-13", "Buy", "Q", -5000, 5)
	addRow(h, "2026-02-13", "Buy", "P", -5000, 5)
	addRow(h, "2026-02-13", "Buy", "Q", -5000, 5)
	f.forgotten = true
	in := accountInputs{
		account: 1, number: fileAccount, files: []*fileInput{f, g, h},
		fileDecisions: []decisionInput{{id: 1, kind: "category", subject: "c:" + f.sha + ":1", value: "transfer_in"}},
	}
	var got []string
	for _, e := range deriveAccount(in).entries {
		got = append(got, e.ref+" "+e.category)
	}
	if !slices.Equal(got, []string{"Cg0000000.1 buy", "Ch0000000.1 buy"}) {
		t.Errorf("a decision whose row's transaction two others now hold reached them: %q", got)
	}
}

// A decision on a file transaction goes stale (13.7 of the design) when
// the rules would read something else of the row it showed, or a row it
// was made with has left the transaction; a row of a file set aside
// leaving it does not count.
func TestStaleFile(t *testing.T) {
	t.Parallel()
	a := testFile(1, "a", "2026-03-01", "", "")
	b := testFile(2, "b", "2026-03-02", "", "")
	addRow(a, "2026-02-13", "Buy", "XYZ", -5000, 5)
	addRow(b, "2026-02-13", "Buy", "XYZ", -5000, 5)
	addRow(b, "2026-02-14", "Sell", "XYZ", 6000, 5)
	files := []*fileInput{a, b}
	both := &cluster[*historyRowInput]{rows: []*historyRowInput{a.rows[0], b.rows[0]}}
	alone := &cluster[*historyRowInput]{rows: []*historyRowInput{b.rows[0]}}
	made := decisionInput{basisShown: b.sha + ":1", basisRows: []string{a.sha + ":1", b.sha + ":1"}}
	for _, c := range []struct {
		name  string
		d     decisionInput
		c     *cluster[*historyRowInput]
		shown *historyRowInput
		want  bool
	}{
		{"as made", made, both, b.rows[0], false},
		{"a row left", made, alone, b.rows[0], true},
		{"another row shown", decisionInput{basisShown: b.sha + ":2", basisRows: made.basisRows}, both, b.rows[0], true},
		{"a row unknown", decisionInput{basisShown: b.sha + ":9", basisRows: made.basisRows}, both, b.rows[0], true},
	} {
		if got := staleFile(c.d, c.c, c.shown, files); got != c.want {
			t.Errorf("%s: stale %v, want %v", c.name, got, c.want)
		}
	}
	a.forgotten = true
	if staleFile(made, alone, b.rows[0], files) {
		t.Error("a row of a file set aside leaving the transaction makes the decision stale")
	}
}

// A file holds an API entry when one of its rows has the entry's amount,
// booked from three days before the entry's day to seven after.
func TestFileHolds(t *testing.T) {
	t.Parallel()
	f := testFile(1, "a", "2026-03-01", "", "")
	addRow(f, "2026-02-12", "Buy", "XYZ", -5000, 5)
	for _, c := range []struct {
		day    string
		amount int64
		want   bool
	}{
		{"2026-02-12", -5000, true},
		{"2026-02-05", -5000, true},
		{"2026-02-04", -5000, false},
		{"2026-02-15", -5000, true},
		{"2026-02-16", -5000, false},
		{"2026-02-12", -5001, false},
	} {
		e := ledgerEntry{entryRow: entryRow{day: nullString(c.day), amount: sql.NullInt64{Int64: c.amount, Valid: true}}}
		if got := fileHolds(f, e); got != c.want {
			t.Errorf("an entry of %d on %s: held %v, want %v", c.amount, c.day, got, c.want)
		}
	}
}

// What complete files disagree on is flagged (14.5 and 14.6 of the
// design): a file that names one symbol alone, beside API rows of another
// symbol of its family, is filtered by symbol; two transactions of one
// day, action and symbol but of other amounts, which no file holds both
// of, are near duplicates, and the earlier one missing from the later
// export; a row that only the later export holds is not missing from the
// earlier one.
func TestAcrossFiles(t *testing.T) {
	t.Parallel()
	xyz := testFile(1, "a", "2026-03-01", "2026-02-01", "2026-02-28")
	addRow(xyz, "2026-02-03", "Qualified Dividend", "XYZ", 100, 0)
	addRow(xyz, "2026-02-05", "Qualified Dividend", "XYZ", 200, 0)
	api := []ledgerEntry{{entryRow: entryRow{
		day: nullString("2026-02-04"), amount: sql.NullInt64{Int64: 300, Valid: true},
		family: "income", symbol: nullString("ABCD"), txn: sql.NullInt64{Int64: 1, Valid: true},
	}}}
	runs := []coverageRun{{source: "api", from: "2026-02-01", to: "2026-02-10"}}
	if got := completenessOf(accountInputs{files: []*fileInput{xyz}}, api, runs); !slices.Equal(got, []string{"filtered lacks_rows"}) {
		t.Errorf("an XYZ-only file beside the API's ABCD dividend: %q", got)
	}
	// What it lacks names another symbol, or is of a family it has none
	// of, with no symbol: the detail says so of each.
	lacks := func(api ...ledgerEntry) string {
		for _, f := range deriveFiles(accountInputs{files: []*fileInput{xyz}}, api, runs).files[0].flags.sorted() {
			if f.code == "lacks_rows" {
				return f.detail
			}
		}
		return ""
	}
	wire := ledgerEntry{entryRow: entryRow{
		day: nullString("2026-02-06"), amount: sql.NullInt64{Int64: -500, Valid: true},
		family: "cash", txn: sql.NullInt64{Int64: 2, Valid: true},
	}}
	for _, c := range []struct {
		api  []ledgerEntry
		want string
	}{
		{api, "lacks 1 row that Schwab's API holds between 2026-02-04 and 2026-02-04, " +
			"every one naming another symbol than the one this file's rows name: an export filtered by symbol?"},
		{[]ledgerEntry{wire}, "lacks 1 row that Schwab's API holds between 2026-02-06 and 2026-02-06, " +
			"every one of a family this file has none of: an export filtered by type?"},
		{append(slices.Clone(api), wire), "lacks 2 rows that Schwab's API holds between 2026-02-04 and 2026-02-06, " +
			"each of a family this file has none of, or naming another symbol than the one its rows name: an export filtered by type or symbol?"},
	} {
		if got := lacks(c.api...); got != c.want {
			t.Errorf("lacks_rows says %q, want %q", got, c.want)
		}
	}

	a := testFile(1, "a", "2026-03-01", "2026-02-01", "2026-02-28")
	b := testFile(2, "b", "2026-03-02", "2026-02-01", "2026-02-28")
	addRow(a, "2026-02-02", "Qualified Dividend", "ABCD", 100, 0)
	addRow(a, "2026-02-06", "Qualified Dividend", "ABCD", 300, 0)
	addRow(b, "2026-02-02", "Qualified Dividend", "ABCD", 110, 0)
	addRow(b, "2026-02-06", "Qualified Dividend", "ABCD", 300, 0)
	addRow(b, "2026-02-08", "Qualified Dividend", "ABCD", 400, 0)
	a.completeness, b.completeness = "complete", "complete"
	var got []string
	for _, e := range deriveFiles(accountInputs{files: []*fileInput{a, b}}, nil, nil).entries {
		var codes []string
		for _, f := range e.flags.sorted() {
			codes = append(codes, f.code+" "+f.detail)
		}
		got = append(got, fmt.Sprintf("%s %d %s", e.day.String, e.amount.Int64, strings.Join(codes, ",")))
	}
	want := []string{
		"2026-02-02 100 missing_from_later Fb0000000 () does not hold it,near_duplicate Cb0000000.1",
		"2026-02-06 300 ",
		"2026-02-02 110 near_duplicate Ca0000000.1",
		"2026-02-08 400 ",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the entries are %q, want %q", got, want)
	}
}

// Near duplicates need two transactions that no one file holds both of,
// each covered by a file of the other: one file holding both is a file of
// two transactions, and a file that does not cover the day cannot say it
// lacks the other.
func TestNearDuplicateNeeds(t *testing.T) {
	t.Parallel()
	nearDuplicates := func(files ...*fileInput) int {
		n := 0
		for _, e := range deriveFiles(accountInputs{files: files}, nil, nil).entries {
			if e.flags.has("near_duplicate") {
				n++
			}
		}
		return n
	}
	one := testFile(1, "a", "2026-03-01", "2026-02-01", "2026-02-28")
	addRow(one, "2026-02-02", "Qualified Dividend", "ABCD", 100, 0)
	addRow(one, "2026-02-02", "Qualified Dividend", "ABCD", 110, 0)
	if n := nearDuplicates(one); n != 0 {
		t.Errorf("two rows of one file are %d near duplicates", n)
	}
	// Exported on the day, the first file covers it no more.
	early := testFile(1, "a", "2026-02-02", "2026-01-01", "2026-02-02")
	later := testFile(2, "b", "2026-03-02", "2026-02-01", "2026-02-28")
	addRow(early, "2026-02-02", "Qualified Dividend", "ABCD", 100, 0)
	addRow(later, "2026-02-02", "Qualified Dividend", "ABCD", 110, 0)
	if n := nearDuplicates(early, later); n != 0 {
		t.Errorf("a row of a day its file does not cover is one of %d near duplicates", n)
	}
	early.exportedAt = later.exportedAt // exported later, it covers the day
	if n := nearDuplicates(early, later); n != 2 {
		t.Errorf("covered both ways, the two rows are %d near duplicates, want 2", n)
	}
}

// A transaction's rows that differ in quantity are restated_quantity only
// when it moves holdings; rows that name different symbols are
// symbol_unsure unless one security holds both.
func TestClusterFlags(t *testing.T) {
	t.Parallel()
	a := testFile(1, "a", "2026-03-01", "", "")
	b := testFile(2, "b", "2026-03-02", "", "")
	addRow(a, "2026-02-13", "Qualified Dividend", "OLD", 500, 5)
	addRow(b, "2026-02-13", "Qualified Dividend", "NEW", 500, 10)
	c := &cluster[*historyRowInput]{rows: []*historyRowInput{a.rows[0], b.rows[0]}}
	refs := map[*fileInput]string{a: "a", b: "b"}
	codes := func(family string, secs *securities) []string {
		var fs flagSet
		clusterFlags(&fs, c, refs, secs, family)
		var out []string
		for _, f := range fs.sorted() {
			out = append(out, f.code)
		}
		return out
	}
	apart := &securities{}
	joined := &securities{keys: map[string]secInfo{a.rows[0].fileKey: {id: 3}, b.rows[0].fileKey: {id: 3}}}
	for _, k := range []struct {
		family string
		secs   *securities
		want   []string
	}{
		{"income", apart, []string{"restated", "symbol_unsure"}},
		{"buy", apart, []string{"restated", "restated_quantity", "symbol_unsure"}},
		{"income", joined, []string{"restated"}},
	} {
		if got := codes(k.family, k.secs); !slices.Equal(got, k.want) {
			t.Errorf("%s rows of OLD and NEW, of 5 and 10, keys %v: flagged %q, want %q", k.family, k.secs.keys, got, k.want)
		}
	}
}

// A file looks filtered by its rows alone when more than 20 of them, over
// more than 60 days, are all of one family.
func TestShapeCheck(t *testing.T) {
	t.Parallel()
	family := func(*historyRowInput) string { return "income" }
	for _, c := range []struct {
		rows, days int
		looks      bool
	}{{21, 61, true}, {20, 61, false}, {21, 60, false}} {
		f := testFile(1, "a", "2026-06-01", "", "")
		for i := range c.rows {
			day := min(i*4, c.days)
			if i == c.rows-1 {
				day = c.days
			}
			addRow(f, addDays("2026-01-01", day), "Qualified Dividend", "", int64(100+i), 0)
		}
		if got := shapeCheck(f.rows, family) != ""; got != c.looks {
			t.Errorf("%d rows over %d days: looks filtered %v, want %v", c.rows, c.days, got, c.looks)
		}
	}
}
