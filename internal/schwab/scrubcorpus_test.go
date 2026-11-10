package schwab

import (
	"bytes"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// jsonAnswer returns an answer of status with body, JSON, to path and
// query.
func jsonAnswer(path, query string, status int, body string) testAnswer {
	return testAnswer{path: path, query: query, status: status, header: http.Header{"Content-Type": {"application/json"}}, body: body}
}

// escapedDigits returns s, a JSON string's text, with each digit written as
// a \u escape, as JSON may write any character, so that a search of the
// bytes as they are does not find the digits.
func escapedDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if '0' <= r && r <= '9' {
			b.WriteString(escaped(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escaped returns r written as a \u escape of JSON.
func escaped(r rune) string {
	return fmt.Sprintf("%cu%04x", '\\', r)
}

// cashRow returns a transaction of account number, with the activity ID
// id: cash of 1.25 received, its one leg the cash, with changes made, each
// a key and its raw JSON, as withKeys makes them.
func cashRow(number, id string, changes ...string) string {
	row := `{"activityId":` + id + `,"time":"2024-04-04T15:56:07+0000","accountNumber":"` + number + `",` +
		`"type":"DIVIDEND_OR_INTEREST","status":"VALID","subAccount":"CASH","tradeDate":"2024-04-04T04:00:00+0000","netAmount":1.25,` +
		`"transferItems":[{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency",` +
		`"closingPrice":0.0},"amount":1.25,"cost":0.0}]}`
	return withKeys(row, changes...)
}

// cashLeg returns a transaction's leg of cash, of amount, with the
// instrument's description.
func cashLeg(amount, description string) string {
	return `[{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"` + description +
		`","closingPrice":0.0},"amount":` + amount + `,"cost":0.0}]`
}

// preferences returns the user's preferences of accounts, each an account
// as preference gives it, with no streamer and no offers.
func preferences(accounts ...string) string {
	return `{"accounts":[` + strings.Join(accounts, ",") + `],"streamerInfo":[],"offers":[]}`
}

// preference returns an account of the user's preferences: of number, with
// nickname and display, its masked number.
func preference(number, nickname, display string) string {
	return `{"accountNumber":"` + number + `","primaryAccount":true,"type":"BROKERAGE","nickName":"` + nickname + `",` +
		`"displayAcctId":"` + display + `","autoPositionEffect":false,"accountColor":"Green"}`
}

// A corpusBase is what the cases of the planted corpus share: accounts,
// hashes and an id planted by a planter, none a fixture's value.
type corpusBase struct {
	p               *planter
	numbers, hashes []string
}

// newCorpusBase returns n accounts planted from seed.
func newCorpusBase(t *testing.T, seed uint64, n int) corpusBase {
	t.Helper()
	b := corpusBase{p: newPlanter(t, seed, apiFixtures(t))}
	for i := range n {
		b.numbers = append(b.numbers, b.p.value(idAccountNumber, fmt.Sprint("account ", i)))
		b.hashes = append(b.hashes, b.p.value(idHash, fmt.Sprint("hash ", i)))
	}
	return b
}

// accountNumbers returns the account numbers' answer that lists b's
// accounts.
func (b corpusBase) accountNumbers() testAnswer {
	var elems []string
	for i, n := range b.numbers {
		elems = append(elems, `{"accountNumber":"`+n+`","hashValue":"`+b.hashes[i]+`"}`)
	}
	return jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, "["+strings.Join(elems, ",")+"]")
}

// id returns an activity ID of 11 digits, planted.
func (b corpusBase) id() string {
	return b.p.draw(11, plantDigits, plantDigits)
}

// transactions returns the answer of the first account's transactions that
// lists rows.
func (b corpusBase) transactions(rows ...string) testAnswer {
	return jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/transactions", "types=TRADE", 200, "["+strings.Join(rows, ",")+"]")
}

// leftOut returns why review.txt says that the answer called name was left
// out, after its route, or "" if it was not.
func leftOut(o *scrubOutput, name string) string {
	for line := range strings.SplitSeq(o.review, "\n") {
		if rest, ok := strings.CutPrefix(line, name+": "); ok {
			_, why, _ := strings.Cut(rest, ": ")
			return why
		}
	}
	return ""
}

// The planted corpus beyond F reaches the routes by which a value of the
// recording could leak that F's own values cannot reach, one case each:
// each planted value is mapped, so that the output holds it nowhere,
// written or decoded, but as a value of an enum's list, Schwab's own word;
// or its file is left out with a reason that holds no value. review.txt
// lists each identifier replaced inside a value kept otherwise, with the
// paths' identifiers counted among those mapped.
func TestScrubberPlantedCorpus(t *testing.T) {
	keep, scale := amountsPolicy{mode: "keep"}, amountsPolicy{mode: "scale", k: 20}
	akamai := string(readTestdata(t, "errors/akamai_403.html"))
	asked := "http&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;v1&#47;oauth&#47;token"
	reference := strings.ReplaceAll(schwabtest.AkamaiReference, ".", "&#46;")
	fake := func(i int) string { return schwabtest.FixtureAccounts[i].Number }
	for _, tc := range []struct {
		name     string
		amounts  amountsPolicy
		accounts int
		// answers returns the answers after the account numbers', which is
		// 001, and the values planted besides the accounts'.
		answers func(b corpusBase) ([]testAnswer, []string)
		// written holds the answers written, by name, each with what its
		// body must hold; leftOut those left out, with a word of why; and
		// review the lines that review.txt must hold.
		written map[string]string
		leftOut map[string]string
		review  []string
		check   func(t *testing.T, b corpusBase, o *scrubOutput)
	}{
		{
			// A number where the records take a string is a problem, which
			// TestCheckFixtures fails on; TestScrubberMapsInKind shows the
			// number mapped as a number.
			name: "an accountNumber as a JSON number", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "accountNumber", b.numbers[0]))}, []string{id}
			},
			leftOut: map[string]string{"002": "problem: accountNumber: Schwab sent a number where a string was expected"},
		},
		{
			name: "an account number inside a description", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "description", `"Journal to `+b.numbers[1]+`"`))}, []string{id}
			},
			written: map[string]string{"002": `"description":"DESCRIPTION 1"`},
		},
		{
			name: "an account number inside an error message", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				return []testAnswer{jsonAnswer("/trader/v1/accounts/"+b.numbers[2], "", 400, `{"message":"Invalid account number `+b.numbers[2]+`"}`)}, nil
			},
			written: map[string]string{"002": `{"message":"MESSAGE 1"}`},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if p := fileNamed(t, o, "002").entry.Request.Path; p != "/trader/v1/accounts/"+fake(2) {
					t.Errorf("the path %s, want account 3's fake number", p)
				}
			},
		},
		{
			// Its fake would be an account number inside another value,
			// which no committed fixture may hold.
			name: "an account number inside a value that is kept", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "transferItems", cashLeg("1.25", "TRUST "+b.numbers[1])))}, []string{id}
			},
			leftOut: map[string]string{"002": "transferItems.instrument.description: holds an account number, which TestTestdataAccountNumbersStandAlone refuses"},
		},
		{
			name: "a nickname inside a value that is kept", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				pref := preferences(preference(b.numbers[0], "Kip", "..."+b.numbers[0][5:]))
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, pref),
					b.transactions(cashRow(b.numbers[0], id, "transferItems", cashLeg("1.25", "TRUST Kip"))),
				}, []string{"Kip", id}
			},
			written: map[string]string{"003": `"description":"TRUST Account 1"`},
			review:  []string{"003.json: transferItems.instrument.description: 1 of an account's nickname"},
		},
		{
			// Pass two and gate (iii) find a nickname in the text unescaped,
			// where an escape next to it leaves it a whole token, and keep
			// the escapes as they were.
			name: "a nickname between escapes inside a value that is kept", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				pref := preferences(preference(b.numbers[0], "Kip", "..."+b.numbers[0][5:]))
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, pref),
					b.transactions(cashRow(b.numbers[0], id, "transferItems", cashLeg("1.25", "TRUST"+escaped('&')+"Kip"+escaped(' ')+"FUND"))),
				}, []string{"Kip", id}
			},
			written: map[string]string{"003": `"description":"TRUST` + escaped('&') + `Account 1` + escaped(' ') + `FUND"`},
			review:  []string{"003.json: transferItems.instrument.description: 1 of an account's nickname"},
		},
		{
			// A value on its enum's closed list is Schwab's word, whatever a
			// nickname is spelled as, so it is kept, and is no leak.
			name: "a nickname that is a value of an enum", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, preferences(preference(b.numbers[0], "CASH", "..."+b.numbers[0][5:]))),
					b.transactions(cashRow(b.numbers[0], id)),
				}, []string{"CASH", id}
			},
			written: map[string]string{"002": `"nickName":"Account 1"`, "003": `"subAccount":"CASH"`},
		},
		{
			// A symbol is kept only as a symbol, which its nickname's fake is
			// not, so its file is left out, and the rest written.
			name: "a nickname that a symbol is", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				leg := `[{"instrument":{"assetType":"EQUITY","status":"ACTIVE","symbol":"KIP"},"amount":1.25,"cost":0.0}]`
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, preferences(preference(b.numbers[0], "KIP", "..."+b.numbers[0][5:]))),
					b.transactions(cashRow(b.numbers[0], id, "transferItems", leg)),
				}, []string{"KIP", id}
			},
			written: map[string]string{"001": `"hashValue"`, "002": `"nickName":"Account 1"`},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				// Pass two leaves it out itself, before the value hygiene
				// tests would.
				if why := leftOut(o, "003"); why != "transferItems.instrument.symbol: not a symbol" {
					t.Errorf("answer 003 is left out for %q, want for its symbol", why)
				}
			},
		},
		{
			// Kept, its digits replaced, the wording would hold an account
			// number, so it is a placeholder.
			name: "an account number inside Schwab's wording", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				quoted := `{"message":"'2024-04-04T15:54:14.` + b.numbers[1] + `Z' is not a valid value for startDate"}`
				return []testAnswer{jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/transactions", "", 400, quoted)}, nil
			},
			written: map[string]string{"002": `{"message":"MESSAGE 1"}`},
		},
		{
			// An account's type is one or two digits, so a longer number is
			// not Schwab's wording.
			name: "a long number where Schwab's wording has an account's type", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id, long := b.id(), b.p.draw(12, plantDigits, plantDigits)
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "description", `"TRF FDS TO TYPE `+long+`"`))}, []string{id, long}
			},
			written: map[string]string{"002": `"description":"DESCRIPTION 1"`},
		},
		{
			name: "an account number in a header's value", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				a := jsonAnswer("/trader/v1/accounts/"+b.hashes[2], "", 400, `{"message":"Invalid account number"}`)
				a.header.Set("Vary", "Accept, x-account-"+b.numbers[2])
				return []testAnswer{a}, nil
			},
			written: map[string]string{"002": `{"message":"Invalid account number"}`},
			review:  []string{"002.json: the header Vary: 1 of an account number"},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if v := fileNamed(t, o, "002").entry.Response.Headers["Vary"]; len(v) != 1 || v[0] != "Accept, x-account-"+fake(2) {
					t.Errorf("Vary is %q, want account 3's fake in place of its number", v)
				}
			},
		},
		{
			name: "an account number that a value kept as a number is", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				leg := `[{"instrument":{"assetType":"CURRENCY","symbol":"CURRENCY_USD","instrumentId":` + b.numbers[1] + `},"amount":1.25,"cost":0.0}]`
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "transferItems", leg))}, []string{id}
			},
			leftOut: map[string]string{"002": "transferItems.instrument.instrumentId: an account number that the scrubber could not replace"},
		},
		{
			name: "an account number in a query", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				a := b.transactions(cashRow(b.numbers[0], id))
				a.query += "&symbol=" + b.numbers[1]
				return []testAnswer{a}, []string{id}
			},
			review: []string{
				"002.json: GET /trader/v1/accounts/{hash}/transactions?types&symbol, 200 (transactions); " +
					"mapped 1 of an account number, 1 of an account's hash, 1 of an activity ID",
				"002.json: the request's query: 1 of an account number",
			},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if q := fileNamed(t, o, "002").entry.Request.Query; q != "types=TRADE&symbol="+fake(1) {
					t.Errorf("the query %s, want account 2's fake in place of its number", q)
				}
			},
		},
		{
			// Schwab's wording takes an account's type of two digits, which
			// an id of two digits can be; an escape next to it, here of the
			// space, leaves it a whole token.
			name: "an id inside Schwab's wording", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id, other := b.id(), b.id()
				return []testAnswer{b.transactions(
					cashRow(b.numbers[0], id, "positionId", "47"),
					cashRow(b.numbers[0], other, "description", `"TRF FDS TO TYPE`+escaped(' ')+`47"`),
				)}, []string{id, other}
			},
			written: map[string]string{"002": `"description":"TRF FDS TO TYPE` + escaped(' ') + `10"`},
			review:  []string{"002.json: description: 1 of a position ID"},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if body := fileNamed(t, o, "002").body; !bytes.Contains(body, []byte(`"positionId":10}`)) {
					t.Errorf("the position ID is not 10, the fake that the wording holds: %s", body)
				}
			},
		},
		{
			name: "an account number escaped in a query", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				a := b.transactions(cashRow(b.numbers[0], id))
				var q strings.Builder
				for _, c := range b.numbers[2] {
					fmt.Fprintf(&q, "%%%02X", c)
				}
				a.query += "&symbol=" + q.String()
				return []testAnswer{a}, []string{id}
			},
			leftOut: map[string]string{"002": "the request's query: an account number that the scrubber could not replace"},
		},
		{
			name: "an account number written with escapes, at its key and in free text", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				row := cashRow(escapedDigits(b.numbers[0]), id, "description", `"Wire to `+escapedDigits(b.numbers[1])+`"`)
				return []testAnswer{b.transactions(row)}, []string{id}
			},
			written: map[string]string{"002": `"accountNumber":"` + fake(0) + `","type"`},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if !bytes.Contains(fileNamed(t, o, "002").body, []byte(`"DESCRIPTION 1"`)) {
					t.Error("the description that holds an escaped account number is not a placeholder")
				}
			},
		},
		{
			name: "an account number written with escapes in a value that is kept", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "transferItems", cashLeg("1.25", "FUND "+escapedDigits(b.numbers[1]))))}, []string{id}
			},
			leftOut: map[string]string{"002": "transferItems.instrument.description: holds an account number written with escapes"},
		},
		{
			name: "an Akamai page with a hash in its quoted address and a Reference written with &#46;", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				planted := "47&#46;3c5d7e9f&#46;1792539999&#46;4a5b6c"
				page := strings.ReplaceAll(akamai, reference, planted)
				page = strings.Replace(page, asked, "https&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;trader&#47;v1&#47;accounts&#47;"+b.hashes[1]+"&#47;transactions", 1)
				a := testAnswer{
					path: "/trader/v1/accounts/" + b.hashes[1] + "/transactions", status: 403,
					header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, body: page,
				}
				return []testAnswer{a}, []string{"47.3c5d7e9f.1792539999.4a5b6c"}
			},
			written: map[string]string{"002": "accounts&#47;" + schwabtest.FixtureAccounts[1].Hash + "&#47;transactions"},
			review:  []string{"002.html: the address quoted: 1 of an account's hash"},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if n := strings.Count(string(fileNamed(t, o, "002").body), reference); n != 2 {
					t.Errorf("the page holds AkamaiReference, its dots written &#46;, %d times, want 2", n)
				}
			},
		},
		{
			// A dot after it ends a sentence, as errors.go reads it, and is
			// kept after the fake.
			name: "an Akamai Reference that a dot ends", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				a := testAnswer{
					path: "/trader/v1/accounts/" + b.hashes[1], status: 403, header: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
					body: strings.ReplaceAll(akamai, reference, "47&#46;3c5d7e9f&#46;1792539999&#46;4a5b6c&#46;"),
				}
				return []testAnswer{a}, []string{"47.3c5d7e9f.1792539999.4a5b6c"}
			},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if n := strings.Count(string(fileNamed(t, o, "002").body), reference+"&#46;"); n != 2 {
					t.Errorf("the page holds AkamaiReference and its dot, written &#46;, %d times, want 2", n)
				}
			},
		},
		{
			name: "a transactions/{id} path with no body", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/transactions/"+id, "", 404, "")}, []string{id}
			},
			// Its fake is no row's, which the value hygiene tests refuse.
			leftOut: map[string]string{"002": "an activity ID that is no row's"},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				for _, v := range o.ids.of(idActivity) {
					if v.fake == "" {
						t.Error("the id that only a path holds has no fake")
					}
				}
				if len(o.ids.of(idActivity)) != 1 {
					t.Errorf("%d activity IDs, want the path's", len(o.ids.of(idActivity)))
				}
			},
		},
		{
			name: "an orders/{id} path whose id no body holds", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				order := b.p.draw(13, plantDigits, plantDigits)
				return []testAnswer{jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/orders/"+order, "", 404, `{"message":"Order not found"}`)}, []string{order}
			},
			// Its fake is no row's, which the value hygiene tests refuse.
			leftOut: map[string]string{"002": "the request's path: an order ID that is no row's"},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if ids := o.ids.of(idOrder); len(ids) != 1 || ids[0].fake == "" {
					t.Errorf("%d order IDs, want the path's, with its fake", len(ids))
				}
			},
		},
		{
			name: "an orders/{id} path whose id a row holds", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id, order := b.id(), b.p.draw(13, plantDigits, plantDigits)
				return []testAnswer{
					b.transactions(cashRow(b.numbers[0], id, "orderId", order)),
					jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/orders/"+order, "", 404, `{"message":"Order not found"}`),
				}, []string{id, order}
			},
			written: map[string]string{"003": `{"message":"MESSAGE 1"}`},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				ids := o.ids.of(idOrder)
				path := fileNamed(t, o, "003").entry.Request.Path
				if len(ids) != 1 || !regexp.MustCompile(`"orderId":`+ids[0].fake+`[,}]`).Match(fileNamed(t, o, "002").body) ||
					path != "/trader/v1/accounts/"+schwabtest.FixtureAccounts[0].Hash+"/orders/"+ids[0].fake {
					t.Errorf("the order's path %s, want its row's order ID's fake", path)
				}
			},
		},
		{
			name: "an id in a path where no id goes", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.p.draw(12, plantDigits, plantDigits)
				return []testAnswer{jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/positions/"+id, "", 404, `{"message":"Not Found"}`)}, []string{id}
			},
			leftOut: map[string]string{"002": "the request's path holds " + string(idPathID) + ", which has no fake"},
		},
		{
			// The Extras probe asks for both, which no answer holds.
			name: "UnknownHash and UnknownActivityID in paths", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				return []testAnswer{
					jsonAnswer("/trader/v1/accounts/"+schwabtest.UnknownHash, "", 400, `{"message":"Invalid account number"}`),
					jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/transactions/99999999999", "", 404, ""),
				}, nil
			},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				for name, want := range map[string]string{
					"002": "/trader/v1/accounts/" + schwabtest.UnknownHash,
					"003": "/trader/v1/accounts/" + schwabtest.FixtureAccounts[0].Hash + "/transactions/99999999999",
				} {
					if path := fileNamed(t, o, name).entry.Request.Path; path != want {
						t.Errorf("answer %s's path %s, want %s", name, path, want)
					}
				}
			},
		},
		{
			name: "a 401 body with errors[].id", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := strings.ToLower(b.p.draw(8, plantHex, plantHex)) + "-4a5b-4c6d-8e7f-" + strings.ToLower(b.p.draw(12, plantHex, plantHex))
				body := `{"errors":[{"id":"` + id + `","status":401,"title":"Unauthorized","detail":"Client not authorized"}]}`
				return []testAnswer{jsonAnswer("/trader/v1/userPreference", "", 401, body)}, []string{id}
			},
			written: map[string]string{"002": `"id":"` + counterID(1) + `"`},
		},
		{
			name: "a nickname that is a common word", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				pref := preferences(preference(b.numbers[0], "Funds", "..."+b.numbers[0][5:]))
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, pref),
					b.transactions(cashRow(b.numbers[0], id, "description", `"Funds Received"`)),
				}, []string{"Funds", id}
			},
			written: map[string]string{"002": `"nickName":"Account 1"`, "003": `"description":"DESCRIPTION 1"`},
		},
		{
			name: "a nickname in another case inside a value that is kept", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id, other := b.id(), b.id()
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, preferences(preference(b.numbers[0], "Rainy Day Fund", "..."+b.numbers[0][5:]))),
					b.transactions(
						cashRow(b.numbers[0], id, "transferItems", cashLeg("1.25", "RAINY DAY FUND")),
						cashRow(b.numbers[0], other, "transferItems", cashLeg("1.25", "rainy day fund"))),
				}, []string{"Rainy Day Fund", id, other}
			},
			written: map[string]string{"003": `"description":"ACCOUNT 1"`},
			review:  []string{"003.json: transferItems.instrument.description: 2 of an account's nickname"},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if body := fileNamed(t, o, "003").body; !bytes.Contains(body, []byte(`"description":"account 1"`)) {
					t.Errorf("the nickname in lower case is not replaced in lower case: %s", body)
				}
			},
		},
		{
			// A query unescapes a + as a space, which the checks for leaks
			// do too.
			name: "a nickname in a query, a + for its space", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				a := b.transactions(cashRow(b.numbers[0], id))
				a.query += "&n=Zebulon+Fund"
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, preferences(preference(b.numbers[0], "Zebulon Fund", "..."+b.numbers[0][5:]))),
					a,
				}, []string{"Zebulon Fund", id}
			},
			leftOut: map[string]string{"003": "the request's query: an account's nickname that the scrubber could not replace"},
		},
		{
			// In another case, a word of Schwab's own is far likelier
			// Schwab's word than the nickname.
			name: "a nickname that is Schwab's word in another case", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				return []testAnswer{jsonAnswer("/trader/v1/userPreference", "", 200, preferences(preference(b.numbers[0], "Brokerage", "..."+b.numbers[0][5:])))}, nil
			},
			written: map[string]string{"002": `"type":"BROKERAGE","nickName":"Account 1"`},
		},
		{
			// Its fake would be each account's, so it has none.
			name: "a masked number beside two accounts", amounts: keep, accounts: 1,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				twin, display := b.p.draw(5, plantDigits, plantDigits)+b.numbers[0][5:], "..."+b.numbers[0][5:]
				return []testAnswer{
					jsonAnswer("/trader/v1/userPreference", "", 200, preferences(preference(b.numbers[0], "Kip", display), preference(twin, "Kip", display))),
					b.transactions(cashRow(b.numbers[0], b.id(), "transferItems", cashLeg("1.25", "FUND "+display))),
				}, []string{twin}
			},
			written: map[string]string{"002": `"displayAcctId":"...` + fake(0)[5:] + `","autoPositionEffect":false,"accountColor":"Green"},{"accountNumber":"` + fake(1)},
			leftOut: map[string]string{"003": "transferItems.instrument.description: holds " + string(idDisplay) + ", which has no fake"},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if body := fileNamed(t, o, "002").body; !bytes.Contains(body, []byte(`"displayAcctId":"...`+fake(1)[5:]+`"`)) {
					t.Errorf("the second account's masked number is not of its fake: %s", body)
				}
			},
		},
		{
			name: "nulls at keys that are mapped and scaled", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				account := `{"securitiesAccount":{"type":"CASH","accountNumber":"` + b.numbers[0] + `","roundTrips":0,"isDayTrader":false,` +
					`"isClosingOnlyRestricted":false,"pfcbFlag":false,"initialBalances":{"cashBalance":null},"currentBalances":{},"projectedBalances":{}}}`
				return []testAnswer{
					b.transactions(cashRow(b.numbers[0], id, "orderId", "null")),
					jsonAnswer("/trader/v1/accounts/"+b.hashes[0], "", 200, account),
				}, []string{id}
			},
			written: map[string]string{"002": `"orderId":null`, "003": `"initialBalances":{"cashBalance":null}`},
		},
		{
			name: "an exponent literal at a key that is scaled", amounts: scale, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "netAmount", "1.0E-4", "transferItems", cashLeg("1.0E-4", "USD currency")))}, []string{id}
			},
			leftOut: map[string]string{"002": "netAmount: money written with an exponent"},
		},
		{
			name: "a negative zero at keys that are scaled", amounts: scale, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{b.transactions(cashRow(b.numbers[0], id, "netAmount", "-0.0", "transferItems", cashLeg("-0.0", "USD currency")))}, []string{id}
			},
			written: map[string]string{"002": `"netAmount":-0.0,`},
			check: func(t *testing.T, _ corpusBase, o *scrubOutput) {
				if !bytes.Contains(fileNamed(t, o, "002").body, []byte(`"amount":-0.0,"cost":0.0`)) {
					t.Errorf("the leg's zeros are not kept byte for byte: %s", fileNamed(t, o, "002").body)
				}
			},
		},
		{
			name: "a 4th and a 5th account", amounts: keep, accounts: 5,
			answers: func(corpusBase) ([]testAnswer, []string) { return nil, nil },
			written: map[string]string{"001": `{"accountNumber":"` + schwabtest.SpareAccountNumbers[1] + `","hashValue":"` +
				schwabtest.HashFor(schwabtest.SpareAccountNumbers[1]) + `"}]`},
		},
		{
			name: "a hash in a path that no answer names", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				other := b.p.value(idHash, "another")
				return []testAnswer{jsonAnswer("/trader/v1/accounts/"+other+"/transactions", "", 200, "[]")}, []string{other}
			},
			leftOut: map[string]string{"002": "the request's path holds " + string(idPathAccount)},
		},
		{
			name: "an answer of orders, of no kind the policy covers", amounts: keep, accounts: 3,
			answers: func(b corpusBase) ([]testAnswer, []string) {
				id := b.id()
				return []testAnswer{jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/orders", "", 200, `[{"orderId":`+id+`,"accountNumber":`+b.numbers[0]+`}]`)}, []string{id}
			},
			leftOut: map[string]string{"002": "an answer of no kind"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newCorpusBase(t, 11, tc.accounts)
			more, planted := tc.answers(b)
			out, err := scrubAnswers(t, append([]testAnswer{b.accountNumbers()}, more...), scrubConfig{amounts: tc.amounts})
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range tc.written {
				if body := fileNamed(t, out, name).body; !bytes.Contains(body, []byte(want)) {
					t.Errorf("answer %s holds no %s: %s", name, want, body)
				}
			}
			for name, want := range tc.leftOut {
				if why := leftOut(out, name); !strings.Contains(why, want) {
					t.Errorf("answer %s is left out for %q, want %q", name, why, want)
				}
			}
			for _, line := range tc.review {
				if !strings.Contains(out.review, "\n"+line+"\n") {
					t.Errorf("review.txt holds no line %q:\n%s", line, out.review)
				}
			}
			if leaks := leaksOf(out, slices.Concat(planted, b.numbers, b.hashes)); len(leaks) > 0 {
				t.Errorf("the output holds %d planted values", len(leaks))
			}
			if tc.check != nil {
				tc.check(t, b, out)
			}
		})
	}
}

// A recording that the scrubber cannot keep apart from its fakes is
// refused whole, with a reason that holds no value: more accounts than
// fakes, a hash of two accounts, an account of two hashes, a fixed fake that
// is a value of the recording, as an account's, a hash's or the streamer's
// ID's can be, and an answer whose time sent is not known, which leaves the
// recording's day unknown.
func TestScrubberRefusesRecordings(t *testing.T) {
	b := newCorpusBase(t, 13, 24)
	for _, tc := range []struct {
		name    string
		answers []testAnswer
		want    string
	}{
		{"24 accounts", []testAnswer{b.accountNumbers()}, "24 accounts, more than the 23 fake account numbers"},
		{"a hash of two accounts", []testAnswer{
			jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"accountNumber":"`+b.numbers[0]+`","hashValue":"`+b.hashes[0]+`"}]`),
			jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"accountNumber":"`+b.numbers[1]+`","hashValue":"`+b.hashes[0]+`"}]`),
		}, "one hash to two accounts"},
		{"an account of two hashes", []testAnswer{
			jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"accountNumber":"`+b.numbers[0]+`","hashValue":"`+b.hashes[0]+`"}]`),
			jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"accountNumber":"`+b.numbers[0]+`","hashValue":"`+b.hashes[1]+`"}]`),
		}, "an account two hashes"},
		{"a hash that is the fake hash of its account", []testAnswer{
			jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"accountNumber":"`+b.numbers[0]+`","hashValue":"`+schwabtest.FixtureAccounts[0].Hash+`"}]`),
		}, "the fake of an account's hash could be taken for an identifier of the recording"},
		{"a streamer's ID that is its fake", []testAnswer{
			jsonAnswer("/trader/v1/userPreference", "", 200, `{"accounts":[],"streamerInfo":[{"streamerSocketUrl":"wss://streamer-api.schwab.com/ws",`+
				`"schwabClientCustomerId":"`+schwabtest.StreamerCustomerID+`","schwabClientCorrelId":"`+b.p.value(idCorrelation, "c")+`",`+
				`"schwabClientChannel":"N9","schwabClientFunctionId":"APIAPP"}],"offers":[]}`),
		}, "the fake of the streamer's customer ID could be taken for an identifier of the recording"},
		{"an answer without its time sent", nil, "answer 001 has no time sent"},
		{"an account whose fake is in the recording", []testAnswer{
			jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"accountNumber":"`+b.numbers[0]+`","hashValue":"`+b.hashes[0]+`"},`+
				`{"accountNumber":"`+schwabtest.FixtureAccounts[0].Number+`","hashValue":"`+b.hashes[1]+`"}]`),
		}, "the fake of account 1 could be taken for an identifier of the recording"},
	} {
		var out *scrubOutput
		var err error
		if tc.answers == nil {
			unsent := []recordedAnswer{{name: "001", body: []byte(" \n")}}
			unsent[0].meta.Request.Method, unsent[0].meta.Request.Path, unsent[0].meta.Response.Status = "GET", "/marketdata/v1/quotes", 429
			out, err = scrubRecording(t, unsent, scrubConfig{amounts: amountsPolicy{mode: "refuse"}})
		} else {
			out, err = scrubAnswers(t, tc.answers, scrubConfig{})
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error that says %q", tc.name, err, tc.want)
			continue
		}
		if out != nil || slices.ContainsFunc(slices.Concat(b.numbers, b.hashes), func(v string) bool { return strings.Contains(err.Error(), v) }) {
			t.Errorf("%s: an output, or a value in the error: %v", tc.name, err)
		}
	}
}

// A masked number whose fake could be taken for one of the recording's
// values, as when an account ends with the digits that its own fake or
// another account's ends with, leaves out each answer that holds it, with
// why, and the rest of the recording is written. The fakes follow the
// accounts' places, so every recording of such a login would meet it, and
// refusing the recording whole would refuse them all.
func TestScrubberLeavesOutAMaskedNumberLikeAFake(t *testing.T) {
	b := newCorpusBase(t, 13, 2)
	for _, tc := range []struct {
		name    string
		numbers []string // the accounts, whose fakes' masked numbers are ...135 and ...398
	}{
		{"its own fake's digits", []string{"22222135", "33333777"}},
		{"another's fake's digits", []string{"33333777", "22222135"}},
	} {
		numbers := `[{"accountNumber":"` + tc.numbers[0] + `","hashValue":"` + b.hashes[0] + `"},` +
			`{"accountNumber":"` + tc.numbers[1] + `","hashValue":"` + b.hashes[1] + `"}]`
		answers := []testAnswer{
			jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, numbers),
			jsonAnswer("/trader/v1/userPreference", "", 200, preferences(
				preference(tc.numbers[0], "Kip", "..."+tc.numbers[0][5:]), preference(tc.numbers[1], "Lou", "..."+tc.numbers[1][5:]))),
		}
		out, err := scrubAnswers(t, answers, scrubConfig{})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if why, want := leftOut(out, "002"), "accounts.displayAcctId: a masked account number whose fake could be taken for an identifier of the recording"; why != want {
			t.Errorf("%s: the preferences are left out for %q, want %q", tc.name, why, want)
		}
		if len(out.files) != 1 || out.files[0].from.raw.name != "001" {
			t.Errorf("%s: wrote %d files, want the account numbers' answer alone", tc.name, len(out.files))
		}
		if leaks := leaksOf(out, slices.Concat(tc.numbers, b.hashes, []string{"Kip", "Lou", "...135", "...777"})); len(leaks) > 0 {
			t.Errorf("%s: the output holds %q", tc.name, leaks)
		}
	}
}

// An answer that the policy does not take is left out, with a reason that
// names where and holds no value, and the rest of the recording is
// written: an answer cut short, a key that no rule covers, an account's
// answer that is not JSON, a value outside its grammar, money that is not a
// number, an id that is not one, a masked number that does not end with its
// account's digits or holds others, a hash with no account number beside
// it or that is not a string, free text that is not a string, a key that
// is an identifier, which review.txt shows as {…}, a page whose text is not
// read as html.UnescapeString reads it or that quotes an address of
// another's, an answer that checkAnswer finds a key in another case in, and
// one that the value hygiene tests would fail on, such as for eight digits
// in a kept value, and then any that they would fail on once it is left
// out, such as the path to its row. review.txt says what was mapped at keys
// in another case, the placeholders, the same for the same text as sent,
// so that copies that differ in their escapes differ, and the rows of W-8
// withholding, in any case.
func TestScrubberLeavesOut(t *testing.T) {
	b := newCorpusBase(t, 17, 3)
	id := b.id
	asked := "http&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;v1&#47;oauth&#47;token"
	akamai := strings.ReplaceAll(string(readTestdata(t, "errors/akamai_403.html")),
		strings.ReplaceAll(schwabtest.AkamaiReference, ".", "&#46;"), "47&#46;3c5d7e9f&#46;1792539999&#46;4a5b6c")
	pref := func(display string) string {
		return `{"accounts":[{"accountNumber":"` + b.numbers[0] + `","primaryAccount":true,"type":"BROKERAGE","NickName":"Kip",` +
			`"displayAcctId":"` + display + `","autoPositionEffect":false,"accountColor":"Green"}],"streamerInfo":[],"offers":[]}`
	}
	cut := b.transactions(cashRow(b.numbers[0], id()))
	cut.cut = true
	row := id()
	answers := []testAnswer{
		b.accountNumbers(), // 001
		cut,                // 002
		b.transactions(cashRow(b.numbers[0], id(), "zzNote", `"x"`)),                                                           // 003
		{path: "/trader/v1/accounts/" + b.hashes[0], status: 200, header: http.Header{}, body: "<html><body>Hi</body></html>"}, // 004
		b.transactions(cashRow(b.numbers[0], id(), "type", `"GIFT"`)),                                                          // 005
		b.transactions(cashRow(b.numbers[0], id(), "netAmount", `"1.25"`)),                                                     // 006
		b.transactions(cashRow(b.numbers[0], `"12a"`)),                                                                         // 007
		jsonAnswer("/trader/v1/userPreference", "", 200, pref("...1")),                                                         // 008: no planted number ends in 1
		jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"hashValue":"`+b.hashes[1]+`"}]`),                         // 009
		jsonAnswer("/trader/v1/userPreference", "", 200, pref("..."+b.numbers[0][5:])),                                         // 010
		b.transactions( // 011
			cashRow(b.numbers[0], id(), "description", `"Wire from ACME BANK"`),
			cashRow(b.numbers[0], id(), "description", `"Wire from ACME BANK"`),
			cashRow(b.numbers[0], id(), "description", `"Gift from a friend"`),
			cashRow(b.numbers[0], id(), "description", `"W-8 Withholding~BND"`),
			cashRow(b.numbers[0], id(), "description", `"Ordinary Dividend~BND"`),
			cashRow(b.numbers[0], id(), "description", `"Gift from a fri\u0065nd"`),
			cashRow(b.numbers[0], id(), "description", `"W-8 WITHHOLDING~XYZ"`)),
		b.transactions(cashRow(b.numbers[0], id(), b.numbers[1], "1")),                                                    // 012, its key an account number
		jsonAnswer("/trader/v1/userPreference", "", 200, pref("1..."+b.numbers[0][5:])),                                   // 013
		jsonAnswer("/trader/v1/accounts/accountNumbers", "", 200, `[{"accountNumber":"`+b.numbers[0]+`","hashValue":7}]`), // 014
		b.transactions(cashRow(b.numbers[0], id(), "description", "5")),                                                   // 015
		{path: "/trader/v1/accounts/" + b.hashes[0], status: 403, header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, // 016
			body: strings.Replace(akamai, asked, asked+"&amp x", 1)},
		{path: "/trader/v1/accounts/" + b.hashes[0], status: 403, header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, // 017
			body: strings.Replace(akamai, asked, "https&#58;&#47;&#47;example&#46;com&#47;", 1)},
		jsonAnswer("/trader/v1/userPreference", "", 200, strings.ReplaceAll(pref("...223456789"), b.numbers[0], "223456789")), // 018
		b.transactions(cashRow(b.numbers[0], row, "transferItems", cashLeg("1.25", "FUND 98765410"))),                         // 019
		jsonAnswer("/trader/v1/accounts/"+b.hashes[0]+"/transactions/"+row, "", 404, ""),                                      // 020: 019's row
	}
	out, err := scrubAnswers(t, answers, scrubConfig{amounts: amountsPolicy{mode: "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"002": "its body was cut short",
		"003": "zzNote: no rule of the scrub policy for transactions covers it",
		"004": "an answer of account that is not JSON",
		"005": "type: not a value of its closed list",
		"006": "netAmount: money that is not a number",
		"007": "activityId: an activity ID that is neither a whole number nor a string of digits",
		"008": "accounts.displayAcctId: a masked account number that does not end with digits of the account number beside it",
		"009": "hashValue: a hash with no account number beside it",
		"010": "checkAnswer finds what TestCheckFixtures fails on: unknown key: accounts.NickName: nickName in another case",
		"012": "{…}: no rule of the scrub policy for transactions covers it",
		"013": "accounts.displayAcctId: a masked account number that holds other digits",
		"014": "hashValue: a hash that is not a string",
		"015": "description: a description that is not a string",
		"016": "the address quoted: text written in a way html.UnescapeString reads otherwise",
		"017": "the address quoted: not an address of Schwab's API",
		"018": "accounts.displayAcctId: a masked account number that ends with more digits than its fake has",
		"019": "the value hygiene tests would fail on it: transferItems.instrument.description: eight digits that are no fake account number",
		"020": "the value hygiene tests would fail on it: the request's path: an activity ID that is no row's and not UnknownActivityID",
	} {
		if why := leftOut(out, name); why != want {
			t.Errorf("answer %s is left out for %q, want %q", name, why, want)
		}
	}
	for _, name := range []string{"001", "011"} {
		fileNamed(t, out, name)
	}
	for _, line := range []string{
		"010: accounts.NickName\n",
		"011.json: description: DESCRIPTION 1\n011.json: description: DESCRIPTION 1\n011.json: description: DESCRIPTION 2\n" +
			"011.json: description: DESCRIPTION 3\n011.json: description: DESCRIPTION 4\n",
	} {
		if !strings.Contains(out.review, line) {
			t.Errorf("review.txt holds no %q:\n%s", line, out.review)
		}
	}
	if n := strings.Count(out.review, "011.json: the row of activity ID "); n != 2 {
		t.Errorf("review.txt names %d rows of W-8 withholding, want 2:\n%s", n, out.review)
	}
	body := string(fileNamed(t, out, "011").body)
	if strings.Count(body, `"DESCRIPTION 1"`) != 2 || !strings.Contains(body, `"W-8 Withholding~BND"`) || !strings.Contains(body, `"Ordinary Dividend~BND"`) {
		t.Errorf("the descriptions written: %s", body)
	}
	if leaks := leaksOf(out, slices.Concat(b.numbers, b.hashes, []string{"Kip", "ACME BANK", "Gift from a friend"})); len(leaks) > 0 {
		t.Errorf("the output holds %d planted values", len(leaks))
	}
}

// The amounts policies: refuse writes only answers that hold no money, an
// amount of zero being money still, and the shapes of the rest, which hold
// no value; keep keeps every number as it was; and scale:20 multiplies
// every amount by 20 exactly, its decimals kept and a zero kept byte for
// byte, leaves prices and the rest as they were, and a residual of the
// fixtures', 0.000637 on the sale of 0.4513 shares, is 20 times what it
// was.
func TestScrubberAmounts(t *testing.T) {
	planted, p := plantF(t, 5)
	byName := map[string]testAnswer{}
	for i, a := range planted {
		byName[fmt.Sprintf("%03d", i+1)] = a
	}
	refuse, err := scrubAnswers(t, planted, scrubConfig{amounts: amountsPolicy{mode: "refuse"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range refuse.files {
		scalars := scalarsOf(f.body)
		for _, sc := range scalars {
			if f.from.table == nil {
				break
			}
			if r, ok := f.from.table.rule(sc.path); ok && r.class == classScale && string(f.body[sc.start:sc.end]) != "null" {
				t.Errorf("refuse wrote %s, which holds money at %s", f.entry.File, pathName(sc.path))
			}
		}
	}
	if names := slices.Sorted(maps.Keys(refuse.shapes)); !slices.Equal(names, []string{"002", "003", "004"}) {
		t.Fatalf("shapes of %q, want those of the accounts and the transactions", names)
	}
	for name, shape := range refuse.shapes {
		body := []byte(byName[name].body)
		walkJSON(body, func(sc jsonScalar) {
			raw := body[sc.start:sc.end]
			text, isString := jsonString(raw)
			switch {
			case isString && len(text) >= 4 && !strings.Contains(shape, text+":") && strings.Contains(shape, text):
				t.Errorf("shapes/%s.txt holds the value at %s", name, pathName(sc.path))
			case isJSONNumber(raw) && (bytes.ContainsAny(raw, ".eE") || len(raw) >= 3) && strings.Contains(shape, string(raw)):
				t.Errorf("shapes/%s.txt holds the number at %s", name, pathName(sc.path))
			}
		}, nil)
	}
	if leaks := leaksOf(refuse, p.values()); len(leaks) > 0 {
		t.Errorf("refuse's output holds %d planted values", len(leaks))
	}

	for _, policy := range []amountsPolicy{{mode: "keep"}, {mode: "scale", k: 20}} {
		out, err := scrubAnswers(t, planted, scrubConfig{amounts: policy})
		if err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
		scaled := 0
		for _, f := range out.files {
			if f.from.table == nil {
				continue
			}
			before := []byte(byName[f.from.raw.name].body)
			bs, as := scalarsOf(before), scalarsOf(f.body)
			for i, sc := range as {
				o, n := before[bs[i].start:bs[i].end], f.body[sc.start:sc.end]
				r, _ := f.from.table.rule(sc.path)
				switch {
				case r.class == classMap || !isJSONNumber(o):
				case r.class != classScale || policy.mode == "keep" || isZeroLiteral(o):
					if !bytes.Equal(o, n) {
						t.Errorf("%s: %s: %s became %s", policy, pathName(sc.path), o, n)
					}
				default:
					ro, _ := ratOf(o)
					if rn, _ := ratOf(n); rn == nil || new(big.Rat).Mul(ro, big.NewRat(20, 1)).Cmp(rn) != 0 || decimalsOf(o) != decimalsOf(n) {
						t.Errorf("%s: %s: %s became %s, not 20 times it", policy, pathName(sc.path), o, n)
					}
					scaled++
				}
			}
		}
		if policy.mode == "scale" && scaled < 100 {
			t.Errorf("only %d amounts were scaled", scaled)
		}
		if policy.mode == "scale" {
			if !strings.Contains(out.review, "Scaling hides nothing") {
				t.Error("review.txt does not say that scaling hides nothing")
			}
			sale := residualsOf("transactions", fileNamed(t, out, "004").body)
			found := false
			for _, r := range sale {
				found = found || r.r.Cmp(big.NewRat(1274, 100000)) == 0 || r.r.Cmp(big.NewRat(-1274, 100000)) == 0
			}
			if !found {
				t.Error("no residual of 0.01274, 20 times the sale's 0.000637")
			}
		}
	}

	b := newCorpusBase(t, 31, 1)
	zero := b.transactions(cashRow(b.numbers[0], b.id(), "netAmount", "0.0", "transferItems", cashLeg("0.0", "USD currency")))
	out, err := scrubAnswers(t, []testAnswer{b.accountNumbers(), zero}, scrubConfig{amounts: amountsPolicy{mode: "refuse"}})
	if err != nil {
		t.Fatal(err)
	}
	if why := leftOut(out, "002"); !strings.HasPrefix(why, "it holds money") {
		t.Errorf("refuse left an answer of amounts of zero out for %q, want for its money", why)
	}
}

// The identities are each linear relation of a record's numbers, computed
// exactly from the numbers as written: a residual that the recording has
// is the same in what keep writes and K times it under scale:K, while any
// number changed by other than K, such as a leg's cost by a cent, moves a
// residual, which gate (iv) refuses, as it refuses records that hold other
// identities than before.
func TestScrubberIdentities(t *testing.T) {
	planted, _ := plantF(t, 6)
	for _, name := range []string{"002", "003", "004"} {
		a := &scrubAnswer{raw: recordedAnswer{name: name, body: []byte(planted[name[2]-'1'].body)}}
		a.kind = map[string]string{"002": "accounts", "003": "transaction", "004": "transactions"}[name]
		residuals := residualsOf(a.kind, a.raw.body)
		if len(residuals) < 2 {
			t.Fatalf("%s: only %d identities", a.kind, len(residuals))
		}
		for _, k := range []int64{1, 20} {
			s := &scrubber{cfg: scrubConfig{amounts: amountsPolicy{mode: "scale", k: k}}}
			if k == 1 {
				s.cfg.amounts = amountsPolicy{mode: "keep"}
			}
			scaled := a.raw.body
			if k > 1 {
				var splices []splice
				walkJSON(a.raw.body, func(sc jsonScalar) {
					raw := a.raw.body[sc.start:sc.end]
					if r, ok := policyTables[a.kind].rule(sc.path); ok && r.class == classScale && isJSONNumber(raw) && !isZeroLiteral(raw) {
						splices = append(splices, splice{start: sc.start, end: sc.end, with: scaleLiteral(raw, k)})
					}
				}, nil)
				scaled = applySplices(a.raw.body, splices)
			}
			if why := s.identities(a, scaled); why != "" {
				t.Errorf("%s at K = %d: %s", a.kind, k, why)
			}
			moved := bytes.Replace(scaled, []byte(`"cost"`), []byte(`"cost":1.01,"zz"`), 1)
			if a.kind == "accounts" {
				moved = bytes.Replace(scaled, []byte(`"marketValue": `), []byte(`"marketValue": 1`), 1)
			}
			if bytes.Equal(moved, scaled) {
				t.Fatalf("%s: nothing to move", a.kind)
			}
			if why := s.identities(a, moved); !strings.Contains(why, "is not off by K times what it was") {
				t.Errorf("%s at K = %d, a number moved: %q", a.kind, k, why)
			}
		}
	}
	// The sale of 0.4513 shares in the fixtures is off by 0.000637, which
	// fixed tolerances would refuse at K = 20 and the exact rule keeps.
	found := false
	for _, r := range residualsOf("transactions", readTestdata(t, "transactions.json")) {
		found = found || new(big.Rat).Abs(r.r).Cmp(big.NewRat(637, 1000000)) == 0
	}
	if !found {
		t.Error("no residual of 0.000637 in the fixtures")
	}
	// Records that hold other identities than before, as a trade made a
	// journal does, are refused whatever their residuals.
	s := &scrubber{cfg: scrubConfig{amounts: amountsPolicy{mode: "keep"}}}
	a := &scrubAnswer{kind: "transactions", raw: recordedAnswer{body: []byte(planted[3].body)}}
	journal := bytes.Replace(a.raw.body, []byte(`"type": "TRADE"`), []byte(`"type": "JOURNAL"`), 1)
	if bytes.Equal(journal, a.raw.body) {
		t.Fatal("no trade to make a journal")
	}
	if why := s.identities(a, journal); why != "its records hold other identities than the recording's" {
		t.Errorf("a trade made a journal: %q", why)
	}
}

// A recording's answers are read in the order of their numbers, 1000 after
// 999, a folder's other files are left alone, and an answer whose .json
// names no request and status is refused.
func TestReadRecordingOrder(t *testing.T) {
	dir := t.TempDir()
	meta := `{"request":{"method":"GET","path":"/trader/v1/accounts","query":""},"response":{"status":200,"proto":"HTTP/2.0",` +
		`"uncompressed":false,"headers":{},"otherHeaders":[]},"sent":"2026-11-08T12:00:00Z","go":"go1.27.3","commit":"c0ffee"}`
	for _, n := range []string{"1000", "999", "010"} {
		for name, b := range map[string]string{n + ".json": meta, n + ".body": "[]"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(b), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	answers, err := readRecording(dir)
	if err != nil {
		t.Fatal(err)
	}
	unnamed := t.TempDir()
	for name, b := range map[string]string{"001.json": strings.Replace(meta, `"status":200`, `"status":0`, 1), "001.body": "[]"} {
		if err := os.WriteFile(filepath.Join(unnamed, name), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readRecording(unnamed); err == nil || !strings.Contains(err.Error(), "names no request and status") {
		t.Errorf("an answer of status 0: %v", err)
	}
	var names []string
	for _, a := range answers {
		names = append(names, a.name)
	}
	if !slices.Equal(names, []string{"010", "999", "1000"}) || answers[0].meta.Response.Proto != "HTTP/2.0" || string(answers[0].body) != "[]" {
		t.Errorf("read %q: %+v", names, answers[0].meta)
	}
}

// The accounts take their fakes in the order of the recording's first
// answer of account numbers, wherever else they come first.
func TestScrubberAccountOrder(t *testing.T) {
	b := newCorpusBase(t, 19, 3)
	out, err := scrubAnswers(t, []testAnswer{b.transactions(cashRow(b.numbers[1], b.id())), b.accountNumbers()}, scrubConfig{
		amounts: amountsPolicy{mode: "keep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body := fileNamed(t, out, "001").body; !bytes.Contains(body, []byte(`"accountNumber":"`+schwabtest.FixtureAccounts[1].Number+`"`)) {
		t.Errorf("the second account's row is not of FixtureAccounts' second: %s", body)
	}
}

// The fake of a mapped value keeps its JSON kind: an account number or an
// id sent as a number is a number, and one sent as a string a string, as
// Schwab sent them; and an id of another kind has none.
func TestScrubberMapsInKind(t *testing.T) {
	b := newCorpusBase(t, 23, 2)
	activity, order := b.id(), b.id()
	row := cashRow(b.numbers[0], activity, "accountNumber", b.numbers[1], "orderId", `"`+order+`"`, "positionId", "7.5")
	answers, err := readRecording(recordAnswers(t, []testAnswer{b.accountNumbers(), b.transactions(row)}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := newScrubber(t, answers, scrubConfig{amounts: amountsPolicy{mode: "keep"}})
	if err != nil {
		t.Fatal(err)
	}
	a := s.answers[1]
	got := map[string]string{}
	walkJSON(a.raw.body, func(sc jsonScalar) {
		if r, ok := a.table.rule(sc.path); ok && r.class == classMap {
			out, why := s.mapValue(a, r.id, sc, a.raw.body[sc.start:sc.end])
			got[pathName(sc.path)] = string(out) + why
		}
	}, nil)
	activityFake, orderFake := s.ids.get(idActivity, activity).fake, s.ids.get(idOrder, order).fake
	want := map[string]string{
		"activityId":    activityFake,
		"accountNumber": schwabtest.FixtureAccounts[1].Number,
		"orderId":       `"` + orderFake + `"`,
		"positionId":    "a position ID that is neither a whole number nor a string of digits",
	}
	if !maps.Equal(got, want) || len(activityFake) != 11 || len(orderFake) != 11 {
		t.Errorf("mapped %q, want %q", got, want)
	}
}

// The texts that the checks for leaks search are each JSON key and string,
// decoded and then unescaped as a path and a query, each number as written
// and as the whole number it is, a page's text unescaped, an entry's file,
// path, query and headers, and last a body's bytes as written, in which
// only identifiers of 6 characters or more count. A value on its enum's
// closed list is in none of them, but one off it is, and the same text at
// a path of free text.
func TestLeakTexts(t *testing.T) {
	has := slices.Contains[[]leakText]
	b := []byte(`{"k%32":"a%2529b","n":2.9470135E7}`)
	texts := textsOf(b, nil)
	for _, want := range []leakText{
		{where: `"k%32", its key`, text: "k%32", key: true},
		{where: `"k%32", its key`, text: "k2", key: true},
		{where: `"k%32"`, text: "a%2529b"},
		{where: `"k%32"`, text: "a%29b"},
		{where: `"k%32"`, text: "a)b"},
		{where: "n", text: "2.9470135E7", number: true},
		{where: "n", text: "29470135", number: true},
	} {
		if !has(texts, want) {
			t.Errorf("the texts of %s lack %+v: %+v", b, want, texts)
		}
	}
	if last := texts[len(texts)-1]; last.text != string(b) || !last.long || last.where != "the answer" {
		t.Errorf("the last text is %+v, want the bytes as written", last)
	}
	page := textsOf([]byte("<html><p>18&#46;9e%2Fx</p></html>"), nil)
	if !has(page, leakText{where: "the page", text: "<html><p>18.9e%2Fx</p></html>"}) ||
		!has(page, leakText{where: "the page", text: "<html><p>18.9e/x</p></html>"}) {
		t.Errorf("a page's texts: %+v", page)
	}
	if white := textsOf([]byte(" \n"), nil); len(white) != 1 || !white[0].long {
		t.Errorf("white space's texts: %+v", white)
	}
	row := []byte(`{"subAccount":"CASH","status":"GIFT","description":"CASH"}`)
	var got []string
	for _, l := range textsOf(row, transactionTable) {
		got = append(got, l.where+"="+l.text)
	}
	if want := []string{
		"subAccount, its key=subAccount", "status, its key=status", "description, its key=description",
		"status=GIFT", "description=CASH", `the answer={"subAccount":"","status":"GIFT","description":"CASH"}`,
	}; !slices.Equal(got, want) {
		t.Errorf("the texts of a row: %q, want %q", got, want)
	}
	var e schwabtest.Entry
	e.File, e.Request.Path, e.Request.Query = "recorded/2026-11-08/001.json", "/trader/v1/accounts/a%2Fb", "x=%41"
	e.Response.Headers = map[string][]string{"Vary": {"Accept"}}
	e.Response.OtherHeaders = []string{"X-Other"}
	entry := entryTexts(e)
	for _, want := range []leakText{
		{where: "its name", text: e.File},
		{where: "the request's path", text: "/trader/v1/accounts/a/b"},
		{where: "the request's query", text: "x=A"},
		{where: "a header's name", text: "Vary"},
		{where: "the header Vary", text: "Accept"},
		{where: "a header's name", text: "X-Other"},
	} {
		if !has(entry, want) {
			t.Errorf("an entry's texts lack %+v: %+v", want, entry)
		}
	}
}

// The recording's day is the day in UTC of its first answer sent, whatever
// the order of its files.
func TestScrubberDate(t *testing.T) {
	answers := make([]recordedAnswer, 2)
	for i, sent := range []time.Time{
		time.Date(2026, 11, 10, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 8, 23, 0, 0, 0, time.FixedZone("EST", -5*60*60)),
	} {
		a := &answers[i]
		a.name, a.body = fmt.Sprintf("%03d", i+1), []byte(" \n")
		a.meta.Request.Method, a.meta.Request.Path, a.meta.Response.Status, a.meta.Sent = "GET", "/marketdata/v1/quotes", 429, sent
	}
	s, err := newScrubber(t, answers, scrubConfig{amounts: amountsPolicy{mode: "refuse"}})
	if err != nil || s.date != "2026-11-09" {
		t.Errorf("the day %q, %v; want 2026-11-09, the first sent in UTC", s.date, err)
	}
}

// review.txt names a request by its route, or by its path with every part
// that is not a word shown as {…}, and its query by its keys alone: so that
// a symbol, an id or a hash in a path that no Route serves, or a value in a
// query, is not named.
func TestRouteName(t *testing.T) {
	for _, tc := range []struct {
		method, path, query string
		status              int
		want                string
	}{
		{
			"GET", "/trader/v1/accounts/" + schwabtest.FixtureAccounts[0].Hash + "/transactions", "types=TRADE&startDate=2024-01-01", 200,
			"GET /trader/v1/accounts/{hash}/transactions?types&startDate, 200",
		},
		{
			"GET", "/trader/v1/accounts/" + schwabtest.FixtureAccounts[0].Hash + "/orders/1004517592340", "", 200,
			"GET /trader/v1/accounts/{…}/orders/{…}, 200",
		},
		{"GET", "/marketdata/v1/AAPL/quotes", "fields=quote%2Creference&a%20b=1", 401, "GET /marketdata/v1/{…}/quotes?fields&{…}, 401"},
	} {
		e := policyEntry(tc.method, tc.path, tc.status)
		e.Request.Query = tc.query
		if got := routeName(e); got != tc.want {
			t.Errorf("routeName(%s?%s) = %q, want %q", tc.path, tc.query, got, tc.want)
		}
	}
}
