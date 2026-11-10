package schwab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"maps"
	"math/big"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// checkGates checks o, what pass two made, against the recording before
// anything is written, and returns the first gate that refuses it, with
// why, naming the gate, the answer and the place, never a value:
//
//   - (i) decoding: each answer, decoded by its endpoint's decoder, has the
//     same problems as the recording's;
//   - (ii) the census: for records, the census's same key sets, unknown
//     keys and nulls, and the same keys sent twice, the same or not;
//   - (iii) the tokens: the same keys, punctuation and white space, the
//     values only of the same JSON kinds and forms, each at a path the
//     table covers, a kept value of its grammar and changed only where it
//     held an identifier, money as the amounts policy has it, free text
//     Schwab's wording or its placeholder, a page the same page, and a
//     request and headers changed only where they held identifiers;
//   - (iv) relations: each account's number and hash, wherever they are,
//     have one fake each, the hash HashFor of the number's; a masked number
//     ends with its account's fake; an id has one fake, and no fake is two
//     ids'; and each linear identity of the records, such as a trade's
//     netAmount against its legs' costs, is off by K times what it was, K
//     the scale factor or 1, computed exactly from the numbers as written;
//   - (v) leaks: no identifier of the recording, no local path and not the
//     user name in any file, entry, review.txt or shape, searched as
//     written and decoded.
func (s *scrubber) checkGates(o *scrubOutput) error {
	for _, g := range []struct {
		name  string
		check func(f *scrubbedFile) string
	}{
		{"(i), decoding", s.gateDecoding},
		{"(ii), the census", s.gateCensus},
		{"(iii), the tokens", s.gateTokens},
	} {
		for _, f := range o.files {
			if why := g.check(f); why != "" {
				return fmt.Errorf("gate %s: answer %s: %s", g.name, f.from.raw.name, why)
			}
		}
	}
	if why := s.gateRelations(o); why != "" {
		return fmt.Errorf("gate (iv), relations: %s", why)
	}
	if why := s.gateLeaks(o); why != "" {
		return fmt.Errorf("gate (v), leaks: %s", why)
	}
	return nil
}

// gateDecoding is gate (i) for one file.
func (s *scrubber) gateDecoding(f *scrubbedFile) string {
	before, after := decodedOf(f.from, f.from.raw.body), decodedOf(f.from, f.body)
	if !slices.Equal(before, after) {
		return fmt.Sprintf("decoded, it reads otherwise than the recording's: %s, where the recording's read %s",
			plural(len(after), "finding"), plural(len(before), "finding"))
	}
	return ""
}

// decodedOf returns what a's endpoint's decoder reads of body, with no
// value: for an answer of an account route, each record's problems; for an
// error, which newError reads, its status, its code, whether it has a
// Reference, and whether it says anything.
func decodedOf(a *scrubAnswer, body []byte) []string {
	if k, ok := kindByName(a.kind); ok {
		records, err := k.decode(&reply{header: http.Header{}, body: body})
		if err != nil {
			return []string{"not read: " + valueFree(err.Error())}
		}
		var out []string
		for _, rec := range records {
			recordsIn(rec, "", func(path string, ps []Problem) {
				for _, p := range ps {
					out = append(out, joinPath(path, p.Path)+": "+valueFree(p.What))
				}
			})
		}
		return out
	}
	e := newError(a.raw.meta.Response.Status, http.Header{}, body)
	return []string{
		"status " + strconv.Itoa(e.Status), "code " + e.Code, "a Reference " + strconv.FormatBool(e.Reference != ""),
		"says something " + strconv.FormatBool(e.Message != "" || len(e.Errors) > 0),
	}
}

// gateCensus is gate (ii) for one file.
func (s *scrubber) gateCensus(f *scrubbedFile) string {
	a := f.from
	if a.ext != ".json" {
		return ""
	}
	if !json.Valid(f.body) {
		return "it is no longer JSON"
	}
	if k, ok := kindByName(a.kind); ok && !reflect.DeepEqual(censusOfBody(k, a.raw.body), censusOfBody(k, f.body)) {
		return "the census of its records differs from the recording's"
	}
	if !reflect.DeepEqual(repeats(a.raw.body, ""), repeats(f.body, "")) {
		return "its keys sent twice differ from the recording's"
	}
	return ""
}

// A bodyCensus is what a census finds in an answer's records: the keys
// that no field takes, the nulls that read as absent, and each record's
// keys, by type.
type bodyCensus struct {
	unknown []unknownKey
	nulls   []string
	records map[reflect.Type][]map[string]bool
}

// censusOfBody returns what a census of body's records, an answer of kind
// k, finds.
func censusOfBody(k answerKind, body []byte) bodyCensus {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	c := newCensus()
	var out bodyCensus
	for _, rec := range recordsOf(v) {
		unknown, nulls := c.add(k.typ, rec)
		out.unknown = append(out.unknown, unknown...)
		out.nulls = append(out.nulls, nulls...)
	}
	out.records = c.records
	return out
}

// gateTokens is gate (iii) for one file: its body by its kind, and its
// index entry.
func (s *scrubber) gateTokens(f *scrubbedFile) string {
	a := f.from
	var why string
	switch {
	case a.ext == ".json":
		why = s.tokensOfJSON(a, f.body)
	case a.ext == ".html":
		why = s.tokensOfPage(a, f.body)
	case !bytes.Equal(a.raw.body, f.body):
		why = "it differs from the recording's"
	}
	if why != "" {
		return why
	}
	return s.tokensOfEntry(a, f.entry)
}

// scalarsOf returns the scalars of body, valid JSON, in order.
func scalarsOf(body []byte) []jsonScalar {
	var out []jsonScalar
	walkJSON(body, func(sc jsonScalar) { out = append(out, sc) }, nil)
	return out
}

// skeleton returns body with each of its scalars, at, made one zero byte,
// which JSON cannot hold outside a string: its keys, punctuation and white
// space as they are.
func skeleton(body []byte, at []jsonScalar) []byte {
	var b bytes.Buffer
	prev := 0
	for _, sc := range at {
		b.Write(body[prev:sc.start])
		b.WriteByte(0)
		prev = sc.end
	}
	b.Write(body[prev:])
	return b.Bytes()
}

// jsonKindOf returns the JSON kind of raw, a scalar: a string, a number, a
// boolean or null.
func jsonKindOf(raw []byte) string {
	switch {
	case raw[0] == '"':
		return "a string"
	case isJSONBool(raw):
		return "a boolean"
	case string(raw) == "null":
		return "null"
	}
	return "a number"
}

// tokensOfJSON is gate (iii) for a JSON answer, out being what pass two
// wrote of it.
func (s *scrubber) tokensOfJSON(a *scrubAnswer, out []byte) string {
	if !json.Valid(out) {
		return "it is no longer JSON"
	}
	before, after := scalarsOf(a.raw.body), scalarsOf(out)
	if len(before) != len(after) || !bytes.Equal(skeleton(a.raw.body, before), skeleton(out, after)) {
		return "its keys, punctuation or white space differ from the recording's"
	}
	k := big.NewRat(1, 1)
	if s.cfg.amounts.mode == "scale" {
		k.SetInt64(s.cfg.amounts.k)
	}
	for i, sc := range after {
		o, n := a.raw.body[before[i].start:before[i].end], out[sc.start:sc.end]
		where := pathName(sc.path)
		if jsonKindOf(o) != jsonKindOf(n) {
			return where + ": a value of another JSON kind than the recording's"
		}
		if string(n) == "null" {
			continue
		}
		r, ok := a.table.rule(sc.path)
		if !ok {
			return where + ": a value at a path that no rule of the policy covers"
		}
		switch r.class {
		case classMap:
			if isJSONNumber(o) && isWholeNumber(o) != isWholeNumber(n) {
				return where + ": an identifier of another form than the recording's"
			}
		case classScale:
			ro, _ := ratOf(o)
			rn, ok := ratOf(n)
			switch {
			case isZeroLiteral(o) || s.cfg.amounts.mode != "scale":
				if !bytes.Equal(o, n) {
					return where + ": money that the policy keeps as it was, changed"
				}
			case !ok || hasExponent(n) || decimalsOf(n) != decimalsOf(o):
				return where + ": money of another form than the recording's"
			case new(big.Rat).Mul(ro, k).Cmp(rn) != 0:
				return where + ": money that is not K times the recording's"
			}
		case classKeep:
			if !r.gram.match(n) {
				return where + ": not " + r.gram.name
			}
			if !bytes.Equal(o, n) && !s.changedAtIdentifiers(string(o), string(n)) {
				return where + ": a kept value changed where it held no identifier"
			}
		case classPlaceholder:
			text, _ := jsonString(n)
			wording := r.text.isWording(text) && s.changedAtIdentifiers(string(o), string(n))
			if !bytes.Equal(o, n) && !r.text.isPlaceholder(text) && !wording {
				return where + ": free text changed into what is neither its placeholder nor Schwab's wording"
			}
		}
	}
	return ""
}

// changedAtIdentifiers reports whether after is before with each
// identifier in before replaced by some other text, and nothing else
// changed, and before holds one.
func (s *scrubber) changedAtIdentifiers(before, after string) bool {
	ms := s.ids.find(before, false)
	if len(ms) == 0 {
		return false
	}
	var pattern strings.Builder
	pattern.WriteString("^")
	prev := 0
	for _, m := range ms {
		pattern.WriteString(regexp.QuoteMeta(before[prev:m.start]) + "(.+?)")
		prev = m.end
	}
	pattern.WriteString(regexp.QuoteMeta(before[prev:]) + "$")
	re, err := regexp.Compile(pattern.String())
	return err == nil && re.MatchString(after)
}

// tokensOfPage is gate (iii) for an error page, out being what pass two
// wrote of it: the same page, every byte but its slots' as it was, a slot
// that is mapped its value's fake, and one that is kept of its grammar and
// changed only where it held an identifier.
func (s *scrubber) tokensOfPage(a *scrubAnswer, out []byte) string {
	p, before := matchPageSpans(s.pages, a.raw.body)
	q, after := matchPageSpans(s.pages, out)
	if p == nil || p != q {
		return "it is no longer the page it was"
	}
	prevBefore, prevAfter := 0, 0
	for i, slot := range p.slots {
		bs, be, as, ae := before[2*i+2], before[2*i+3], after[2*i+2], after[2*i+3]
		if !bytes.Equal(a.raw.body[prevBefore:bs], out[prevAfter:as]) {
			return "the page differs from the recording's outside its slots"
		}
		prevBefore, prevAfter = be, ae
		was, now := html.UnescapeString(string(a.raw.body[bs:be])), html.UnescapeString(string(out[as:ae]))
		switch slot.rule.class {
		case classMap:
			value := strings.TrimRight(was, ".")
			if v := s.ids.get(slot.rule.id, value); v == nil || v.fake+was[len(value):] != now {
				return slot.rule.path + ": not the fake of the recording's"
			}
		default:
			if !slot.rule.gram.match(jsonText(now)) {
				return slot.rule.path + ": not " + slot.rule.gram.name
			}
			if was != now && !s.changedAtIdentifiers(was, now) {
				return slot.rule.path + ": changed where it held no identifier"
			}
		}
	}
	if !bytes.Equal(a.raw.body[prevBefore:], out[prevAfter:]) {
		return "the page differs from the recording's outside its slots"
	}
	return ""
}

// tokensOfEntry is gate (iii) for an index entry, e: the request and the
// answer's status and protocol as recorded, the path and the query changed
// only where they held identifiers, the values of the headers that
// KeptHeader keeps but Content-Length, a correlation ID its fake and any
// other changed only where it held an identifier, and the names of the
// rest.
func (s *scrubber) tokensOfEntry(a *scrubAnswer, e schwabtest.Entry) string {
	m := a.raw.meta
	switch {
	case e.Request.Method != m.Request.Method || e.Response.Status != m.Response.Status || e.Response.Proto != m.Response.Proto ||
		e.Response.Uncompressed != m.Response.Uncompressed:
		return "its entry's request or status differs from the recording's"
	case e.Request.Path != m.Request.Path && !s.changedAtIdentifiers(m.Request.Path, e.Request.Path):
		return "its entry's path changed where it held no identifier"
	case e.Request.Query != m.Request.Query && !s.changedAtIdentifiers(m.Request.Query, e.Request.Query):
		return "its entry's query changed where it held no identifier"
	}
	kept := 0
	for _, name := range slices.Sorted(maps.Keys(m.Response.Headers)) {
		values := m.Response.Headers[name]
		if !schwabtest.KeptHeader(name) || strings.EqualFold(name, "Content-Length") {
			if !slices.Contains(e.Response.OtherHeaders, name) {
				return "its entry lost the name of a header"
			}
			continue
		}
		kept++
		got, ok := e.Response.Headers[name]
		if !ok || len(got) != len(values) {
			return "its entry's header " + name + " differs from the recording's"
		}
		for i, v := range values {
			switch {
			case got[i] == v && v == "":
			case isCorrelationHeader(name):
				if t := s.ids.get(idTrace, v); t == nil || t.fake != got[i] {
					return "its entry's header " + name + " is not the fake of the recording's"
				}
			case got[i] != v && !s.changedAtIdentifiers(v, got[i]):
				return "its entry's header " + name + " changed where it held no identifier"
			}
		}
	}
	if len(e.Response.Headers) != kept {
		return "its entry has a header that the recording's does not keep"
	}
	for _, name := range m.Response.OtherHeaders {
		if !slices.Contains(e.Response.OtherHeaders, name) {
			return "its entry lost the name of a header"
		}
	}
	return ""
}

// A pairing is how one kind of identifier was mapped, as gate (iv) reads
// it from what pass two wrote: each value's fake, and each fake's value.
type pairing struct {
	fakeOf, origOf map[string]string
	broken         bool
}

// add records that orig became fake once more, and whether that agrees
// with what was recorded before: one fake for each value, and one value
// for each fake.
func (p *pairing) add(orig, fake string) {
	if p.fakeOf == nil {
		p.fakeOf, p.origOf = map[string]string{}, map[string]string{}
	}
	if f, ok := p.fakeOf[orig]; ok && f != fake {
		p.broken = true
	}
	if r, ok := p.origOf[fake]; ok && r != orig {
		p.broken = true
	}
	p.fakeOf[orig], p.origOf[fake] = fake, orig
}

// gateRelations is gate (iv).
func (s *scrubber) gateRelations(o *scrubOutput) string {
	pairings := map[identifier]*pairing{}
	pair := func(id identifier, orig, fake string) {
		if pairings[id] == nil {
			pairings[id] = &pairing{}
		}
		pairings[id].add(orig, fake)
	}
	for _, f := range o.files {
		a := f.from
		if a.table != nil {
			before, after := scalarsOf(a.raw.body), scalarsOf(f.body)
			for i, sc := range after {
				r, ok := a.table.rule(sc.path)
				was, now := a.raw.body[before[i].start:before[i].end], f.body[sc.start:sc.end]
				if !ok || r.class != classMap || string(now) == "null" {
					continue
				}
				orig, fake := idOf(was), idOf(now)
				switch r.id {
				case idHash:
					orig, fake = strings.ToUpper(orig), strings.ToUpper(fake)
					if number, ok := besideAccount(f.body, sc.in); !ok || fake != schwabtest.HashFor(number) {
						return "answer " + a.raw.name + ": " + pathName(sc.path) + ": a hash that is not HashFor of the account number beside it"
					}
				case idDisplay:
					// Two accounts can end with the same digits, and their
					// fakes do not, so a masked number is held only to its
					// own account.
					if why := displayKept(orig, fake, a.raw.body, f.body, before[i].in, sc.in); why != "" {
						return "answer " + a.raw.name + ": " + pathName(sc.path) + ": " + why
					}
					continue
				}
				pair(r.id, orig, fake)
			}
		}
		if before, after := strings.Split(a.raw.meta.Request.Path, "/"), strings.Split(f.entry.Request.Path, "/"); len(before) == len(after) &&
			len(before) > 4 && strings.HasPrefix(a.raw.meta.Request.Path, "/trader/v1/accounts/") {
			account := unescapedPart(before[4])
			switch {
			case s.ids.get(idHash, account) != nil:
				pair(idHash, strings.ToUpper(account), strings.ToUpper(unescapedPart(after[4])))
			case s.ids.get(idAccountNumber, account) != nil:
				pair(idAccountNumber, account, unescapedPart(after[4]))
			}
			if len(before) > 6 {
				if kind := pathIDKind(before[5:], 1); kind != "" && s.ids.get(kind, unescapedPart(before[6])) != nil {
					pair(kind, unescapedPart(before[6]), unescapedPart(after[6]))
				}
			}
		}
		if why := s.identities(a, f.body); why != "" {
			return "answer " + a.raw.name + ": " + why
		}
	}
	for _, id := range []identifier{idAccountNumber, idHash, idNickname, idActivity, idOrder, idPosition, idError} {
		if p := pairings[id]; p != nil && p.broken {
			return "the fakes of " + string(id) + " are not one for each value and one value for each"
		}
	}
	return ""
}

// idOf returns an identifier as raw holds it: a string's text, or a
// number as written.
func idOf(raw []byte) string {
	if s, ok := jsonString(raw); ok {
		return s
	}
	return string(raw)
}

// displayKept returns why fake, the masked number that pass two wrote in
// place of orig, does not keep orig's form: the same text before the
// digits, as many digits, and those the end of the account number beside
// it, in after's object in; or "". before's object was.
func displayKept(orig, fake string, before, after []byte, was, in *jsonObject) string {
	tail, fakeTail := trailingDigits.FindString(orig), trailingDigits.FindString(fake)
	number, ok := besideAccount(after, in)
	_, hadNumber := besideAccount(before, was)
	switch {
	case !ok || !hadNumber:
		return "a masked account number with no account number beside it"
	case len(tail) != len(fakeTail) || orig[:len(orig)-len(tail)] != fake[:len(fake)-len(fakeTail)]:
		return "a masked account number of another form than the recording's"
	case !strings.HasSuffix(number, fakeTail):
		return "a masked account number that does not end with the account number beside it"
	}
	return ""
}

// A residual is how far one linear identity of a record is from holding,
// such as a trade's netAmount less the sum of its legs' costs, computed
// exactly from the numbers as written.
type residual struct {
	name string
	r    *big.Rat
}

// identities returns why the linear identities of a's records, in out as
// pass two wrote them, are not off by K times what they were in the
// recording, K the scale factor, or 1 under keep; or "". Fixed tolerances
// would refuse good output: a residual of 0.000637 in the recording is
// 0.01274 at K = 20.
func (s *scrubber) identities(a *scrubAnswer, out []byte) string {
	before, after := residualsOf(a.kind, a.raw.body), residualsOf(a.kind, out)
	if len(before) != len(after) {
		return "its records hold other identities than the recording's"
	}
	k := big.NewRat(1, 1)
	if s.cfg.amounts.mode == "scale" {
		k.SetInt64(s.cfg.amounts.k)
	}
	for i, b := range before {
		if after[i].name != b.name || new(big.Rat).Mul(b.r, k).Cmp(after[i].r) != 0 {
			return "the identity of " + b.name + " is not off by K times what it was"
		}
	}
	return ""
}

// liquidationParts are the balances whose sum is an account's current
// liquidation value.
var liquidationParts = []string{
	"cashBalance", "longMarketValue", "mutualFundValue", "longOptionMarketValue", "shortOptionMarketValue",
	"shortMarketValue", "bondValue", "moneyMarketFund", "savings",
}

// residualsOf returns the residuals of the linear identities of body's
// records, an answer of kind: for an account, its current liquidation
// value against its parts, the aggregated against the current, the initial
// account value against the initial liquidation value, a position's cost
// basis against its average price times its quantity, and the positions'
// market values against the balances'; for a transaction, a trade's
// netAmount against its legs' costs, a fee leg's cost against its amount,
// a security leg's cost against its amount times its price and multiplier,
// and a cash row's netAmount against its amounts.
func residualsOf(kind string, body []byte) []residual {
	if !json.Valid(body) {
		return nil
	}
	var out []residual
	add := func(name string, terms ...*big.Rat) {
		sum := new(big.Rat)
		for _, t := range terms {
			sum.Add(sum, t)
		}
		out = append(out, residual{name: name, r: sum})
	}
	root := parseNode(body)
	records := []*node{root}
	if root.array {
		records = root.vals
	}
	for _, rec := range records {
		switch kind {
		case "accounts", "account":
			accountResiduals(rec, add)
		case "transactions", "transaction":
			transactionResiduals(rec, add)
		}
	}
	return out
}

// lastMember returns the value of n's last member with key, in any case,
// or nil, as encoding/json takes the last of several.
func lastMember(n *node, key string) *node {
	if n == nil || n.raw != nil || n.array {
		return nil
	}
	for i, k := range slices.Backward(n.keys) {
		if strings.EqualFold(k, key) {
			return n.vals[i]
		}
	}
	return nil
}

// numberAt returns the number at keys within n, and whether there is one.
func numberAt(n *node, keys ...string) (*big.Rat, bool) {
	for _, k := range keys {
		n = lastMember(n, k)
	}
	if n == nil || n.raw == nil {
		return nil, false
	}
	return ratOf(n.raw)
}

// orZero returns the number at keys within n, or 0.
func orZero(n *node, keys ...string) *big.Rat {
	if r, ok := numberAt(n, keys...); ok {
		return r
	}
	return new(big.Rat)
}

// textAt returns the string at keys within n, or "".
func textAt(n *node, keys ...string) string {
	for _, k := range keys {
		n = lastMember(n, k)
	}
	if n == nil || n.raw == nil {
		return ""
	}
	s, _ := jsonString(n.raw)
	return s
}

// neg returns -r.
func neg(r *big.Rat) *big.Rat { return new(big.Rat).Neg(r) }

// times returns the product of rs.
func times(rs ...*big.Rat) *big.Rat {
	p := big.NewRat(1, 1)
	for _, r := range rs {
		p.Mul(p, r)
	}
	return p
}

// accountResiduals adds the residuals of an account's identities.
func accountResiduals(rec *node, add func(string, ...*big.Rat)) {
	sa := lastMember(rec, "securitiesAccount")
	cb, ib := lastMember(sa, "currentBalances"), lastMember(sa, "initialBalances")
	if lv, ok := numberAt(cb, "liquidationValue"); ok {
		terms := []*big.Rat{lv}
		all := true
		for _, p := range liquidationParts {
			r, ok := numberAt(cb, p)
			all = all && ok
			if ok {
				terms = append(terms, neg(r))
			}
		}
		if all {
			add("the current liquidation value against its parts", terms...)
		}
		if agg, ok := numberAt(rec, "aggregatedBalance", "currentLiquidationValue"); ok {
			add("the aggregated liquidation value against the current", agg, neg(lv))
		}
	}
	if av, ok := numberAt(ib, "accountValue"); ok {
		if lv, ok := numberAt(ib, "liquidationValue"); ok {
			add("the initial account value against the initial liquidation value", av, neg(lv))
		}
	}
	positions := lastMember(sa, "positions")
	if positions == nil || !positions.array || len(positions.vals) == 0 {
		return
	}
	long, options, funds := new(big.Rat), new(big.Rat), new(big.Rat)
	for _, p := range positions.vals {
		mult := big.NewRat(1, 1)
		asset := AssetType(textAt(p, "instrument", "assetType"))
		if asset == AssetOption {
			mult = big.NewRat(100, 1)
		}
		mv := orZero(p, "marketValue")
		if q, ok := numberAt(p, "longQuantity"); ok && q.Sign() > 0 {
			if pl, ok := numberAt(p, "longOpenProfitLoss"); ok {
				add("a long position's cost basis against its average price", mv, neg(pl), neg(times(orZero(p, "averagePrice"), q, mult)))
			}
		}
		if q, ok := numberAt(p, "shortQuantity"); ok && q.Sign() > 0 {
			if pl, ok := numberAt(p, "shortOpenProfitLoss"); ok {
				add("a short position's cost basis against its average price", pl, neg(mv), neg(times(orZero(p, "averagePrice"), q, mult)))
			}
		}
		switch asset {
		case AssetOption:
			options.Add(options, mv)
		case AssetMutualFund:
			funds.Add(funds, mv)
		default:
			long.Add(long, mv)
		}
	}
	if lmv, ok := numberAt(cb, "longMarketValue"); ok {
		add("the positions' market values against the balances'", long, neg(lmv), neg(orZero(cb, "shortMarketValue")))
		if lo, ok := numberAt(cb, "longOptionMarketValue"); ok {
			add("the options' market values against the balances'", options, neg(lo), neg(orZero(cb, "shortOptionMarketValue")))
		}
		if mf, ok := numberAt(cb, "mutualFundValue"); ok {
			add("the funds' market values against the balances'", funds, neg(mf))
		}
	}
}

// transactionResiduals adds the residuals of a transaction's identities.
func transactionResiduals(rec *node, add func(string, ...*big.Rat)) {
	legs := lastMember(rec, "transferItems")
	var items []*node
	if legs != nil && legs.array {
		items = legs.vals
	}
	net := orZero(rec, "netAmount")
	if textAt(rec, "type") == string(TransactionTrade) {
		terms := []*big.Rat{net}
		for _, l := range items {
			terms = append(terms, neg(orZero(l, "cost")))
		}
		add("a trade's netAmount against its legs' costs", terms...)
		for _, l := range items {
			asset := AssetType(textAt(l, "instrument", "assetType"))
			if asset == AssetCurrency {
				add("a fee leg's cost against its amount", orZero(l, "cost"), orZero(l, "amount"))
				continue
			}
			price, ok := numberAt(l, "price")
			if !ok {
				continue
			}
			mult := big.NewRat(1, 1)
			if m, ok := numberAt(l, "instrument", "optionPremiumMultiplier"); ok {
				mult = m
			} else if asset == AssetOption {
				mult = big.NewRat(100, 1)
			}
			add("a security leg's cost against its amount, price and multiplier", orZero(l, "cost"), times(orZero(l, "amount"), price, mult))
		}
		return
	}
	terms := []*big.Rat{net}
	for _, l := range items {
		if AssetType(textAt(l, "instrument", "assetType")) != AssetCurrency {
			return
		}
		terms = append(terms, neg(orZero(l, "amount")))
	}
	if len(items) > 0 {
		add("a cash row's netAmount against its amounts", terms...)
	}
}

// gateLeaks is gate (v): no identifier of the recording, no local path
// and not the user name in what would be written: each file, as written
// and decoded, its entry, entries.json, review.txt and each shape report.
func (s *scrubber) gateLeaks(o *scrubOutput) string {
	var texts []leakText
	for _, f := range o.files {
		for _, t := range slices.Concat(textsOf(f.body), entryTexts(f.entry)) {
			t.where = "answer " + f.from.raw.name + ": " + t.where
			texts = append(texts, t)
		}
	}
	for _, t := range textsOf(o.entriesJSON()) {
		t.where = "entries.json: " + t.where
		texts = append(texts, t)
	}
	plain := map[string]string{"review.txt": o.review}
	for name, shape := range o.shapes {
		plain["shapes/"+name+".txt"] = shape
	}
	for _, name := range slices.Sorted(maps.Keys(plain)) {
		for _, f := range unescapedForms(plain[name]) {
			texts = append(texts, leakText{where: name, text: f})
		}
	}
	local := newLiveScrubber()
	local.setHome(s.cfg.home)
	for _, p := range s.cfg.paths {
		local.addPath("{path}", p)
	}
	user := ""
	if s.cfg.home != "" {
		user = filepath.Base(s.cfg.home)
	}
	for _, t := range texts {
		if v := s.leakInText(t); v != nil {
			return t.where + ": " + string(v.id) + " of the recording"
		}
		if local.holds(t.text) {
			return t.where + ": a local path"
		}
		// The user name is looked for in values, not in Schwab's own names,
		// the keys, which a short user name could be, as type or cost.
		if user != "" && !t.key && !t.long && holdsUserName(t.text, user) {
			return t.where + ": the user name"
		}
	}
	return ""
}

// holdsUserName reports whether text holds word, the user name, as a whole
// token, with no letter, digit or underscore next to it, in any case where
// the file system takes names in any case, as on macOS and Windows. A user
// name may be a short word, such as al, which "Free Balance" holds but not
// as a token.
func holdsUserName(text, word string) bool {
	if len(word) < 2 {
		return false
	}
	t, w := text, word
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t, w = asciiLower(text), asciiLower(word)
	}
	for i := 0; ; {
		j := strings.Index(t[i:], w)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(w)
		if isWholeToken(t, start, end) {
			return true
		}
		i = start + 1
	}
}

// isWholeToken reports whether s[i:end] has no letter, digit or underscore
// next to it.
func isWholeToken(s string, i, end int) bool {
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[end:])
	return (i == 0 || !wordRune(before)) && (end == len(s) || !wordRune(after))
}
