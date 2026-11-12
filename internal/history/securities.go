package history

import (
	"cmp"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A keyObservation is what one observation says of an instrument's key:
// its asset type and kind, and an option's contract. Empty fields say
// nothing. The keys are:
//
//	i:<instrumentId>                Schwab's id of an instrument in a transaction
//	c:<cusip>                       a position's CUSIP
//	x:<ItemIssueId>                 a History JSON export's id of a security
//	s:<assetType>:<symbol>          a position or leg with neither
//	s:FILE:<symbol>@<export date>   a symbol in a file from schwab.com
//
// A security is a group of keys that name one thing (groupKeys).
type keyObservation struct {
	key              string
	assetType, typ   string
	putCall          string
	strike           float64
	expiration       nytime.Date
	multiplier       float64
	underlyingSymbol string
}

// A nameObservation is a name that an observation gives a key: a symbol,
// a uniform symbol, a CUSIP or a description, and where it was seen
// (transaction, deliverable, position, history_file or realized_file).
type nameObservation struct {
	key, kind, value, seenIn string
}

// observeKey adds a key, or takes in what an observation at at says of
// it: the times it was first and last seen take in at, and its fields are
// those of its latest observation. A new key belongs to no security until
// the grouping at the end of the write gives it one.
func observeKey(w *wtx, k keyObservation, at time.Time) error {
	if err := w.deferForeignKeys(); err != nil {
		return err
	}
	when := instant(at)
	var strike, multiplier sql.NullFloat64
	if k.strike != 0 {
		strike = sql.NullFloat64{Float64: k.strike, Valid: true}
	}
	if k.multiplier != 0 {
		multiplier = sql.NullFloat64{Float64: k.multiplier, Valid: true}
	}
	res, err := w.exec(observeKeySQL,
		k.key, nullString(k.assetType), nullString(k.typ), nullString(k.putCall), strike, nullString(string(k.expiration)),
		multiplier, nullString(k.underlyingSymbol), when, when)
	return w.noteKeys(res, err)
}

// observeKeySQL adds a key, or takes in an observation of it. A field of
// the latest observation replaces the key's, but one it does not give is
// kept. Nothing is written when nothing changes.
var observeKeySQL = func() string {
	const latest = "CASE WHEN excluded.last_seen_at >= instrument_key.last_seen_at " +
		"THEN coalesce(excluded.%[1]s, instrument_key.%[1]s) ELSE instrument_key.%[1]s END"
	var set, differ []string
	for _, f := range []string{"asset_type", "type", "put_call", "strike", "expiration_day", "multiplier", "underlying_symbol"} {
		set = append(set, f+" = "+fmt.Sprintf(latest, f))
		differ = append(differ, fmt.Sprintf("("+latest+") IS NOT instrument_key.%[1]s", f))
	}
	return `INSERT INTO instrument_key (key, security_id, asset_type, type, put_call, strike, expiration_day,
		multiplier, underlying_symbol, first_seen_at, last_seen_at) VALUES (?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET ` + strings.Join(set, ", ") + `,
		first_seen_at = min(instrument_key.first_seen_at, excluded.first_seen_at),
		last_seen_at = max(instrument_key.last_seen_at, excluded.last_seen_at)
		WHERE excluded.first_seen_at < instrument_key.first_seen_at OR excluded.last_seen_at > instrument_key.last_seen_at
		OR ` + strings.Join(differ, " OR ")
}()

// seenInOrder ranks where a name was seen, so that a name seen in several
// places keeps the first of them in this order, whatever order the
// observations came in.
const seenInOrder = "transaction,deliverable,position,history_file,realized_file"

// observeName adds a name of a key, or takes in at as a time it was seen.
// A description is free text, kept masked.
func observeName(w *wtx, n nameObservation, at time.Time) error {
	if n.value == "" {
		return nil
	}
	if n.kind == "description" {
		n.value = mask.Text(n.value)
	}
	// ?1 is seenInOrder.
	res, err := w.exec(`INSERT INTO key_name (key, kind, value, seen_in, first_seen_at, last_seen_at) VALUES (?2, ?3, ?4, ?5, ?6, ?6)
		ON CONFLICT (key, kind, value) DO UPDATE SET
		first_seen_at = min(key_name.first_seen_at, excluded.first_seen_at),
		last_seen_at = max(key_name.last_seen_at, excluded.last_seen_at),
		seen_in = CASE WHEN instr(?1, excluded.seen_in) < instr(?1, key_name.seen_in) THEN excluded.seen_in ELSE key_name.seen_in END
		WHERE excluded.first_seen_at < key_name.first_seen_at OR excluded.last_seen_at > key_name.last_seen_at
		OR instr(?1, excluded.seen_in) < instr(?1, key_name.seen_in)`,
		seenInOrder, n.key, n.kind, n.value, n.seenIn, instant(at))
	return w.noteKeys(res, err)
}

// observeTxn adds the keys and names that a transaction's raw JSON, as
// served at at and as the record t reads it, gives its instruments: each leg's instrument but cash, by
// its instrumentId when it has one, else by its asset type and symbol;
// each of an option's deliverables that has an instrumentId; and an
// option's underlyingCusip, as a name of its one deliverable that is its
// underlying. A symbol that is a CUSIP, as Schwab sends for a delisted
// security, is a CUSIP name too.
func observeTxn(w *wtx, t schwab.Transaction, at time.Time) error {
	for _, leg := range t.TransferItems {
		in := leg.Instrument
		if in.AssetType == schwab.AssetCurrency {
			continue
		}
		if in.InstrumentID <= 1 {
			if in.Symbol == "" {
				continue
			}
			key := "s:" + string(in.AssetType) + ":" + in.Symbol
			if err := observe(w, at, keyObservation{key: key, assetType: string(in.AssetType), typ: in.Type},
				nameObservation{kind: "symbol", value: in.Symbol, seenIn: "transaction"},
				nameObservation{kind: "description", value: in.Description, seenIn: "transaction"}); err != nil {
				return err
			}
			continue
		}
		k := keyObservation{
			key:              "i:" + strconv.FormatInt(in.InstrumentID, 10),
			assetType:        string(in.AssetType),
			typ:              in.Type,
			putCall:          in.PutCall,
			strike:           in.StrikePrice,
			expiration:       nytime.DateOf(in.ExpirationDate),
			multiplier:       in.OptionPremiumMultiplier,
			underlyingSymbol: in.UnderlyingSymbol,
		}
		names := []nameObservation{
			{kind: "symbol", value: in.Symbol, seenIn: "transaction"},
			{kind: "uniform_symbol", value: in.UniformSymbol, seenIn: "transaction"},
			{kind: "description", value: in.Description, seenIn: "transaction"},
		}
		if schwab.IsCUSIP(in.Symbol) {
			names = append(names, nameObservation{kind: "cusip", value: in.Symbol, seenIn: "transaction"})
		}
		if err := observe(w, at, k, names...); err != nil {
			return err
		}
		var underlying []string // the deliverables' keys whose symbol is the option's underlying
		for _, od := range in.OptionDeliverables {
			d := od.Deliverable
			if d == nil || d.InstrumentID <= 1 {
				continue
			}
			key := "i:" + strconv.FormatInt(d.InstrumentID, 10)
			if err := observe(w, at, keyObservation{key: key, assetType: string(d.AssetType), typ: d.Type},
				nameObservation{kind: "symbol", value: d.Symbol, seenIn: "deliverable"},
				nameObservation{kind: "description", value: d.Description, seenIn: "deliverable"},
				nameObservation{kind: "cusip", value: d.CUSIP, seenIn: "deliverable"}); err != nil {
				return err
			}
			if d.Symbol != "" && d.Symbol == in.UnderlyingSymbol && !slices.Contains(underlying, key) {
				underlying = append(underlying, key)
			}
		}
		if in.UnderlyingCUSIP != "" && len(underlying) == 1 {
			if err := observeName(w, nameObservation{key: underlying[0], kind: "cusip", value: in.UnderlyingCUSIP, seenIn: "transaction"}, at); err != nil {
				return err
			}
		}
	}
	return nil
}

// observe adds a key and its names, seen at at.
func observe(w *wtx, at time.Time, k keyObservation, names ...nameObservation) error {
	if err := observeKey(w, k, at); err != nil {
		return err
	}
	for _, n := range names {
		n.key = k.key
		if err := observeName(w, n, at); err != nil {
			return err
		}
	}
	return nil
}

// A keyInfo is a key as the grouping reads it.
type keyInfo struct {
	key              string
	assetType, typ   string
	putCall          string
	strike           sql.NullFloat64
	expiration       nytime.Date
	multiplier       sql.NullFloat64
	underlyingSymbol string
	first, last      time.Time
}

// A nameInfo is a name of a key as the grouping reads it: when it was
// first and last seen.
type nameInfo struct {
	key, kind, value string
	first, last      time.Time
}

// A keyLink is a decision about two keys: that they are one security
// (same) or that they are not (differ).
type keyLink struct {
	a, b string
	same bool
}

// A securityGroup is a security that the grouping gives: its keys, in
// order, the first being its canonical key, and what its keys say of it.
type securityGroup struct {
	keys             []string
	definite         bool
	assetType, typ   string
	putCall          string
	strike           sql.NullFloat64
	expiration       nytime.Date
	multiplier       sql.NullFloat64
	underlyingSymbol string
	// underlying is the canonical key of the security that is an option's
	// underlying, or "".
	underlying  string
	first, last time.Time
	// contractSeen is when the key that gave the option's contract was
	// last seen.
	contractSeen time.Time
}

// symbolSlack is how far apart two periods in which keys held a symbol
// may be and still overlap, and how near a day a name must have been seen
// to name a security that day.
const symbolSlack = 7 * 24 * time.Hour

// keyKind returns the kind of key the grouping tells apart: i, c, x, s for
// a position's or a leg's key by symbol, and f for a file's.
func keyKind(key string) string {
	if strings.HasPrefix(key, "s:FILE:") {
		return "f"
	}
	return key[:1]
}

// keyRank orders keys for choosing a security's canonical key: i before c
// before x before s, then by the rest.
func keyRank(a, b string) int {
	rank := func(k string) int { return strings.Index("icxs", k[:1]) }
	return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b))
}

// definiteKey reports whether a key carries an id of Schwab's: an
// instrumentId, a CUSIP or an ItemIssueId.
func definiteKey(key string) bool {
	k := keyKind(key)
	return k == "i" || k == "c" || k == "x"
}

// fileSymbol returns the symbol of a file's key, s:FILE:<symbol>@<date>.
func fileSymbol(key string) string {
	s := strings.TrimPrefix(key, "s:FILE:")
	if i := strings.LastIndex(s, "@"); i >= 0 {
		return s[:i]
	}
	return s
}

// groupKeys groups the keys into securities, the connected components of
// these links:
//
//  1. A key that holds a CUSIP name C, and the key c:C.
//  2. Keys of different kinds that share a symbol name in periods that
//     overlap within symbolSlack, when over the union of those periods the
//     symbol names exactly one key of each of the two kinds, so that a
//     ticker used by two companies at different times links each to its
//     own.
//  3. After the others, the keys of files of one symbol that joined no
//     security holding a key of Schwab's: one ticker in files with nothing
//     to tell its exports apart.
//  4. Your decisions: same links two keys; differ keeps links 2 and 3 from
//     putting two keys in one security.
//
// The links depend on the keys, names and decisions alone, never on the
// order they came in, so a rebuild gives the groups that writing them one
// by one did. The groups come in the order of their canonical keys.
func groupKeys(keys []keyInfo, names []nameInfo, links []keyLink) []securityGroup {
	keys = slices.Clone(keys)
	slices.SortFunc(keys, func(a, b keyInfo) int { return keyRank(a.key, b.key) })
	index := map[string]int{}
	for i, k := range keys {
		index[k.key] = i
	}
	parent := make([]int, len(keys))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	// members holds each component's keys, for the differ check.
	members := make([][]int, len(keys))
	for i := range members {
		members[i] = []int{i}
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if keyRank(keys[ra].key, keys[rb].key) > 0 {
			ra, rb = rb, ra
		}
		parent[rb] = ra
		members[ra] = append(members[ra], members[rb]...)
		members[rb] = nil
	}
	differ := map[[2]int]bool{}
	for _, l := range links {
		a, okA := index[l.a]
		b, okB := index[l.b]
		switch {
		case !okA || !okB || a == b:
		case l.same:
			union(a, b)
		default:
			differ[[2]int{min(a, b), max(a, b)}] = true
		}
	}
	// allowed reports whether joining a's and b's components keeps every
	// pair you said differ apart.
	allowed := func(a, b int) bool {
		ra, rb := find(a), find(b)
		if ra == rb {
			return true
		}
		for _, x := range members[ra] {
			for _, y := range members[rb] {
				if differ[[2]int{min(x, y), max(x, y)}] {
					return false
				}
			}
		}
		return true
	}

	// 1. Co-occurrence of a key and a CUSIP.
	for _, n := range names {
		if n.kind != "cusip" {
			continue
		}
		if a, ok := index[n.key]; ok {
			if b, ok := index["c:"+n.value]; ok {
				union(a, b)
			}
		}
	}
	// 2. A symbol shared without another key of either kind.
	bySymbol := map[string][]nameInfo{}
	for _, n := range names {
		if n.kind == "symbol" {
			if _, ok := index[n.key]; ok {
				bySymbol[n.value] = append(bySymbol[n.value], n)
			}
		}
	}
	var edges [][2]int
	for _, held := range bySymbol {
		for i, a := range held {
			for _, b := range held[i+1:] {
				if keyKind(a.key) == keyKind(b.key) || !overlap(a.first, a.last, b.first, b.last) {
					continue
				}
				from, to := minTime(a.first, b.first), maxTime(a.last, b.last)
				if holders(held, keyKind(a.key), from, to) == 1 && holders(held, keyKind(b.key), from, to) == 1 {
					x, y := index[a.key], index[b.key]
					edges = append(edges, [2]int{min(x, y), max(x, y)})
				}
			}
		}
	}
	link := func(edges [][2]int) {
		slices.SortFunc(edges, func(a, b [2]int) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
		for _, e := range edges {
			if allowed(e[0], e[1]) {
				union(e[0], e[1])
			}
		}
	}
	link(edges)
	// 3. Files' keys of one symbol that nothing else resolves.
	hasDefinite := map[int]bool{}
	for i, k := range keys {
		if definiteKey(k.key) {
			hasDefinite[find(i)] = true
		}
	}
	unresolved := map[string][]int{}
	for i, k := range keys {
		if keyKind(k.key) == "f" && !hasDefinite[find(i)] {
			s := fileSymbol(k.key)
			unresolved[s] = append(unresolved[s], i)
		}
	}
	edges = nil
	for _, ks := range unresolved {
		for j := 1; j < len(ks); j++ {
			edges = append(edges, [2]int{ks[0], ks[j]})
		}
	}
	link(edges)

	var groups []securityGroup
	byRoot := map[int]int{}
	for i, k := range keys { // in order, so a group's first key is its least
		r := find(i)
		g, ok := byRoot[r]
		if !ok {
			g = len(groups)
			byRoot[r] = g
			groups = append(groups, securityGroup{first: k.first, last: k.last})
		}
		groups[g].add(k)
	}
	namesOf := map[string][]nameInfo{}
	for _, n := range names {
		namesOf[n.key] = append(namesOf[n.key], n)
	}
	for i := range groups {
		groups[i].fromSymbols(namesOf)
	}
	for i := range groups {
		groups[i].underlying = underlyingOf(groups, i, namesOf)
	}
	return groups
}

// overlap reports whether two periods overlap within symbolSlack.
func overlap(f1, l1, f2, l2 time.Time) bool {
	return overlapWithin(f1, l1, f2, l2, symbolSlack)
}

// overlapWithin reports whether two periods, from f1 to l1 and from f2 to
// l2, overlap, or come within slack of each other.
func overlapWithin(f1, l1, f2, l2 time.Time, slack time.Duration) bool {
	return !f1.After(l2.Add(slack)) && !f2.After(l1.Add(slack))
}

// holders counts the keys of kind among held, the keys that hold one
// symbol, that held it within symbolSlack of the period from from to to.
func holders(held []nameInfo, kind string, from, to time.Time) int {
	seen := map[string]bool{}
	for _, n := range held {
		if keyKind(n.key) == kind && overlap(n.first, n.last, from, to) {
			seen[n.key] = true
		}
	}
	return len(seen)
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// add takes key k, which comes after the group's keys in keyRank's order,
// into the group.
func (g *securityGroup) add(k keyInfo) {
	g.keys = append(g.keys, k.key)
	g.definite = g.definite || definiteKey(k.key)
	if g.assetType == "" {
		g.assetType = k.assetType
	}
	if g.typ == "" {
		g.typ = k.typ
	}
	g.first, g.last = minTime(g.first, k.first), maxTime(g.last, k.last)
	// An option's contract comes from the instrumentId seen last that gives
	// one.
	if keyKind(k.key) == "i" && k.putCall != "" && (g.putCall == "" || !k.last.Before(g.contractSeen)) {
		g.putCall, g.strike, g.expiration, g.multiplier, g.underlyingSymbol = k.putCall, k.strike, k.expiration, k.multiplier, k.underlyingSymbol
		g.contractSeen = k.last
	}
}

// fromSymbols fills in an option's contract from an OCC symbol among its
// keys' names when no instrumentId gave it.
func (g *securityGroup) fromSymbols(namesOf map[string][]nameInfo) {
	if g.putCall != "" {
		return
	}
	var symbols []string
	for _, k := range g.keys {
		for _, n := range namesOf[k] {
			if n.kind == "symbol" {
				symbols = append(symbols, n.value)
			}
		}
	}
	slices.Sort(symbols)
	for _, s := range symbols {
		if o, ok := schwab.ParseOCC(s); ok {
			g.putCall = o.PutCall
			g.strike = sql.NullFloat64{Float64: float64(o.StrikeThousandths) / 1000, Valid: true}
			g.expiration = o.Expiration
			g.underlyingSymbol = o.Root
			return
		}
	}
}

// underlyingOf returns the canonical key of the security that is group
// i's underlying: the one security whose keys held its underlying symbol
// within symbolSlack of when its own keys were seen, or "".
func underlyingOf(groups []securityGroup, i int, namesOf map[string][]nameInfo) string {
	g := groups[i]
	if g.underlyingSymbol == "" {
		return ""
	}
	found := ""
	for j, h := range groups {
		if j == i {
			continue
		}
		for _, k := range h.keys {
			if slices.ContainsFunc(namesOf[k], func(n nameInfo) bool {
				return n.kind == "symbol" && n.value == g.underlyingSymbol && overlap(n.first, n.last, g.first, g.last)
			}) {
				if found != "" && found != h.keys[0] {
					return ""
				}
				found = h.keys[0]
			}
		}
	}
	return found
}

// groupSecurities groups the store's keys into securities (layer 3 of the
// derivation), when a write has added or changed a key, a name or a
// decision about securities: it keeps a security's row while its canonical
// key leads a group, adds one for a new group and removes one whose group
// is gone, points each key, each leg of a transaction and each position of
// a snapshot at its security, and marks the accounts whose legs moved, so
// that they are derived again.
// Only rows that change are written.
func groupSecurities(w *wtx) error {
	var keys []keyInfo
	at := map[string]int64{} // key -> its security now
	err := queryEach(w, func(rows *sql.Rows) error {
		var k keyInfo
		var id int64
		var asset, typ, putCall, exp, under sql.NullString
		var first, last string
		if err := rows.Scan(&k.key, &id, &asset, &typ, &putCall, &k.strike, &exp, &k.multiplier, &under, &first, &last); err != nil {
			return err
		}
		k.assetType, k.typ, k.putCall, k.expiration, k.underlyingSymbol = asset.String, typ.String, putCall.String, nytime.Date(exp.String), under.String
		at[k.key] = id
		var err error
		if k.first, err = parseInstant(first); err != nil {
			return err
		}
		k.last, err = parseInstant(last)
		keys = append(keys, k)
		return err
	}, `SELECT key, security_id, asset_type, type, put_call, strike, expiration_day, multiplier, underlying_symbol,
		first_seen_at, last_seen_at FROM instrument_key`)
	if err != nil {
		return err
	}
	var names []nameInfo
	err = queryEach(w, func(rows *sql.Rows) error {
		var n nameInfo
		var first, last string
		if err := rows.Scan(&n.key, &n.kind, &n.value, &first, &last); err != nil {
			return err
		}
		var err error
		if n.first, err = parseInstant(first); err != nil {
			return err
		}
		n.last, err = parseInstant(last)
		names = append(names, n)
		return err
	}, "SELECT key, kind, value, first_seen_at, last_seen_at FROM key_name")
	if err != nil {
		return err
	}
	links, err := loadKeyLinks(w)
	if err != nil {
		return err
	}
	ids := map[string]int64{} // canonical key -> security
	err = queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var key string
		err := rows.Scan(&id, &key)
		ids[key] = id
		return err
	}, "SELECT security_id, canonical_key FROM security")
	if err != nil {
		return err
	}
	groups := groupKeys(keys, names, links)
	kept := map[int64]bool{}
	for _, g := range groups {
		canonical := g.keys[0]
		id, ok := ids[canonical]
		if !ok {
			if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO security (canonical_key, definite, first_seen_at, last_seen_at)
				VALUES (?, ?, ?, ?) RETURNING security_id`, canonical, g.definite, instant(g.first), instant(g.last)).Scan(&id); err != nil {
				return err
			}
			ids[canonical] = id
		}
		kept[id] = true
		for _, k := range g.keys {
			if at[k] != id {
				if _, err := w.exec("UPDATE instrument_key SET security_id = ? WHERE key = ?", id, k); err != nil {
					return err
				}
			}
		}
	}
	for _, g := range groups {
		var under sql.NullInt64
		if g.underlying != "" {
			under = sql.NullInt64{Int64: ids[g.underlying], Valid: true}
		}
		cols := []any{
			g.definite, nullString(g.assetType), nullString(g.typ), nullString(g.putCall), g.strike,
			nullString(string(g.expiration)), g.multiplier, nullString(g.underlyingSymbol), under, instant(g.first), instant(g.last),
		}
		if _, err := w.exec(`UPDATE security SET definite = ?1, asset_type = ?2, type = ?3, put_call = ?4, strike = ?5,
			expiration_day = ?6, multiplier = ?7, underlying_symbol = ?8, underlying_id = ?9, first_seen_at = ?10,
			last_seen_at = ?11 WHERE security_id = ?12 AND (definite IS NOT ?1 OR asset_type IS NOT ?2 OR type IS NOT ?3
			OR put_call IS NOT ?4 OR strike IS NOT ?5 OR expiration_day IS NOT ?6 OR multiplier IS NOT ?7
			OR underlying_symbol IS NOT ?8 OR underlying_id IS NOT ?9 OR first_seen_at IS NOT ?10 OR last_seen_at IS NOT ?11)`,
			append(cols, ids[g.keys[0]])...); err != nil {
			return err
		}
	}
	if err := pointLegs(w); err != nil {
		return err
	}
	if err := pointPositions(w); err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Values(ids)) {
		if !kept[id] {
			if _, err := w.exec("DELETE FROM security WHERE security_id = ?", id); err != nil {
				return err
			}
		}
	}
	return nil
}

// pointLegs points each leg of a transaction at the security of its key,
// as observeTxn keys it, and marks the accounts whose legs moved.
func pointLegs(w *wtx) error {
	const key = `(SELECT k.security_id FROM instrument_key k WHERE k.key = CASE WHEN api_leg.instrument_id > 1
		THEN 'i:' || api_leg.instrument_id ELSE 's:' || coalesce(api_leg.asset_type, '') || ':' || api_leg.symbol END)`
	var txns []int64
	err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		err := rows.Scan(&id)
		txns = append(txns, id)
		return err
	}, "UPDATE api_leg SET security_id = "+key+" WHERE asset_type IS NOT 'CURRENCY' AND security_id IS NOT "+key+" RETURNING txn_id")
	if err != nil {
		return err
	}
	for _, id := range txns {
		var acct int64
		if err := w.tx.QueryRowContext(w.ctx, "SELECT account_id FROM api_txn WHERE txn_id = ?", id).Scan(&acct); err != nil {
			return err
		}
		w.touch(acct)
	}
	return nil
}

// loadKeyLinks reads your decisions about securities, the latest of each
// pair holding: "same k:<key>" or "differ k:<key>" on a subject
// k:<key>, and "auto", which withdraws the subject's earlier decisions.
func loadKeyLinks(w *wtx) ([]keyLink, error) {
	type decided struct {
		same    bool
		subject string
	}
	pairs := map[[2]string]decided{}
	err := queryEach(w, func(rows *sql.Rows) error {
		var subject, value string
		if err := rows.Scan(&subject, &value); err != nil {
			return err
		}
		a, ok := strings.CutPrefix(subject, "k:")
		if !ok {
			return nil
		}
		if value == "auto" {
			for p, d := range pairs {
				if d.subject == a {
					delete(pairs, p)
				}
			}
			return nil
		}
		how, other, _ := strings.Cut(value, " ")
		b, ok := strings.CutPrefix(other, "k:")
		if !ok || how != "same" && how != "differ" {
			return nil
		}
		pairs[[2]string{min(a, b), max(a, b)}] = decided{same: how == "same", subject: a}
		return nil
	}, "SELECT subject, value FROM decision WHERE kind = 'security' ORDER BY decision_id")
	links := make([]keyLink, 0, len(pairs))
	for p, d := range pairs {
		links = append(links, keyLink{a: p[0], b: p[1], same: d.same})
	}
	slices.SortFunc(links, func(x, y keyLink) int { return cmp.Or(cmp.Compare(x.a, y.a), cmp.Compare(x.b, y.b)) })
	return links, err
}

// A symbolName is a symbol that a key of a security held, and when.
type symbolName struct {
	security    int64
	value       string
	first, last time.Time
}

// A resolution is the security that a symbol names on a day.
type resolution struct {
	security int64
	// byName means that no key held the symbol within symbolSlack of the
	// day, but one security alone has ever held it, within a year of the
	// day; seen is when it last did.
	byName bool
	seen   time.Time
}

// resolveSymbol returns the security that symbol named on day, as the
// ~SYM of an API dividend or withholding names it: the one security whose
// keys held it within symbolSlack of the day. Else, when one security
// alone has ever held it and held it within 365 days of the day, that one,
// by its name only. Else none: a reused ticker's holder years away may not
// be the security that paid. Symbols compare as written: Schwab writes an
// option's contract in one way, its OCC symbol.
func resolveSymbol(names []symbolName, symbol string, day nytime.Date) (resolution, bool) {
	from, to := midnight(day), midnight(nextDay(day))
	const year = 365 * 24 * time.Hour
	near, ever := map[int64]bool{}, map[int64]bool{}
	recent := map[int64]time.Time{} // when each held it last, if within a year of the day
	for _, n := range names {
		if n.value != symbol {
			continue
		}
		ever[n.security] = true
		if overlapWithin(n.first, n.last, from, to, symbolSlack) {
			near[n.security] = true
		}
		if overlapWithin(n.first, n.last, from, to, year) {
			recent[n.security] = maxTime(recent[n.security], n.last)
		}
	}
	if len(near) == 1 {
		for id := range near {
			return resolution{security: id}, true
		}
	}
	if len(ever) == 1 {
		for id := range ever {
			if seen, ok := recent[id]; ok {
				return resolution{security: id, byName: true, seen: seen}, true
			}
		}
	}
	return resolution{}, false
}

// loadSymbolNames reads the symbols that the keys of each security have
// held.
func loadSymbolNames(w *wtx) ([]symbolName, error) {
	var names []symbolName
	err := queryEach(w, func(rows *sql.Rows) error {
		var n symbolName
		var first, last string
		if err := rows.Scan(&n.security, &n.value, &first, &last); err != nil {
			return err
		}
		var err error
		if n.first, err = parseInstant(first); err != nil {
			return err
		}
		n.last, err = parseInstant(last)
		names = append(names, n)
		return err
	}, `SELECT k.security_id, n.value, n.first_seen_at, n.last_seen_at FROM key_name n JOIN instrument_key k ON k.key = n.key
		WHERE n.kind IN ('symbol', 'uniform_symbol') ORDER BY k.security_id, n.value`)
	return names, err
}
