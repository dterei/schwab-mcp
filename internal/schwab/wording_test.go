package schwab

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// Schwab's own wording: the descriptions of transactions and the messages
// of errors that Schwab has been seen to send, each as Schwab writes it,
// with SYM for the symbol after a "~", # for an account's type, and
// <value> for a date. Text on it names no one, so a report may print it
// and a recording may keep it; any other text, which may name a bank, a
// payer or another account, is never printed. The probe of row shapes
// (Q11) takes the whole of it. The recording's scrubber and the value
// hygiene tests take it by its parts, as the scrub policy's free text
// does: a transaction's description may be one of the descriptions, and an
// error's message one of the messages. A text is on it as onWording says.
var (
	// schwabDescriptions are the descriptions of transactions.
	schwabDescriptions = []string{
		"Dividend Reinvest",
		"Sell Trade",
		"Ordinary Dividend~SYM",
		"W-8 Withholding~SYM",
		"Free Balance Interest Adjustment",
		"Wire Incoming",
		"ACH Credit Received",
		"Funds Received",
		// A sweep and the transfers between an account's own types, as they
		// are recorded (docs/schwab-api.md); other sweeps' wording is not.
		"BANK SWEEP TO BROKERAGE",
		"TRF FDS TO TYPE #",
		"TRF FDS FRM TYPE #",
	}
	// schwabMessages are the messages of errors: the account API's, the
	// Market Data API's, the gateway's faults and the token endpoint's
	// refusals.
	schwabMessages = []string{
		"Invalid account number",
		"Client not authorized",
		"Unauthorized",
		"Body buffer overflow",
		"The Service is temporarily unavailable",
		"Gateway Timeout",
		"Invalid ApiKey",
		"refresh token invalid",
		"'<value>' is not a valid value for startDate",
		`400 Bad Request: {"error_description":"Refresh token is invalid, expired or revoked","error":"invalid_grant"}`,
		`400 Bad Request: "{\"error_description\":\"Exception while authenticating refresh token [tokenDigest=abc], ` +
			`Exception=Failed refresh token authentication [tokenDigest=abc]]\",\"error\":\"refresh_token_authentication_error\"}"`,
	}
	// schwabWording is the whole of Schwab's wording.
	schwabWording = slices.Concat(schwabDescriptions, schwabMessages)
)

// What # and <value> in an entry of schwabWording take: one or two
// digits, an account's type, as in TRF FDS TO TYPE 2; and a date, or a
// date and a time, as a query sends them. Nothing else is taken, so that
// TRF FDS TO TYPE and a long number, or a message that quotes something
// other than a date, is not on the list.
const (
	wordingType  = `[0-9]{1,2}`
	wordingValue = `[0-9]{4}-[0-9]{2}-[0-9]{2}(?:T[0-9]{2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]{1,9})?)?(?:Z|[+-][0-9]{2}(?::?[0-9]{2})?)?)?`
)

// wordingEntries are the entries of schwabWording as onWording matches
// them, with the entry each stands for.
var wordingEntries = func() []struct {
	re    *regexp.Regexp
	entry string
} {
	var es []struct {
		re    *regexp.Regexp
		entry string
	}
	for _, e := range schwabWording {
		pattern := strings.ReplaceAll(regexp.QuoteMeta(e), "#", wordingType)
		pattern = strings.ReplaceAll(pattern, regexp.QuoteMeta("<value>"), wordingValue)
		es = append(es, struct {
			re    *regexp.Regexp
			entry string
		}{regexp.MustCompile("^" + pattern + "$"), e})
	}
	return es
}()

// onWording returns the entry of schwabWording that text is, and whether
// there is one: text is the entry once the symbol after its last "~" is
// made SYM, with # standing for one or two digits and <value> for a date,
// so that "W-8 Withholding~BND" is "W-8 Withholding~SYM" and "TRF FDS TO
// TYPE 12" is "TRF FDS TO TYPE #". What follows the last "~" must be a
// symbol, as isSymbol says, so that "W-8 Withholding~ACME BANK" is not on
// it. The case counts, as Schwab's wording keeps to one, and so does every
// other digit, so that a long number is never on it.
func onWording(text string) (string, bool) {
	return onWordingOf(schwabWording, text)
}

// onWordingOf returns the entry of part, a part of schwabWording such as
// schwabDescriptions, that text is, and whether there is one, as onWording
// matches text with the whole.
func onWordingOf(part []string, text string) (string, bool) {
	if i := strings.LastIndex(text, "~"); i >= 0 {
		if !isSymbol(text[i+1:]) {
			return "", false
		}
		text = text[:i+1] + "SYM"
	}
	for _, e := range wordingEntries {
		if slices.Contains(part, e.entry) && e.re.MatchString(text) {
			return e.entry, true
		}
	}
	return "", false
}

// Schwab's wording is on the list once the symbol after a "~" is masked,
// with an account's type of one or two digits and a date where the list
// takes one, and what differs from it in a word, in case, in a digit or by
// more text is not: every description of the hand-made fixtures and every
// message of their errors is on it, Schwab's reported answer to a date
// alone too, while a message that quotes something other than a date, or
// a transfer to a type of more than two digits, is not.
func TestSchwabWording(t *testing.T) {
	var on []string
	for _, name := range []string{"transactions.json", "transaction.json"} {
		body := readTestdata(t, name)
		if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			body = []byte("[" + string(body) + "]")
		}
		rows, err := decodeList[Transaction](body)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Description != "" {
				on = append(on, row.Description)
			}
		}
	}
	for _, e := range schwabtest.Index() {
		if !strings.HasPrefix(e.File, "errors/") || !strings.HasSuffix(e.File, ".json") {
			continue
		}
		se := newError(e.Response.Status, http.Header{}, readTestdata(t, e.File))
		texts := append([]string{se.Message}, se.Errors...)
		var oauth struct {
			Description string `json:"error_description"`
		}
		if json.Unmarshal(readTestdata(t, e.File), &oauth) == nil {
			texts = append(texts, oauth.Description)
		}
		for _, text := range texts {
			if text != "" {
				on = append(on, text)
			}
		}
	}
	on = append(on, "W-8 Withholding~VTI", "Ordinary Dividend~BRK.B", "TRF FDS TO TYPE 12", "TRF FDS FRM TYPE 2",
		"BANK SWEEP TO BROKERAGE", "'2024-04-04' is not a valid value for startDate",
		"'2024-04-04T15:54:14.000Z' is not a valid value for startDate", "'2024-04-04T10:54:14-05:00' is not a valid value for startDate")
	for _, text := range on {
		if _, ok := onWording(text); !ok {
			t.Errorf("%q is not on Schwab's wording", text)
		}
	}
	if len(on) < 20 {
		t.Errorf("only %d texts were looked at", len(on))
	}
	for text, entry := range map[string]string{
		"W-8 Withholding~BND":                             "W-8 Withholding~SYM",
		"Dividend Reinvest":                               "Dividend Reinvest",
		"TRF FDS TO TYPE 3":                               "TRF FDS TO TYPE #",
		"'2026-11-07' is not a valid value for startDate": "'<value>' is not a valid value for startDate",
	} {
		if got, ok := onWording(text); !ok || got != entry {
			t.Errorf("onWording(%q) = %q, %v; want %q", text, got, ok, entry)
		}
	}
	for _, text := range []string{
		"", "Dividend", "Ordinary Dividend", "dividend reinvest", "Dividend Reinvest ", "Wire Incoming from ACME BANK",
		"BANK SWEEP TO ANOTHER ACCOUNT", "TRF FDS TO TYPE 1 UNKNOWN", "TRF FDS TO TYPE", "W-8 Withholding",
		"'29470135' is not a valid value for startDate", "'abc' is not a valid value for startDate",
		"'2024-04-04 or 29470135' is not a valid value for startDate", "'2024-04-04' is not a valid value for endDate",
		"Invalid account number 29470135", "Exception while authenticating refresh token [tokenDigest=abc]",
		"Ordinary Dividend~", "W-8 Withholding~ACME BANK", "Ordinary Dividend~bnd", "Ordinary Dividend~29470135",
		"Dividend Reinvest~BND", "Ordinary Dividend~BND~",
		"TRF FDS TO TYPE 123456789012", "TRF FDS FRM TYPE 123", "TRF FDS TO TYPE #", "TRF FDS TO TYPE 1.5", "W-9 Withholding~BND",
		"'29470135-04-04' is not a valid value for startDate", "'2024-04-04T15:54:14.0000000000Z' is not a valid value for startDate",
		"401 Bad Request: {\"error_description\":\"Refresh token is invalid, expired or revoked\",\"error\":\"invalid_grant\"}",
	} {
		if entry, ok := onWording(text); ok {
			t.Errorf("onWording(%q) = %q, on the list", text, entry)
		}
	}
}

// The wording has two parts, as the scrub policy's free text takes it: a
// transaction's description may be one of the descriptions alone, and an
// error's message one of the messages alone, while onWording takes either.
func TestSchwabWordingParts(t *testing.T) {
	if len(schwabWording) != len(schwabDescriptions)+len(schwabMessages) {
		t.Errorf("%d entries of wording, from %d descriptions and %d messages", len(schwabWording), len(schwabDescriptions), len(schwabMessages))
	}
	for _, d := range schwabDescriptions {
		if slices.Contains(schwabMessages, d) {
			t.Errorf("%q is a description and a message", d)
		}
	}
	for _, tc := range []struct {
		text                 string
		description, message bool
	}{
		{"Dividend Reinvest", true, false},
		{"Ordinary Dividend~BRK.B", true, false},
		{"W-8 Withholding~00507V109", true, false},
		{"TRF FDS TO TYPE 12", true, false},
		{"Invalid account number", false, true},
		{"'2024-04-04T15:54:14.000Z' is not a valid value for startDate", false, true},
		{"Unauthorized", false, true},
		{"Invalid account number~BND", false, false},
		{"Ordinary Dividend~ACME BANK", false, false},
		{"Wire Incoming from ACME BANK", false, false},
	} {
		_, description := onWordingOf(schwabDescriptions, tc.text)
		_, message := onWordingOf(schwabMessages, tc.text)
		_, either := onWording(tc.text)
		if description != tc.description || message != tc.message || either != (tc.description || tc.message) {
			t.Errorf("%q: a description %v, a message %v, either %v; want %v, %v and %v",
				tc.text, description, message, either, tc.description, tc.message, tc.description || tc.message)
		}
	}
}
