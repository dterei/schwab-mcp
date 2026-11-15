package history

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// This file reads the files of a PP export as Portfolio Performance 0.90.0's
// CSV import reads them (11.4 of the design), by the rules read in its code
// and cited from name.abuchen.portfolio/src/name/abuchen/portfolio/:
// ppCheck, which refuses what PP refuses, what it would read otherwise than
// the export means, and what breaks the export's own conventions; and
// ppModel, a model of a PP file across imports, which marks rows as PP's
// review page does, takes the rows ticked, and moves cash and shares as PP
// books them. Neither shares code with the export's writers.

// A ppField is a field of a target of PP's CSV import, as its extractor
// lists it (datatransfer/csv/CSV*Extractor.java): its code, its English
// column name (messages.properties), the kind of value it reads, and whether
// the import's wizard needs it mapped.
type ppField struct {
	code, label, kind string
	required          bool
}

// The kinds of field (datatransfer/csv/CSVImporter.java): text, read as it
// is; a date or an amount, read in its column's format; a type, read through
// its column's map of labels; and an ISIN, cut out of its cell.
const (
	ppKindText   = "text"
	ppKindDate   = "date"
	ppKindAmount = "amount"
	ppKindType   = "type"
	ppKindISIN   = "isin"
)

// A ppTarget is a target of PP's CSV import: its fields, and the types of its
// Type field (model/AccountTransaction.java, model/PortfolioTransaction.java),
// in their order.
type ppTarget struct {
	fields []ppField
	types  []string
}

// ppTargets are the targets that the export's files are read into, by code
// (CSVAccountTransactionExtractor.java:36-56,
// CSVPortfolioTransactionExtractor.java:36-55, CSVSecurityExtractor.java:33-43).
var ppTargets = map[string]ppTarget{
	"account-transaction": {
		fields: []ppField{
			{"date", "Date", ppKindDate, true},
			{"time", "Time", ppKindText, false},
			{"isin", "ISIN", ppKindISIN, false},
			{"ticker", "Ticker Symbol", ppKindText, false},
			{"wkn", "WKN", ppKindText, false},
			{"value", "Value", ppKindAmount, true},
			{"currency", "Transaction Currency", ppKindText, false},
			{"type", "Type", ppKindType, false},
			{"name", "Security Name", ppKindText, false},
			{"shares", "Shares", ppKindAmount, false},
			{"note", "Note", ppKindText, false},
			{"taxes", "Taxes", ppKindAmount, false},
			{"fees", "Fees", ppKindAmount, false},
			{"account", "Cash Account", ppKindText, false},
			{"account2nd", "Offset Account", ppKindText, false},
			{"portfolio", "Securities Account", ppKindText, false},
			{"gross", "Gross Amount", ppKindAmount, false},
			{"currencyGross", "Currency Gross Amount", ppKindText, false},
			{"exchangeRate", "Exchange Rate", ppKindAmount, false},
		},
		types: []string{
			"DEPOSIT", "REMOVAL", "INTEREST", "INTEREST_CHARGE", "DIVIDENDS", "FEES", "FEES_REFUND", "TAXES", "TAX_REFUND",
			"BUY", "SELL", "TRANSFER_IN", "TRANSFER_OUT",
		},
	},
	"portfolio-transaction": {
		fields: []ppField{
			{"date", "Date", ppKindDate, true},
			{"time", "Time", ppKindText, false},
			{"isin", "ISIN", ppKindISIN, false},
			{"ticker", "Ticker Symbol", ppKindText, false},
			{"wkn", "WKN", ppKindText, false},
			{"name", "Security Name", ppKindText, false},
			{"value", "Value", ppKindAmount, true},
			{"currency", "Transaction Currency", ppKindText, false},
			{"fees", "Fees", ppKindAmount, false},
			{"taxes", "Taxes", ppKindAmount, false},
			{"gross", "Gross Amount", ppKindAmount, false},
			{"currencyGross", "Currency Gross Amount", ppKindText, false},
			{"exchangeRate", "Exchange Rate", ppKindAmount, false},
			{"shares", "Shares", ppKindAmount, true},
			{"type", "Type", ppKindType, false},
			{"note", "Note", ppKindText, false},
			{"account", "Cash Account", ppKindText, false},
			{"portfolio", "Securities Account", ppKindText, false},
			{"portfolio2nd", "Offset securities account", ppKindText, false},
		},
		types: []string{"BUY", "SELL", "TRANSFER_IN", "TRANSFER_OUT", "DELIVERY_INBOUND", "DELIVERY_OUTBOUND"},
	},
	"investment-vehicle": {
		fields: []ppField{
			{"isin", "ISIN", ppKindText, false},
			{"wkn", "WKN", ppKindText, false},
			{"ticker", "Ticker Symbol", ppKindText, false},
			{"name", "Security Name", ppKindText, false},
			{"currency", "Currency", ppKindText, false},
			{"note", "Note", ppKindText, false},
			{"date", "Date of Quote", ppKindDate, false},
			{"quote", "Quote", ppKindAmount, false},
		},
	},
}

// field returns the target's field of code, and false when it has none.
func (t ppTarget) field(code string) (ppField, bool) {
	i := slices.IndexFunc(t.fields, func(f ppField) bool { return f.code == code })
	if i < 0 {
		return ppField{}, false
	}
	return t.fields[i], true
}

// ppKinds says, for each configuration of a PP export, the files it reads
// and its target.
var ppKinds = map[string]struct{ kind, target string }{
	ppSecuritiesConfig: {ppSecurities, "investment-vehicle"},
	ppAccountConfig:    {ppAccount, "account-transaction"},
	ppPortfolioConfig:  {ppPortfolio, "portfolio-transaction"},
}

// ppConfigOf returns the name of the configuration that reads the file name
// of a PP export, and "" for a name that no configuration reads.
func ppConfigOf(name string) string {
	switch {
	case name == ppSecuritiesFile:
		return ppSecuritiesConfig
	case strings.HasPrefix(name, "account-transactions-A") && strings.HasSuffix(name, ".csv"):
		return ppAccountConfig
	case strings.HasPrefix(name, "portfolio-transactions-A") && strings.HasSuffix(name, ".csv"):
		return ppPortfolioConfig
	}
	return ""
}

// A ppConfig is an import configuration as PP reads it
// (datatransfer/csv/CSVConfig.java:213-262): what CSVConfig.writeTo applies.
type ppConfig struct {
	label, target, delimiter, encoding string
	skipLines                          int64
	firstLineHeader                    bool
	columns                            []ppColumn
}

// A ppColumn is a column of a configuration: its label, its field's code,
// and its format, "" for none.
type ppColumn struct {
	label, field, format string
}

// labels returns the labels of the configuration's columns, in their order.
func (c *ppConfig) labels() []string {
	out := make([]string, len(c.columns))
	for i, col := range c.columns {
		out[i] = col.label
	}
	return out
}

// typeLabels returns the map of the configuration's Type column, from each
// label to its type, and nil when it has no Type column.
func (c *ppConfig) typeLabels() map[string]string {
	for _, col := range c.columns {
		if col.field != "type" {
			continue
		}
		out := map[string]string{}
		for e := range strings.SplitSeq(col.format, ";") {
			if k, v, ok := strings.Cut(e, "="); ok && k != "" && v != "" && !strings.Contains(v, "=") {
				out[v] = k
			}
		}
		return out
	}
	return nil
}

// ppParseConfig reads the configuration name, whose bytes are data, as PP's
// menu "Import configuration..." does, and checks it against the target
// that it is for (want): it returns the configuration, nil when PP could not
// apply it, and what is wrong with it. PP needs a JSON object with a label,
// a target, a delimiter, a whole number of lines to skip, whether the first
// line is a header and columns (CSVConfig.java:213-262); a text field with a
// format makes applying the configuration fail, and a date or an amount
// field without one makes reading a cell fail (CSVImporter.java:236-244,
// CSVConfig.java:156-161, CSVExtractor.java:107,129); an unknown date or
// amount format, or a type of the map that its target lacks, is silently
// read as another (CSVImporter.java:395-405,577-586,623-648). The export's
// configurations also pin what its files need: a comma, UTF-8, a header on
// the first line, each column labelled with PP's English name of its field,
// a type map that lists every type of its target, and no ISIN, which the
// export never writes.
func ppParseConfig(name string, data []byte, want string) (*ppConfig, []string) {
	var probs []string
	bad := func(format string, args ...any) { probs = append(probs, name+": "+fmt.Sprintf(format, args...)) }
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		bad("is not a JSON object")
		return nil, probs
	}
	allowed := []string{"label", "target", "delimiter", "encoding", "skipLines", "isFirstLineHeader", "columns"}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if !slices.Contains(allowed, k) {
			bad("holds the key %q, which PP's configurations have not", k)
		}
	}
	str := func(m map[string]json.RawMessage, key string) (string, bool) {
		raw, ok := m[key]
		if !ok {
			return "", false
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	}
	c := &ppConfig{}
	var ok bool
	if c.label, ok = str(top, "label"); !ok || c.label == "" {
		bad("lacks a label")
	}
	target, hasTarget := ppTargets[want]
	if c.target, ok = str(top, "target"); !ok || c.target != want {
		bad("has the target %q, not %s", c.target, want)
	}
	if c.delimiter, ok = str(top, "delimiter"); !ok || c.delimiter != "," {
		bad("has the delimiter %q, not a comma", c.delimiter)
	}
	if c.encoding, ok = str(top, "encoding"); !ok || c.encoding != "UTF-8" {
		bad("has the encoding %q, not UTF-8", c.encoding)
	}
	// PP reads skipLines as a whole number and isFirstLineHeader as a
	// boolean, and fails on any other.
	if raw, ok := top["skipLines"]; !ok || !regexp.MustCompile(`^-?[0-9]+$`).Match(raw) {
		bad("has skipLines %s, not a whole number", raw)
	} else if c.skipLines, _ = strconv.ParseInt(string(raw), 10, 64); c.skipLines != 0 {
		bad("skips %d lines: the files' first line is their header", c.skipLines)
	}
	if raw, ok := top["isFirstLineHeader"]; !ok || json.Unmarshal(raw, &c.firstLineHeader) != nil {
		bad("has isFirstLineHeader %s, not a boolean", raw)
	} else if !c.firstLineHeader {
		bad("says the first line is no header: the files' first line is their header")
	}
	var columns []map[string]json.RawMessage
	if raw, ok := top["columns"]; !ok || json.Unmarshal(raw, &columns) != nil || len(columns) == 0 {
		bad("has no columns")
		return nil, probs
	}
	fatal := false
	seen := map[string]bool{}
	for i, col := range columns {
		where := fmt.Sprintf("column %d", i+1)
		for _, k := range slices.Sorted(maps.Keys(col)) {
			if !slices.Contains([]string{"label", "field", "format"}, k) {
				bad("%s holds the key %q, which PP's columns have not", where, k)
			}
		}
		var cc ppColumn
		if cc.label, ok = str(col, "label"); !ok {
			bad("%s has no label", where)
			fatal = true
		}
		cc.field, _ = str(col, "field")
		format, hasFormat := str(col, "format")
		cc.format = format
		c.columns = append(c.columns, cc)
		if !hasTarget {
			continue
		}
		f, ok := target.field(cc.field)
		switch {
		case cc.field == "":
			bad("%s (%s) has no field: its cells would be lost", where, cc.label)
			continue
		case !ok:
			bad("%s (%s) has the field %q, which %s has not", where, cc.label, cc.field, want)
			continue
		case seen[cc.field]:
			bad("%s maps the field %s twice", where, cc.field)
		case f.kind == ppKindISIN:
			bad("%s maps an ISIN, which the export never writes", where)
		}
		seen[cc.field] = true
		if cc.label != f.label {
			bad("%s is labelled %q, but its field %s is PP's %q", where, cc.label, cc.field, f.label)
		}
		switch f.kind {
		case ppKindText, ppKindISIN:
			if hasFormat && f.kind == ppKindText {
				bad("%s (%s) has a format, which a text field must not have", where, cc.label)
				fatal = true
			}
		case ppKindDate:
			if format != "yyyy-MM-dd" {
				bad("%s (%s) has the format %q, not yyyy-MM-dd", where, cc.label, format)
				fatal = fatal || !hasFormat
			}
		case ppKindAmount:
			if format != "0,000.00" {
				bad("%s (%s) has the format %q, not 0,000.00", where, cc.label, format)
				fatal = fatal || !hasFormat
			}
		case ppKindType:
			probs = append(probs, ppCheckTypeMap(name+": "+where, format, target.types)...)
		}
	}
	for _, f := range target.fields {
		if f.required && !seen[f.code] {
			bad("does not map %s, which %s requires", f.code, want)
		}
	}
	if fatal {
		return nil, probs
	}
	return c, probs
}

// ppCheckTypeMap checks the format of a Type column, which PP reads as
// ENUM=label pairs joined by ";": an entry without exactly one "=", or of a
// type that its target lacks, is skipped (CSVImporter.java:623-648), and a
// type not listed keeps its label in PP's language. So each entry must be
// one type of types and a label, every type once, every label once.
func ppCheckTypeMap(where, format string, types []string) []string {
	var probs []string
	bad := func(f string, args ...any) { probs = append(probs, where+": "+fmt.Sprintf(f, args...)) }
	listed, labels := map[string]bool{}, map[string]bool{}
	for e := range strings.SplitSeq(format, ";") {
		k, v, ok := strings.Cut(e, "=")
		switch {
		case !ok || strings.Contains(v, "=") || k == "" || v == "":
			bad("the type entry %q is not ENUM=label", e)
		case !slices.Contains(types, k):
			bad("the type entry %q names no type of its target", e)
		case listed[k] || labels[v]:
			bad("the type entry %q repeats a type or a label", e)
		default:
			listed[k], labels[v] = true, true
		}
	}
	for _, k := range types {
		if !listed[k] {
			bad("the types do not list %s", k)
		}
	}
	return probs
}

// A ppRow is a row of a file below its header, as PP reads it through a
// configuration: its line, and its cells by field code.
type ppRow struct {
	line  int
	cells map[string]string
}

// ppReadCSV reads the file name, whose bytes are data, as PP reads a CSV
// through configuration c (CSVImporter.java:935-993): Commons CSV with the
// configuration's delimiter and RFC 4180 quotes, skipLines lines skipped,
// the first line taken as the header, and each cell given to the field of
// its column by position, the header not consulted (CSVConfig.java:133-170).
// It returns the rows, and what breaks the export's form: anything but UTF-8
// with LF line ends and no byte order mark, a header other than the
// configuration's labels, a row of another width, or a cell that PP would
// trim (BaseCSVExtractor.java:87).
func ppReadCSV(name string, data []byte, c *ppConfig) ([]ppRow, []string) {
	var probs []string
	bad := func(format string, args ...any) { probs = append(probs, name+": "+fmt.Sprintf(format, args...)) }
	switch {
	case !utf8.Valid(data):
		bad("is not UTF-8")
	case bytes.HasPrefix(data, []byte("\xef\xbb\xbf")):
		bad("starts with a byte order mark")
	case bytes.ContainsRune(data, '\r'):
		bad("holds a CR: its lines must end with LF alone")
	case len(data) > 0 && data[len(data)-1] != '\n':
		bad("does not end with a line end")
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		bad("cannot be parsed: %v", err)
		return nil, probs
	}
	if int64(len(records)) < c.skipLines+1 {
		bad("has no header")
		return nil, probs
	}
	records = records[c.skipLines:]
	if !slices.Equal(records[0], c.labels()) {
		bad("has the header %q, not its configuration's labels %q", records[0], c.labels())
	}
	var rows []ppRow
	for i, rec := range records[1:] {
		line := i + 1
		if len(rec) != len(c.columns) {
			bad("row %d has %d cells, not %d", line, len(rec), len(c.columns))
			continue
		}
		row := ppRow{line: line, cells: map[string]string{}}
		for j, cell := range rec {
			if ppTrim(cell) != cell {
				bad("row %d: the %s %q starts or ends with a space, which PP trims", line, c.columns[j].label, cell)
			}
			if c.columns[j].field != "" {
				row.cells[c.columns[j].field] = ppTrim(cell)
			}
		}
		rows = append(rows, row)
	}
	return rows, probs
}

// The forms of a PP export's cells: a day, a time of New York's clocks, an
// amount in dollars and cents, and a quantity or a price with up to 8
// places.
var (
	ppDayForm      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	ppTimeForm     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	ppCentsForm    = regexp.MustCompile(`^[0-9]+\.[0-9]{2}$`)
	ppQuantityForm = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,8})?$`)
)

// ppDay reports whether s is a day of the calendar written yyyy-MM-dd: PP's
// lenient date format would take 2025-02-30 for March 2.
func ppDay(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return ppDayForm.MatchString(s) && err == nil
}

// ppRat returns s, an amount or quantity in one of the forms above, exactly.
func ppRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// ppSuffix returns the suffix of the files of the account named name,
// "Schwab A1 ...135" being A1-135, and "" for a name that is not an
// account's of the export's form.
func ppSuffix(name string) string {
	m := regexp.MustCompile(`^Schwab (A[0-9]+)(?: (.*))?$`).FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	digits := regexp.MustCompile(`[^0-9]`).ReplaceAllString(m[2], "")
	if digits == "" {
		return m[1]
	}
	return m[1] + "-" + digits
}

// ppProblems returns what is wrong with the folder dir, a PP export whose
// accounts are named names, read as PP 0.90.0 reads it (11.4 of the design):
// its three configurations; each file through its configuration, in the
// order to import them; each cell's form; each record by its type, as PP's
// extractors and review page take it (CSVAccountTransactionExtractor.java,
// CSVPortfolioTransactionExtractor.java, CSVSecurityExtractor.java,
// actions/Check*.java) and as the export means it: a Buy, a Sell or a
// delivery only in the portfolio file, with a security and Shares; a
// Dividend with a security; no security on a Deposit, a Withdrawal,
// Interest, an Interest Charge or a Transfer; Fees only on a Dividend, a Buy
// or a Sell, and on a Buy no more than its Value; the file's account in Cash
// Account and Securities Account; and on a Transfer, an Offset Account that
// names another account of the export, since PP falls back to an account
// of its own choosing for a name it lacks. And a delivery's value must be
// within a factor of 3 of its security's nearest price (a trade's or a
// quote), unless it is 0, which catches a bond's price taken per dollar of
// face rather than per 100.
func ppProblems(dir string, names []string) []string {
	var probs []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{err.Error()}
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			probs = append(probs, err.Error())
		}
		return b
	}
	configs := map[string]*ppConfig{}
	for _, name := range slices.Sorted(maps.Keys(ppKinds)) {
		if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == name }) {
			probs = append(probs, name+" is missing")
			continue
		}
		c, p := ppParseConfig(name, read(name), ppKinds[name].target)
		configs[name] = c
		probs = append(probs, p...)
	}
	var files []string
	for _, e := range entries {
		switch name := e.Name(); {
		case ppKinds[name].kind != "" || name == "manifest.json":
		case ppConfigOf(name) == "":
			probs = append(probs, name+" is no file that a configuration reads")
		default:
			files = append(files, name)
		}
	}
	if !slices.Contains(files, ppSecuritiesFile) {
		probs = append(probs, ppSecuritiesFile+" is missing")
	}
	files = ppImportOrder(files)

	ck := &ppChecker{names: names, prices: map[*ppSecurity][]ppPrice{}, listed: map[*ppSecurity]int{}, unpriced: map[*ppSecurity]int{}}
	for _, name := range files {
		c := configs[ppConfigOf(name)]
		if c == nil {
			probs = append(probs, name+" has no configuration that PP can apply")
			continue
		}
		rows, p := ppReadCSV(name, read(name), c)
		probs = append(probs, p...)
		account := ""
		if name != ppSecuritiesFile {
			suffix := strings.TrimSuffix(name[strings.Index(name, "-transactions-")+len("-transactions-"):], ".csv")
			i := slices.IndexFunc(names, func(n string) bool { return ppSuffix(n) == suffix })
			if i < 0 {
				probs = append(probs, name+" is of no account of the export")
				continue
			}
			account = names[i]
		}
		probs = append(probs, ck.file(name, ppKinds[ppConfigOf(name)].kind, account, c, rows)...)
	}
	return append(probs, ck.plausible()...)
}

// ppImportOrder returns the names of a PP export's files in the order to
// import them (5.1 of the design): securities.csv, then each account's
// account file and portfolio file, accounts in the order of their ids.
func ppImportOrder(names []string) []string {
	rank := func(name string) (int, int64, int) {
		if name == ppSecuritiesFile {
			return 0, 0, 0
		}
		m := regexp.MustCompile(`^(account|portfolio)-transactions-A([0-9]+)`).FindStringSubmatch(name)
		if m == nil {
			return 2, 0, 0
		}
		id, _ := strconv.ParseInt(m[2], 10, 64)
		if m[1] == ppAccount {
			return 1, id, 0
		}
		return 1, id, 1
	}
	return slices.SortedFunc(slices.Values(names), func(a, b string) int {
		ga, ia, ka := rank(a)
		gb, ib, kb := rank(b)
		return cmp.Or(cmp.Compare(ga, gb), cmp.Compare(ia, ib), cmp.Compare(ka, kb), strings.Compare(a, b))
	})
}

// A ppChecker checks a PP export's files one after another, in the order to
// import them, finding each row's security as PP would, so that the prices
// of a security in one file check its deliveries in another, and a record
// that finds no security that securities.csv made names it otherwise than
// securities.csv does.
type ppChecker struct {
	names      []string
	securities []*ppSecurity
	// listed counts the rows of securities.csv of each security, and
	// unpriced those of them without a price.
	listed, unpriced map[*ppSecurity]int
	// prices are each security's prices, from its trades and quotes;
	// deliveries the deliveries with a value, to check against them.
	prices     map[*ppSecurity][]ppPrice
	deliveries []ppDelivery
}

// A ppPrice is a security's price on a day, in dollars per share.
type ppPrice struct {
	day   string
	price *big.Rat
}

// A ppDelivery is a delivery with a value, to check against its security's
// prices: where it is, its security, its day and its price per share.
type ppDelivery struct {
	where    string
	security *ppSecurity
	ppPrice
}

// file checks the rows of the file name, of kind, read through its
// configuration c; account is the name of the account whose records it
// holds, "" for securities.csv.
func (ck *ppChecker) file(name, kind, account string, c *ppConfig, rows []ppRow) []string {
	var probs []string
	cache := newPPCache(ck.securities)
	types := c.typeLabels()
	for _, r := range rows {
		where := fmt.Sprintf("%s row %d", name, r.line)
		bad := func(format string, args ...any) { probs = append(probs, where+": "+fmt.Sprintf(format, args...)) }
		cell := r.cells
		var security *ppSecurity
		if cell["ticker"] != "" || cell["wkn"] != "" || cell["name"] != "" {
			s, created, err := cache.lookup(cell["ticker"], cell["wkn"], cell["name"])
			if err != nil {
				bad("%v", err)
				continue
			}
			if created {
				ck.securities = append(ck.securities, s)
				if kind != ppSecurities {
					bad("names a security that %s does not list: %q %q %q", ppSecuritiesFile, cell["ticker"], cell["wkn"], cell["name"])
				}
			}
			security = s
		}
		if strings.Contains(cell["ticker"], ".") {
			bad("the Ticker Symbol %q holds a dot, before which alone PP compares tickers", cell["ticker"])
		}
		if cell["currency"] != ppCurrency {
			bad("the currency is %q, not USD", cell["currency"])
		}
		if kind == ppSecurities {
			if security != nil {
				ck.listed[security]++
				if cell["date"] == "" && cell["quote"] == "" {
					ck.unpriced[security]++
				}
				if ck.unpriced[security] > 0 && ck.listed[security] > 1 {
					bad("lists its security again, which a row without a price lists alone")
				}
			}
			switch {
			case security == nil:
				bad("names no security")
			case cell["date"] == "" && cell["quote"] == "":
			case !ppDay(cell["date"]):
				bad("the Date of Quote %q is not a day of the form yyyy-MM-dd", cell["date"])
			case !ppQuantityForm.MatchString(cell["quote"]) || ppRat(cell["quote"]).Sign() <= 0:
				bad("the Quote %q is not a price above 0 with up to 8 places", cell["quote"])
			default:
				ck.prices[security] = append(ck.prices[security], ppPrice{cell["date"], ppRat(cell["quote"])})
			}
			continue
		}
		probs = append(probs, ck.record(where, kind, account, types, security, cell)...)
	}
	return probs
}

// record checks a row of an account or a portfolio file, of the account
// named account, whose security is security (nil for none).
func (ck *ppChecker) record(where, kind, account string, types map[string]string, security *ppSecurity, cell map[string]string) []string {
	var probs []string
	bad := func(format string, args ...any) { probs = append(probs, where+": "+fmt.Sprintf(format, args...)) }
	if !ppDay(cell["date"]) {
		bad("the Date %q is not a day of the form yyyy-MM-dd", cell["date"])
	}
	if t := cell["time"]; t != "" && !ppTimeForm.MatchString(t) {
		bad("the Time %q is not HH:mm", t)
	}
	if !ppCentsForm.MatchString(cell["value"]) {
		bad("the Value %q is not dollars and cents, unsigned", cell["value"])
	}
	fees := cell["fees"]
	if fees != "" && (!ppCentsForm.MatchString(fees) || ppRat(fees).Sign() == 0) {
		bad("the Fees %q are not dollars and cents above 0", fees)
	}
	if !strings.HasPrefix(cell["note"], "schwabtx ") {
		bad("the Note %q does not name the row", cell["note"])
	}
	typ, ok := types[cell["type"]]
	if !ok {
		bad("the Type %q is no label of its configuration", cell["type"])
		return probs
	}
	switch kind {
	case ppPortfolio:
		shares := cell["shares"]
		switch typ {
		case "BUY", "SELL", "DELIVERY_INBOUND", "DELIVERY_OUTBOUND":
		default:
			bad("a row of Type %s is not written to the portfolio file", cell["type"])
		}
		if security == nil {
			bad("a row of Type %s names no security", cell["type"])
		}
		if !ppQuantityForm.MatchString(shares) || ppRat(shares).Sign() <= 0 {
			bad("a row of Type %s has the Shares %q, not a quantity above 0 with up to 8 places", cell["type"], shares)
		}
		buySell := typ == "BUY" || typ == "SELL"
		switch {
		case fees != "" && !buySell:
			bad("a row of Type %s has Fees, which only a Buy or a Sell has", cell["type"])
		case fees != "" && typ == "BUY" && ppRat(fees).Cmp(ppRat(cell["value"])) > 0:
			bad("a Buy's Fees %s exceed its Value %s", fees, cell["value"])
		}
		if cell["portfolio"] != account {
			bad("the Securities Account %q is not the file's, %q", cell["portfolio"], account)
		}
		if buySell && cell["account"] != account || !buySell && cell["account"] != "" {
			bad("a row of Type %s has the Cash Account %q", cell["type"], cell["account"])
		}
		if probs != nil || security == nil {
			return probs
		}
		price := new(big.Rat).Quo(ppRat(cell["value"]), ppRat(shares))
		if buySell {
			ck.prices[security] = append(ck.prices[security], ppPrice{cell["date"], price})
		} else if price.Sign() > 0 {
			ck.deliveries = append(ck.deliveries, ppDelivery{where, security, ppPrice{cell["date"], price}})
		}
	default:
		transfer := typ == "TRANSFER_IN" || typ == "TRANSFER_OUT"
		switch typ {
		case "BUY", "SELL":
			bad("a row of Type %s is written to the portfolio file, not the account file", cell["type"])
		case "DIVIDENDS":
			if security == nil {
				bad("a row of Type Dividend names no security")
			}
		case "DEPOSIT", "REMOVAL", "INTEREST", "INTEREST_CHARGE", "TRANSFER_IN", "TRANSFER_OUT":
			if security != nil {
				bad("a row of Type %s names a security, which PP refuses on it", cell["type"])
			}
		}
		if fees != "" && typ != "DIVIDENDS" {
			bad("a row of Type %s has Fees, which in the account file only a Dividend has", cell["type"])
		}
		if cell["account"] != account {
			bad("the Cash Account %q is not the file's, %q", cell["account"], account)
		}
		offset := cell["account2nd"]
		switch {
		case !transfer && offset != "":
			bad("a row of Type %s has the Offset Account %q", cell["type"], offset)
		case transfer && (offset == account || !slices.Contains(ck.names, offset)):
			bad("a row of Type %s has the Offset Account %q, which names no other account of the export", cell["type"], offset)
		}
	}
	return probs
}

// plausible checks each delivery with a value against the nearest price of
// its security, by day: within a factor of 3.
func (ck *ppChecker) plausible() []string {
	var probs []string
	for _, d := range ck.deliveries {
		var nearest *ppPrice
		gap := math.MaxInt
		for _, p := range ck.prices[d.security] {
			a, _ := time.Parse(time.DateOnly, d.day)
			b, _ := time.Parse(time.DateOnly, p.day)
			if g := int(math.Abs(a.Sub(b).Hours())); g < gap {
				nearest, gap = &p, g
			}
		}
		if nearest == nil {
			continue
		}
		ratio := new(big.Rat).Quo(d.price, nearest.price)
		if ratio.Cmp(big.NewRat(1, 3)) < 0 || ratio.Cmp(big.NewRat(3, 1)) > 0 {
			probs = append(probs, fmt.Sprintf("%s: a delivery at %s a share is not within a factor of 3 of its security's price of %s on %s",
				d.where, d.price.FloatString(4), nearest.price.FloatString(4), nearest.day))
		}
	}
	return probs
}

// ppCheck requires that PP 0.90.0 import the folder dir, a PP export whose
// accounts are named names, as the export means it (ppProblems).
func ppCheck(t *testing.T, dir string, names []string) {
	t.Helper()
	for _, p := range ppProblems(dir, names) {
		t.Error(p)
	}
}

// A ppSecurity is a security of a PP file: its identifiers and its currency,
// and its prices by day, in 10^-8 dollars.
type ppSecurity struct {
	ticker, wkn, name, currency string
	prices                      map[string]int64
}

// ppDuplicate marks an identifier that two securities of a PP file share
// (SecurityCache.java:23,37-49).
var ppDuplicate = &ppSecurity{}

// A ppCache finds a row's security as PP's SecurityCache does
// (datatransfer/SecurityCache.java:33-159): by ticker, compared up to its
// first dot (model/Security.java:280-287), then WKN, the first that finds
// one winning even when the other differs; then by exact name, when the
// row's ticker and WKN do not differ from the security's; else it makes a
// new one, which later rows of the file find. An identifier that two
// securities share finds neither, and fails the row unless another finds
// one. The export writes no ISIN, which PP tries first.
type ppCache struct {
	byTicker, byWKN, byName map[string]*ppSecurity
}

// newPPCache returns the cache of an import into a PP file that holds
// securities.
func newPPCache(securities []*ppSecurity) *ppCache {
	c := &ppCache{byTicker: map[string]*ppSecurity{}, byWKN: map[string]*ppSecurity{}, byName: map[string]*ppSecurity{}}
	put := func(m map[string]*ppSecurity, k string, s *ppSecurity) {
		if k == "" {
			return
		}
		if _, ok := m[k]; ok {
			m[k] = ppDuplicate
		} else {
			m[k] = s
		}
	}
	for _, s := range securities {
		put(c.byTicker, ppTickerPrefix(s.ticker), s)
		put(c.byWKN, s.wkn, s)
		put(c.byName, s.name, s)
	}
	return c
}

// ppTickerPrefix returns a ticker up to its first dot, as PP compares
// tickers.
func ppTickerPrefix(t string) string {
	before, _, _ := strings.Cut(t, ".")
	return before
}

// lookup returns the security of a row whose Ticker Symbol, WKN and
// Security Name are ticker, wkn and name, and whether the row made it; or
// the error with which PP fails the row.
func (c *ppCache) lookup(ticker, wkn, name string) (*ppSecurity, bool, error) {
	if name == "" {
		// BaseCSVExtractor.java:138-149.
		name = "Imported security: " + cmp.Or(ticker, wkn)
	}
	duplicate := ""
	for _, try := range []struct {
		what, key string
		m         map[string]*ppSecurity
	}{{"ticker", ppTickerPrefix(ticker), c.byTicker}, {"WKN", wkn, c.byWKN}} {
		s := try.m[try.key]
		if try.key == "" || s == nil {
			continue
		}
		if s != ppDuplicate {
			return s, false, nil
		}
		if duplicate == "" {
			duplicate = try.what + " " + try.key
		}
	}
	if duplicate != "" {
		return nil, false, fmt.Errorf("the %s names several securities", duplicate)
	}
	if s := c.byName[name]; s != nil && s != ppDuplicate {
		differs := func(a, b string) bool { return a != "" && b != "" && !strings.EqualFold(a, b) }
		if !differs(ticker, s.ticker) && !differs(ppTickerPrefix(ticker), ppTickerPrefix(s.ticker)) && !differs(wkn, s.wkn) {
			return s, false, nil
		}
	}
	s := &ppSecurity{ticker: ticker, wkn: wkn, name: name, prices: map[string]int64{}}
	for _, put := range []struct {
		m map[string]*ppSecurity
		k string
	}{{c.byTicker, ppTickerPrefix(ticker)}, {c.byWKN, wkn}, {c.byName, name}} {
		if put.k != "" {
			put.m[put.k] = s
		}
	}
	return s, true, nil
}

// A ppModel is a PP file, as far as importing a PP export moves it (11.4 of
// the design): its deposit accounts and securities accounts, which an
// import never makes, its securities, and the transactions that the
// imports made, each with the other half of its pair: a buy or a sale's
// account and portfolio transactions, a transfer's two legs. Its base
// currency is the euro, a PP file's in its default locale, so that a row
// whose currency PP does not read is refused against the US dollar
// accounts. Its methods importFile and deleteAccount are the design's
// ppImport and ppDelete.
type ppModel struct {
	accounts, portfolios []*ppOwner
	securities           []*ppSecurity
}

// A ppOwner is a deposit account or a securities account of a ppModel, with
// its transactions.
type ppOwner struct {
	name string
	txs  []*ppTx
}

// A ppTx is a transaction of a ppModel: its type (PP's enum name: DEPOSIT,
// REMOVAL, BUY, DELIVERY_INBOUND, ...), its day, currency and amount in
// cents, its shares in 10^-8, its security, its note, its fees and taxes in
// cents, the account or securities account that holds it, and the other half
// of its pair, nil for none.
type ppTx struct {
	typ, day, currency string
	amount, shares     int64
	security           *ppSecurity
	note               string
	fees, taxes        int64
	owner              *ppOwner
	other              *ppTx
}

// newPPModel returns a PP file with a deposit account in US dollars and a
// securities account named each of names, as the export's how-to has you
// make them before the first import.
func newPPModel(names ...string) *ppModel {
	m := &ppModel{}
	for _, n := range names {
		m.make(n)
	}
	return m
}

// make makes in m a deposit account in US dollars and a securities account
// named name, as you make them in PP before an import that names them.
func (m *ppModel) make(name string) {
	m.accounts = append(m.accounts, &ppOwner{name: name})
	m.portfolios = append(m.portfolios, &ppOwner{name: name})
}

// ppOwnerNamed returns the account or securities account of owners named
// name, nil for none (CSVExtractor.java:218-254: an exact name).
func ppOwnerNamed(owners []*ppOwner, name string) *ppOwner {
	i := slices.IndexFunc(owners, func(o *ppOwner) bool { return o.name == name })
	if i < 0 {
		return nil
	}
	return owners[i]
}

// ppDefaultTypes are the labels of the types in a PP whose language is
// German (model/labels_de.properties), which a Type column's map replaces
// only for the types it lists.
var ppDefaultTypes = map[string]string{
	"DEPOSIT": "Einlage", "REMOVAL": "Entnahme", "INTEREST": "Zinsen", "INTEREST_CHARGE": "Zinsbelastung", "DIVIDENDS": "Dividende",
	"FEES": "Gebühren", "FEES_REFUND": "Gebührenerstattung", "TAXES": "Steuern", "TAX_REFUND": "Steuerrückerstattung",
	"BUY": "Kauf", "SELL": "Verkauf", "TRANSFER_IN": "Umbuchung (Eingang)", "TRANSFER_OUT": "Umbuchung (Ausgang)",
	"DELIVERY_INBOUND": "Einlieferung", "DELIVERY_OUTBOUND": "Auslieferung",
}

// A ppItem is what PP's extractor makes of a row, which the review page
// checks and the import inserts: a transaction of an account or a
// securities account, a buy or sale (txs holds its account side, then its
// portfolio side), or a transfer (its paying leg, then its receiving leg).
type ppItem struct {
	line int
	txs  []*ppTx
}

// importFile imports the file name of the PP export in dir into m, as PP
// 0.90.0's CSV import does through the file's configuration, with the
// review page set to the account named page, the file's own, as its deposit
// account and securities account; the rows of ticks (from 1, below the
// header) are ticked on the review page. It returns the rows that the
// review page marks as existing (DetectDuplicatesAction.java), which it
// imports only when ticked, and what PP refuses, of the file as a whole or
// of a row, which it does not import.
func (m *ppModel) importFile(dir, name, page string, ticks map[int]bool) (marked []int, refused []string) {
	marked, _, refused = m.importRows(dir, name, page, ticks, false)
	return marked, refused
}

// importTicking imports the file name of the PP export in dir into m as
// importFile does, and ticks too each row that the review page marks for
// what you booked by hand alone (ppByHand), as the how-to has you (step 3):
// it returns those rows apart, byHand, among the rows marked.
func (m *ppModel) importTicking(dir, name, page string, ticks map[int]bool) (marked, byHand []int, refused []string) {
	return m.importRows(dir, name, page, ticks, true)
}

// ppByHand reports whether PP holds tx by what you booked by hand: its Note
// is not one that the export writes (ppNote), which begins "schwabtx ".
func ppByHand(tx *ppTx) bool {
	return !strings.HasPrefix(tx.note, "schwabtx ")
}

// importRows imports the file name of the PP export in dir into m as
// importFile does, and, with tickHand, as importTicking does. It returns
// the rows marked, those of them marked for what you booked by hand alone,
// and what PP refuses.
func (m *ppModel) importRows(dir, name, page string, ticks map[int]bool, tickHand bool) (marked, byHand []int, refused []string) {
	bad := func(format string, args ...any) { refused = append(refused, name+": "+fmt.Sprintf(format, args...)) }
	cfgName := ppConfigOf(name)
	if cfgName == "" {
		bad("no configuration reads it")
		return nil, nil, refused
	}
	cfgData, err := os.ReadFile(filepath.Join(dir, cfgName))
	if err != nil {
		bad("%v", err)
		return nil, nil, refused
	}
	c, err := ppApplyConfig(cfgData)
	if err != nil {
		bad("%s: %v", cfgName, err)
		return nil, nil, refused
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		bad("%v", err)
		return nil, nil, refused
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		bad("cannot be parsed: %v", err)
		return nil, nil, refused
	}
	if int64(len(records)) > c.skipLines {
		records = records[c.skipLines:]
	} else {
		records = nil
	}
	if c.firstLineHeader && len(records) > 0 {
		records = records[1:]
	}
	types := maps.Clone(ppDefaultTypes)
	for label, typ := range c.typeLabels() {
		types[typ] = label
	}

	cache := newPPCache(m.securities)
	var items []ppItem
	var made []*ppSecurity
	var prices []func()
	for i, rec := range records {
		line := i + 1
		cell := func(field string) string {
			j := slices.IndexFunc(c.columns, func(col ppColumn) bool { return col.field == field })
			if j < 0 || j >= len(rec) {
				return ""
			}
			return ppTrim(rec[j])
		}
		item, security, price, err := m.extract(c, types, cell, cache, page)
		if err != nil {
			if fatal := ppFatal(""); errors.As(err, &fatal) {
				bad("row %d stops the import: %s", line, string(fatal))
				return nil, nil, refused
			}
			bad("row %d: %v", line, err)
			continue
		}
		if security != nil && !slices.Contains(m.securities, security) && !slices.Contains(made, security) {
			made = append(made, security)
		}
		if price != nil {
			prices = append(prices, price)
		}
		if item.txs != nil {
			item.line = line
			items = append(items, item)
		}
	}

	// The review page checks every row against what the file held before
	// the import; then the import inserts what is not refused, and what is
	// marked only when ticked.
	var insert []ppItem
	for _, it := range items {
		if err := m.review(it); err != nil {
			bad("row %d: %v", it.line, err)
			continue
		}
		if holders := m.holders(it); len(holders) > 0 {
			marked = append(marked, it.line)
			hand := !slices.ContainsFunc(holders, func(tx *ppTx) bool { return !ppByHand(tx) })
			if hand {
				byHand = append(byHand, it.line)
			}
			if !ticks[it.line] && (!tickHand || !hand) {
				continue
			}
		}
		insert = append(insert, it)
	}
	m.securities = append(m.securities, made...)
	for _, p := range prices {
		p()
	}
	for _, it := range insert {
		for _, tx := range it.txs {
			tx.owner.txs = append(tx.owner.txs, tx)
		}
	}
	return marked, byHand, refused
}

// A ppFatal is a failure that stops PP's import of a whole file, not of a
// row: the exception escapes the extractor's catch of a row's errors
// (BaseCSVExtractor.java:89-97).
type ppFatal string

// Error returns the failure.
func (f ppFatal) Error() string { return string(f) }

// ppApplyConfig reads a configuration as PP applies it
// (CSVConfig.java:133-170,213-262; CSVImporter.java:236-244,389-405,
// 570-586,623-648): the error is what makes reading or applying it fail.
// The model reads dates only as yyyy-MM-dd, and fails on any other format.
func ppApplyConfig(data []byte) (*ppConfig, error) {
	var top struct {
		Label             *string `json:"label"`
		Target            *string `json:"target"`
		Delimiter         *string `json:"delimiter"`
		SkipLines         *int64  `json:"skipLines"`
		IsFirstLineHeader *bool   `json:"isFirstLineHeader"`
		Columns           []struct {
			Label  *string `json:"label"`
			Field  string  `json:"field"`
			Format *string `json:"format"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	switch {
	case top.Label == nil || top.Target == nil || top.Delimiter == nil || *top.Delimiter == "" || top.SkipLines == nil ||
		top.IsFirstLineHeader == nil:
		return nil, errors.New("a key that PP requires is missing")
	case len(top.Columns) == 0:
		return nil, errors.New("no columns")
	}
	target, ok := ppTargets[*top.Target]
	if !ok {
		return nil, fmt.Errorf("the model has no target %q", *top.Target)
	}
	c := &ppConfig{label: *top.Label, target: *top.Target, delimiter: *top.Delimiter, skipLines: *top.SkipLines, firstLineHeader: *top.IsFirstLineHeader}
	if !strings.ContainsAny((*top.Delimiter)[:1], ",;\t") {
		c.delimiter = ";"
	}
	if c.delimiter != "," {
		return nil, fmt.Errorf("the model reads only commas, not %q", c.delimiter)
	}
	for _, col := range top.Columns {
		if col.Label == nil {
			return nil, errors.New("a column has no label")
		}
		cc := ppColumn{label: *col.Label}
		f, ok := target.field(col.Field)
		if ok {
			// A field that the target lacks leaves its column unread.
			cc.field = col.Field
		}
		switch {
		case !ok:
		case col.Format != nil && f.kind == ppKindText:
			return nil, fmt.Errorf("the text field %s has a format", f.code)
		case col.Format == nil && (f.kind == ppKindDate || f.kind == ppKindAmount):
			// The format guessed is dropped, and reading a cell of the field
			// fails on the format missing.
			cc.format = "missing"
		case f.kind == ppKindDate && *col.Format != "yyyy-MM-dd":
			return nil, fmt.Errorf("the model reads dates only as yyyy-MM-dd, not %q", *col.Format)
		case f.kind == ppKindAmount && *col.Format != "0,000.00":
			cc.format = "0.000,00"
		case col.Format != nil:
			cc.format = *col.Format
		}
		c.columns = append(c.columns, cc)
	}
	return c, nil
}

// ppAmount reads an amount's cell as PP does through the amount format of
// its column (CSVImporter.java:466-537, CSVExtractor.java:98-117): the
// format's decimal separator becomes a point, digits and minus signs are
// kept, any other character is dropped, and the number is read as a double
// and rounded to whole units of 10^-places. A cell of no digits fails.
func ppAmount(cell, format string, places int) (int64, error) {
	if format == "missing" {
		return 0, ppFatal("the amount field has no format")
	}
	decimal := '.'
	if format == "0.000,00" {
		decimal = ','
	}
	var b strings.Builder
	digit := false
	for _, r := range cell {
		switch {
		case r == decimal:
			b.WriteRune('.')
		case (r == 'e' || r == 'E') && digit:
			b.WriteRune('E')
		case '0' <= r && r <= '9' || r == '-':
			b.WriteRune(r)
			digit = true
		}
	}
	v, err := strconv.ParseFloat(b.String(), 64)
	if err != nil {
		return 0, fmt.Errorf("the amount %q cannot be read", cell)
	}
	return int64(math.Round(v * math.Pow10(places))), nil
}

// extract makes what PP's extractor of the configuration's target makes of
// a row (CSVAccountTransactionExtractor.java:65-254,
// CSVPortfolioTransactionExtractor.java:64-215,
// CSVSecurityExtractor.java:65-127): an item, the row's security, and for
// securities.csv a price to add; or the error that fails the row.
func (m *ppModel) extract(c *ppConfig, types map[string]string, cell func(string) string, cache *ppCache, page string) (ppItem, *ppSecurity, func(), error) {
	var security *ppSecurity
	if ticker, wkn, name := cell("ticker"), cell("wkn"), cell("name"); ticker != "" || wkn != "" || name != "" {
		s, created, err := cache.lookup(ticker, wkn, name)
		if err != nil {
			return ppItem{}, nil, nil, err
		}
		if created {
			currency := cell("currency")
			if currency == "" || !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(currency) {
				currency = "EUR"
			}
			s.currency = currency
		}
		security = s
	}
	amount := func(field string, places int) (int64, bool, error) {
		j := slices.IndexFunc(c.columns, func(col ppColumn) bool { return col.field == field })
		if j < 0 || cell(field) == "" {
			return 0, false, nil
		}
		v, err := ppAmount(cell(field), c.columns[j].format, places)
		return v, true, err
	}
	if c.target == "investment-vehicle" {
		if security == nil {
			return ppItem{}, nil, nil, errors.New("no security")
		}
		quote, ok, err := amount("quote", 8)
		if err != nil || !ok {
			return ppItem{}, security, nil, err
		}
		// A quote without its date is today's (BaseCSVExtractor.java:176-188).
		day := cmp.Or(cell("date"), time.Now().Format(time.DateOnly))
		if !ppDay(day) {
			return ppItem{}, nil, nil, fmt.Errorf("the date %q cannot be read", day)
		}
		return ppItem{}, security, func() { security.prices[day] = max(quote, -quote) }, nil
	}
	if c.target == "portfolio-transaction" && security == nil {
		return ppItem{}, nil, nil, errors.New("no security")
	}
	value, ok, err := amount("value", 2)
	if err != nil {
		return ppItem{}, nil, nil, err
	}
	if !ok {
		return ppItem{}, nil, nil, errors.New("no Value")
	}
	currency := cell("currency")
	if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(currency) {
		currency = "EUR"
	}
	typ := ""
	if label := cell("type"); label != "" {
		for _, t := range ppTargets[c.target].types {
			if strings.EqualFold(label, types[t]) {
				typ = t
				break
			}
		}
		for _, t := range ppTargets[c.target].types {
			// A label that is no regular expression is passed over.
			if re, err := regexp.Compile(types[t]); typ == "" && err == nil && re.MatchString(label) {
				typ = t
			}
		}
		if typ == "" {
			return ppItem{}, nil, nil, fmt.Errorf("the type %q cannot be read", label)
		}
	}
	day := cell("date")
	switch j := slices.IndexFunc(c.columns, func(col ppColumn) bool { return col.field == "date" }); {
	case day == "":
		return ppItem{}, nil, nil, errors.New("no date")
	case c.columns[j].format == "missing":
		return ppItem{}, nil, nil, ppFatal("the date field has no format")
	case !ppDay(day):
		return ppItem{}, nil, nil, fmt.Errorf("the date %q cannot be read", day)
	}
	shares, hasShares, err := amount("shares", 8)
	if err != nil {
		return ppItem{}, nil, nil, err
	}
	shares = max(shares, -shares)
	fees, _, err := amount("fees", 2)
	if err != nil {
		return ppItem{}, nil, nil, err
	}
	taxes, _, err := amount("taxes", 2)
	if err != nil {
		return ppItem{}, nil, nil, err
	}
	fees, taxes = max(fees, -fees), max(taxes, -taxes)
	value = max(value, -value)
	note := cell("note")
	// A name that matches none leaves the review page's choice: the page's
	// account, or for a transfer's other side the first account of the
	// currency by name (Extractor.java:287-310,472-494;
	// ReviewExtractedItemsPage.java:584-603).
	account := cmp.Or(ppOwnerNamed(m.accounts, cell("account")), ppOwnerNamed(m.accounts, page))
	portfolio := cmp.Or(ppOwnerNamed(m.portfolios, cell("portfolio")), ppOwnerNamed(m.portfolios, page))
	tx := func(typ string, owner *ppOwner) *ppTx {
		return &ppTx{typ: typ, day: day, currency: currency, amount: value, security: security, note: note, owner: owner}
	}

	if c.target == "portfolio-transaction" {
		if typ == "" {
			// A negative value is a Buy, else a Sell.
			typ = "SELL"
			if strings.Contains(cell("value"), "-") {
				typ = "BUY"
			}
		}
		if !hasShares {
			// setShares(null) throws a NullPointerException.
			return ppItem{}, nil, nil, ppFatal("a row has no Shares")
		}
		switch typ {
		case "BUY", "SELL":
			a, p := tx(typ, account), tx(typ, portfolio)
			p.shares, p.fees, p.taxes = shares, fees, taxes
			a.other, p.other = p, a
			return ppItem{txs: []*ppTx{a, p}}, security, nil, nil
		case "DELIVERY_INBOUND", "DELIVERY_OUTBOUND":
			p := tx(typ, portfolio)
			p.shares, p.fees, p.taxes = shares, fees, taxes
			return ppItem{txs: []*ppTx{p}}, security, nil, nil
		}
		return ppItem{}, nil, nil, fmt.Errorf("the model does not take a %s of a portfolio file", typ)
	}

	if typ == "" {
		switch {
		case strings.Contains(cell("value"), "-"):
			typ = "REMOVAL"
		case security != nil:
			typ = "DIVIDENDS"
		default:
			typ = "DEPOSIT"
		}
	}
	switch typ {
	case "TRANSFER_IN", "TRANSFER_OUT":
		other := ppOwnerNamed(m.accounts, cell("account2nd"))
		if other == nil {
			names := make([]*ppOwner, len(m.accounts))
			copy(names, m.accounts)
			slices.SortFunc(names, func(a, b *ppOwner) int { return strings.Compare(a.name, b.name) })
			other = names[0]
		}
		source, target := account, other
		if typ == "TRANSFER_IN" {
			source, target = other, account
		}
		out, in := tx("TRANSFER_OUT", source), tx("TRANSFER_IN", target)
		out.security, in.security = nil, nil
		out.other, in.other = in, out
		return ppItem{txs: []*ppTx{out, in}}, nil, nil, nil
	case "BUY", "SELL":
		if security == nil || !hasShares {
			return ppItem{}, nil, nil, fmt.Errorf("a %s needs a security and Shares", typ)
		}
		a, p := tx(typ, account), tx(typ, portfolio)
		p.shares, p.fees, p.taxes = shares, fees, taxes
		a.other, p.other = p, a
		if typ == "SELL" && value == 0 {
			// A sale of value 0 is an outbound delivery
			// (CSVAccountTransactionExtractor.java:151-157).
			p.typ, p.other = "DELIVERY_OUTBOUND", nil
			return ppItem{txs: []*ppTx{p}}, security, nil, nil
		}
		return ppItem{txs: []*ppTx{a, p}}, security, nil, nil
	case "DIVIDENDS":
		if security == nil {
			return ppItem{}, nil, nil, errors.New("a Dividend needs a security")
		}
	}
	t := tx(typ, account)
	switch typ {
	case "DIVIDENDS":
		if hasShares {
			t.shares = shares
		}
		t.fees, t.taxes = fees, taxes
	case "INTEREST":
		t.taxes = taxes
	}
	if !slices.Contains([]string{"DIVIDENDS", "TAXES", "TAX_REFUND", "FEES", "FEES_REFUND"}, typ) {
		// The security, made from the row, is dropped.
		t.security = nil
		security = nil
	}
	return ppItem{txs: []*ppTx{t}}, security, nil, nil
}

// review returns what PP's review page refuses of item
// (ReviewExtractedItemsPage.java:654-662): a transaction whose currency is
// not its account's, or not its security's (CheckCurrenciesAction.java:
// 40-98,130-179), or a Buy or an inbound delivery whose fees and taxes exceed
// its value (:83-95). CheckValidTypesAction and
// CheckSecurityRelatedValuesAction refuse nothing that the extractors make
// of a CSV row: they drop a security, fees or taxes that a type cannot
// carry.
func (m *ppModel) review(it ppItem) error {
	for _, tx := range it.txs {
		if tx.owner == nil {
			return errors.New("no account takes it")
		}
		if slices.Contains(m.accounts, tx.owner) && tx.currency != ppCurrency {
			return fmt.Errorf("its currency %s is not its account's, USD", tx.currency)
		}
		if tx.security != nil && tx.security.currency != tx.currency && (tx.typ != "BUY" && tx.typ != "SELL" || slices.Contains(m.portfolios, tx.owner)) {
			return fmt.Errorf("its security %q is in %s, and it in %s", tx.security.name, tx.security.currency, tx.currency)
		}
		if slices.Contains(m.portfolios, tx.owner) && (tx.typ == "BUY" || tx.typ == "DELIVERY_INBOUND") && tx.fees+tx.taxes > tx.amount {
			return errors.New("its fees and taxes exceed its value")
		}
	}
	return nil
}

// holders returns the transactions for which PP's review page marks item
// as existing (DetectDuplicatesAction.java:30-185), none when it does not
// mark it: each of an equivalent type that the account or securities
// account holds already, of the same day, currency, amount, shares (a
// dividend without shares matching any) and security. A buy or sale is
// checked by its account side and its portfolio side; a transfer by its
// paying leg in the paying account.
func (m *ppModel) holders(it ppItem) []*ppTx {
	check := func(tx *ppTx) []*ppTx {
		portfolio := slices.Contains(m.portfolios, tx.owner)
		// class returns what a type is equivalent to: a deposit to an
		// inbound transfer and a withdrawal to an outbound one in an
		// account; a buy to an inbound delivery and a sale to an outbound
		// one in a securities account; any other type to itself alone.
		class := func(t string) string {
			switch {
			case !portfolio && (t == "DEPOSIT" || t == "TRANSFER_IN"), portfolio && (t == "BUY" || t == "DELIVERY_INBOUND"):
				return "in"
			case !portfolio && (t == "REMOVAL" || t == "TRANSFER_OUT"), portfolio && (t == "SELL" || t == "DELIVERY_OUTBOUND"):
				return "out"
			}
			return t
		}
		// interest reports whether two interest transactions of other
		// amounts are one, the one's taxes making up the difference
		// (DetectDuplicatesAction.java:168-178).
		interest := func(a, b *ppTx) bool {
			return a.typ == "INTEREST" && (a.taxes > 0 && b.taxes == 0 && a.amount+a.taxes == b.amount ||
				b.taxes > 0 && a.taxes == 0 && b.amount+b.taxes == a.amount)
		}
		var out []*ppTx
		for _, old := range tx.owner.txs {
			switch {
			case class(old.typ) != class(tx.typ):
			case old.day != tx.day || old.currency != tx.currency || old.security != tx.security:
			case old.amount != tx.amount && !interest(tx, old):
			case old.shares != tx.shares && (tx.typ != "DIVIDENDS" || tx.shares != 0 && old.shares != 0):
			default:
				out = append(out, old)
			}
		}
		return out
	}
	switch {
	case it.txs[0].typ == "TRANSFER_OUT":
		return check(it.txs[0])
	case len(it.txs) == 2:
		return append(check(it.txs[0]), check(it.txs[1])...)
	}
	return check(it.txs[0])
}

// deleteAccount deletes every transaction of the deposit account and the
// securities account named name, each with the other half of its pair,
// wherever that is (model/TransactionOwner.java:30-43).
func (m *ppModel) deleteAccount(name string) {
	gone := map[*ppTx]bool{}
	for _, o := range []*ppOwner{ppOwnerNamed(m.accounts, name), ppOwnerNamed(m.portfolios, name)} {
		if o == nil {
			continue
		}
		for _, tx := range o.txs {
			gone[tx] = true
			if tx.other != nil {
				gone[tx.other] = true
			}
		}
	}
	for _, o := range slices.Concat(m.accounts, m.portfolios) {
		o.txs = slices.DeleteFunc(o.txs, func(tx *ppTx) bool { return gone[tx] })
	}
}

// cash returns the cash of the deposit account named name, in cents: each
// transaction's amount, in or out by its type (model/AccountTransaction.java:
// 12-20).
func (m *ppModel) cash(name string) int64 {
	var sum int64
	o := ppOwnerNamed(m.accounts, name)
	if o == nil {
		return 0
	}
	for _, tx := range o.txs {
		switch tx.typ {
		case "REMOVAL", "INTEREST_CHARGE", "FEES", "TAXES", "BUY", "TRANSFER_OUT":
			sum -= tx.amount
		default:
			sum += tx.amount
		}
	}
	return sum
}

// shares returns the shares of each security that the securities account
// named name holds, in 10^-8 of a share: a buy or an inbound delivery adds
// them, a sale or an outbound delivery takes them away.
func (m *ppModel) shares(name string) map[*ppSecurity]int64 {
	out := map[*ppSecurity]int64{}
	o := ppOwnerNamed(m.portfolios, name)
	if o == nil {
		return out
	}
	for _, tx := range o.txs {
		switch tx.typ {
		case "BUY", "DELIVERY_INBOUND", "TRANSFER_IN":
			out[tx.security] += tx.shares
		default:
			out[tx.security] -= tx.shares
		}
	}
	return out
}

// ppCheckerInput is a PP export's input with a record of each kind that the
// export writes, in two accounts: a deposit, a withdrawal, a transfer, a buy
// and a sale with fees, a sale whose fees PP gets apart, a dividend with
// fees and without, a withholding and its reclaim, interest and an interest
// charge, a fee and its refund, a bond bought and delivered in, a delivery
// out, and an option sold and expired, which a snapshot prices.
// TestToolCheckersPP breaks its files one way at a time.
func ppCheckerInput() toolInput {
	row := func(ref string, account int64, at, kind, description, category, key, symbol string, quantity, amount, fees int64, price float64) toolRow {
		r := toolRow{
			ref: ref, account: account, source: "api", kind: kind, description: description, category: category,
			securityKey: key, symbol: symbol, amount: centsOf(amount), fees: centsOf(fees),
		}
		if strings.Contains(at, ":") {
			r.time = tcNY(at)
		} else {
			r.source, r.posted = "file", nytime.Date(at)
		}
		if quantity != 0 {
			r.quantity = centsOf(quantity)
		}
		if price != 0 {
			r.price = tcPrice(price)
		}
		return r
	}
	transferIn := row("T62000115", 1, "2025-04-02 08:00:00", "JOURNAL", "JOURNAL FRM ...398", "journal", "", "", 0, 10_000, 0, 0)
	transferOut := row("T62000201", 2, "2025-04-01 09:30:00", "JOURNAL", "JOURNAL TO ...135", "journal", "", "", 0, -10_000, 0, 0)
	transferIn.pairKind, transferIn.pairRef, transferIn.pairAccount = "journal", "T62000201", 2
	transferOut.pairKind, transferOut.pairRef, transferOut.pairAccount = "journal", "T62000115", 1
	return tcInput(toolInput{
		rows: []toolRow{
			row("T62000101", 1, "2025-01-02 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", "", "", 0, 1_000_000, 0, 0),
			row("T62000102", 1, "2025-01-03 10:00:00", "TRADE", "", "buy", keyXYZ, "XYZ", 10_000_000, -50_065, 65, 50),
			row("Cf6000001.1", 1, "2025-01-06", "Buy", "TREASURY NOTE", "buy", keyBondA, "9128ZZQ15", 5_000_000_000, -498_250, 0, 99.25),
			row("T62000203", 2, "2025-01-14 10:00:00", "TRADE", "", "sell", keyPut, symPut, -2_000_000, 31_870, 130, 1.6),
			row("Cf6000001.2", 1, "2025-02-03", "Journaled Shares", "TREASURY NOTE", "transfer_in", keyBondA, "9128ZZQ15", 1_000_000_000, 0, 0, 99.5),
			row("T62000105", 1, "2025-02-10 11:00:00", "DIVIDEND_OR_INTEREST", "Ordinary Dividend~XYZ", "dividend", keyXYZ, "XYZ", 0, 386, 0, 0),
			row("T62000106", 1, "2025-02-10 11:01:00", "JOURNAL", "W-8 Withholding~XYZ", "tax", keyXYZ, "XYZ", 0, -116, 0, 0),
			row("Cf6000001.3", 1, "2025-02-11", "Qualified Dividend", "XYZ CORP", "dividend", keyXYZ, "XYZ", 0, 950, 50, 0),
			row("Cf6000002.1", 2, "2025-02-21", "Expired", "PUT XYZ", "other", keyPut, symPut, 2_000_000, 0, 0, 0),
			row("T62000108", 1, "2025-02-28 21:00:00", "DIVIDEND_OR_INTEREST", "Credit Interest", "interest", "", "", 0, 42, 0, 0),
			row("Cf6000001.4", 1, "2025-03-03", "Margin Interest", "MARGIN INTEREST", "interest", "", "", 0, -317, 0, 0),
			row("Cf6000001.5", 1, "2025-03-05", "Service Fee", "SERVICE FEE", "fee", "", "", 0, -2500, 0, 0),
			row("Cf6000001.6", 1, "2025-03-06", "Service Fee", "SERVICE FEE REVERSAL", "fee", "", "", 0, 2500, 0, 0),
			row("Cf6000001.7", 1, "2025-03-07", "Foreign Tax Reclaim", "XYZ CORP", "tax", keyXYZ, "XYZ", 0, 80, 0, 0),
			row("T62000113", 1, "2025-03-10 14:00:00", "TRADE", "", "sell", keyXYZ, "XYZ", -4_000_000, 21_997, 3, 55),
			row("T62000114", 1, "2025-03-11 09:00:00", "ELECTRONIC_FUND", "ACH Debit", "withdrawal", "", "", 0, -25_000, 0, 0),
			transferOut, transferIn,
			row("T62000202", 2, "2025-04-03 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", "", "", 0, 50_000, 0, 0),
			row("Cf6000001.8", 1, "2025-06-02", "Security Transfer", "XYZ CORP", "transfer_out", keyXYZ, "XYZ", -1_000_000, 0, 0, 52),
			row("T62000117", 1, "2025-06-03 10:00:00", "TRADE", "", "sell", keyEFGH, "EFGH", -10_000, -5, 10, 5),
		},
		positions: []toolPosition{{
			key: keyPut, account: 2, marketDate: "2025-01-31", fetchedAt: tcNY("2025-01-31 18:05:00"),
			long: centsOf(0), short: centsOf(2_000_000), marketValue: tcPrice(-310),
		}},
	})
}

// A ppFolder is the files of a PP export, by name.
type ppFolder map[string][]byte

// ppFolderOf returns the files of the PP export of in.
func ppFolderOf(in toolInput) ppFolder {
	in.tool = toolPP
	events, _ := toolEvents(&in)
	f := ppFolder{}
	for _, file := range ppFiles(&in, events) {
		f[file.name] = file.data
	}
	return f
}

// write writes the files into a new folder, and returns it.
func (f ppFolder) write(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range f {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A ppEdit breaks a PP export's files.
type ppEdit func(t *testing.T, f ppFolder)

// ppReplace replaces old by with in the file name, once.
func ppReplace(name, old, with string) ppEdit {
	return func(t *testing.T, f ppFolder) {
		t.Helper()
		if !bytes.Contains(f[name], []byte(old)) {
			t.Fatalf("%s holds no %q", name, old)
		}
		f[name] = bytes.Replace(f[name], []byte(old), []byte(with), 1)
	}
}

// ppRecords edits the records of the file name: change gets them, the
// header first, and the index of the first row whose Note names ref.
func ppRecords(name, ref string, change func(records [][]string, i int) [][]string) ppEdit {
	return func(t *testing.T, f ppFolder) {
		t.Helper()
		records, err := csv.NewReader(bytes.NewReader(f[name])).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		note := slices.Index(records[0], "Note")
		i := slices.IndexFunc(records, func(r []string) bool {
			return note >= 0 && (strings.Contains(r[note], " "+ref+":") || strings.Contains(r[note], " "+ref+" "))
		})
		if ref == "" && len(records) > 1 {
			i = 1
		}
		if i < 0 {
			t.Fatalf("%s has no row of %s", name, ref)
		}
		var b bytes.Buffer
		w := csv.NewWriter(&b)
		if err := w.WriteAll(change(records, i)); err != nil {
			t.Fatal(err)
		}
		f[name] = b.Bytes()
	}
}

// ppSetCell sets the cell labelled label of the row of the file name whose
// Note names ref, or of its first row for ref "".
func ppSetCell(name, ref, label, value string) ppEdit {
	return ppRecords(name, ref, func(records [][]string, i int) [][]string {
		records[i][slices.Index(records[0], label)] = value
		return records
	})
}

// ppSetConfig changes the configuration name, decoded.
func ppSetConfig(name string, change func(c map[string]any)) ppEdit {
	return func(t *testing.T, f ppFolder) {
		t.Helper()
		var c map[string]any
		if err := json.Unmarshal(f[name], &c); err != nil {
			t.Fatal(err)
		}
		change(c)
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		f[name] = b
	}
}

// ppSetColumn changes the column labelled label of the configuration name.
func ppSetColumn(name, label string, change func(col map[string]any)) ppEdit {
	return ppSetConfig(name, func(c map[string]any) {
		for _, col := range c["columns"].([]any) {
			if col := col.(map[string]any); col["label"] == label {
				change(col)
			}
		}
	})
}

// ppCheckerNames are the names of the accounts of TestToolCheckersPP's
// export.
var ppCheckerNames = []string{"Schwab A1 ...135", "Schwab A2 ...398", "Schwab A3 ...783"}

// ppImportAll imports the export in dir into m, its files in the order to
// import them, each with the review page set to the file's account, and
// returns what PP refuses and the rows it marks, as file:line.
func ppImportAll(m *ppModel, dir string, names []string, ticks map[string]map[int]bool) (refused, marked []string) {
	entries, _ := os.ReadDir(dir)
	var files []string
	for _, e := range entries {
		if ppConfigOf(e.Name()) != "" {
			files = append(files, e.Name())
		}
	}
	for _, name := range ppImportOrder(files) {
		page := ""
		if name != ppSecuritiesFile {
			suffix := strings.TrimSuffix(name[strings.Index(name, "-transactions-")+len("-transactions-"):], ".csv")
			if i := slices.IndexFunc(names, func(n string) bool { return ppSuffix(n) == suffix }); i >= 0 {
				page = names[i]
			}
		}
		lines, r := m.importFile(dir, name, page, ticks[name])
		refused = append(refused, r...)
		for _, l := range lines {
			marked = append(marked, fmt.Sprintf("%s:%d", name, l))
		}
	}
	return refused, marked
}

// TestToolCheckersPP shows that ppCheck and the model of PP refuse what PP
// 0.90.0 refuses, and ppCheck what PP would read otherwise than the export
// means or what breaks the export's conventions (11.4 of the design): from
// a good export, which both take, each change alone must be refused for
// what it breaks.
func TestToolCheckersPP(t *testing.T) {
	t.Parallel()
	good := ppFolderOf(ppCheckerInput())
	const (
		acct1  = "account-transactions-A1-135.csv"
		acct2  = "account-transactions-A2-398.csv"
		port1  = "portfolio-transactions-A1-135.csv"
		port2  = "portfolio-transactions-A2-398.csv"
		secs   = ppSecuritiesFile
		config = ppAccountConfig
	)
	t.Run("good", func(t *testing.T) {
		t.Parallel()
		dir := good.write(t)
		ppCheck(t, dir, ppCheckerNames)
		in := ppCheckerInput()
		in.tool = toolPP
		events, outcomes := toolEvents(&in)
		m := newPPModel(ppCheckerNames...)
		refused, marked := ppImportAll(m, dir, ppCheckerNames, nil)
		for _, r := range refused {
			t.Error(r)
		}
		// Nothing is marked: the transfer is one row, of its paying
		// account's file.
		if len(marked) > 0 {
			t.Errorf("PP marks %v", marked)
		}
		ppReconcile(t, m, &in, events, outcomes)
		for _, f := range []string{acct1, acct2, port1, port2, secs} {
			if len(good[f]) == 0 {
				t.Errorf("the good export has no %s", f)
			}
		}
	})

	t.Run("check", func(t *testing.T) {
		t.Parallel()
		without := func(name string) ppEdit { return func(_ *testing.T, f ppFolder) { delete(f, name) } }
		whole := func(name, data string) ppEdit { return func(_ *testing.T, f ppFolder) { f[name] = []byte(data) } }
		keyOf := func(name, key string, value any) ppEdit {
			return ppSetConfig(name, func(c map[string]any) {
				if value == nil {
					delete(c, key)
				} else {
					c[key] = value
				}
			})
		}
		cols := func(name string, change func(cols []any) []any) ppEdit {
			return ppSetConfig(name, func(c map[string]any) { c["columns"] = change(c["columns"].([]any)) })
		}
		for _, c := range []struct {
			what string
			edit ppEdit
			want string
		}{
			// The configurations.
			{"a configuration that is no object", whole(config, "[]"), config + ": is not a JSON object"},
			{"a key of no configuration", keyOf(config, "quote", `"`), `holds the key "quote"`},
			{"no label", keyOf(config, "label", nil), "lacks a label"},
			{"another target", keyOf(config, "target", "portfolio-transaction"), `has the target "portfolio-transaction"`},
			{"a semicolon", keyOf(config, "delimiter", ";"), "has the delimiter"},
			{"another encoding", keyOf(config, "encoding", "ISO-8859-1"), "has the encoding"},
			{"lines skipped as a fraction", ppReplace(config, `"skipLines": 0`, `"skipLines": 0.0`), "not a whole number"},
			{"the header skipped", keyOf(config, "skipLines", 1), "skips 1 lines"},
			{"a header flag as text", keyOf(config, "isFirstLineHeader", "true"), "not a boolean"},
			{"no header", keyOf(config, "isFirstLineHeader", false), "the first line is no header"},
			{"no columns", keyOf(config, "columns", []any{}), "has no columns"},
			{"a key of no column", ppSetColumn(config, "Value", func(c map[string]any) { c["width"] = 3 }), `holds the key "width"`},
			{"a column without a label", ppSetColumn(config, "Note", func(c map[string]any) { delete(c, "label") }), "has no label"},
			{"a field of no target", ppSetColumn(config, "Value", func(c map[string]any) { c["field"] = "valeu" }), `has the field "valeu"`},
			{"a field twice", ppSetColumn(config, "Note", func(c map[string]any) { c["field"] = "name"; c["label"] = "Security Name" }), "maps the field name twice"},
			{"a column without a field", ppSetColumn(config, "Note", func(c map[string]any) { delete(c, "field") }), "has no field"},
			{"Fees read as taxes", ppSetColumn(config, "Fees", func(c map[string]any) { c["field"] = "taxes" }), `labelled "Fees", but its field taxes`},
			{"a format on a ticker", ppSetColumn(config, "Ticker Symbol", func(c map[string]any) { c["format"] = "x" }), "which a text field must not have"},
			{"no date format", ppSetColumn(config, "Date", func(c map[string]any) { delete(c, "format") }), `(Date) has the format "", not yyyy-MM-dd`},
			{"another date format", ppSetColumn(config, "Date", func(c map[string]any) { c["format"] = "dd.MM.yyyy" }), `"dd.MM.yyyy", not yyyy-MM-dd`},
			{"no format of a quote's date", ppSetColumn(ppSecuritiesConfig, "Date of Quote", func(c map[string]any) { delete(c, "format") }), `(Date of Quote) has the format ""`},
			{"a comma for decimals", ppSetColumn(config, "Value", func(c map[string]any) { c["format"] = "0.000,00" }), `"0.000,00", not 0,000.00`},
			{"a type without =", ppReplace(config, "DEPOSIT=Deposit;", "DEPOSIT;"), `"DEPOSIT" is not ENUM=label`},
			{"a type of no target", ppReplace(config, "REMOVAL=Withdrawal", "WITHDRAWAL=Withdrawal"), `"WITHDRAWAL=Withdrawal" names no type`},
			{"a type not listed", ppReplace(config, "DEPOSIT=Deposit;", ""), "do not list DEPOSIT"},
			{"a label twice", ppReplace(config, "FEES_REFUND=Fees Refund", "FEES_REFUND=Fees"), `repeats a type or a label`},
			{"shares not mapped", cols(ppPortfolioConfig, func(cols []any) []any {
				return slices.DeleteFunc(cols, func(c any) bool { return c.(map[string]any)["field"] == "shares" })
			}), "does not map shares"},
			{"an ISIN", cols(config, func(cols []any) []any {
				return append(cols, map[string]any{"label": "ISIN", "field": "isin"})
			}), "maps an ISIN"},
			{"a configuration missing", without(ppSecuritiesConfig), ppSecuritiesConfig + " is missing"},

			// The files' form.
			{"a byte order mark", func(_ *testing.T, f ppFolder) { f[acct1] = append([]byte("\xef\xbb\xbf"), f[acct1]...) }, "byte order mark"},
			{"CRLF", func(_ *testing.T, f ppFolder) { f[acct1] = bytes.ReplaceAll(f[acct1], []byte("\n"), []byte("\r\n")) }, "holds a CR"},
			{"no UTF-8", ppReplace(acct1, "Wire Incoming", "Wire \xffIncoming"), "is not UTF-8"},
			{"no last line end", func(_ *testing.T, f ppFolder) { f[acct1] = bytes.TrimSuffix(f[acct1], []byte("\n")) }, "does not end with a line end"},
			{"a bare quote", ppReplace(acct1, "Wire Incoming", `Wire "Incoming`), "cannot be parsed"},
			{"an empty file", whole(acct1, ""), acct1 + ": has no header"},
			{"a row short of a cell", ppRecords(acct1, "T62000101", func(r [][]string, i int) [][]string {
				r[i] = r[i][:len(r[i])-1]
				return r
			}), "cells, not 12"},
			{"another header", ppReplace(acct1, "Type,Value,", "Type,Amount,"), "has the header"},
			{"a file no configuration reads", whole("notes.txt", "x"), "notes.txt is no file"},
			{"no securities.csv", without(secs), "securities.csv is missing"},
			{"a file of no account", func(_ *testing.T, f ppFolder) {
				f["account-transactions-A9-999.csv"] = f[acct2]
				delete(f, acct2)
			}, "of no account of the export"},
			{"a space PP trims", ppReplace(acct1, "Wire Incoming", "Wire Incoming "), "starts or ends with a space"},

			// The cells.
			{"a date of the US form", ppSetCell(acct1, "T62000101", "Date", "01/02/2025"), `the Date "01/02/2025"`},
			{"no such date", ppSetCell(acct1, "T62000101", "Date", "2025-02-30"), `the Date "2025-02-30"`},
			{"a time without its zero", ppSetCell(acct1, "T62000101", "Time", "9:30"), `the Time "9:30"`},
			{"a thousands separator", ppSetCell(acct1, "T62000101", "Value", "1,000.00"), `the Value "1,000.00"`},
			{"a sign", ppSetCell(acct1, "T62000114", "Value", "-250.00"), `the Value "-250.00"`},
			{"one decimal", ppSetCell(acct1, "T62000101", "Value", "10000.0"), `the Value "10000.0"`},
			{"fees of 0", ppSetCell(acct1, "Cf6000001.3", "Fees", "0.00"), `the Fees "0.00"`},
			{"no shares", ppSetCell(port1, "T62000102", "Shares", "0"), `the Shares "0"`},
			{"too many places", ppSetCell(port1, "T62000102", "Shares", "10.123456789"), `the Shares "10.123456789"`},
			{"a quote of 0", ppReplace(secs, ",1.55\n", ",0\n"), `the Quote "0"`},
			{"a quote without its date", ppReplace(secs, "USD,2025-01-31,", "USD,,"), `the Date of Quote ""`},
			{"a date without its quote", ppReplace(secs, ",1.55\n", ",\n"), `the Quote ""`},
			{"another currency", ppSetCell(acct1, "T62000101", "Transaction Currency", "EUR"), `the currency is "EUR"`},
			{"a security in another currency", ppReplace(secs, "XYZ,98389B100,XYZ CORP COMMON STOCK,USD", "XYZ,98389B100,XYZ CORP COMMON STOCK,EUR"), `the currency is "EUR"`},
			{"a misspelt type", ppSetCell(acct1, "T62000114", "Type", "Withdraw"), `the Type "Withdraw"`},
			{"a type in lower case", ppSetCell(acct1, "T62000114", "Type", "withdrawal"), `the Type "withdrawal"`},
			{"no type", ppSetCell(acct1, "T62000114", "Type", ""), `the Type ""`},
			{"a ticker with a dot", ppReplace(port1, ",XYZ,98389B100,", ",XYZ.U,98389B100,"), "holds a dot"},
			{"no note", ppSetCell(acct1, "T62000101", "Note", ""), `the Note ""`},

			// The records, by their types.
			{"a security on interest", ppSetCell(acct1, "T62000108", "Ticker Symbol", "XYZ"), "Type Interest names a security"},
			{"a security on a deposit", ppSetCell(acct1, "T62000101", "Security Name", "XYZ CORP COMMON STOCK"), "Type Deposit names a security"},
			{"a security on a transfer", ppSetCell(acct2, "T62000201", "WKN", "98389B100"), "Type Transfer (Outbound) names a security"},
			{"a dividend of no security", ppRecords(acct1, "T62000105", func(r [][]string, i int) [][]string {
				for _, l := range []string{"Ticker Symbol", "WKN", "Security Name"} {
					r[i][slices.Index(r[0], l)] = ""
				}
				return r
			}), "Type Dividend names no security"},
			{"fees on a deposit", ppSetCell(acct1, "T62000101", "Fees", "1.00"), "Type Deposit has Fees"},
			{"fees on a withholding", ppSetCell(acct1, "T62000106", "Fees", "1.00"), "Type Taxes has Fees"},
			{"fees on a delivery", ppSetCell(port1, "Cf6000001.2", "Fees", "1.00"), "Type Delivery (Inbound) has Fees"},
			{"a buy's fees above its value", ppSetCell(port1, "T62000102", "Fees", "600.00"), "a Buy's Fees 600.00 exceed its Value 500.65"},
			{"a buy without shares", ppSetCell(port1, "T62000102", "Shares", ""), `Type Buy has the Shares ""`},
			{"a buy of no security", ppRecords(port1, "T62000102", func(r [][]string, i int) [][]string {
				for _, l := range []string{"Ticker Symbol", "WKN", "Security Name"} {
					r[i][slices.Index(r[0], l)] = ""
				}
				return r
			}), "Type Buy names no security"},
			{"a buy in the account file", ppSetCell(acct1, "T62000101", "Type", "Buy"), "Type Buy is written to the portfolio file"},
			{"a deposit in the portfolio file", ppSetCell(port1, "T62000102", "Type", "Deposit"), `the Type "Deposit" is no label`},
			{"a transfer of a security", ppSetCell(port1, "Cf6000001.8", "Type", "Transfer (Outbound)"), "Type Transfer (Outbound) is not written to the portfolio file"},
			{"an offset account of no account", ppSetCell(acct2, "T62000201", "Offset Account", "Schwab A9 ...999"), `Offset Account "Schwab A9 ...999", which names no other account`},
			{"a transfer to its own account", ppSetCell(acct2, "T62000201", "Offset Account", "Schwab A2 ...398"), `Offset Account "Schwab A2 ...398", which names no other account`},
			{"a transfer of no offset account", ppSetCell(acct2, "T62000201", "Offset Account", ""), `Offset Account "", which names no other account`},
			{"an offset account on a deposit", ppSetCell(acct1, "T62000101", "Offset Account", "Schwab A2 ...398"), "Type Deposit has the Offset Account"},
			{"another account's cash", ppSetCell(acct1, "T62000101", "Cash Account", "Schwab A2 ...398"), `the Cash Account "Schwab A2 ...398" is not the file's`},
			{"another securities account", ppSetCell(port1, "T62000102", "Securities Account", "Schwab A2 ...398"), `the Securities Account "Schwab A2 ...398" is not the file's`},
			{"cash on a delivery", ppSetCell(port1, "Cf6000001.8", "Cash Account", "Schwab A1 ...135"), `Type Delivery (Outbound) has the Cash Account "Schwab A1 ...135"`},
			{"a sale of no cash account", ppSetCell(port1, "T62000113", "Cash Account", ""), `Type Sell has the Cash Account ""`},
			{"a security of no identifier", ppReplace(secs, "EFGH,,EFGH TOTAL MARKET ETF,USD,,", ",,,USD,,"), "names no security"},
			{"a security listed twice", ppReplace(secs, "EFGH,,EFGH TOTAL MARKET ETF,USD,,\n", "EFGH,,EFGH TOTAL MARKET ETF,USD,,\nEFGH,,EFGH TOTAL MARKET ETF,USD,,\n"), "lists its security again"},
			{"a security not listed", ppReplace(secs, "EFGH,,EFGH TOTAL MARKET ETF,USD,,\n", ""), `names a security that securities.csv does not list: "EFGH"`},
			{"a security named otherwise", ppRecords(acct1, "T62000105", func(r [][]string, i int) [][]string {
				ticker, wkn := slices.Index(r[0], "Ticker Symbol"), slices.Index(r[0], "WKN")
				r[i][ticker], r[i][wkn] = r[i][wkn], r[i][ticker]
				return r
			}), `names a security that securities.csv does not list: "98389B100" "XYZ"`},

			// A delivery of a bond valued as if its price were per dollar of
			// face, 100 times its trade's price.
			{"a bond delivered at 100 times", ppSetCell(port1, "Cf6000001.2", "Value", "99500.00"), "a delivery at 99.5000 a share is not within a factor of 3"},
		} {
			t.Run(c.what, func(t *testing.T) {
				f := maps.Clone(good)
				c.edit(t, f)
				probs := ppProblems(f.write(t), ppCheckerNames)
				if !slices.ContainsFunc(probs, func(p string) bool { return strings.Contains(p, c.want) }) {
					t.Errorf("%s: no problem says %q; they are %q", c.what, c.want, probs)
				}
			})
		}
	})

	t.Run("nearest price", func(t *testing.T) {
		t.Parallel()
		// A delivery is checked against its security's price of the
		// nearest day: here a buy at 10 three days before it, not one at
		// 100 a year after.
		buy := func(ref string, posted nytime.Date, amount int64, price float64) toolRow {
			return toolRow{
				ref: ref, account: 1, source: "file", posted: posted, kind: "Buy", description: "EFGH", category: "buy",
				securityKey: keyEFGH, symbol: "EFGH", quantity: centsOf(1_000_000), amount: centsOf(amount), price: tcPrice(price),
			}
		}
		in := tcInput(toolInput{rows: []toolRow{
			buy("Cf6000003.1", "2025-01-02", -1000, 10),
			{
				ref: "Cf6000003.2", account: 1, source: "file", posted: "2025-01-05", kind: "Journaled Shares", description: "EFGH",
				category: "transfer_in", securityKey: keyEFGH, symbol: "EFGH", quantity: centsOf(1_000_000), amount: centsOf(0), price: tcPrice(11),
			},
			buy("Cf6000003.3", "2026-01-05", -10_000, 100),
		}})
		ppCheck(t, ppFolderOf(in).write(t), ppCheckerNames)
	})

	t.Run("model", func(t *testing.T) {
		t.Parallel()
		// What PP's import and review page refuse, of a file as a whole or
		// of a row; the model imports the rest.
		for _, c := range []struct {
			what string
			edit ppEdit
			held []*ppSecurity
			want string
		}{
			{"a dividend of no security", ppRecords(acct1, "T62000105", func(r [][]string, i int) [][]string {
				for _, l := range []string{"Ticker Symbol", "WKN", "Security Name"} {
					r[i][slices.Index(r[0], l)] = ""
				}
				return r
			}), nil, acct1 + ": row 2: a Dividend needs a security"},
			{"a buy of no security", ppRecords(port1, "T62000102", func(r [][]string, i int) [][]string {
				for _, l := range []string{"Ticker Symbol", "WKN", "Security Name"} {
					r[i][slices.Index(r[0], l)] = ""
				}
				return r
			}), nil, port1 + ": row 1: no security"},
			{"a buy without shares", ppSetCell(port1, "T62000102", "Shares", ""), nil, port1 + ": row 1 stops the import: a row has no Shares"},
			{"a misspelt type", ppSetCell(acct1, "T62000114", "Type", "Withdraw"), nil, `the type "Withdraw" cannot be read`},
			{"no value", ppSetCell(acct1, "T62000114", "Value", ""), nil, "no Value"},
			{"no such date", ppSetCell(acct1, "T62000114", "Date", "2025-02-30"), nil, `the date "2025-02-30" cannot be read`},
			{"another currency", ppSetCell(acct1, "T62000114", "Transaction Currency", "EUR"), nil, "its currency EUR is not its account's"},
			{"no currency", ppSetCell(acct1, "T62000114", "Transaction Currency", ""), nil, "its currency EUR is not its account's"},
			{"a security held in another currency", func(*testing.T, ppFolder) {}, []*ppSecurity{
				{ticker: "XYZ", wkn: "98389B100", name: "XYZ CORP", currency: "EUR", prices: map[string]int64{}},
			}, "is in EUR, and it in USD"},
			{"a buy's fees above its value", ppSetCell(port1, "T62000102", "Fees", "600.00"), nil, "its fees and taxes exceed its value"},
			{"no date format", ppSetColumn(config, "Date", func(c map[string]any) { delete(c, "format") }), nil, "stops the import: the date field has no format"},
			{"no amount format", ppSetColumn(config, "Value", func(c map[string]any) { delete(c, "format") }), nil, "stops the import: the amount field has no format"},
			{"a format on a ticker", ppSetColumn(config, "Ticker Symbol", func(c map[string]any) { c["format"] = "x" }), nil, "the text field ticker has a format"},
			{"a configuration without a key", ppSetConfig(config, func(c map[string]any) { delete(c, "skipLines") }), nil, "a key that PP requires is missing"},
			{"a WKN two securities share", func(*testing.T, ppFolder) {}, []*ppSecurity{
				{wkn: "98389B100", name: "XYZ ONE", currency: "USD", prices: map[string]int64{}},
				{wkn: "98389B100", name: "XYZ TWO", currency: "USD", prices: map[string]int64{}},
			}, "the WKN 98389B100 names several securities"},
			// In a PP whose language is German, a type that the map does not
			// list keeps its German label.
			{"a type not listed", ppReplace(config, "DEPOSIT=Deposit;", ""), nil, `the type "Deposit" cannot be read`},
			{"a configuration missing", func(_ *testing.T, f ppFolder) { delete(f, config) }, nil, acct1 + ": open "},
			{"a bare quote", ppReplace(acct1, "Wire Incoming", `Wire "Incoming`), nil, acct1 + ": cannot be parsed"},
		} {
			t.Run(c.what, func(t *testing.T) {
				f := maps.Clone(good)
				c.edit(t, f)
				m := newPPModel(ppCheckerNames...)
				m.securities = c.held
				refused, _ := ppImportAll(m, f.write(t), ppCheckerNames, nil)
				if !slices.ContainsFunc(refused, func(p string) bool { return strings.Contains(p, c.want) }) {
					t.Errorf("%s: PP refuses %q, not %q", c.what, refused, c.want)
				}
			})
		}

		// A file that no configuration reads, and one that is not there.
		m0 := newPPModel(ppCheckerNames...)
		dir0 := good.write(t)
		for name, want := range map[string]string{"notes.txt": "no configuration reads it", "account-transactions-A9-999.csv": "open "} {
			if _, refused := m0.importFile(dir0, name, "", nil); len(refused) != 1 || !strings.Contains(refused[0], want) {
				t.Errorf("PP refuses %s for %q, not %q", name, refused, want)
			}
		}

		// What PP takes without a word: an Offset Account that names no
		// account sends the transfer to the review page's first account by
		// name, here another of yours, Checking, which sorts before the
		// receiving account, so that the receiving leg lands there; and a
		// comma for decimals reads 10000.00 as 1000000.
		f := maps.Clone(good)
		ppSetCell(acct2, "T62000201", "Offset Account", "Schwab A9 ...999")(t, f)
		m := newPPModel(append([]string{"Checking"}, ppCheckerNames...)...)
		if refused, _ := ppImportAll(m, f.write(t), ppCheckerNames, nil); len(refused) > 0 {
			t.Fatal(refused)
		}
		if checking := ppOwnerNamed(m.accounts, "Checking"); !slices.ContainsFunc(checking.txs, func(tx *ppTx) bool {
			return tx.typ == "TRANSFER_IN" && tx.other.owner.name == "Schwab A2 ...398"
		}) {
			t.Errorf("the transfer's receiving leg is not in Checking: %v", checking.txs)
		}
		f = maps.Clone(good)
		ppSetColumn(config, "Value", func(c map[string]any) { c["format"] = "0.000,00" })(t, f)
		m = newPPModel(ppCheckerNames...)
		ppImportAll(m, f.write(t), ppCheckerNames, nil)
		if tx := ppOwnerNamed(m.accounts, "Schwab A1 ...135").txs[0]; tx.amount != 100_000_000 {
			t.Errorf("10000.00 read with a comma for decimals is %d cents", tx.amount)
		}
	})
}

// ppMade returns a file of kind, ppAccount or ppPortfolio, of the rows given
// by their cells, by label; a cell not given is empty, but the currency,
// which is USD, and the note, which names the row's line.
func ppMade(kind string, rows ...map[string]string) []byte {
	header := ppAccountHeader
	if kind == ppPortfolio {
		header = ppPortfolioHeader
	}
	var records [][]string
	for i, r := range rows {
		rec := make([]string, len(header))
		for j, label := range header {
			rec[j] = r[label]
		}
		rec[slices.Index(header, "Transaction Currency")] = ppCurrency
		if r["Note"] == "" {
			rec[slices.Index(header, "Note")] = fmt.Sprintf("schwabtx A1 ...135 R%d: MADE", i+1)
		}
		records = append(records, rec)
	}
	return ppCSV(header, records)
}

// TestPPModel shows that the model of PP marks and takes rows as PP's
// review page and import do (DetectDuplicatesAction.java, InsertAction.java,
// SecurityCache.java, TransactionOwner.java), which the tests of a chain of
// exports rest on.
func TestPPModel(t *testing.T) {
	t.Parallel()
	a1, a2 := "Schwab A1 ...135", "Schwab A2 ...398"
	dirOf := func(t *testing.T, files ppFolder) string {
		t.Helper()
		f := ppFolderOf(toolInput{tool: toolPP})
		maps.Copy(f, files)
		return f.write(t)
	}
	deposit := func(date, value string) map[string]string {
		return map[string]string{"Date": date, "Type": "Deposit", "Value": value, "Cash Account": a1}
	}

	t.Run("an export imported twice", func(t *testing.T) {
		t.Parallel()
		// The second import marks every row, and takes none.
		dir := ppFolderOf(ppCheckerInput()).write(t)
		m := newPPModel(ppCheckerNames...)
		ppImportAll(m, dir, ppCheckerNames, nil)
		cash := m.cash(a1)
		refused, marked := ppImportAll(m, dir, ppCheckerNames, nil)
		if len(refused) > 0 || len(marked) != 21 || m.cash(a1) != cash {
			t.Errorf("the second import marks %d rows %v, refuses %v; A1's cash %s, was %s", len(marked), marked, refused, formatCents(m.cash(a1)), formatCents(cash))
		}
	})

	t.Run("ticks", func(t *testing.T) {
		t.Parallel()
		// A row marked is taken when ticked; one existing transaction
		// marks every identical new row.
		dir := dirOf(t, ppFolder{"account-transactions-A1-135.csv": ppMade(ppAccount, deposit("2025-01-02", "10.00"))})
		again := dirOf(t, ppFolder{"account-transactions-A1-135.csv": ppMade(ppAccount,
			deposit("2025-01-02", "10.00"), deposit("2025-01-02", "10.00"), deposit("2025-01-03", "10.00"), deposit("2025-01-02", "11.00"))})
		m := newPPModel(a1)
		m.importFile(dir, "account-transactions-A1-135.csv", a1, nil)
		marked, _ := m.importFile(again, "account-transactions-A1-135.csv", a1, map[int]bool{2: true})
		if !slices.Equal(marked, []int{1, 2}) || m.cash(a1) != 4100 {
			t.Errorf("PP marks %v and holds %s", marked, formatCents(m.cash(a1)))
		}
	})

	t.Run("transfers", func(t *testing.T) {
		t.Parallel()
		// A transfer is checked by its paying leg in the paying account,
		// whichever account's file holds it: its second half is marked,
		// and so is a withdrawal of the paying account of its day and
		// amount. Deleting an account's transactions deletes the other
		// halves of its transfers.
		in := map[string]string{"Date": "2025-04-01", "Type": "Transfer (Inbound)", "Value": "100.00", "Cash Account": a1, "Offset Account": a2}
		out := map[string]string{"Date": "2025-04-01", "Type": "Transfer (Outbound)", "Value": "100.00", "Cash Account": a2, "Offset Account": a1}
		withdrawal := map[string]string{"Date": "2025-04-01", "Type": "Withdrawal", "Value": "100.00", "Cash Account": a2}
		other := map[string]string{"Date": "2025-04-02", "Type": "Deposit", "Value": "7.00", "Cash Account": a2}
		dir := dirOf(t, ppFolder{
			"account-transactions-A1-135.csv": ppMade(ppAccount, in),
			"account-transactions-A2-398.csv": ppMade(ppAccount, out, withdrawal, other),
		})
		m := newPPModel(a1, a2)
		refused, marked := ppImportAll(m, dir, []string{a1, a2}, nil)
		if len(refused) > 0 || !slices.Equal(marked, []string{"account-transactions-A2-398.csv:1", "account-transactions-A2-398.csv:2"}) {
			t.Errorf("PP marks %v, refuses %v", marked, refused)
		}
		if m.cash(a1) != 10_000 || m.cash(a2) != -10_000+700 {
			t.Errorf("A1 holds %s, A2 %s", formatCents(m.cash(a1)), formatCents(m.cash(a2)))
		}
		m.deleteAccount(a1)
		if m.cash(a1) != 0 || m.cash(a2) != 700 {
			t.Errorf("after A1's deletion, A1 holds %s, A2 %s", formatCents(m.cash(a1)), formatCents(m.cash(a2)))
		}
		// The receiving leg is not checked: a deposit of the receiving
		// account of the transfer's day and amount marks nothing.
		m = newPPModel(a1, a2)
		dir1 := dirOf(t, ppFolder{"account-transactions-A1-135.csv": ppMade(ppAccount, deposit("2025-04-01", "100.00"))})
		m.importFile(dir1, "account-transactions-A1-135.csv", a1, nil)
		if marked, _ := m.importFile(dir, "account-transactions-A1-135.csv", a1, nil); len(marked) > 0 {
			t.Errorf("PP marks the transfer's rows %v by its receiving leg", marked)
		}

		// A transfer whose Offset Account names no account goes to the review
		// page's first account by name, whatever the file's account.
		a3 := "Schwab A3 ...783"
		m = newPPModel(a1, a2, a3)
		bogus := map[string]string{"Date": "2025-04-01", "Type": "Transfer (Outbound)", "Value": "50.00", "Cash Account": a3, "Offset Account": "Schwab A9 ...999"}
		dir3 := dirOf(t, ppFolder{"account-transactions-A3-783.csv": ppMade(ppAccount, bogus)})
		m.importFile(dir3, "account-transactions-A3-783.csv", a3, nil)
		if m.cash(a1) != 5000 || m.cash(a3) != -5000 {
			t.Errorf("the transfer leaves A1 with %s and A3 with %s", formatCents(m.cash(a1)), formatCents(m.cash(a3)))
		}

		// A buy whose cash is another account's: deleting the securities
		// account's transactions deletes the buy's cash side too.
		m = newPPModel(a1, a2)
		cross := map[string]string{
			"Date": "2025-05-01", "Type": "Buy", "Value": "50.00", "Shares": "1", "Cash Account": a1, "Securities Account": a2,
			"Ticker Symbol": "XYZ", "Security Name": "XYZ CORP",
		}
		dir2 := dirOf(t, ppFolder{"portfolio-transactions-A2-398.csv": ppMade(ppPortfolio, cross)})
		m.importFile(dir2, "portfolio-transactions-A2-398.csv", a2, nil)
		if m.cash(a1) != -5000 {
			t.Errorf("the buy leaves A1 with %s", formatCents(m.cash(a1)))
		}
		m.deleteAccount(a2)
		if m.cash(a1) != 0 {
			t.Errorf("after A2's deletion, A1 holds %s", formatCents(m.cash(a1)))
		}

		// Ticked, the withdrawal that the transfer's paying leg marks is
		// taken, and the transfer's second half, left unticked, is not.
		m = newPPModel(a1, a2)
		ticks := map[string]map[int]bool{"account-transactions-A2-398.csv": {2: true}}
		if refused, _ := ppImportAll(m, dir, []string{a1, a2}, ticks); len(refused) > 0 || m.cash(a1) != 10_000 || m.cash(a2) != -20_000+700 {
			t.Errorf("ticked, A1 holds %s, A2 %s; PP refuses %v", formatCents(m.cash(a1)), formatCents(m.cash(a2)), refused)
		}
	})

	t.Run("equivalent types", func(t *testing.T) {
		t.Parallel()
		// A deposit is marked by a transfer in, a buy by a delivery in, a
		// sale by a delivery out; a buy's account side by a buy alone.
		security := map[string]string{"Ticker Symbol": "XYZ", "WKN": "98389B100", "Security Name": "XYZ CORP"}
		port := func(typ, value, shares, cash string) map[string]string {
			r := map[string]string{"Date": "2025-05-01", "Type": typ, "Value": value, "Shares": shares, "Cash Account": cash, "Securities Account": a1}
			maps.Copy(r, security)
			return r
		}
		first := dirOf(t, ppFolder{
			"account-transactions-A1-135.csv": ppMade(ppAccount, map[string]string{
				"Date": "2025-05-01", "Type": "Transfer (Inbound)", "Value": "5.00", "Cash Account": a1, "Offset Account": a2,
			}),
			"portfolio-transactions-A1-135.csv": ppMade(ppPortfolio, port("Delivery (Inbound)", "50.00", "1", ""), port("Delivery (Outbound)", "60.00", "2", "")),
		})
		// A buy of other shares, or of another security, is none.
		other := port("Buy", "50.00", "1", a1)
		other["Ticker Symbol"], other["WKN"], other["Security Name"] = "EFGH", "", "EFGH FUND"
		second := dirOf(t, ppFolder{
			"account-transactions-A1-135.csv": ppMade(ppAccount, deposit("2025-05-01", "5.00")),
			"portfolio-transactions-A1-135.csv": ppMade(ppPortfolio,
				port("Buy", "50.00", "1", a1), port("Sell", "60.00", "2", a1), port("Buy", "60.00", "2", a1), port("Buy", "50.00", "3", a1), other),
		})
		m := newPPModel(a1, a2)
		ppImportAll(m, first, []string{a1, a2}, nil)
		_, marked := ppImportAll(m, second, []string{a1, a2}, nil)
		if !slices.Equal(marked, []string{"account-transactions-A1-135.csv:1", "portfolio-transactions-A1-135.csv:1", "portfolio-transactions-A1-135.csv:2"}) {
			t.Errorf("PP marks %v", marked)
		}
	})

	t.Run("account file", func(t *testing.T) {
		t.Parallel()
		// A sale of value 0 in the account file is an outbound delivery,
		// which moves no cash; a security named on a deposit is dropped
		// with the row's, and PP makes none. PP's account file takes a sale
		// with Shares, for which the export's configuration has no column:
		// this one adds it at the end.
		config := strings.Replace(ppAccountConfiguration, `{"label": "Offset Account", "field": "account2nd"}`,
			`{"label": "Offset Account", "field": "account2nd"}, {"label": "Shares", "field": "shares", "format": "0,000.00"}`, 1)
		header := append(slices.Clone(ppAccountHeader), "Shares")
		sale := make([]string, len(header))
		copy(sale, []string{"2025-05-01", "", "Sell", "0.00", ppCurrency, "", "XYZ", "", "XYZ CORP", "schwabtx A1 ...135 R1: MADE", a1, ""})
		sale[len(sale)-1] = "1"
		named := []string{"2025-05-02", "", "Deposit", "5.00", ppCurrency, "", "ABCD", "", "", "schwabtx A1 ...135 R2: MADE", a1, "", ""}
		dir := dirOf(t, ppFolder{ppAccountConfig: []byte(config), "account-transactions-A1-135.csv": ppCSV(header, [][]string{sale, named})})
		m := newPPModel(a1)
		if _, refused := m.importFile(dir, "account-transactions-A1-135.csv", a1, nil); len(refused) > 0 {
			t.Fatal(refused)
		}
		port := ppOwnerNamed(m.portfolios, a1)
		if m.cash(a1) != 500 || len(port.txs) != 1 || port.txs[0].typ != "DELIVERY_OUTBOUND" || port.txs[0].other != nil {
			t.Errorf("A1 holds %s and %v", formatCents(m.cash(a1)), port.txs)
		}
		if len(m.securities) != 1 || m.securities[0].ticker != "XYZ" {
			t.Errorf("PP holds the securities %v", m.securities)
		}
	})

	t.Run("securities", func(t *testing.T) {
		t.Parallel()
		// A ticker is compared up to its dot; a WKN finds a security whose
		// ticker changed; a name finds one when the row's ticker does not
		// differ from the security's, as it does not when the security has
		// none.
		buy := func(ticker, wkn, name string) map[string]string {
			return map[string]string{
				"Date": "2025-05-01", "Type": "Buy", "Value": "10.00", "Shares": "1", "Cash Account": a1, "Securities Account": a1,
				"Ticker Symbol": ticker, "WKN": wkn, "Security Name": name,
			}
		}
		dir := dirOf(t, ppFolder{"portfolio-transactions-A1-135.csv": ppMade(ppPortfolio,
			buy("BRK.A", "", "BERKSHIRE A"), buy("BRK.B", "", "BERKSHIRE B"),
			buy("PQR", "ZZR00K200", "PQR GROUP INC"), buy("PQRS", "ZZR00K200", "PQRS GROUP INC"),
			buy("", "", "QRS TRUST PFD SER K"), buy("", "", "QRS TRUST PFD SER K"), buy("QRSX", "", "QRS TRUST PFD SER K"),
			buy("ABCA", "", "ABC FUND"), buy("ABCB", "", "ABC FUND"),
		)})
		m := newPPModel(a1)
		if _, refused := m.importFile(dir, "portfolio-transactions-A1-135.csv", a1, nil); len(refused) > 0 {
			t.Fatal(refused)
		}
		var got []string
		for s, q := range m.shares(a1) {
			got = append(got, fmt.Sprintf("%s|%s|%s %d", s.ticker, s.wkn, s.name, q/100_000_000))
		}
		slices.Sort(got)
		want := []string{"ABCA||ABC FUND 1", "ABCB||ABC FUND 1", "BRK.A||BERKSHIRE A 2", "PQR|ZZR00K200|PQR GROUP INC 2", "||QRS TRUST PFD SER K 3"}
		if !slices.Equal(got, want) {
			t.Errorf("PP holds %q, want %q", got, want)
		}
	})
}
