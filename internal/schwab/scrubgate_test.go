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
	"testing"
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
//   - (v) leaks: no identifier of the recording and no local path, the
//     user name as the folder of a home among them, in any file, entry,
//     review.txt or shape, searched as written and decoded.
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
			if !bytes.Equal(o, n) && !s.changedInJSON(o, n) {
				return where + ": a kept value changed where it held no identifier"
			}
		case classPlaceholder:
			// Every value, changed or not: free text that pass two kept as
			// it was must be Schwab's wording, as much as a changed one.
			text, isString := jsonString(n)
			switch {
			case !isString:
				return where + ": free text that is not a string"
			case r.text.isPlaceholder(text):
			case !r.text.isWording(text):
				return where + ": free text that is neither its placeholder nor Schwab's wording"
			case !bytes.Equal(o, n) && !s.changedInJSON(o, n):
				return where + ": Schwab's wording changed where it held no identifier"
			}
		}
	}
	return ""
}

// changedAtIdentifiers reports whether after is before, a path, a query or
// a header's value, with each identifier in before replaced by some other
// text, and nothing else changed, and before holds one.
func (s *scrubber) changedAtIdentifiers(before, after string) bool {
	at := make([]int, len(before))
	for i := range at {
		at[i] = i
	}
	return s.changedInPlace([]byte(before), []byte(after), []byte(before), at)
}

// changedInJSON reports whether after is before, a JSON string as written,
// with each identifier in its text replaced in place by some other text,
// and every other byte as it was, escapes included, and its text holds
// one. The identifiers are found in the text unescaped, as pass two finds
// them, so that an escape next to a short one, such as & written as an
// escape before a nickname, leaves it a whole token.
func (s *scrubber) changedInJSON(before, after []byte) bool {
	text, at := unescapeJSON(before)
	return s.changedInPlace(before, after, text, at)
}

// changedInPlace reports whether after is raw, a value as written, with
// each identifier in text replaced in place by some other text, and every
// other byte of raw as it was, and text holds one: text being what raw
// holds unescaped, and at the offset in raw of each byte of text, as
// unescapeJSON and unescapeHTML give it. An identifier written with
// escapes cannot have been replaced in place.
func (s *scrubber) changedInPlace(raw, after, text []byte, at []int) bool {
	ms := s.ids.find(string(text), false)
	if len(ms) == 0 {
		return false
	}
	var pattern strings.Builder
	pattern.WriteString("^")
	prev := 0
	for _, m := range ms {
		start, end, ok := rawSpan(at, m.start, m.end)
		if !ok {
			return false
		}
		pattern.WriteString(regexp.QuoteMeta(string(raw[prev:start])) + "(.+?)")
		prev = end
	}
	pattern.WriteString(regexp.QuoteMeta(string(raw[prev:])) + "$")
	re, err := regexp.Compile(pattern.String())
	return err == nil && re.Match(after)
}

// tokensOfPage is gate (iii) for an error page, out being what pass two
// wrote of it: the same page, every byte but its slots' as it was, a slot
// that is mapped its value's fake, and one that is kept of its grammar and
// changed only where its text held an identifier, its every other byte,
// character references included, as it was.
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
		rawWas, rawNow := a.raw.body[bs:be], out[as:ae]
		was, now := html.UnescapeString(string(rawWas)), html.UnescapeString(string(rawNow))
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
			text, at, _ := unescapeHTML(rawWas)
			if !bytes.Equal(rawWas, rawNow) && !s.changedInPlace(rawWas, rawNow, text, at) {
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

// gateLeaks is gate (v): no identifier of the recording and no local path
// in what would be written: each file, as written and decoded, its entry,
// entries.json, review.txt and each shape report. A local path is the home
// folder or another path the scrubber was given, in any of their
// spellings, or the user name, the home folder's last element, as the
// folder of a home elsewhere. The user name alone is not looked for:
// Schwab's words and the scrubber's own can spell it, as max does in a
// header's max-age, trader in every path and json in every file's name and
// in review.txt, and nothing but a path would bring it into the output.
func (s *scrubber) gateLeaks(o *scrubOutput) string {
	var texts []leakText
	for _, f := range o.files {
		for _, t := range slices.Concat(textsOf(f.body, f.from.table), entryTexts(f.entry)) {
			t.where = "answer " + f.from.raw.name + ": " + t.where
			texts = append(texts, t)
		}
	}
	for _, t := range textsOf(o.entriesJSON(), nil) {
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
	local.addPath(homeLabel, s.cfg.home)
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
		if user != "" && holdsHomeOf(t.text, user) {
			return t.where + ": the user name"
		}
	}
	return ""
}

// homeFolders are the folders that hold the users' homes, as a path spells
// them on Linux, on macOS and on Windows.
var homeFolders = []string{"/home/", "/Users/", `\Users\`}

// holdsHomeOf reports whether text holds a home of user, a user name: the
// name after one of homeFolders, with no letter, digit, underscore or
// hyphen after it, which would make it part of another name; in any case
// where the file system takes names in any case, as on macOS and Windows.
func holdsHomeOf(text, user string) bool {
	fold := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	if fold {
		text = asciiLower(text)
	}
	for _, folder := range homeFolders {
		home := folder + user
		if fold {
			home = asciiLower(home)
		}
		for i := 0; ; {
			j := strings.Index(text[i:], home)
			if j < 0 {
				break
			}
			end := i + j + len(home)
			if after, _ := utf8.DecodeRuneInString(text[end:]); end == len(text) || !wordRune(after) && after != '-' {
				return true
			}
			i += j + 1
		}
	}
	return false
}

// Each gate refuses an output broken against it, through the test hook,
// before anything is written, naming itself, the answer and why, and
// holding no value. Each check of a gate has a case that the gates before
// it let through:
//
//   - (i): a problem introduced, an error's code changed, and an error made
//     to say nothing;
//   - (ii): a key dropped, and a key sent twice made to differ;
//   - (iii): white space changed, two keys swapped, a value of another JSON
//     kind, an id in another form, a kept value off its grammar or changed,
//     or its identifier's fake deleted, money that keep keeps changed, a
//     zero changed under scale:K, money not K times what it was or in
//     another form, a decimal more or an exponent, free text put back or
//     Schwab's wording made other wording, a 429's white space changed, a
//     page changed outside its slots, its Reference not its fake, its
//     address off its grammar, changed or spelled otherwise, and an entry's
//     correlation ID, method, status, protocol, compression, path, query,
//     header, a header's values, extra header or header's name changed;
//   - (iv): two hashes swapped; a hash's, an account number's, an activity
//     ID's or an order ID's fake changed in a path alone; an id, an error's
//     ID or a nickname given two fakes, and two position IDs one; and a
//     masked number off its account or of another form;
//   - (v): one mapping switched off in a body, in a query, in a path where
//     no id goes, in a header's value, in a string written with an escape,
//     and in a page written with a character reference; an identifier in a
//     shape; a local path, under the home folder or not; and the user name
//     as the folder of a home elsewhere.
func TestScrubberGatesRefuse(t *testing.T) {
	planted, p := plantF(t, 3)
	home := filepath.Join(t.TempDir(), "zq9user")
	// The recording's folder, which is not under the home folder.
	raw := filepath.Join(t.TempDir(), "raw")
	keep, refuse, scale := amountsPolicy{mode: "keep"}, amountsPolicy{mode: "refuse"}, amountsPolicy{mode: "scale", k: 20}
	replace := func(name, old, with string) func(t *testing.T, o *scrubOutput) {
		return func(t *testing.T, o *scrubOutput) {
			t.Helper()
			f := fileNamed(t, o, name)
			if !bytes.Contains(f.body, []byte(old)) {
				t.Fatalf("answer %s holds no %s", name, old)
			}
			f.body = bytes.Replace(f.body, []byte(old), []byte(with), 1)
		}
	}
	entry := func(name string, change func(e *schwabtest.Entry)) func(t *testing.T, o *scrubOutput) {
		return func(t *testing.T, o *scrubOutput) {
			t.Helper()
			change(&fileNamed(t, o, name).entry)
		}
	}
	swapHashes := func(name string) func(t *testing.T, o *scrubOutput) {
		return func(t *testing.T, o *scrubOutput) {
			h1, h2 := schwabtest.FixtureAccounts[0].Hash, schwabtest.FixtureAccounts[1].Hash
			replace(name, h1, "swap")(t, o)
			replace(name, h2, h1)(t, o)
			replace(name, "swap", h2)(t, o)
		}
	}
	dropName := func(name, header string) func(t *testing.T, o *scrubOutput) {
		return entry(name, func(e *schwabtest.Entry) {
			e.Response.OtherHeaders = slices.DeleteFunc(slices.Clone(e.Response.OtherHeaders), func(n string) bool { return n == header })
		})
	}
	fake := func(i int) schwabtest.FixtureAccount { return schwabtest.FixtureAccounts[i] }
	nickname := p.planted[idNickname][fake(0).Nickname]
	errorID := p.planted[idError]["00000000-0000-4000-8000-000000000000"]

	// Recordings beyond F, each for the cases that F cannot reach.
	b := newCorpusBase(t, 29, 3)
	page := strings.Replace(strings.ReplaceAll(string(readTestdata(t, "errors/akamai_403.html")),
		strings.ReplaceAll(schwabtest.AkamaiReference, ".", "&#46;"), "47&#46;3c5d7e9f&#46;1792539999&#46;4a5b6c"),
		"http&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;v1&#47;oauth&#47;token",
		"https&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;trader&#47;v1&#47;accounts&#47;"+b.hashes[0], 1)
	withPage := []testAnswer{b.accountNumbers(), {
		path: "/trader/v1/accounts/" + b.hashes[0], status: 403, header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, body: page,
	}}
	onlyNumbers := []testAnswer{b.accountNumbers()}
	freeText := []testAnswer{b.accountNumbers(), b.transactions(cashRow(b.numbers[0], b.id(), "description", `"Wire Incoming JOHN Q PUBLIC 5551234"`))}
	etag := b.accountNumbers()
	etag.header.Set("ETag", `"x1"`)
	withETag := []testAnswer{etag}
	order := b.p.draw(13, plantDigits, plantDigits)
	withOrder := []testAnswer{
		b.accountNumbers(), b.transactions(cashRow(b.numbers[0], b.id(), "orderId", order)),
		jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/orders/"+order, "", 404, `{"message":"Order not found"}`),
	}
	refusal := `{"errors":[{"id":"` + b.p.value(idError, "e") + `","status":401,"title":"Unauthorized","detail":"Client not authorized"}]}`
	twoErrors := []testAnswer{
		b.accountNumbers(), jsonAnswer("/trader/v1/userPreference", "", 401, refusal), jsonAnswer("/trader/v1/accounts", "", 401, refusal),
	}
	twoPrefs := append(slices.Clone(planted), planted[4])
	withQuery := []testAnswer{b.accountNumbers(), jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/transactions", "types=TRADE&symbol="+b.numbers[1], 200, "[]")}
	withPathHash := []testAnswer{b.accountNumbers(), jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/x/"+b.hashes[1], "", 404, `{"message":"Not Found"}`)}
	vary := jsonAnswer("/trader/v1/accounts/"+b.hashes[2], "", 400, `{"message":"Invalid account number"}`)
	vary.header.Set("Vary", "Accept, x-account-"+b.numbers[2])
	withHeader := []testAnswer{b.accountNumbers(), vary}
	withAmounts := []testAnswer{b.accountNumbers(), b.transactions(
		cashRow(b.numbers[0], b.id(), "netAmount", "-0.0", "transferItems", cashLeg("-0.0", "USD currency")),
		cashRow(b.numbers[0], b.id(), "netAmount", "1.250", "transferItems", cashLeg("1.250", "USD currency")),
	)}
	withNickname := []testAnswer{
		b.accountNumbers(), jsonAnswer("/trader/v1/userPreference", "", 200, preferences(preference(b.numbers[0], "Kip", "..."+b.numbers[0][5:]))),
		b.transactions(cashRow(b.numbers[0], b.id(), "transferItems", cashLeg("1.25", "TRUST Kip"))),
	}

	for _, tc := range []struct {
		name    string
		want    string // the refusal's start: the gate, the answer and why
		hook    func(t *testing.T, o *scrubOutput)
		answers []testAnswer  // the recording, or F planted
		amounts amountsPolicy // what the scrubber does with money
	}{
		// (i)
		{
			"a problem introduced", "gate (i), decoding: answer 003: decoded, it reads otherwise than the recording's",
			replace("003", `"time": "2024-04-04T15:56:07+0000"`, `"time": "zq9"`), nil, keep,
		},
		{
			"an error's code changed", "gate (i), decoding: answer 006: decoded, it reads otherwise than the recording's",
			replace("006", "flow.GatewayTimeout", "flow.ServiceUnavailable"), nil, keep,
		},
		{
			"an error made to say nothing", "gate (i), decoding: answer 007: decoded, it reads otherwise than the recording's",
			replace("007", `"Invalid account number"`, `""`), nil, keep,
		},

		// (ii)
		{
			"a key dropped", "gate (ii), the census: answer 003: the census of its records differs from the recording's",
			replace("003", `"description": "Dividend Reinvest",`, ""), nil, keep,
		},
		{
			"a key sent twice made to differ", "gate (ii), the census: answer 003: its keys sent twice differ from the recording's",
			replace("003", "\"assetType\": \"COLLECTIVE_INVESTMENT\"\n", "\"assetType\": \"EQUITY\"\n"), nil, keep,
		},

		// (iii)
		{
			"white space changed", "gate (iii), the tokens: answer 003: its keys, punctuation or white space differ from the recording's",
			replace("003", "{\n", "{\r\n"), nil, keep,
		},
		{
			"two keys swapped", "gate (iii), the tokens: answer 003: its keys, punctuation or white space differ from the recording's",
			replace("003", "\"status\": \"VALID\",\n    \"subAccount\": \"CASH\",", "\"subAccount\": \"CASH\",\n    \"status\": \"VALID\","), nil, keep,
		},
		{
			"a value of another JSON kind", "gate (iii), the tokens: answer 009: errors.status: a value of another JSON kind than the recording's",
			replace("009", `"status":401`, `"status":"401"`), nil, keep,
		},
		{
			"an id in another form", "gate (iii), the tokens: answer 003: activityId: an identifier of another form than the recording's",
			replace("003", `"activityId": `, `"activityId": -`), nil, keep,
		},
		{
			"a kept value off its grammar", "gate (iii), the tokens: answer 003: subAccount: not a value of its closed list",
			replace("003", `"subAccount": "CASH"`, `"subAccount": "GIFT"`), nil, keep,
		},
		{
			"a kept value changed", "gate (iii), the tokens: answer 003: subAccount: a kept value changed where it held no identifier",
			replace("003", `"subAccount": "CASH"`, `"subAccount": "MARGIN"`), nil, keep,
		},
		{
			"money that keep keeps, changed", "gate (iii), the tokens: answer 003: netAmount: money that the policy keeps as it was, changed",
			replace("003", `"netAmount": -2.7,`, `"netAmount": -2.8,`), nil, keep,
		},
		{
			"an amount not K times the recording's", "gate (iii), the tokens: answer 003: netAmount: money that is not K times the recording's",
			replace("003", `"netAmount": -54.0,`, `"netAmount": -54.1,`), nil, scale,
		},
		{
			"an amount with a decimal more", "gate (iii), the tokens: answer 003: netAmount: money of another form than the recording's",
			replace("003", `"netAmount": -54.0,`, `"netAmount": -54.00,`), nil, scale,
		},
		{
			"an amount given an exponent of as many decimals", "gate (iii), the tokens: answer 002: netAmount: money of another form than the recording's",
			replace("002", `"netAmount":25.000,`, `"netAmount":2.5E1,`), withAmounts, scale,
		},
		{
			"a zero changed under scale:K", "gate (iii), the tokens: answer 002: netAmount: money that the policy keeps as it was, changed",
			replace("002", `"netAmount":-0.0,`, `"netAmount":0.0,`), withAmounts, scale,
		},
		{
			"an identifier's fake deleted from a kept value",
			"gate (iii), the tokens: answer 003: transferItems.instrument.description: a kept value changed where it held no identifier",
			replace("003", `"TRUST Account 1"`, `"TRUST "`), withNickname, keep,
		},
		{
			"free text put back", "gate (iii), the tokens: answer 002: description: free text that is neither its placeholder nor Schwab's wording",
			replace("002", `"DESCRIPTION 1"`, `"Wire Incoming JOHN Q PUBLIC 5551234"`), freeText, keep,
		},
		{
			"Schwab's wording made other wording", "gate (iii), the tokens: answer 003: description: Schwab's wording changed where it held no identifier",
			replace("003", `"Dividend Reinvest"`, `"Sell Trade"`), nil, keep,
		},
		{
			"a 429's white space changed", "gate (iii), the tokens: answer 010: it differs from the recording's",
			func(t *testing.T, o *scrubOutput) { fileNamed(t, o, "010").body = []byte("  \n") }, nil, keep,
		},
		{
			"a page changed outside its slots", "gate (iii), the tokens: answer 013: it is no longer the page it was",
			replace("013", "Whitelabel Error Page", "Whitelabel Error page"), nil, keep,
		},
		{
			"a page's line end changed before its slots", "gate (iii), the tokens: answer 002: the page differs from the recording's outside its slots",
			replace("002", "\n", "\r\n"), withPage, keep,
		},
		{
			"a page's line end changed after its slots", "gate (iii), the tokens: answer 002: the page differs from the recording's outside its slots",
			func(t *testing.T, o *scrubOutput) {
				f := fileNamed(t, o, "002")
				f.body = append(bytes.TrimSuffix(f.body, []byte("\n")), "\r\n"...)
			}, withPage, keep,
		},
		{
			"a page's Reference not its fake", "gate (iii), the tokens: answer 002: the Reference: not the fake of the recording's",
			replace("002", strings.ReplaceAll(schwabtest.AkamaiReference, ".", "&#46;"), "77&#46;0a1b2c3d&#46;1792539138&#46;5e6f"), withPage, keep,
		},
		{
			"a page's address off its grammar", "gate (iii), the tokens: answer 002: the address quoted: not an address of Schwab's API",
			replace("002", "api&#46;schwabapi&#46;com", "example&#46;com"), withPage, keep,
		},
		{
			"a page's address changed", "gate (iii), the tokens: answer 002: the address quoted: changed where it held no identifier",
			replace("002", "trader&#47;v1", "marketdata&#47;v1"), withPage, keep,
		},
		{
			"a page's address spelled otherwise", "gate (iii), the tokens: answer 002: the address quoted: changed where it held no identifier",
			replace("002", "&#47;trader", "/trader"), withPage, keep,
		},
		{
			"an entry's correlation ID changed", "gate (iii), the tokens: answer 006: its entry's header Schwab-Client-CorrelID is not the fake of the recording's",
			entry("006", func(e *schwabtest.Entry) { e.Response.Headers["Schwab-Client-CorrelID"] = []string{counterID(99)} }), nil, keep,
		},
		{
			"an entry's method changed", "gate (iii), the tokens: answer 007: its entry's request or status differs from the recording's",
			entry("007", func(e *schwabtest.Entry) { e.Request.Method = http.MethodHead }), nil, keep,
		},
		{
			"an entry's status changed", "gate (iii), the tokens: answer 007: its entry's request or status differs from the recording's",
			entry("007", func(e *schwabtest.Entry) { e.Response.Status = 401 }), nil, keep,
		},
		{
			"an entry's compression changed", "gate (iii), the tokens: answer 007: its entry's request or status differs from the recording's",
			entry("007", func(e *schwabtest.Entry) { e.Response.Uncompressed = !e.Response.Uncompressed }), nil, keep,
		},
		{
			"an entry's protocol changed", "gate (iii), the tokens: answer 007: its entry's request or status differs from the recording's",
			entry("007", func(e *schwabtest.Entry) { e.Response.Proto = "HTTP/1.0" }), nil, keep,
		},
		{
			"an entry's path changed", "gate (iii), the tokens: answer 004: its entry's path changed where it held no identifier",
			entry("004", func(e *schwabtest.Entry) { e.Request.Path += "x" }), nil, keep,
		},
		{
			"an entry's query changed", "gate (iii), the tokens: answer 002: its entry's query changed where it held no identifier",
			entry("002", func(e *schwabtest.Entry) { e.Request.Query = "fields=orders" }), nil, keep,
		},
		{
			"an entry's Content-Type changed", "gate (iii), the tokens: answer 007: its entry's header Content-Type changed where it held no identifier",
			entry("007", func(e *schwabtest.Entry) { e.Response.Headers["Content-Type"] = []string{"text/plain"} }), nil, keep,
		},
		{
			"a header given a second value", "gate (iii), the tokens: answer 007: its entry's header Content-Type differs from the recording's",
			entry("007", func(e *schwabtest.Entry) {
				e.Response.Headers["Content-Type"] = append(slices.Clone(e.Response.Headers["Content-Type"]), "text/plain")
			}), nil, keep,
		},
		{
			"a header added to an entry", "gate (iii), the tokens: answer 007: its entry has a header that the recording's does not keep",
			entry("007", func(e *schwabtest.Entry) { e.Response.Headers["Vary"] = []string{"Accept"} }), nil, keep,
		},
		{
			"Content-Length's name lost", "gate (iii), the tokens: answer 007: its entry lost the name of a header",
			dropName("007", "Content-Length"), nil, keep,
		},
		{
			"an unkept header's name lost", "gate (iii), the tokens: answer 001: its entry lost the name of a header",
			dropName("001", "Etag"), withETag, keep,
		},

		// (iv)
		{
			"two hashes swapped", "gate (iv), relations: answer 001: hashValue: a hash that is not HashFor of the account number beside it",
			swapHashes("001"), nil, keep,
		},
		{
			"two hashes swapped where only the account numbers name them",
			"gate (iv), relations: answer 001: hashValue: a hash that is not HashFor of the account number beside it", swapHashes("001"), onlyNumbers, keep,
		},
		{
			"a hash's fake changed in a path alone", "gate (iv), relations: the fakes of an account's hash are not one for each value",
			entry("004", func(e *schwabtest.Entry) {
				e.Request.Path = strings.Replace(e.Request.Path, fake(0).Hash, fake(1).Hash, 1)
			}), nil, keep,
		},
		{
			"an account number's fake changed in a path alone", "gate (iv), relations: the fakes of an account number are not one for each value",
			entry("007", func(e *schwabtest.Entry) {
				e.Request.Path = strings.Replace(e.Request.Path, fake(0).Number, fake(1).Number, 1)
			}), nil, keep,
		},
		{
			"an activity ID's fake changed in a path alone", "gate (iv), relations: the fakes of an activity ID are not one for each value",
			entry("003", func(e *schwabtest.Entry) {
				e.Request.Path = regexp.MustCompile(`[0-9]+$`).ReplaceAllString(e.Request.Path, "10000000099")
			}), nil, keep,
		},
		{
			"an order ID's fake changed in a path alone", "gate (iv), relations: the fakes of an order ID are not one for each value",
			entry("003", func(e *schwabtest.Entry) {
				e.Request.Path = regexp.MustCompile(`[0-9]+$`).ReplaceAllString(e.Request.Path, "1000000000099")
			}), withOrder, keep,
		},
		{
			"an id given two fakes", "gate (iv), relations: the fakes of an activity ID are not one for each value",
			func(t *testing.T, o *scrubOutput) {
				f := fileNamed(t, o, "003")
				f.body = regexp.MustCompile(`"activityId": [0-9]+`).ReplaceAll(f.body, []byte(`"activityId": 10000000099`))
			}, nil, keep,
		},
		{
			"an error's ID given two fakes", "gate (iv), relations: the fakes of an error's ID are not one for each value",
			replace("003", counterID(1), counterID(99)), twoErrors, keep,
		},
		{
			"a nickname given two fakes", "gate (iv), relations: the fakes of an account's nickname are not one for each value",
			replace("014", `"Account 1"`, `"Account 9"`), twoPrefs, keep,
		},
		{
			"two position IDs given one fake", "gate (iv), relations: the fakes of a position ID are not one for each value",
			func(t *testing.T, o *scrubOutput) {
				f := fileNamed(t, o, "004")
				ids := regexp.MustCompile(`"positionId": ([0-9]+)`).FindAllSubmatchIndex(f.body, -1)
				if len(ids) < 2 || bytes.Equal(f.body[ids[0][2]:ids[0][3]], f.body[ids[1][2]:ids[1][3]]) {
					t.Fatal("answer 004 holds no two position IDs")
				}
				f.body = slices.Concat(f.body[:ids[1][2]], f.body[ids[0][2]:ids[0][3]], f.body[ids[1][3]:])
			}, nil, keep,
		},
		{
			"a masked number off its account",
			"gate (iv), relations: answer 005: accounts.displayAcctId: a masked account number that does not end with the account number beside it",
			replace("005", `"...135"`, `"...136"`), nil, keep,
		},
		{
			"a masked number of another form", "gate (iv), relations: answer 005: accounts.displayAcctId: a masked account number of another form than the recording's",
			replace("005", `"...135"`, `"..135"`), nil, keep,
		},

		// (v)
		{
			"one mapping switched off", "gate (v), leaks: answer 005: accounts.nickName: an account's nickname of the recording",
			replace("005", `"Account 1"`, `"`+nickname+`"`), nil, keep,
		},
		{
			"one mapping switched off in a query", "gate (v), leaks: answer 002: the request's query: an account number of the recording",
			entry("002", func(e *schwabtest.Entry) {
				e.Request.Query = strings.Replace(e.Request.Query, fake(1).Number, b.numbers[1], 1)
			}), withQuery, keep,
		},
		{
			"one mapping switched off in a path where no id goes", "gate (v), leaks: answer 002: the request's path: an account's hash of the recording",
			entry("002", func(e *schwabtest.Entry) {
				e.Request.Path = strings.Replace(e.Request.Path, fake(1).Hash, b.hashes[1], 1)
			}), withPathHash, keep,
		},
		{
			"one mapping switched off in a header's value", "gate (v), leaks: answer 002: the header Vary: an account number of the recording",
			entry("002", func(e *schwabtest.Entry) { e.Response.Headers["Vary"] = []string{"Accept, x-account-" + b.numbers[2]} }), withHeader, keep,
		},
		{
			"one mapping switched off in a string written with an escape", "gate (v), leaks: answer 009: errors.id: an error's ID of the recording",
			func(t *testing.T, o *scrubOutput) {
				f := fileNamed(t, o, "009")
				escaped := fmt.Sprintf(`%cu%04x`, '\\', errorID[0]) + errorID[1:]
				f.body = regexp.MustCompile(`"id":"[^"]*"`).ReplaceAll(f.body, []byte(`"id":"`+escaped+`"`))
			}, nil, keep,
		},
		{
			"one mapping switched off in a page written with a character reference", "gate (v), leaks: answer 002: the page: an account's hash of the recording",
			replace("002", fake(0).Hash, "&#"+strconv.Itoa(int(b.hashes[0][0]))+";"+b.hashes[0][1:]), withPage, keep,
		},
		{
			"an identifier in a shape", "gate (v), leaks: shapes/002.txt: an account number of the recording",
			func(_ *testing.T, o *scrubOutput) {
				o.shapes["002"] += p.planted[idAccountNumber][fake(0).Number] + "\n"
			}, nil, refuse,
		},
		{
			"a local path", "gate (v), leaks: review.txt: a local path",
			func(_ *testing.T, o *scrubOutput) { o.review += filepath.Join(home, "raw") + "\n" }, nil, keep,
		},
		{
			"the recording's folder, outside the home folder", "gate (v), leaks: review.txt: a local path",
			func(_ *testing.T, o *scrubOutput) { o.review += raw + "\n" }, nil, keep,
		},
		{
			"the user name as a home's folder", "gate (v), leaks: review.txt: the user name",
			func(_ *testing.T, o *scrubOutput) { o.review += "/Users/zq9user/raw\n" }, nil, keep,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := tc.answers
			if answers == nil {
				answers = planted
			}
			out, err := scrubAnswers(t, answers, scrubConfig{
				amounts: tc.amounts, home: home, paths: []string{raw}, hook: func(o *scrubOutput) { tc.hook(t, o) },
			})
			if err == nil || out != nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("%v, want %s", err, tc.want)
			}
			for _, v := range slices.Concat(p.values(), b.p.values()) {
				if strings.Contains(err.Error(), v) {
					t.Errorf("the refusal holds a planted value: %v", err)
				}
			}
		})
	}
	if _, err := scrubAnswers(t, planted, scrubConfig{amounts: keep, home: home, hook: func(*scrubOutput) {}}); err != nil {
		t.Errorf("an output that the hook leaves alone: %v", err)
	}
	// A user name that Schwab's words or the scrubber's own spell is not
	// taken for the user's: amount, a key; max, in a header's max-age;
	// trader, in every path; json, in every file's name.
	if _, err := scrubAnswers(t, planted, scrubConfig{amounts: keep, home: filepath.Join(t.TempDir(), "amount")}); err != nil {
		t.Errorf("the user amount: %v", err)
	}
	hsts := b.accountNumbers()
	hsts.header.Set("Strict-Transport-Security", "max-age=31536000 ; includeSubDomains")
	hsts.header.Set("Cache-Control", "no-cache, no-store, max-age=0, must-revalidate")
	for _, user := range []string{"max", "trader", "json"} {
		if out, err := scrubAnswers(t, []testAnswer{hsts}, scrubConfig{amounts: keep, home: filepath.Join(t.TempDir(), user)}); err != nil || len(out.files) != 1 {
			t.Errorf("the user %s: %v", user, err)
		}
	}
}

// The user name counts in an output only as the folder of a home, in each
// system's form of one, and in any case where the file system takes names
// in any case.
func TestHoldsHomeOf(t *testing.T) {
	folds := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	for _, tc := range []struct {
		text, user string
		want       bool
	}{
		{"/home/zq9user", "zq9user", true},
		{"open /home/zq9user/raw: denied", "zq9user", true},
		{"see /Users/zq9user: gone", "zq9user", true},
		{`C:\Users\zq9user\AppData`, "zq9user", true},
		{"/home/zq9user.old", "zq9user", true},
		{"/home/zq9user2", "zq9user", false},
		{"/home/zq9user-old", "zq9user", false},
		{"/home/zq9user_old", "zq9user", false},
		{"/var/zq9user", "zq9user", false},
		{"by zq9user", "zq9user", false},
		{"no-cache, max-age=0", "max", false},
		{"GET /trader/v1/accounts", "trader", false},
		{"recorded/2026-11-08/001.json", "json", false},
		{"/users/ZQ9USER", "zq9user", folds},
	} {
		if got := holdsHomeOf(tc.text, tc.user); got != tc.want {
			t.Errorf("holdsHomeOf(%q, %q) = %v, want %v", tc.text, tc.user, got, tc.want)
		}
	}
}
