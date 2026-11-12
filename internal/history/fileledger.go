package history

import (
	"cmp"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A fileOutput is what the derivation gives a file of the account: its
// coverage, its completeness and its flags.
type fileOutput struct {
	id int64
	// from and to are the days it covers, "" when none, and source where
	// its range came from: stated, user or rows.
	from, to nytime.Date
	source   string
	// completeness is complete, filtered or unknown for a History file, ""
	// for a Realized one or one set aside; completeBy says what
	// established complete (you, api or file), and lacks counts the rows
	// it lacks in a filter's shape.
	completeness, completeBy string
	lacks                    sql.NullInt64
	flags                    flagSet
}

// A csvTxnOutput is a file transaction: the rows of History files that
// are one transaction (14.5 of the design).
type csvTxnOutput struct {
	ref, subject string
	coarse       [32]byte
	// shown is the row its entry shows, the newest export's, and asOf
	// that export's date, the basis of its quantity.
	shown               *historyRowInput
	quantityAsOf        nytime.Date
	posted, asOf, event nytime.Date
	files               int
	members             []*historyRowInput
}

// A lotOutput is a closed lot: the lots of Realized files that are one.
type lotOutput struct {
	ref, subject    string
	shown           *lotRowInput
	versions, files int
	security        sql.NullInt64
	members         []*lotRowInput
	flags           flagSet
}

// A fileLedger is what deriveFiles gives: what the account's files cover,
// how complete they are and their flags, their transactions and lots, and
// their entries.
type fileLedger struct {
	files    []fileOutput
	txns     []csvTxnOutput
	lots     []lotOutput
	entries  []ledgerEntry
	coverage []coverageRun
	// decided are the latest of your decisions of each kind that reach
	// each of txns, and all every one that does; complete are what the
	// complete History files cover.
	decided  []map[string]decisionInput
	all      [][]decisionInput
	complete []schwabfile.Coverage
	// rowSubjects are the transactions that hold the rows that your pair
	// decisions name (accountOutputs).
	rowSubjects map[string]string
}

// holdsFamilies are the families of file transactions that move holdings,
// whose quantities restated_quantity watches.
var holdsFamilies = []string{"buy", "sell", "shares", "holding", "reversal"}

// deriveFiles derives what the account's files give (14.5 and 14.6 of the
// design): each file's coverage, clipped as its warnings show; its
// transactions, each counted once across files by its coarse identity
// and known differences, and its closed lots likewise; the files'
// completeness, established by your decision or by agreement with the API
// or a complete file; the entries of the file transactions, classified,
// with your decisions applied to the transaction that holds the row they
// name; and the flags of files, transactions and lots. api are the
// account's API entries and apiRuns its API coverage, which the
// completeness reads.
func deriveFiles(in accountInputs, api []ledgerEntry, apiRuns []coverageRun) fileLedger {
	var out fileLedger
	var shas []string
	for _, f := range in.files {
		shas = append(shas, f.sha)
	}
	refs := map[*fileInput]string{} // the hex of each file's C and L refs
	for _, f := range in.files {
		refs[f] = hexRef(f.sha, shas)
	}
	covs := map[*fileInput]schwabfile.Coverage{}
	var rows []*historyRowInput
	var lots []*lotRowInput
	for _, f := range in.files {
		if f.forgotten {
			continue
		}
		covs[f] = fileCoverage(f)
		for _, r := range f.rows {
			if r.posted != "" {
				rows = append(rows, r)
			}
		}
		lots = append(lots, f.lots...)
	}
	secs := in.secs
	if secs == nil {
		secs = &securities{}
	}
	clusters := clusterRows(rows, secs, historyScore)
	families := map[*historyRowInput]string{}
	familyOf := func(r *historyRowInput) string {
		f, ok := families[r]
		if !ok {
			f = classifyFile(r.class()).family
			families[r] = f
		}
		return f
	}
	byRow := map[*historyRowInput]int{} // row -> its cluster
	for i, c := range clusters {
		for _, r := range c.rows {
			byRow[r] = i
		}
	}

	comp := completeness(in.files, covs, clusters, familyOf, api, apiRuns)
	out.txns = make([]csvTxnOutput, len(clusters))
	for i, c := range clusters {
		o := c.opener()
		s := shown(c)
		t := csvTxnOutput{
			ref:     "C" + refs[o.file] + "." + strconv.Itoa(o.ordinal),
			subject: "c:" + o.file.sha + ":" + strconv.Itoa(o.ordinal),
			coarse:  o.coarse, shown: s, quantityAsOf: s.file.exported(),
			posted: s.posted, asOf: s.asOf, event: s.event(), members: c.rows,
		}
		files := map[*fileInput]bool{}
		for _, r := range c.rows {
			files[r.file] = true
		}
		t.files = len(files)
		out.txns[i] = t
	}
	reach := newRowReach(in, byRow, secs)
	out.decided, out.all = fileDecisions(in, reach, len(clusters))
	out.rowSubjects = map[string]string{}
	for _, d := range in.pairDecisions {
		for _, row := range rowsNamed(d) {
			i, ok := reach.of(row)
			switch {
			case ok:
				out.rowSubjects[row] = out.txns[i].subject
			case reach.holds(row):
				out.rowSubjects[row] = ""
			}
		}
	}
	for i, c := range clusters {
		out.entries = append(out.entries, fileEntry(in, out.txns[i], c, out.decided[i], refs, secs, familyOf))
	}
	acrossFiles(out.entries, clusters, comp, covs, familyOf, refs)

	for _, f := range in.files {
		out.files = append(out.files, fileFlags(in, f, covs, comp[f]))
		if comp[f].completeness == "complete" && covs[f].From != "" {
			out.complete = append(out.complete, covs[f])
		}
	}
	out.lots = deriveLots(lots, secs, refs)
	out.coverage = fileCoverageRuns(in, covs, comp)
	return out
}

// fileCoverage returns the days that f covers: its span's coverage with
// the range you gave, which an empty file that you said is complete (its
// range had no activity) covers whole.
func fileCoverage(f *fileInput) schwabfile.Coverage {
	span := f.span
	if f.completeness == "complete" && slices.Contains(span.Warnings, schwabfile.WarnEmpty) {
		span.Warnings = slices.DeleteFunc(slices.Clone(span.Warnings), func(c string) bool { return c == schwabfile.WarnEmpty })
	}
	return span.Coverage(f.from, f.to, f.exported())
}

// covers reports whether a coverage holds day.
func covers(c schwabfile.Coverage, day nytime.Date) bool {
	return c.From != "" && day >= c.From && day <= c.To
}

// A fileComplete is how complete a History file is (14.6 of the design).
type fileComplete struct {
	completeness, by string
	// lacks counts the rows it lacks in a filter's shape, and lacksDetail
	// says what; looksFiltered says that its rows have a filter's shape.
	lacks         int
	lacksDetail   string
	looksFiltered string
}

// A lack is a row that another source of the account holds inside a
// file's coverage and the file does not.
type lack struct {
	day            nytime.Date
	family, symbol string
	// source is the ref of the file that holds it, or "" for the API.
	source string
}

// completeness establishes how complete each History file is (14.6 of the
// design): by your decision; else filtered when it lacks, in a filter's
// shape, rows that another source holds inside its coverage (of the API's,
// those that Schwab still holds) (it holds no
// row of their family, or names one symbol alone and they another); else
// unknown when its rows have a filter's shape by themselves; else complete
// when it shares days holding rows with the API's coverage, or with a file
// established complete, and lacks none of that source's rows there but
// for a correction's shape; else unknown. Files are settled in rounds, each
// using the files the rounds before made complete, until none changes, so
// that the result does not depend on their order.
func completeness(files []*fileInput, covs map[*fileInput]schwabfile.Coverage, clusters []*cluster[*historyRowInput],
	familyOf func(*historyRowInput) string, api []ledgerEntry, apiRuns []coverageRun,
) map[*fileInput]fileComplete {
	out := map[*fileInput]fileComplete{}
	inRuns := func(day nytime.Date, sources ...string) bool {
		for _, r := range apiRuns {
			if slices.Contains(sources, r.source) && day >= r.from && day <= r.to {
				return true
			}
		}
		return false
	}
	var active []*fileInput
	for _, f := range files {
		if !f.forgotten && f.isHistory() {
			active = append(active, f)
		}
	}
	// rowsIn returns the dated rows of f inside its coverage.
	rowsIn := func(f *fileInput) []*historyRowInput {
		var rs []*historyRowInput
		for _, r := range f.rows {
			if r.posted != "" && covers(covs[f], r.posted) {
				rs = append(rs, r)
			}
		}
		return rs
	}
	// lacksOf returns what f lacks, on the days that within holds, of the
	// transactions of other files (with files; of fromFile alone when it
	// is set) and of the API's entries (with fromAPI).
	lacksOf := func(f *fileInput, within func(nytime.Date) bool, files bool, fromFile *fileInput, fromAPI bool) []lack {
		var ls []lack
		cov := covs[f]
		if cov.From == "" {
			return nil
		}
		if files {
			for _, c := range clusters {
				var has, other bool
				var src *fileInput
				for _, r := range c.rows {
					switch {
					case r.file == f:
						has = true
					case fromFile == nil || r.file == fromFile:
						other, src = true, r.file
					}
				}
				o := c.opener()
				if has || !other || !covers(cov, o.posted) || !within(o.posted) {
					continue
				}
				s := shown(c)
				ls = append(ls, lack{day: o.posted, family: familyOf(s), symbol: fineSymbol(s.symbol), source: src.ref})
			}
		}
		if fromAPI {
			for _, e := range api {
				day := nytime.Date(e.day.String)
				if !e.day.Valid || !schwabHolds(e) || !covers(cov, day) || !within(day) || !inRuns(day, "api", "api_file") {
					continue
				}
				if !fileHolds(f, e) {
					ls = append(ls, lack{day: day, family: e.family, symbol: fineSymbol(e.symbol.String)})
				}
			}
		}
		return ls
	}
	everywhere := func(nytime.Date) bool { return true }
	settled := map[*fileInput]bool{}
	for _, f := range active {
		rs := rowsIn(f)
		var c fileComplete
		switch f.completeness {
		case "complete", "filtered":
			c = fileComplete{completeness: f.completeness}
			if c.completeness == "complete" {
				c.by = "you"
			}
			out[f], settled[f] = c, true
			continue
		}
		var shaped []lack
		for _, l := range lacksOf(f, everywhere, true, nil, true) {
			if typeShaped(l, rs, familyOf) || symbolShaped(l, rs) {
				shaped = append(shaped, l)
			}
		}
		if len(shaped) > 0 {
			out[f] = fileComplete{completeness: "filtered", lacks: len(shaped), lacksDetail: lacksDetail(shaped, rs, familyOf)}
			settled[f] = true
			continue
		}
		if why := shapeCheck(f.rows, familyOf); why != "" {
			out[f] = fileComplete{completeness: "unknown", looksFiltered: why}
			settled[f] = true
		}
	}
	// agrees reports whether f agrees with a source on the days it shares
	// with it: at least one holds a row there, and f lacks none of the
	// source's rows there but in a correction's shape.
	agrees := func(f *fileInput, shared func(nytime.Date) bool, fromFile *fileInput, rowsThere bool) bool {
		for _, r := range rowsIn(f) {
			rowsThere = rowsThere || shared(r.posted)
		}
		if !rowsThere {
			return false
		}
		rs := rowsIn(f)
		for _, l := range lacksOf(f, shared, fromFile != nil, fromFile, fromFile == nil) {
			if !correctionShaped(l, rs, familyOf) {
				return false
			}
		}
		return true
	}
	for _, f := range active {
		if settled[f] {
			continue
		}
		shared := func(d nytime.Date) bool { return covers(covs[f], d) && inRuns(d, "api") }
		apiRows := slices.ContainsFunc(api, func(e ledgerEntry) bool {
			return e.day.Valid && schwabHolds(e) && shared(nytime.Date(e.day.String))
		})
		if agrees(f, shared, nil, apiRows) {
			out[f], settled[f] = fileComplete{completeness: "complete", by: "api"}, true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, f := range active {
			if settled[f] {
				continue
			}
			for _, g := range active {
				if g == f || out[g].completeness != "complete" {
					continue
				}
				shared := func(d nytime.Date) bool { return covers(covs[f], d) && covers(covs[g], d) }
				gRows := slices.ContainsFunc(rowsIn(g), func(r *historyRowInput) bool { return shared(r.posted) })
				if agrees(f, shared, g, gRows) {
					out[f], settled[f], changed = fileComplete{completeness: "complete", by: "file"}, true, true
					break
				}
			}
		}
	}
	for _, f := range active {
		if !settled[f] {
			out[f] = fileComplete{completeness: "unknown"}
		}
	}
	return out
}

// schwabHolds reports whether Schwab still holds API entry e: it is not
// gone (9.5 of the design) and its status is not INVALID. A complete
// export rightly leaves out what Schwab no longer holds, so such an entry
// is no row that a file lacks (14.6). Your count decision does not change
// what Schwab holds: an entry you decided not to count is still one.
func schwabHolds(e ledgerEntry) bool {
	return !e.flags.has("gone") && !slices.Contains(e.flags, entryFlag{"status", string(schwab.TransactionStatusInvalid)})
}

// fileHolds reports whether file f holds a row that may be API entry e:
// one of the same amount, booked within the widest of the windows that
// matching allows (from three days before to seven after the API's day).
func fileHolds(f *fileInput, e ledgerEntry) bool {
	for _, r := range f.rows {
		amount := r.class().amount
		if r.posted == "" || !amount.Valid || !e.amount.Valid || amount.Int64 != e.amount.Int64 {
			continue
		}
		if d, ok := daysApart(e.day.String, string(r.event())); ok && d >= -3 && d <= 7 {
			return true
		}
	}
	return false
}

// typeShaped reports whether a lack has the shape of a filter by type:
// the file holds no row of its family.
func typeShaped(l lack, rs []*historyRowInput, familyOf func(*historyRowInput) string) bool {
	return !slices.ContainsFunc(rs, func(r *historyRowInput) bool { return familyOf(r) == l.family })
}

// symbolShaped reports whether a lack has the shape of a filter by symbol:
// every row of the file names one symbol, and it another.
func symbolShaped(l lack, rs []*historyRowInput) bool {
	one := onlySymbol(rs)
	return one != "" && l.symbol != "" && l.symbol != one
}

// onlySymbol returns the one symbol that every row of rs names, or "".
func onlySymbol(rs []*historyRowInput) string {
	one := ""
	for _, r := range rs {
		s := fineSymbol(r.symbol)
		if s == "" || one != "" && s != one {
			return ""
		}
		one = s
	}
	return one
}

// correctionShaped reports whether a lack has a correction's shape: the
// file holds rows of its family, and of its symbol when it has one,
// within a week of it, so that Schwab dropped or changed that one row
// between exports.
func correctionShaped(l lack, rs []*historyRowInput, familyOf func(*historyRowInput) string) bool {
	return slices.ContainsFunc(rs, func(r *historyRowInput) bool {
		d, ok := daysApart(string(l.day), string(r.posted))
		return ok && d >= -7 && d <= 7 && familyOf(r) == l.family && (l.symbol == "" || fineSymbol(r.symbol) == l.symbol)
	})
}

// shapeCheck returns why a file's rows look filtered, or "": more than 20
// rows over more than 60 days, all of one family or all naming one symbol.
func shapeCheck(rows []*historyRowInput, familyOf func(*historyRowInput) string) string {
	var dated []*historyRowInput
	var oldest, newest nytime.Date
	for _, r := range rows {
		if r.posted == "" {
			continue
		}
		dated = append(dated, r)
		if oldest == "" || r.posted < oldest {
			oldest = r.posted
		}
		newest = max(newest, r.posted)
	}
	if d, _ := daysApart(string(oldest), string(newest)); len(dated) <= 20 || d <= 60 {
		return ""
	}
	if one := onlySymbol(dated); one != "" {
		return fmt.Sprintf("its %d rows from %s to %s all name %s: an export filtered by symbol?", len(dated), oldest, newest, one)
	}
	family := familyOf(dated[0])
	if !slices.ContainsFunc(dated, func(r *historyRowInput) bool { return familyOf(r) != family }) {
		return fmt.Sprintf("its %d rows from %s to %s are all of the %s family: an export filtered by type?", len(dated), oldest, newest, family)
	}
	return ""
}

// lacksDetail says what a file lacks in a filter's shape: how many rows,
// which sources hold them, between which days, and which filter's shape.
func lacksDetail(ls []lack, rs []*historyRowInput, familyOf func(*historyRowInput) string) string {
	var sources []string
	from, to := ls[0].day, ls[0].day
	byType := true
	for _, l := range ls {
		s := cmp.Or(l.source, "Schwab's API")
		if !slices.Contains(sources, s) {
			sources = append(sources, s)
		}
		from, to = min(from, l.day), max(to, l.day)
		byType = byType && typeShaped(l, rs, familyOf)
	}
	slices.Sort(sources)
	shape := "every one of a family this file has none of: an export filtered by type?"
	if !byType {
		shape = "every one naming another symbol than the one this file's rows name: an export filtered by symbol?"
	}
	return fmt.Sprintf("lacks %d %s that %s %s between %s and %s, %s", len(ls), plural(len(ls), "row", "rows"),
		joinAnd(sources), plural(len(sources), "holds", "hold"), from, to, shape)
}

// joinAnd joins words as a list in English: "a", "a and b", "a, b and c".
func joinAnd(words []string) string {
	if len(words) <= 1 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// A rowReach finds the cluster of the account's that holds a row of its
// files now (13.7 of the design): the cluster the row is in; for a row of a
// file set aside, the cluster that holds the other rows of the cluster it
// would be in were every file taken part, when exactly one does.
type rowReach struct {
	in    accountInputs
	secs  *securities
	byRow map[*historyRowInput]int
	// rowOf maps each row's <sha256 hex>:<ordinal> to it; it is made only
	// when needed.
	rowOf map[string]*historyRowInput
	// whole maps each row, those of files set aside included, to its
	// cluster were every file taken part; it is made only when needed.
	whole map[*historyRowInput]*cluster[*historyRowInput]
}

// newRowReach returns the rowReach of the account's files, whose rows
// byRow maps to the clusters they are in.
func newRowReach(in accountInputs, byRow map[*historyRowInput]int, secs *securities) *rowReach {
	return &rowReach{in: in, secs: secs, byRow: byRow}
}

// row returns the row of the account's files that subject,
// c:<sha256 hex>:<ordinal>, names, or nil.
func (rr *rowReach) row(subject string) *historyRowInput {
	if rr.rowOf == nil {
		rr.rowOf = map[string]*historyRowInput{}
		for _, f := range rr.in.files {
			for _, r := range f.rows {
				rr.rowOf[f.sha+":"+strconv.Itoa(r.ordinal)] = r
			}
		}
	}
	return rr.rowOf[strings.TrimPrefix(subject, "c:")]
}

// holds reports whether subject names a row of the account's files.
func (rr *rowReach) holds(subject string) bool { return rr.row(subject) != nil }

// of returns the cluster that holds the row that subject names, or false
// when none does: the row is not the account's, is undated, or is of a
// file set aside whose transaction no other file has, or two now hold.
func (rr *rowReach) of(subject string) (int, bool) {
	r := rr.row(subject)
	if r == nil {
		return 0, false
	}
	if i, ok := rr.byRow[r]; ok {
		return i, true
	}
	if !r.file.forgotten || r.posted == "" {
		return 0, false
	}
	if rr.whole == nil {
		var all []*historyRowInput
		for _, f := range rr.in.files {
			for _, x := range f.rows {
				if x.posted != "" {
					all = append(all, x)
				}
			}
		}
		rr.whole = map[*historyRowInput]*cluster[*historyRowInput]{}
		for _, c := range clusterRows(all, rr.secs, historyScore) {
			for _, x := range c.rows {
				rr.whole[x] = c
			}
		}
	}
	found := map[int]bool{}
	for _, x := range rr.whole[r].rows {
		if i, ok := rr.byRow[x]; ok {
			found[i] = true
		}
	}
	if len(found) != 1 {
		return 0, false
	}
	for i := range found {
		return i, true
	}
	return 0, false
}

// fileDecisions returns, for each of the n clusters, the latest of your
// decisions of each kind on an entry (category, count, pair,
// entry_security, match) that reach it, and every one that does, rejects
// among them, in the order they were made (13.7 of the design): a decision
// names a row, and reaches the cluster that holds that row now
// (rowReach); else it waits.
func fileDecisions(in accountInputs, reach *rowReach, n int) ([]map[string]decisionInput, [][]decisionInput) {
	out := make([]map[string]decisionInput, n)
	all := make([][]decisionInput, n)
	for _, d := range in.fileDecisions {
		i, ok := reach.of(d.subject)
		if !ok {
			continue
		}
		all[i] = append(all[i], d)
		if out[i] == nil {
			out[i] = map[string]decisionInput{}
		}
		if l, ok := out[i][d.kind]; !ok || d.id > l.id {
			out[i][d.kind] = d
		}
	}
	for i, m := range out {
		for k, d := range m {
			if d.value == "auto" {
				delete(m, k)
			}
		}
		slices.SortFunc(all[i], func(a, b decisionInput) int { return cmp.Compare(a.id, b.id) })
	}
	return out, all
}

// fileEntry makes the ledger entry of a file transaction t, the cluster c
// (13.1 of the design): its fields from the row of the newest export,
// classified by its action, your decisions that reach it applied, and its
// flags; it counts, as history the API does not hold, unless you say.
func fileEntry(in accountInputs, t csvTxnOutput, c *cluster[*historyRowInput], decided map[string]decisionInput,
	refs map[*fileInput]string, secs *securities, familyOf func(*historyRowInput) string,
) ledgerEntry {
	s := t.shown
	e := ledgerEntry{account: in.account, text: s.description, csvSubject: t.subject}
	e.ref, e.subject = t.ref, t.subject
	e.kind, e.description = nullString(s.action), nullString(s.description)
	e.day, e.posted = nullString(string(t.event)), nullString(string(t.posted))
	cl := s.class()
	e.amount = cl.amount
	e.fees = s.fees
	if s.feesBlank {
		e.fees = sql.NullInt64{Valid: true}
	}
	e.gross = addCents(e.amount, e.fees)
	e.quantity, e.price = s.quantity, s.price
	e.quantityAsOf = nullString(string(t.quantityAsOf))
	e.symbol = sql.NullString{String: s.symbol, Valid: true}
	if d, ok := decided["entry_security"]; ok {
		if id, ok := in.keySecurities[strings.TrimPrefix(d.value, "k:")]; ok {
			e.security = sql.NullInt64{Int64: id, Valid: true}
		}
	}
	if !e.security.Valid {
		if sec, ok := secs.of(s.xKey, s.fileKey); ok {
			e.security = sql.NullInt64{Int64: sec.id, Valid: true}
		}
	}
	k := classifyFile(cl)
	e.family, e.category, e.rule = k.family, k.category, k.rule
	for _, f := range k.flags {
		e.flags.add(f.code, f.detail)
	}
	if d, ok := decided["category"]; ok {
		e.category, e.rule, e.userCategory = d.value, "user", true
		e.flags.drop(answeredByCategory...)
	}
	if strings.HasPrefix(s.description, "TDA TRAN -") {
		e.flags.add("tda_history", "")
	}
	for _, p := range s.problems {
		switch p.Flag {
		case "inexact":
			e.flags.add("inexact", p.Path+" "+p.Value)
		case "placeholder":
			e.flags.add("problems", p.Path+" is "+p.Value)
		case "":
			e.flags.add("problems", p.What)
		}
	}
	clusterFlags(&e.flags, c, refs, secs, familyOf(s))
	var stale []string
	for _, kind := range decidedKinds {
		d, ok := decided[kind]
		if ok && d.basisRows != nil && staleFile(d, c, s, in.files) {
			stale = append(stale, kind+" decided against other rows")
		}
	}
	if len(stale) > 0 {
		e.flags.add("stale_decision", strings.Join(stale, "; "))
	}
	e.counted, e.countedWhy = true, "file"
	if d, ok := decided["count"]; ok {
		e.counted, e.countedWhy = d.value == "yes", "user"
	}
	return e
}

// staleFile reports whether a decision on a file transaction is stale
// (13.7 of the design): the rules would now read something else of the
// row it showed, or a row it was made with has left the transaction, files
// set aside apart.
func staleFile(d decisionInput, c *cluster[*historyRowInput], shown *historyRowInput, files []*fileInput) bool {
	rowOf := map[string]*historyRowInput{}
	for _, f := range files {
		for _, r := range f.rows {
			rowOf[f.sha+":"+strconv.Itoa(r.ordinal)] = r
		}
	}
	b := rowOf[d.basisShown]
	if b == nil || b.class() != shown.class() {
		return true
	}
	for _, id := range d.basisRows {
		r := rowOf[id]
		if r == nil {
			return true
		}
		if !r.file.forgotten && !slices.Contains(c.rows, r) {
			return true
		}
	}
	return false
}

// clusterFlags flags what the rows of a file transaction show between
// them (14.5 of the design): restated when they differ in symbol,
// description or price; restated_quantity when they differ in quantity
// and the transaction moves holdings; symbol_unsure when they name
// different symbols that no security joins; split_by_security when it was
// opened only because its row was known different from a transaction
// that held no row of its file.
func clusterFlags(flags *flagSet, c *cluster[*historyRowInput], refs map[*fileInput]string, secs *securities, family string) {
	var restated, quantities []string
	symbols := map[string]*historyRowInput{}
	for _, r := range c.rows {
		v := fmt.Sprintf("F%s: %s, %s, %s", refs[r.file], cmp.Or(r.symbol, "no symbol"), cmp.Or(fineText(r.description), "no description"), cmp.Or(r.priceText, "no price"))
		if !slices.Contains(restated, v) {
			restated = append(restated, v)
		}
		q := fmt.Sprintf("F%s: %s", refs[r.file], cmp.Or(r.quantityText, "none"))
		if !slices.ContainsFunc(c.rows, func(o *historyRowInput) bool { return o.quantityText != r.quantityText }) {
			continue
		}
		if !slices.Contains(quantities, q) {
			quantities = append(quantities, q)
		}
		if s := fineSymbol(r.symbol); s != "" {
			symbols[s] = r
		}
	}
	for _, r := range c.rows {
		if s := fineSymbol(r.symbol); s != "" {
			symbols[s] = r
		}
	}
	differ := false
	o := c.opener()
	for _, r := range c.rows[1:] {
		differ = differ || fineSymbol(r.symbol) != fineSymbol(o.symbol) || fineText(r.description) != fineText(o.description) ||
			r.priceText != o.priceText
	}
	if differ {
		flags.add("restated", strings.Join(restated, "; "))
	}
	if len(quantities) > 0 && slices.Contains(holdsFamilies, family) {
		flags.add("restated_quantity", strings.Join(quantities, "; "))
	}
	if len(symbols) > 1 {
		names := slices.Sorted(maps.Keys(symbols))
		var a, b *historyRowInput
		for i, x := range names {
			for _, y := range names[i+1:] {
				sx, okX := secs.of(symbols[x].xKey, symbols[x].fileKey)
				sy, okY := secs.of(symbols[y].xKey, symbols[y].fileKey)
				if a == nil && (!okX || !okY || sx.id != sy.id) {
					a, b = symbols[x], symbols[y]
				}
			}
		}
		if a != nil {
			var held []string
			for _, n := range names {
				held = append(held, "F"+refs[symbols[n].file]+": "+n)
			}
			flags.add("symbol_unsure", fmt.Sprintf("%s; if they are two securities: schwabtx edit %s -differ %s",
				strings.Join(held, "; "), shellQuote("K:"+a.fileKey), shellQuote("K:"+b.fileKey)))
		}
	}
	if c.split {
		flags.add("split_by_security", "its security differs from that of another file's row of its day, action and amount")
	}
}

// shellQuote quotes a word for a POSIX shell when it needs it, so that a
// command printed for you to copy gives back the ref.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789:@._/+-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// acrossFiles flags the file transactions that complete files disagree on
// (14.5 of the design): counts_differ when two complete files that cover
// its posted date hold different numbers, none of them 0, of rows of its
// coarse identity;
// missing_from_later when a complete file exported after its first file
// covers its posted date and holds rows of its family, and of its symbol,
// within a week of it, but not it; near_duplicate on two transactions of
// one posted date, action and symbol but of different coarse identities,
// which no file holds both of, each covered by a file of the other.
func acrossFiles(entries []ledgerEntry, clusters []*cluster[*historyRowInput], comp map[*fileInput]fileComplete,
	covs map[*fileInput]schwabfile.Coverage, familyOf func(*historyRowInput) string, refs map[*fileInput]string,
) {
	var complete []*fileInput
	for f, c := range comp {
		if c.completeness == "complete" {
			complete = append(complete, f)
		}
	}
	slices.SortFunc(complete, func(a, b *fileInput) int { return cmp.Compare(a.id, b.id) })
	byCoarse := map[[32]byte][]int{}
	for i, c := range clusters {
		byCoarse[c.opener().coarse] = append(byCoarse[c.opener().coarse], i)
	}
	for i, c := range clusters {
		o := c.opener()
		e := &entries[i]
		counts := map[string]int{}
		for _, f := range complete {
			if !covers(covs[f], o.posted) {
				continue
			}
			n := 0
			for _, j := range byCoarse[o.coarse] {
				for _, r := range clusters[j].rows {
					if r.file == f {
						n++
					}
				}
			}
			counts["F"+refs[f]] = n
		}
		if ns := slices.Sorted(maps.Values(counts)); len(ns) > 1 && ns[0] > 0 && ns[0] != ns[len(ns)-1] {
			var parts []string
			for _, ref := range slices.Sorted(maps.Keys(counts)) {
				parts = append(parts, fmt.Sprintf("%s holds %d", ref, counts[ref]))
			}
			e.flags.add("counts_differ", strings.Join(parts, ", "))
		}
		family, symbol := familyOf(shown(c)), fineSymbol(shown(c).symbol)
		for _, f := range complete {
			if !covers(covs[f], o.posted) || !f.exportedAt.After(o.file.exportedAt) || slices.ContainsFunc(c.rows, func(r *historyRowInput) bool { return r.file == f }) {
				continue
			}
			l := lack{day: o.posted, family: family, symbol: symbol}
			if correctionShaped(l, f.rows, familyOf) {
				e.flags.add("missing_from_later", "F"+refs[f]+" ("+f.label+") does not hold it")
			}
		}
	}
	for i, a := range clusters {
		for j := i + 1; j < len(clusters); j++ {
			b := clusters[j]
			oa, ob := a.opener(), b.opener()
			if oa.coarse == ob.coarse || oa.posted != ob.posted || normalAction(oa.action) != normalAction(ob.action) ||
				fineSymbol(oa.symbol) != fineSymbol(ob.symbol) {
				continue
			}
			filesA, filesB := map[*fileInput]bool{}, map[*fileInput]bool{}
			for _, r := range a.rows {
				filesA[r.file] = true
			}
			for _, r := range b.rows {
				filesB[r.file] = true
			}
			overlap, coverA, coverB := false, false, false
			for f := range filesA {
				overlap = overlap || filesB[f]
				coverA = coverA || covers(covs[f], ob.posted)
			}
			for f := range filesB {
				coverB = coverB || covers(covs[f], oa.posted)
			}
			if overlap || !coverA || !coverB {
				continue
			}
			entries[i].flags.add("near_duplicate", entries[j].ref)
			entries[j].flags.add("near_duplicate", entries[i].ref)
		}
	}
}

// fileFlags returns what the derivation gives a file: its coverage and
// completeness, and its flags, those its content gives by itself and those
// of its coverage, its completeness, its name and its actions.
func fileFlags(in accountInputs, f *fileInput, covs map[*fileInput]schwabfile.Coverage, comp fileComplete) fileOutput {
	o := fileOutput{id: f.id, flags: slices.Clone(f.flags)}
	if f.nameDigits != "" && !strings.HasSuffix(in.number, f.nameDigits) {
		o.flags.add("name_mismatch", fmt.Sprintf("the digits in its name, %s, are not those of A%d %s", f.nameDigits, in.account, maskNumber(in.number)))
	}
	if f.forgotten {
		return o
	}
	cov := covs[f]
	o.from, o.to, o.source = cov.From, cov.To, cov.Source
	for _, w := range cov.Warnings {
		o.flags.add(w.Code, w.Detail)
	}
	var unknown []string
	n := 0
	for _, r := range f.rows {
		if _, ok := fileActions[normalAction(r.action)]; !ok {
			n++
			if a := mask.Text(fineText(r.action)); !slices.Contains(unknown, a) {
				unknown = append(unknown, a)
			}
		}
	}
	if n > 0 {
		o.flags.add("unknown_actions", fmt.Sprintf("%d %s %s that schwabtx does not know (%s); they are kept as other: please report the action's name",
			n, plural(n, "row", "rows"), plural(n, "has an action", "have actions"), strings.Join(unknown, ", ")))
	}
	if !f.isHistory() {
		return o
	}
	o.completeness, o.completeBy = comp.completeness, comp.by
	switch {
	case comp.lacks > 0:
		o.lacks = sql.NullInt64{Int64: int64(comp.lacks), Valid: true}
		o.flags.add("lacks_rows", comp.lacksDetail)
	case comp.looksFiltered != "":
		o.flags.add("looks_filtered", comp.looksFiltered)
	}
	if comp.completeness == "unknown" {
		o.flags.add("completeness_unknown", fmt.Sprintf("if you exported every type and no symbol, say so: schwabtx edit %s -completeness complete", f.ref))
	}
	if comp.completeness != "filtered" {
		o.lacks = sql.NullInt64{Int64: int64(comp.lacks), Valid: true}
	}
	return o
}

// maskNumber returns an account's number masked, as the store shows it.
func maskNumber(n string) string { return mask.Number(n) }

// deriveLots clusters the account's closed lots across Realized files as
// file transactions are clustered (14.5 of the design), by their coarse
// identity, and flags each: lot_revised when its rows differ in any
// column, restated when they differ in symbol or name, symbol_unsure and
// split_by_security as transactions, and what its shown row's columns
// show: placeholders, values off the grid, columns that disagree.
func deriveLots(rows []*lotRowInput, secs *securities, refs map[*fileInput]string) []lotOutput {
	var out []lotOutput
	for _, c := range clusterRows(rows, secs, lotScore) {
		o, s := c.opener(), shown(c)
		l := lotOutput{
			ref: "L" + refs[o.file] + "." + strconv.Itoa(o.ordinal), subject: "l:" + o.file.sha + ":" + strconv.Itoa(o.ordinal),
			shown: s, members: c.rows,
		}
		fines, files := map[[32]byte]bool{}, map[*fileInput]bool{}
		symbols, names := map[string]bool{}, map[string]bool{}
		for _, r := range c.rows {
			fines[r.fine], files[r.file] = true, true
			symbols[fineSymbol(r.symbol)], names[fineText(r.name)] = true, true
		}
		l.versions, l.files = len(fines), len(files)
		if sec, ok := secs.of("", s.fileKey); ok {
			l.security = sql.NullInt64{Int64: sec.id, Valid: true}
		}
		if l.versions > 1 {
			l.flags.add("lot_revised", fmt.Sprintf("%d versions in %d files", l.versions, l.files))
		}
		if len(symbols) > 1 || len(names) > 1 {
			var vs []string
			for _, r := range c.rows {
				v := fmt.Sprintf("F%s: %s, %s", refs[r.file], cmp.Or(r.symbol, "no symbol"), cmp.Or(fineText(r.name), "no name"))
				if !slices.Contains(vs, v) {
					vs = append(vs, v)
				}
			}
			l.flags.add("restated", strings.Join(vs, "; "))
		}
		if len(symbols) > 1 {
			var held []string
			distinct := false
			for _, r := range c.rows {
				held = append(held, "F"+refs[r.file]+": "+cmp.Or(r.symbol, "no symbol"))
				sa, okA := secs.of("", r.fileKey)
				sb, okB := secs.of("", o.fileKey)
				distinct = distinct || !okA || !okB || sa.id != sb.id
			}
			if distinct {
				l.flags.add("symbol_unsure", strings.Join(held, "; "))
			}
		}
		if c.split {
			l.flags.add("split_by_security", "its security differs from that of another file's lot of its dates, quantity and proceeds")
		}
		for _, p := range s.problems {
			switch p.Flag {
			case "inexact":
				l.flags.add("inexact", p.Path+" "+p.Value)
			case "placeholder":
				l.flags.add("placeholder", p.Path)
			case "columns_disagree":
				l.flags.add("columns_disagree", "")
			}
		}
		out = append(out, l)
	}
	return out
}

// fileCoverageRuns returns the runs of days that the account's files
// cover, merged by source: complete History files' as file, filtered ones'
// as file_filtered, the rest as file_unknown, and Realized files' as
// realized.
func fileCoverageRuns(in accountInputs, covs map[*fileInput]schwabfile.Coverage, comp map[*fileInput]fileComplete) []coverageRun {
	var runs []coverageRun
	for _, f := range in.files {
		cov, ok := covs[f]
		if !ok || cov.From == "" {
			continue
		}
		source := "realized"
		if f.isHistory() {
			switch comp[f].completeness {
			case "complete":
				source = "file"
			case "filtered":
				source = "file_filtered"
			default:
				source = "file_unknown"
			}
		}
		runs = append(runs, coverageRun{source: source, from: cov.From, to: cov.To})
	}
	return mergeRuns(runs)
}

// mergeRuns merges runs of one source that overlap or touch, and returns
// them by source and start.
func mergeRuns(runs []coverageRun) []coverageRun {
	slices.SortFunc(runs, func(a, b coverageRun) int {
		return cmp.Or(cmp.Compare(a.source, b.source), cmp.Compare(a.from, b.from), cmp.Compare(a.to, b.to))
	})
	var out []coverageRun
	for _, r := range runs {
		if n := len(out); n > 0 && out[n-1].source == r.source && r.from <= nextDay(out[n-1].to) {
			out[n-1].to = max(out[n-1].to, r.to)
			continue
		}
		out = append(out, r)
	}
	return out
}

// addDays returns the day n days after d.
func addDays(d nytime.Date, n int) nytime.Date {
	return nytime.DateOf(midnight(d).AddDate(0, 0, n).Add(12 * 3600e9))
}
