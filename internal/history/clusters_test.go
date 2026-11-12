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
// nothing resolves, an empty symbol, or two option roots of one contract.
func TestKnownDifferent(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	secs := &securities{
		keys: map[string]secInfo{
			"x:1": {1, true}, "x:2": {2, true}, "x:3": {3, true}, "s:FILE:AAA@2026-09-20": {4, false}, "s:FILE:BBB@2026-09-20": {5, false},
			"s:FILE:CCC@2026-09-20": {6, false}, "s:FILE:DDD@2026-09-20": {7, false},
		},
		periods: map[int64][][2]time.Time{
			1: {{day, day}}, 2: {{day.AddDate(0, 0, 5), day.AddDate(0, 0, 6)}}, 3: {{day.AddDate(1, 0, 0), day.AddDate(1, 0, 0)}},
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
