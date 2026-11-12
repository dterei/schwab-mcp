package history

import (
	"cmp"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// A matchRule is what a file transaction of a family may be (15.1 of the
// design): API transactions of the families api, any family when api is
// nil, booked from lo to hi days before the file's day (the file's day
// less the API's trade day).
type matchRule struct {
	api    []string
	lo, hi int
}

// matchRules are the rules by the families of file transactions. Trades
// may be filled in the evening or overnight; income, tax and fees may be
// posted days after the day they are as of; a stock leg posts at
// settlement; what is neither may be anything, a few days either way.
var matchRules = map[string]matchRule{
	"buy":      {[]string{"buy"}, -1, 3},
	"sell":     {[]string{"sell"}, -1, 3},
	"reversal": {[]string{"buy", "sell"}, -1, 3},
	"income":   {[]string{"income"}, -1, 5},
	"tax":      {[]string{"tax", "income"}, -1, 5},
	"fee":      {[]string{"fee", "income", "cash", "other"}, -1, 5},
	"cash":     {[]string{"cash", "journal", "sweep"}, -2, 3},
	"journal":  {[]string{"cash", "journal", "sweep"}, -2, 3},
	"sweep":    {[]string{"cash", "journal", "sweep"}, -2, 3},
	"shares":   {[]string{"holding", "shares", "buy", "sell", "cash", "other"}, -2, 7},
	"holding":  {[]string{"holding", "shares", "buy", "sell", "cash", "other"}, -2, 7},
	"proceeds": {[]string{"holding", "shares", "buy", "sell", "cash", "other"}, -2, 7},
	"other":    {nil, -3, 7},
	"unknown":  {nil, -3, 7},
}

// reinvestRule is the rule of Reinvest Shares, which a History export
// may date up to five days after the dividend that the API books it with.
var reinvestRule = matchRule{[]string{"buy"}, 0, 5}

// legsFamilies are the families of file transactions that several rows
// of may be one API transaction: a spread's legs, a cash merger and its
// adjustment, a redemption and its adjustment.
var legsFamilies = []string{"buy", "sell", "holding", "proceeds"}

// maxLegsRows is the most rows of one day that the matcher tries the
// sets of against one API transaction (2^maxLegsRows sets).
const maxLegsRows = 10

// maxCandidates is how many of its best candidates an uncertain file
// transaction keeps.
const maxCandidates = 5

// A matchOutput is a match: API transactions and file transactions that
// are one and the same, how the matcher found them, and the score of a
// unique one.
type matchOutput struct {
	method string
	score  sql.NullFloat64
	// txns are its API transactions' ids, and files its file
	// transactions' subjects, each in order.
	txns  []int64
	files []string
}

// A candidateOutput is an API transaction that an uncertain file
// transaction may be, its score, and why.
type candidateOutput struct {
	file  string
	txn   int64
	score float64
	why   string
}

// A matchInput is what the matcher reads of an account: its API entries
// and their transactions, its file entries and their transactions, your
// decisions that reach each file transaction, your policies by action,
// the days the API covers (api and api_file runs), and what complete
// History files cover.
type matchInput struct {
	api      []ledgerEntry
	txns     []txnInput
	files    []ledgerEntry
	fileTxns []csvTxnOutput
	// decided are the latest of your decisions of each kind that reach each
	// file transaction, and all every one that does, in the order made.
	decided []map[string]decisionInput
	all     [][]decisionInput
	// policies are your action decisions, by the action normalized
	// (normalAction): count or hold.
	policies map[string]string
	apiRuns  []coverageRun
	complete []schwabfile.Coverage
}

// A symbolRef is a symbol as the matcher compares it, with the security
// that the store found for it.
type symbolRef struct {
	symbol   string
	security sql.NullInt64
}

// A matchItem is an entry as the matcher reads it.
type matchItem struct {
	e *ledgerEntry
	// day is the day it happened on: the API's trade day, the file's as-of
	// date, else its posted date; posted is a file transaction's posted
	// date.
	day, posted nytime.Date
	family      string
	amount      sql.NullInt64
	// quantity is its quantity without its sign; fees and price as its
	// entry has them.
	quantity sql.NullInt64
	fees     sql.NullInt64
	price    sql.NullFloat64
	sym      symbolRef
	// text is its description, masked, as the fine fields compare it.
	text string
	// An API transaction's id, order, legs of securities, time and
	// activity ID.
	txn      int64
	order    int64
	legs     []matchLeg
	time     string
	activity int64
	// A file transaction's action, normalized; the file that opened it and
	// the ordinal of the row that did; and whether it is of TD Ameritrade's
	// history.
	action        string
	fileID        int64
	ordinal       int
	tda           bool
	rule          matchRule
	none, decided bool
	rejected      map[string]bool
	// match is the index of its match, or -1.
	match int
}

// A matchLeg is a leg of a security of an API transaction: its symbol and
// its amount without its sign.
type matchLeg struct {
	sym    symbolRef
	amount sql.NullInt64
}

// matchAccount matches an account's file transactions to its API
// transactions (15.2 of the design) and counts the file transactions
// (15.3), a function of its input alone whatever its order. It sets the
// file entries' counting and the flags of both sides in place, and returns
// the matches and the candidates of the uncertain file transactions.
//
// Your decisions come first: a file transaction you decided to count, or
// not, takes no part; one you linked joins every file transaction linked
// to one of the API transactions it names, in one match (user); one you
// said is none counts from the file. Then, of what is left: equal
// amounts, days, symbols, families and quantities pair in order (exact);
// a trade whose amount and quantity are those of two or more fills of one
// order on a day, when one order alone fits (fills); several rows of one
// day whose amounts sum to one API transaction's net and whose symbols and
// quantities are its legs', when one set and one transaction alone fit
// (legs); and equal amounts in each other's window, each the other's
// strictly best by score (unique). Your reject decisions keep their pairs
// apart in all of these. A file transaction you decided counts from the
// file, which an API transaction left over may be, is flagged
// late_api_row: both count until you say which it is.
func matchAccount(in matchInput) ([]matchOutput, []candidateOutput) {
	byTxn := map[int64]*txnInput{}
	for i := range in.txns {
		byTxn[in.txns[i].id] = &in.txns[i]
	}
	var apis, files []*matchItem
	apiBySubject := map[string]*matchItem{}
	for i := range in.api {
		e := &in.api[i]
		t := byTxn[e.txn.Int64]
		it := &matchItem{
			e: e, day: nytime.Date(e.day.String), family: e.family, amount: e.amount, quantity: absNull(e.quantity),
			fees: e.fees, price: e.price, sym: symbolRef{e.symbol.String, e.security}, text: matchText(mask.Text(e.text)),
			txn: e.txn.Int64, time: e.time.String, match: -1,
		}
		if t != nil {
			it.order, it.activity = t.orderID, t.activityID
			for _, l := range t.legs {
				if l.isSecurity() {
					it.legs = append(it.legs, matchLeg{symbolRef{l.symbol, l.security}, absNull(l.amountMicros)})
				}
			}
		}
		apis = append(apis, it)
		apiBySubject[e.subject] = it
	}
	slices.SortFunc(apis, func(a, b *matchItem) int {
		return cmp.Or(cmp.Compare(a.time, b.time), cmp.Compare(a.activity, b.activity), cmp.Compare(a.e.ref, b.e.ref))
	})
	for i := range in.files {
		e := &in.files[i]
		t := in.fileTxns[i]
		it := &matchItem{
			e: e, day: nytime.Date(e.day.String), posted: nytime.Date(e.posted.String), family: e.family,
			amount: e.amount, quantity: absNull(e.quantity), fees: e.fees, price: e.price,
			sym: symbolRef{e.symbol.String, e.security}, text: matchText(e.text), action: normalAction(e.kind.String),
			fileID: t.members[0].file.id, ordinal: t.members[0].ordinal, tda: e.flags.has("tda_history"), match: -1,
		}
		var ok bool
		if it.rule, ok = matchRules[e.family]; !ok {
			it.rule = matchRules["unknown"]
		}
		if it.action == normalAction("Reinvest Shares") {
			it.rule = reinvestRule
		}
		var decided map[string]decisionInput
		if i < len(in.decided) {
			decided = in.decided[i]
		}
		_, it.decided = decided["count"]
		it.none = decided["match"].value == "none"
		var all []decisionInput
		if i < len(in.all) {
			all = in.all[i]
		}
		it.rejected = rejectedBy(all)
		files = append(files, it)
	}
	slices.SortFunc(files, func(a, b *matchItem) int {
		return cmp.Or(cmp.Compare(a.posted, b.posted), cmp.Compare(a.fileID, b.fileID), cmp.Compare(a.ordinal, b.ordinal))
	})

	m := &matcher{}
	m.yours(files, apiBySubject, in.decided, in.fileTxns)
	m.exact(files, apis)
	m.fills(files, apis)
	m.legs(files, apis)
	edges := m.unique(files, apis)
	cands := m.uncertain(files, edges)
	m.differences()
	notInFiles(apis, in.complete, lateRows(files, apis))
	countFiles(files, m.matches, in.policies, in.apiRuns)

	out := make([]matchOutput, len(m.matches))
	for i, x := range m.matches {
		o := matchOutput{method: x.method, score: x.score}
		for _, a := range x.apis {
			o.txns = append(o.txns, a.txn)
		}
		for _, f := range x.files {
			o.files = append(o.files, f.e.csvSubject)
		}
		slices.Sort(o.txns)
		slices.Sort(o.files)
		out[i] = o
	}
	slices.SortFunc(out, func(a, b matchOutput) int { return cmp.Compare(a.files[0], b.files[0]) })
	slices.SortFunc(cands, func(a, b candidateOutput) int {
		return cmp.Or(cmp.Compare(a.file, b.file), cmp.Compare(a.txn, b.txn))
	})
	return out, cands
}

// A matcher holds the matches made so far.
type matcher struct {
	matches []*matched
}

// A matched is a match being made: its method, the score of a unique one,
// and its members.
type matched struct {
	method string
	score  sql.NullFloat64
	files  []*matchItem
	apis   []*matchItem
}

// link makes a match of files and apis.
func (m *matcher) link(method string, score sql.NullFloat64, files, apis []*matchItem) {
	x := &matched{method: method, score: score, files: files, apis: apis}
	for _, it := range append(slices.Clone(files), apis...) {
		it.match = len(m.matches)
	}
	m.matches = append(m.matches, x)
}

// free reports whether a file transaction takes part in the automatic
// steps: not in a match, not one you decided to count or not, nor one you
// said no API transaction is, and with an amount.
func (it *matchItem) free() bool {
	return it.match < 0 && !it.decided && !it.none && it.amount.Valid
}

// yours makes the matches of your match decisions (step 0): file
// transactions whose decisions name a common API transaction form one
// match with every API transaction they name. A decision that names no
// API transaction of the account makes none, and its entry is flagged
// stale_decision.
func (m *matcher) yours(files []*matchItem, apiBySubject map[string]*matchItem, decided []map[string]decisionInput, txns []csvTxnOutput) {
	bySubject := map[string]*matchItem{}
	for _, f := range files {
		bySubject[f.e.csvSubject] = f
	}
	// named holds the API transactions that each file transaction's
	// decision names, and parent the groups of file transactions.
	named := map[*matchItem][]*matchItem{}
	parent := map[*matchItem]*matchItem{}
	find := func(x *matchItem) *matchItem {
		for parent[x] != x {
			x = parent[x]
		}
		return x
	}
	byAPI := map[*matchItem]*matchItem{} // API transaction -> a file transaction naming it
	for i := range min(len(decided), len(txns)) {
		d, ok := decided[i]["match"]
		f := bySubject[txns[i].subject]
		if !ok || d.value == "none" || f == nil || f.decided {
			continue
		}
		for s := range strings.SplitSeq(d.value, ",") {
			if a := apiBySubject[s]; a != nil && !slices.Contains(named[f], a) {
				named[f] = append(named[f], a)
			}
		}
		if len(named[f]) == 0 {
			f.e.flags.add("stale_decision", "your link names no transaction of the account")
			continue
		}
		parent[f] = f
		for _, a := range named[f] {
			if o, ok := byAPI[a]; ok {
				if ra, rb := find(f), find(o); ra != rb {
					parent[ra] = rb
				}
			} else {
				byAPI[a] = f
			}
		}
	}
	groups := map[*matchItem][]*matchItem{}
	for _, f := range files {
		if _, ok := parent[f]; ok {
			r := find(f)
			groups[r] = append(groups[r], f)
		}
	}
	for _, f := range files { // in the files' order, so the matches' order is fixed
		g, ok := groups[f]
		if !ok {
			continue
		}
		var apis []*matchItem
		for _, x := range g {
			for _, a := range named[x] {
				if !slices.Contains(apis, a) {
					apis = append(apis, a)
				}
			}
		}
		m.link("user", sql.NullFloat64{}, g, apis)
	}
}

// exact pairs file and API transactions of equal amounts, days, families,
// quantities and symbols, each side in its order (step 1), so that two
// identical trades pair with two identical fills.
func (m *matcher) exact(files, apis []*matchItem) {
	type bucket struct {
		amount   int64
		day      nytime.Date
		family   string
		quantity sql.NullInt64
	}
	byBucket := map[bucket][]*matchItem{}
	for _, a := range apis {
		if a.match < 0 && a.amount.Valid {
			k := bucket{a.amount.Int64, a.day, a.family, a.quantity}
			byBucket[k] = append(byBucket[k], a)
		}
	}
	for _, f := range files {
		if !f.free() {
			continue
		}
		for _, a := range byBucket[bucket{f.amount.Int64, f.day, f.family, f.quantity}] {
			if a.match < 0 && sameSymbol(f.sym, a.sym) && !f.rejected[a.e.subject] {
				m.link("exact", sql.NullFloat64{}, []*matchItem{f}, []*matchItem{a})
				break
			}
		}
	}
}

// fills links a file's buy or sell to two or more fills of one order on
// one day in its window, of its symbol, whose amounts and quantities sum
// to its own, when one order alone does (step 2); the file transaction is
// flagged merged_fills.
func (m *matcher) fills(files, apis []*matchItem) {
	type key struct {
		order int64
		day   nytime.Date
	}
	groups := map[key][]*matchItem{}
	var keys []key
	for _, a := range apis {
		if a.match >= 0 || a.order == 0 || !a.amount.Valid || (a.family != "buy" && a.family != "sell") {
			continue
		}
		k := key{a.order, a.day}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], a)
	}
	byAmount := map[int64][]key{} // the groups of two or more fills, by their amounts' sum
	for _, k := range keys {
		if g := groups[k]; len(g) >= 2 {
			var sum int64
			for _, a := range g {
				sum += a.amount.Int64
			}
			byAmount[sum] = append(byAmount[sum], k)
		}
	}
	for _, f := range files {
		if !f.free() || (f.family != "buy" && f.family != "sell") || !f.quantity.Valid {
			continue
		}
		var fit []key
		for _, k := range byAmount[f.amount.Int64] {
			g := groups[k]
			if !inWindow(f, g[0]) {
				continue
			}
			var quantity int64
			ok := true
			for _, a := range g {
				ok = ok && a.match < 0 && a.family == f.family && a.quantity.Valid && sameSymbol(f.sym, a.sym) && !f.rejected[a.e.subject]
				quantity += a.quantity.Int64
			}
			if ok && quantity == f.quantity.Int64 {
				fit = append(fit, k)
			}
		}
		if len(fit) == 1 {
			m.link("fills", sql.NullFloat64{}, []*matchItem{f}, slices.Clone(groups[fit[0]]))
			f.e.flags.add("merged_fills", fmt.Sprintf("%d fills of order %d", len(groups[fit[0]]), fit[0].order))
		}
	}
}

// legs links several file transactions of one day, of the families whose
// rows may be the legs of one event, to one API transaction (step 3):
// their amounts sum to its net, each names the symbol of one of its legs
// of a security, and each quantity given is that leg's amount; when one
// set alone fits the API transaction, and no other fitting set holds any
// of the set's rows. Each file transaction is flagged merged_rows.
func (m *matcher) legs(files, apis []*matchItem) {
	byDay := map[nytime.Date][]*matchItem{}
	for _, f := range files {
		if f.free() && slices.Contains(legsFamilies, f.family) && fineSymbol(f.sym.symbol) != "" {
			byDay[f.day] = append(byDay[f.day], f)
		}
	}
	type fit struct {
		api  *matchItem
		rows []*matchItem
	}
	var fits []fit
	for _, a := range apis {
		if a.match >= 0 || !a.amount.Valid || len(a.legs) == 0 {
			continue
		}
		if a.day == "" {
			continue
		}
		for d := -2; d <= 7; d++ { // the widest window of legsFamilies, file day less API day
			day := addDays(a.day, d)
			var rows []*matchItem
			for _, f := range byDay[day] {
				if inWindow(f, a) && !f.rejected[a.e.subject] && fitsLeg(f, a) {
					rows = append(rows, f)
				}
			}
			if len(rows) < 2 || len(rows) > maxLegsRows {
				continue
			}
			for set := 1; set < 1<<len(rows); set++ {
				if bitsSet(set) < 2 {
					continue
				}
				var sum int64
				var picked []*matchItem
				for i, f := range rows {
					if set&(1<<i) != 0 {
						sum += f.amount.Int64
						picked = append(picked, f)
					}
				}
				if sum == a.amount.Int64 {
					fits = append(fits, fit{a, picked})
				}
			}
		}
	}
	perAPI := map[*matchItem]int{}
	perRow := map[*matchItem]int{}
	for _, x := range fits {
		perAPI[x.api]++
		for _, f := range x.rows {
			perRow[f]++
		}
	}
	for _, x := range fits {
		if perAPI[x.api] != 1 || slices.ContainsFunc(x.rows, func(f *matchItem) bool { return perRow[f] != 1 }) {
			continue
		}
		m.link("legs", sql.NullFloat64{}, x.rows, []*matchItem{x.api})
		for _, f := range x.rows {
			f.e.flags.add("merged_rows", fmt.Sprintf("one of %d rows linked to %s", len(x.rows), x.api.e.ref))
		}
	}
}

// bitsSet counts the bits set in n.
func bitsSet(n int) int {
	c := 0
	for ; n != 0; n &= n - 1 {
		c++
	}
	return c
}

// fitsLeg reports whether file transaction f names the symbol of a leg
// of API transaction a and, when it gives a quantity, that leg's amount.
func fitsLeg(f, a *matchItem) bool {
	return slices.ContainsFunc(a.legs, func(l matchLeg) bool {
		return sameSymbol(f.sym, l.sym) && (!f.quantity.Valid || l.amount == f.quantity)
	})
}

// An edge is a pair of a file transaction and an API transaction that
// may be one, and its score.
type edge struct {
	f, a  *matchItem
	score int
}

// unique links file and API transactions of equal amounts, in each
// other's window, of compatible families and symbols (step 4): each file
// transaction whose best edge is strictly better than its second, to the
// API transaction whose best edge is strictly that one, again and again
// until none is. It returns the edges of the file transactions left.
func (m *matcher) unique(files, apis []*matchItem) map[*matchItem][]edge {
	byAmount := map[int64][]*matchItem{}
	for _, a := range apis {
		if a.match < 0 && a.amount.Valid {
			byAmount[a.amount.Int64] = append(byAmount[a.amount.Int64], a)
		}
	}
	edges := map[*matchItem][]edge{}
	for _, f := range files {
		if !f.free() {
			continue
		}
		for _, a := range byAmount[f.amount.Int64] {
			if a.match < 0 && mayBe(f, a) {
				edges[f] = append(edges[f], edge{f, a, score(f, a)})
			}
		}
	}
	for {
		// best returns the best edge, whether it is strictly better than
		// the second, of the edges of x left.
		type best struct {
			e      edge
			strict bool
		}
		bestF := map[*matchItem]best{}
		bestA := map[*matchItem]best{}
		consider := func(bs map[*matchItem]best, x *matchItem, e edge) {
			b, ok := bs[x]
			switch {
			case !ok || e.score > b.e.score:
				bs[x] = best{e, true}
			case e.score == b.e.score:
				b.strict = false
				bs[x] = b
			}
		}
		for _, f := range files {
			for _, e := range edges[f] {
				if f.match < 0 && e.a.match < 0 {
					consider(bestF, f, e)
					consider(bestA, e.a, e)
				}
			}
		}
		linked := false
		for _, f := range files {
			b, ok := bestF[f]
			if !ok || !b.strict {
				continue
			}
			if p := bestA[b.e.a]; p.strict && p.e.f == f {
				m.link("unique", sql.NullFloat64{Float64: float64(b.e.score), Valid: true}, []*matchItem{f}, []*matchItem{b.e.a})
				linked = true
			}
		}
		if !linked {
			break
		}
	}
	left := map[*matchItem][]edge{}
	for f, es := range edges {
		if f.match >= 0 {
			continue
		}
		for _, e := range es {
			if e.a.match < 0 {
				left[f] = append(left[f], e)
			}
		}
	}
	return left
}

// uncertain flags each file transaction left with edges uncertain_match
// and returns its best candidates (step 5).
func (m *matcher) uncertain(files []*matchItem, edges map[*matchItem][]edge) []candidateOutput {
	var out []candidateOutput
	for _, f := range files {
		es := edges[f]
		if len(es) == 0 {
			continue
		}
		slices.SortFunc(es, func(x, y edge) int {
			return cmp.Or(cmp.Compare(y.score, x.score), cmp.Compare(x.a.time, y.a.time), cmp.Compare(x.a.e.ref, y.a.e.ref))
		})
		es = es[:min(len(es), maxCandidates)]
		var refs []string
		for _, e := range es {
			out = append(out, candidateOutput{file: f.e.csvSubject, txn: e.a.txn, score: float64(e.score), why: why(e.f, e.a)})
			refs = append(refs, e.a.e.ref)
		}
		f.e.flags.add("uncertain_match", fmt.Sprintf("it could be %s; say which: schwabtx edit %s -link %s, or -link none if none is",
			joinOr(refs), f.e.ref, refs[0]))
	}
	return out
}

// joinOr joins words as a choice in English: "a", "a or b", "a, b or c".
func joinOr(words []string) string {
	if len(words) <= 1 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// differences flags what the two sides of each match differ in (step 6):
// the file transaction's quantity, fees, price (beyond a hundredth of a
// percent) and day, differs_from_api; the API transaction's category,
// category_disagrees, but where its rule awaits the probes, which the
// file's action informs.
func (m *matcher) differences() {
	for _, x := range m.matches {
		// The API's side: its quantities and fees summed, NULL when one
		// is missing.
		quantity, fees := sql.NullInt64{Valid: true}, sql.NullInt64{Valid: true}
		for _, a := range x.apis {
			quantity, fees = addCents(quantity, a.quantity), addCents(fees, a.fees)
		}
		for _, f := range x.files {
			var diffs []string
			if len(x.files) == 1 {
				if f.quantity.Valid && quantity.Valid && f.quantity != quantity {
					diffs = append(diffs, fmt.Sprintf("quantity %s here, %s at Schwab: a split since?", formatMicros(f.quantity.Int64), formatMicros(quantity.Int64)))
				}
				if f.fees.Valid && fees.Valid && f.fees != fees {
					diffs = append(diffs, fmt.Sprintf("fees %s here, %s at Schwab", formatCents(f.fees.Int64), formatCents(fees.Int64)))
				}
				if len(x.apis) == 1 {
					p, q := f.price, x.apis[0].price
					if p.Valid && q.Valid && math.Abs(p.Float64-q.Float64) > 1e-4*max(math.Abs(p.Float64), math.Abs(q.Float64)) {
						diffs = append(diffs, fmt.Sprintf("price %s here, %s at Schwab", formatPrice(p.Float64), formatPrice(q.Float64)))
					}
				}
			}
			var days []string
			for _, a := range x.apis {
				if a.day != f.day && !slices.Contains(days, string(a.day)) {
					days = append(days, string(a.day))
				}
			}
			if len(days) > 0 {
				diffs = append(diffs, fmt.Sprintf("day %s here, %s at Schwab", f.day, strings.Join(days, " and ")))
			}
			if len(diffs) > 0 {
				f.e.flags.add("differs_from_api", strings.Join(diffs, "; "))
			}
		}
		for _, a := range x.apis {
			if a.e.rule == "awaits_probe" {
				var actions []string
				for _, f := range x.files {
					if k := mask.Text(f.e.kind.String); !slices.Contains(actions, k) {
						actions = append(actions, k)
					}
				}
				a.e.flags.add("awaits_probe", "the file calls it "+joinAnd(actions))
				continue
			}
			if len(x.files) != 1 || a.e.rule == "user" {
				continue
			}
			if f := x.files[0]; f.e.category != a.e.category {
				a.e.flags.add("category_disagrees", fmt.Sprintf("%s (%s) is booked as %s", f.e.ref, mask.Text(f.e.kind.String), f.e.category))
			}
		}
	}
}

// mayBe reports whether API transaction a may be file transaction f, as
// the unique step pairs them: equal amounts, f's day in its window after
// a's, a family that f's may be, symbols that may be one security (the
// same one, for an amount of 0), and no reject decision of yours between
// them.
func mayBe(f, a *matchItem) bool {
	if !f.amount.Valid || f.amount != a.amount || f.rejected[a.e.subject] || !inWindow(f, a) || !familyFits(f, a) {
		return false
	}
	return (f.amount.Int64 != 0 || sameSymbol(f.sym, a.sym)) && compatibleSymbols(f.sym, a.sym)
}

// lateRows flags late_api_row each file transaction in no match that you
// decided counts from the file (-link none, or -count yes) and that a
// counted API transaction in no match may be (mayBe): one that came after
// your decision, as a row the API serves late does, and that counts beside
// it until you say which it is. It returns those API transactions.
func lateRows(files, apis []*matchItem) map[*matchItem]bool {
	near := map[*matchItem]bool{}
	for _, f := range files {
		if f.match >= 0 || !f.none && (!f.decided || !f.e.counted) {
			continue
		}
		var refs []string
		for _, a := range apis {
			if a.match < 0 && a.e.counted && mayBe(f, a) {
				refs = append(refs, a.e.ref)
				near[a] = true
			}
		}
		if len(refs) == 0 {
			continue
		}
		link := fmt.Sprintf("schwabtx edit %s -link %s", f.e.ref, refs[0])
		if !f.none {
			link = fmt.Sprintf("schwabtx edit %s -count auto", f.e.ref)
		}
		f.e.flags.add("late_api_row", fmt.Sprintf("it may be %s, which counts beside it; if it is: %s; if not: schwabtx edit %s -reject %s",
			joinOr(refs), link, f.e.ref, refs[0]))
	}
	return near
}

// notInFiles flags each API transaction in no match whose day a complete
// History file of the account covers not_in_files (step 7): it counts. An
// API transaction that a file transaction you decided counts may be
// (late, from lateRows) is not flagged: a file row of it may be there.
func notInFiles(apis []*matchItem, complete []schwabfile.Coverage, late map[*matchItem]bool) {
	for _, a := range apis {
		if a.match >= 0 || a.day == "" || late[a] {
			continue
		}
		if slices.ContainsFunc(complete, func(c schwabfile.Coverage) bool { return covers(c, a.day) }) {
			a.e.flags.add("not_in_files", "")
		}
	}
}

// countFiles counts each file transaction (15.3 of the design): one in a
// match never counts (the API's record does), and is flagged
// linked_api_uncounted when none of its match's API transactions counts
// either; one you decided counts as you said, and one you said is none
// counts; an uncertain one does not; one inside the API's coverage, every
// day from the day before its day to the day after its posted date, is
// held out unless it is of TD Ameritrade's history or your policy for its
// action says count; any other is history the API does not serve, and
// counts.
func countFiles(files []*matchItem, matches []*matched, policies map[string]string, runs []coverageRun) {
	for _, f := range files {
		e := f.e
		switch {
		case f.match >= 0:
			x := matches[f.match]
			e.counted, e.countedWhy = false, "linked"
			if !slices.ContainsFunc(x.apis, func(a *matchItem) bool { return a.e.counted }) {
				var refs []string
				for _, a := range x.apis {
					refs = append(refs, a.e.ref+" "+a.e.countedWhy)
				}
				e.flags.add("linked_api_uncounted", fmt.Sprintf("%s; to count it from the file: schwabtx edit %s -link none", joinAnd(refs), e.ref))
			}
		case f.decided:
			// fileEntry counted it as you said.
		case f.none:
			e.counted, e.countedWhy = true, "user"
		case e.flags.has("uncertain_match"):
			e.counted, e.countedWhy = false, "uncertain"
		default:
			inside, byFile := apiCovers(runs, f.day, f.posted)
			switch {
			case !inside:
				e.counted, e.countedWhy = true, "file"
			case f.tda:
				e.counted, e.countedWhy = true, "tda"
			case policies[f.action] == "count":
				e.counted, e.countedWhy = true, "policy"
			default:
				e.counted, e.countedWhy = false, "held_out"
				detail := ""
				if byFile {
					detail = "api_file"
				}
				e.flags.add("held_out", detail)
			}
		}
	}
}

// apiCovers reports whether the API's coverage, runs of api and api_file,
// holds every day from the day before day to the day after posted, and
// whether some of those days only a `-json` file's rows cover.
func apiCovers(runs []coverageRun, day, posted nytime.Date) (inside, byFile bool) {
	from, to := addDays(min(day, cmp.Or(posted, day)), -1), addDays(max(day, posted), 1)
	in := func(d nytime.Date, source string) bool {
		return slices.ContainsFunc(runs, func(r coverageRun) bool { return r.source == source && d >= r.from && d <= r.to })
	}
	for d := from; d <= to; d = nextDay(d) {
		switch {
		case in(d, "api"):
		case in(d, "api_file"):
			byFile = true
		default:
			return false, false
		}
	}
	return true, byFile
}

// inWindow reports whether file transaction f's day lies in its window
// after API transaction a's.
func inWindow(f, a *matchItem) bool {
	d, ok := daysApart(string(a.day), string(f.day))
	return ok && d >= f.rule.lo && d <= f.rule.hi
}

// familyFits reports whether file transaction f's family may be API
// transaction a's.
func familyFits(f, a *matchItem) bool {
	return f.rule.api == nil || slices.Contains(f.rule.api, a.family)
}

// score scores an edge (15.2 of the design): 100, less 10 a day apart, and
// more for equal symbols (20), quantities (10), fees (5) and descriptions
// (5).
func score(f, a *matchItem) int {
	d, _ := daysApart(string(a.day), string(f.day))
	s := 100 - 10*max(d, -d)
	if sameSymbol(f.sym, a.sym) {
		s += 20
	}
	if f.quantity == a.quantity {
		s += 10
	}
	if f.fees.Valid && f.fees == a.fees {
		s += 5
	}
	if f.text != "" && f.text == a.text {
		s += 5
	}
	return s
}

// why says why an API transaction may be a file transaction: how many days
// apart, and what else is equal.
func why(f, a *matchItem) string {
	d, _ := daysApart(string(a.day), string(f.day))
	parts := []string{"the same amount"}
	switch d = max(d, -d); d {
	case 0:
		parts = append(parts, "the same day")
	case 1:
		parts = append(parts, "a day apart")
	default:
		parts = append(parts, strconv.Itoa(d)+" days apart")
	}
	if fineSymbol(f.sym.symbol) != "" && sameSymbol(f.sym, a.sym) {
		parts = append(parts, "the same symbol")
	}
	if f.quantity == a.quantity && f.quantity.Valid {
		parts = append(parts, "the same quantity")
	}
	if f.fees.Valid && f.fees == a.fees {
		parts = append(parts, "the same fees")
	}
	if f.text != "" && f.text == a.text {
		parts = append(parts, "the same description")
	}
	return joinAnd(parts)
}

// sameSymbol reports whether two symbols name one security (15.1 of the
// design): two options of one root, expiration, put or call and strike,
// whichever way each is spelled; else two symbols of one security; else
// one text, in upper case. Two missing symbols are the same; a missing one
// and another are not.
func sameSymbol(a, b symbolRef) bool {
	sa, sb := fineSymbol(a.symbol), fineSymbol(b.symbol)
	if sa == "" || sb == "" {
		return sa == sb
	}
	ca, optA := contract(a.symbol)
	cb, optB := contract(b.symbol)
	if optA || optB {
		return optA && optB && ca == cb
	}
	return a.security.Valid && a.security == b.security || sa == sb
}

// compatibleSymbols reports whether two symbols may name one security:
// they are the same, or either is missing.
func compatibleSymbols(a, b symbolRef) bool {
	return fineSymbol(a.symbol) == "" || fineSymbol(b.symbol) == "" || sameSymbol(a, b)
}

// rejectedBy returns the API transactions, by subject, that your reject
// decisions among ds keep apart from the file transaction they reach: each
// one made since the last that withdrew them all (auto).
func rejectedBy(ds []decisionInput) map[string]bool {
	ds = slices.Clone(ds)
	slices.SortFunc(ds, func(a, b decisionInput) int { return cmp.Compare(a.id, b.id) })
	out := map[string]bool{}
	for _, d := range ds {
		switch {
		case d.kind != "reject":
		case d.value == "auto":
			clear(out)
		default:
			out[d.value] = true
		}
	}
	return out
}

// matchText returns a description as the score compares it: its white
// space one space, in upper case.
func matchText(s string) string { return strings.ToUpper(fineText(s)) }

// absNull returns v without its sign.
func absNull(v sql.NullInt64) sql.NullInt64 {
	return sql.NullInt64{Int64: max(v.Int64, -v.Int64), Valid: v.Valid}
}

// formatMicros writes a quantity in millionths as a decimal, without
// trailing zeros.
func formatMicros(m int64) string {
	s := ""
	if m < 0 {
		s, m = "-", -m
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", m%1e6), "0")
	if frac == "" {
		return s + strconv.FormatInt(m/1e6, 10)
	}
	return s + strconv.FormatInt(m/1e6, 10) + "." + frac
}

// formatPrice writes a price as briefly as it reads back.
func formatPrice(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) }

// matchCanon returns the matches as strings in one order, by which the
// store's are compared with the derivation's.
func matchCanon(ms []matchOutput) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		score := ""
		if m.score.Valid {
			score = strconv.FormatFloat(m.score.Float64, 'g', -1, 64)
		}
		txns := make([]string, len(m.txns))
		for j, t := range m.txns {
			txns[j] = strconv.FormatInt(t, 10)
		}
		out[i] = m.method + "|" + score + "|" + strings.Join(txns, ",") + "|" + strings.Join(m.files, ",")
	}
	slices.Sort(out)
	return out
}

// candidateCanon returns the candidates as strings in one order.
func candidateCanon(cs []candidateOutput) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.file + "|" + strconv.FormatInt(c.txn, 10) + "|" + strconv.FormatFloat(c.score, 'g', -1, 64) + "|" + c.why
	}
	slices.Sort(out)
	return out
}

// writeMatches brings account id's matches and candidates to what out
// holds, writing them again only when they differ from the store's. The
// entries of its file transactions hold their row ids by now (writeFiles).
func writeMatches(w *wtx, id int64, out *accountOutputs) error {
	ids := map[string]int64{}
	for _, e := range out.entries {
		if e.csvSubject != "" {
			ids[e.csvSubject] = e.csvTxn.Int64
		}
	}
	stored := map[int64]*matchOutput{}
	err := queryEach(w, func(rows *sql.Rows) error {
		var mid int64
		var method string
		var score sql.NullFloat64
		var txn sql.NullInt64
		var subject sql.NullString
		if err := rows.Scan(&mid, &method, &score, &txn, &subject); err != nil {
			return err
		}
		m := stored[mid]
		if m == nil {
			m = &matchOutput{method: method, score: score}
			stored[mid] = m
		}
		if txn.Valid {
			m.txns = append(m.txns, txn.Int64)
		}
		if subject.Valid {
			m.files = append(m.files, subject.String)
		}
		return nil
	}, `SELECT m.match_id, m.method, m.score, mm.txn_id, c.subject FROM match m LEFT JOIN match_member mm ON mm.match_id = m.match_id
		LEFT JOIN csv_txn c ON c.csv_txn_id = mm.csv_txn_id WHERE m.account_id = ?`, id)
	if err != nil {
		return err
	}
	var have []matchOutput
	for _, mid := range slices.Sorted(maps.Keys(stored)) {
		m := stored[mid]
		slices.Sort(m.txns)
		slices.Sort(m.files)
		have = append(have, *m)
	}
	if !slices.Equal(matchCanon(have), matchCanon(out.matches)) {
		if _, err := w.exec("DELETE FROM match_member WHERE match_id IN (SELECT match_id FROM match WHERE account_id = ?)", id); err != nil {
			return err
		}
		if _, err := w.exec("DELETE FROM match WHERE account_id = ?", id); err != nil {
			return err
		}
		for _, m := range out.matches {
			var mid int64
			if err := w.tx.QueryRowContext(w.ctx, "INSERT INTO match (account_id, method, score) VALUES (?, ?, ?) RETURNING match_id",
				id, m.method, m.score).Scan(&mid); err != nil {
				return err
			}
			for _, t := range m.txns {
				if _, err := w.exec("INSERT INTO match_member (match_id, txn_id) VALUES (?, ?)", mid, t); err != nil {
					return err
				}
			}
			for _, s := range m.files {
				if _, err := w.exec("INSERT INTO match_member (match_id, csv_txn_id) VALUES (?, ?)", mid, ids[s]); err != nil {
					return err
				}
			}
		}
	}
	var haveCands []candidateOutput
	err = queryEach(w, func(rows *sql.Rows) error {
		var c candidateOutput
		err := rows.Scan(&c.file, &c.txn, &c.score, &c.why)
		haveCands = append(haveCands, c)
		return err
	}, `SELECT c.subject, mc.txn_id, mc.score, mc.why FROM match_candidate mc JOIN csv_txn c ON c.csv_txn_id = mc.csv_txn_id
		WHERE c.account_id = ?`, id)
	if err != nil || slices.Equal(candidateCanon(haveCands), candidateCanon(out.candidates)) {
		return err
	}
	if _, err := w.exec("DELETE FROM match_candidate WHERE csv_txn_id IN (SELECT csv_txn_id FROM csv_txn WHERE account_id = ?)", id); err != nil {
		return err
	}
	for _, c := range out.candidates {
		if _, err := w.exec("INSERT INTO match_candidate (csv_txn_id, txn_id, score, why) VALUES (?, ?, ?, ?)",
			ids[c.file], c.txn, c.score, c.why); err != nil {
			return err
		}
	}
	return nil
}
