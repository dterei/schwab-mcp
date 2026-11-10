package schwab

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// schwabWording is Schwab's own wording: the descriptions of transactions
// and the messages of errors that Schwab has been seen to send, each as
// Schwab writes it, with SYM for the symbol after a "~". Text on it names
// no one, so a report may print it and a recording may keep it; any other
// text, which may name a bank, a payer or another account, is never
// printed. The probe of row shapes (Q11) and the recording's scrubber share
// it. A text is on it as onWording says.
var schwabWording = []string{
	// The descriptions of transactions.
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

	// The messages of errors: the account API's, the Market Data API's, the
	// gateway's faults and the token endpoint's refusals.
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

// wordingValue is what <value> in an entry of schwabWording takes, once
// masked: a date, or a date and a time, as a query sends them, and nothing
// else, so that a message that quotes something other than a date is not
// on the list.
const wordingValue = `#-#-#(?:T#:#(?::#(?:\.#)?)?(?:Z|[+-]#(?::#)?)?)?`

// wordingDigits are the runs of digits that maskWording masks.
var wordingDigits = regexp.MustCompile(`[0-9]+`)

// wordingEntries are the entries of schwabWording as onWording matches
// them, each masked, with the entry it stands for.
var wordingEntries = func() []struct {
	re    *regexp.Regexp
	entry string
} {
	var es []struct {
		re    *regexp.Regexp
		entry string
	}
	for _, e := range schwabWording {
		pattern := strings.ReplaceAll(regexp.QuoteMeta(maskWording(e)), regexp.QuoteMeta("<value>"), wordingValue)
		es = append(es, struct {
			re    *regexp.Regexp
			entry string
		}{regexp.MustCompile("^" + pattern + "$"), e})
	}
	return es
}()

// maskWording returns text as schwabWording is matched: the symbol after
// its last "~" made SYM, and each run of digits made #.
func maskWording(text string) string {
	if i := strings.LastIndex(text, "~"); i >= 0 {
		text = text[:i+1] + "SYM"
	}
	return wordingDigits.ReplaceAllString(text, "#")
}

// onWording returns the entry of schwabWording that text is, and whether
// there is one: text and the entry are the same once each is masked as
// maskWording masks it, so that "W-8 Withholding~BND" is "W-8
// Withholding~SYM" and "TRF FDS TO TYPE 12" is "TRF FDS TO TYPE #". The
// case counts, as Schwab's wording keeps to one.
func onWording(text string) (string, bool) {
	masked := maskWording(text)
	for _, e := range wordingEntries {
		if e.re.MatchString(masked) {
			return e.entry, true
		}
	}
	return "", false
}

// Schwab's wording is on the list once the symbol after a "~" and the
// digits are masked, and what differs from it in a word, in case or by more
// text is not: every description of the hand-made fixtures and every
// message of their errors is on it, Schwab's reported answer to a date
// alone too, while a message that quotes something other than a date is
// not.
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
	} {
		if entry, ok := onWording(text); ok {
			t.Errorf("onWording(%q) = %q, on the list", text, entry)
		}
	}
}
