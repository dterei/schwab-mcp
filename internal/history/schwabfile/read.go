package schwabfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// A Kind is what a file is, as Read tells it by its content.
type Kind string

// The kinds of file that Read reads.
const (
	// KindHistoryCSV is a History export of schwab.com in CSV, in any of
	// its layouts.
	KindHistoryCSV Kind = "history_csv"
	// KindHistoryJSON is a History export of schwab.com in JSON.
	KindHistoryJSON Kind = "history_json"
	// KindRealized is a Realized Gain/Loss export of schwab.com, in its
	// Lot Details layout.
	KindRealized Kind = "realized_csv"
	// KindSnapshots is a snapshot file that `schwabtx snapshot` writes.
	KindSnapshots Kind = "snapshots"
	// KindAPIJSON is what `schwabtx transactions -json` prints: Schwab's
	// own transactions, as its API sent them.
	KindAPIJSON Kind = "api_json"
)

// The layouts that Read reads, as File.Layout names them.
const (
	// LayoutBanner24h is the History CSV until 2021: a first line
	// "Transactions  for account XXXX-1234 as of 12/20/2014 21:16:49 ET",
	// with an optional range after it, the header, and a footer
	// "Transactions Total" with the rows' total.
	LayoutBanner24h = "history-banner-24h"
	// LayoutBanner12h is the History CSV of 2022 and 2023: the same, with
	// a 12-hour clock and the account as "...123".
	LayoutBanner12h = "history-banner-12h"
	// LayoutBanner is a History CSV whose first line starts as a banner
	// does, but which Read cannot read further: its rows are read, and
	// nothing of the banner.
	LayoutBanner = "history-banner"
	// LayoutCurrent is the History CSV from late 2023: the header is the
	// first line, and there is no banner and, from December 2023, no
	// footer.
	LayoutCurrent = "history-current"
	// LayoutHistoryJSON is the History export in JSON.
	LayoutHistoryJSON = "history-json"
	// LayoutRealizedLots is Realized Gain/Loss's Lot Details: a title, a
	// header of 25 columns, and one row per closed lot.
	LayoutRealizedLots = "realized-lots"
	// LayoutSnapshots is a snapshot file of lines of version 1.
	LayoutSnapshots = "snapshots-v1"
	// LayoutAPIJSON is `schwabtx transactions -json` output.
	LayoutAPIJSON = "api-json"
)

// A File is what Read made of a file: what it is and what it holds.
type File struct {
	Kind   Kind
	Layout string
	// Encoding is the text's: "utf-8", or "windows-1252" when it was not
	// valid UTF-8. A snapshot file and `-json` output are read as they
	// are, as UTF-8.
	Encoding string
	// BOM is whether a UTF-8 byte order mark, which Read passes over,
	// started the file; CRLF whether a line of it ends in CR LF.
	BOM, CRLF bool
	// LazyQuotes is whether a CSV file broke the rules of quoting, so that
	// Read read it again, more leniently (encoding/csv's LazyQuotes).
	LazyQuotes bool
	// AccountHint is the account's last digits as the file gives them, or
	// else as its name does: "1234" from "XXXX-1234" or "XXXX1234_...",
	// "123" from "...123" or "_XXX123_". It is "" when neither gives any.
	AccountHint string
	// NameDigits is the account's last digits as the file's name gives
	// them, whatever the file itself says: the digits the store checks
	// against the account a file is imported for.
	NameDigits string
	// ExportedAt is when the file was exported, as its banner or title
	// says, else as the name's stamp (_YYYYMMDD-HHMMSS, New York time)
	// does, in UTC; the zero time when neither does.
	ExportedAt time.Time
	// StatedFrom and StatedTo are the range of dates the file says it
	// covers, from History JSON's FromDate and ToDate, a banner's "From
	// ... to ...", or a Realized Gain/Loss title's "from ... to ..."; ""
	// when it says none.
	StatedFrom, StatedTo nytime.Date
	// StatedTotal is the total of the rows' Amounts that the file states:
	// History JSON's TotalTransactionsAmount, or an old CSV footer's
	// total; nil when it states none. StatedFees is History JSON's
	// TotalFeesAndCommAmount, when it has one.
	StatedTotal, StatedFees *Decimal
	// History holds a History export's rows, in the file's order.
	History []HistoryRow
	// Realized holds a Realized Gain/Loss export's lots, in the file's
	// order.
	Realized []RealizedRow
	// Snapshots holds a snapshot file's lines, and SnapshotSkips counts
	// the lines skipped by why (the Skip constants), as ReadSnapshotLines
	// reads them.
	Snapshots     []Snapshot
	SnapshotSkips map[string]int
	// APIRows holds `schwabtx transactions -json` output's transactions,
	// each with its white space taken out (json.Compact), in the file's
	// order.
	APIRows []json.RawMessage
	// Warnings say what an import should report of the file and flag on
	// it, in the order Read found them.
	Warnings []Warning
}

// A Warning is something about a file that its owner should know, which
// does not stop its import. Code is the flag the store raises on the file,
// and Detail says what, composed of what the file holds without its free
// text: counts, line numbers, dates and amounts.
type Warning struct {
	Code, Detail string
}

// The warnings that Read gives, as Warning.Code names them.
const (
	// WarnEncodingFallback: the file is not valid UTF-8 and was read as
	// Windows-1252.
	WarnEncodingFallback = "encoding_fallback"
	// WarnLazyQuotes: the file broke the rules of CSV's quoting and was
	// read leniently.
	WarnLazyQuotes = "lazy_quotes"
	// WarnUndatedRows: rows whose date is not a date; they are kept, but
	// make no transaction.
	WarnUndatedRows = "undated_rows"
	// WarnRowsAfterTotal: rows after a History CSV's footer, which Read
	// does not read.
	WarnRowsAfterTotal = "rows_after_total"
	// WarnEmpty: a History or Realized Gain/Loss file without rows, or
	// `-json` output without transactions.
	WarnEmpty = "empty"
	// WarnAtCap: exactly 1,500 or 10,000 rows, the limits that exports are
	// reported to have; its oldest days may be missing.
	WarnAtCap = "at_cap"
	// WarnTwoYears: a stated range longer than two years whose oldest row
	// is within a week of two years before its end, as if the export had
	// stopped there.
	WarnTwoYears = "two_years"
	// WarnLateStart: a History file whose oldest row is more than a week
	// after the start of its stated range.
	WarnLateStart = "late_start"
	// WarnTotalMismatch: a stated total is not the sum of the rows.
	WarnTotalMismatch = "total_mismatch"
	// WarnCoverageFromRows: the file states no range, and none was given,
	// so it covers the span of its rows.
	WarnCoverageFromRows = "coverage_from_rows"
	// WarnRowsOutsideCoverage: rows outside the range stated or given,
	// which widen it.
	WarnRowsOutsideCoverage = "rows_outside_coverage"
	// WarnCoverageClipped: the file covers less than its range, because of
	// another warning or the day it was exported.
	WarnCoverageClipped = "coverage_clipped"
)

// Read tells what a file is by its content, never by its name or
// extension, and reads it: a History export in CSV (in any of its layouts)
// or JSON, a Realized Gain/Loss export's Lot Details, a snapshot file of
// `schwabtx snapshot`, or `schwabtx transactions -json` output. name is
// the file's name, from which it takes the account's digits and the time
// of the export when the content gives none. It refuses every other file,
// with an error that says what the file is, such as a Schwab Bank or
// Equity Awards export, or that it is none it knows; the error names the
// file by name with its free text masked (mask.FileName). Rows it cannot
// read whole are kept with their problems, never dropped.
func Read(name string, content []byte) (*File, error) {
	shown := mask.FileName(name)
	if len(bytes.TrimSpace(bytes.TrimPrefix(content, []byte("\xef\xbb\xbf")))) == 0 {
		return nil, fmt.Errorf("%s is empty; if it is an export of schwab.com, the export may have exceeded "+
			"a limit, which is reported to give an empty file; export a shorter range", shown)
	}
	var f *File
	var err error
	if IsSnapshotFile(content) {
		f = readSnapshots(content)
	} else {
		text, encoding, bom := decode(content)
		switch first := strings.TrimLeft(text, " \t\r\n")[0]; first {
		case '[':
			f, err = readAPIJSON(shown, content)
		case '{':
			f, err = readJSON(shown, text)
		case '<':
			err = fmt.Errorf("%s is an XML export, whose shape schwabtx does not know; export JSON or CSV", shown)
		default:
			f, err = readCSV(shown, text)
		}
		if err != nil {
			return nil, err
		}
		if f.Kind != KindAPIJSON {
			f.Encoding, f.BOM = encoding, bom
			if encoding == encodingCP1252 {
				f.Warnings = append([]Warning{{WarnEncodingFallback, "not valid UTF-8; read as Windows-1252"}}, f.Warnings...)
			}
		}
	}
	f.CRLF = bytes.Contains(content, []byte("\r\n"))
	f.NameDigits = nameDigits(name)
	if f.AccountHint == "" {
		f.AccountHint = f.NameDigits
	}
	if f.ExportedAt.IsZero() {
		f.ExportedAt = nameStamp(name)
	}
	f.Warnings = append(f.Warnings, f.limits()...)
	return f, nil
}

// readSnapshots reads a snapshot file.
func readSnapshots(content []byte) *File {
	lines, skipped := ReadSnapshotLines(content)
	return &File{
		Kind: KindSnapshots, Layout: LayoutSnapshots, Encoding: encodingUTF8,
		BOM:       bytes.HasPrefix(content, []byte("\xef\xbb\xbf")),
		Snapshots: lines, SnapshotSkips: skipped,
	}
}

// readAPIJSON reads `schwabtx transactions -json` output: a JSON array
// of Schwab's transactions, each an object with an activityId or
// transferItems. Its bytes are read as they are, since they are Schwab's.
func readAPIJSON(shown string, content []byte) (*File, error) {
	notOutput := fmt.Errorf("%s is a JSON array that is not `schwabtx transactions -json` output", shown)
	f := &File{Kind: KindAPIJSON, Layout: LayoutAPIJSON, Encoding: encodingUTF8}
	content, f.BOM = bytes.CutPrefix(content, []byte("\xef\xbb\xbf"))
	var elements []json.RawMessage
	if err := json.Unmarshal(content, &elements); err != nil {
		return nil, notOutput
	}
	for _, e := range elements {
		var keys map[string]json.RawMessage
		if json.Unmarshal(e, &keys) != nil || keys == nil {
			return nil, notOutput
		}
		if _, ok := keys["activityId"]; !ok {
			if _, ok := keys["transferItems"]; !ok {
				return nil, notOutput
			}
		}
		var b bytes.Buffer
		_ = json.Compact(&b, e) // valid: Unmarshal read it
		f.APIRows = append(f.APIRows, b.Bytes())
	}
	if len(f.APIRows) == 0 {
		f.Warnings = append(f.Warnings, Warning{WarnEmpty, "no transactions"})
	}
	return f, nil
}

// readJSON reads a JSON object: a History export, or one of Schwab's
// other JSON exports, which it refuses.
func readJSON(shown, text string) (*File, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &top); err != nil || top == nil {
		return nil, fmt.Errorf("%s is not a JSON object that schwabtx knows how to import", shown)
	}
	switch {
	case top["BrokerageTransactions"] != nil:
		return readHistoryJSON(shown, text)
	case top["PostedTransactions"] != nil:
		return nil, fmt.Errorf("%s is a Schwab Bank export, which schwabtx does not import", shown)
	case top["Transactions"] != nil && bytes.Contains(top["Transactions"], []byte(`"TransactionDetails"`)):
		return nil, fmt.Errorf("%s is a Schwab Equity Awards export, which schwabtx does not import", shown)
	}
	return nil, fmt.Errorf("%s is a JSON file that is not a History export", shown)
}

// nameDigitsRE finds the account's last digits in an export's name:
// "_XXX123_Transactions_" in a History export's, "XXXX1234_GainLoss_" at
// the start of a Realized Gain/Loss export's.
var nameDigitsRE = regexp.MustCompile(`(?i)(?:_XXX([0-9]{3})_Transactions_|^XXXX([0-9]{4})_GainLoss_)`)

// nameDigits returns the account's last digits that a file's name gives,
// or "".
func nameDigits(name string) string {
	m := nameDigitsRE.FindStringSubmatch(baseName(name))
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

// nameStampRE is the time of an export in its name: _YYYYMMDD-HHMMSS
// before the extension.
var nameStampRE = regexp.MustCompile(`_([0-9]{8}-[0-9]{6})\.[A-Za-z]+$`)

// nameStamp returns the time of the export that a file's name gives, as
// New York time, in UTC; the zero time when it gives none or it is not a
// time.
func nameStamp(name string) time.Time {
	m := nameStampRE.FindStringSubmatch(baseName(name))
	if m == nil {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102-150405", m[1], nytime.NewYork())
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// baseName returns a path's last element, after its last slash or
// backslash, whichever system wrote it.
func baseName(name string) string {
	return name[strings.LastIndexAny(name, `/\`)+1:]
}

// hintRE finds an account's last digits at the end of the account as a
// banner or a title names it: "XXXX-1234", "XXXX1234", "...123" or
// "…123", or a whole number of eight digits or more.
var hintRE = regexp.MustCompile(`(?:(?:X{3,4}-?|\.\.\.|…)([0-9]{3,4})|(?:^|\s)([0-9]{8,}))$`)

// accountHint returns the digits that hintRE finds in account, or "".
func accountHint(account string) string {
	m := hintRE.FindStringSubmatch(strings.TrimSpace(account))
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

// errNotDate is the error of a date that is not MM/DD/YYYY.
var errNotDate = errors.New("not MM/DD/YYYY")

// parseDate reads a date as files write it, MM/DD/YYYY with a month and a
// day of one or two digits.
func parseDate(s string) (nytime.Date, error) {
	if !dateRE.MatchString(s) {
		return "", errNotDate
	}
	t, err := time.Parse("1/2/2006", s)
	if err != nil {
		return "", errNotDate
	}
	return nytime.Date(t.Format(time.DateOnly)), nil
}

// dateRE is a date as files write it.
var dateRE = regexp.MustCompile(`^[0-9]{1,2}/[0-9]{1,2}/[0-9]{4}$`)
