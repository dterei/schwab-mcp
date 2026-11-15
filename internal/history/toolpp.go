package history

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// This file writes a tool export's records as the files that Portfolio
// Performance (PP) imports through its CSV import (5 of the design): for
// each account, a file of the records that move only cash and a file of
// those that move shares; one file of securities and their snapshot prices;
// and the three import configurations, which tell PP how to read the three
// kinds of file whatever its language and locale. Like the mapping, it reads
// only what it is given.

// The names of the files of a PP export that do not depend on an account,
// and the start of those that do: each account's records are in
// account-transactions-<suffix>.csv and portfolio-transactions-<suffix>.csv
// (ppFileOf).
const (
	ppSecuritiesFile   = "securities.csv"
	ppSecuritiesConfig = "pp-securities.json"
	ppAccountConfig    = "pp-account-transactions.json"
	ppPortfolioConfig  = "pp-portfolio-transactions.json"
)

// What a file of a PP export holds: securities and their prices; an
// account's records that move only cash (its account file) or shares (its
// portfolio file), as eventKinds names the two; or a configuration.
const (
	ppSecurities    = "securities"
	ppAccount       = "account"
	ppPortfolio     = "portfolio"
	ppConfiguration = "configuration"
)

// ppCurrency is the currency of every record, price and security that a PP
// export writes: Schwab's cash and securities are in US dollars.
const ppCurrency = "USD"

// The import configurations (5.6 of the design), which PP's CSV import
// reads through its menu "Import configuration...": a configuration applies
// its columns to a file by their position, each read as its field's code
// says and in the format given, whatever the file's header says. A date's
// format is yyyy-MM-dd and an amount's 0,000.00 (a point before the
// decimals), and the Type column's labels are PP's English ones, every type
// of the target listed, so that a PP in any language and locale reads the
// files alike. Each label ends with the layout's version, so that a later
// layout is a new configuration, not this one applied to other columns.
const (
	ppSecuritiesConfiguration = `{
  "label": "schwabtx securities v1",
  "target": "investment-vehicle",
  "delimiter": ",",
  "encoding": "UTF-8",
  "skipLines": 0,
  "isFirstLineHeader": true,
  "columns": [
    {"label": "Ticker Symbol", "field": "ticker"},
    {"label": "WKN", "field": "wkn"},
    {"label": "Security Name", "field": "name"},
    {"label": "Note", "field": "note"},
    {"label": "Currency", "field": "currency"},
    {"label": "Date of Quote", "field": "date", "format": "yyyy-MM-dd"},
    {"label": "Quote", "field": "quote", "format": "0,000.00"}
  ]
}
`
	ppAccountConfiguration = `{
  "label": "schwabtx account transactions v1",
  "target": "account-transaction",
  "delimiter": ",",
  "encoding": "UTF-8",
  "skipLines": 0,
  "isFirstLineHeader": true,
  "columns": [
    {"label": "Date", "field": "date", "format": "yyyy-MM-dd"},
    {"label": "Time", "field": "time"},
    {"label": "Type", "field": "type", "format": "DEPOSIT=Deposit;REMOVAL=Withdrawal;INTEREST=Interest;INTEREST_CHARGE=Interest Charge;DIVIDENDS=Dividend;FEES=Fees;FEES_REFUND=Fees Refund;TAXES=Taxes;TAX_REFUND=Tax Refund;BUY=Buy;SELL=Sell;TRANSFER_IN=Transfer (Inbound);TRANSFER_OUT=Transfer (Outbound)"},
    {"label": "Value", "field": "value", "format": "0,000.00"},
    {"label": "Transaction Currency", "field": "currency"},
    {"label": "Fees", "field": "fees", "format": "0,000.00"},
    {"label": "Ticker Symbol", "field": "ticker"},
    {"label": "WKN", "field": "wkn"},
    {"label": "Security Name", "field": "name"},
    {"label": "Note", "field": "note"},
    {"label": "Cash Account", "field": "account"},
    {"label": "Offset Account", "field": "account2nd"}
  ]
}
`
	ppPortfolioConfiguration = `{
  "label": "schwabtx portfolio transactions v1",
  "target": "portfolio-transaction",
  "delimiter": ",",
  "encoding": "UTF-8",
  "skipLines": 0,
  "isFirstLineHeader": true,
  "columns": [
    {"label": "Date", "field": "date", "format": "yyyy-MM-dd"},
    {"label": "Time", "field": "time"},
    {"label": "Type", "field": "type", "format": "BUY=Buy;SELL=Sell;TRANSFER_IN=Transfer (Inbound);TRANSFER_OUT=Transfer (Outbound);DELIVERY_INBOUND=Delivery (Inbound);DELIVERY_OUTBOUND=Delivery (Outbound)"},
    {"label": "Value", "field": "value", "format": "0,000.00"},
    {"label": "Transaction Currency", "field": "currency"},
    {"label": "Shares", "field": "shares", "format": "0,000.00"},
    {"label": "Fees", "field": "fees", "format": "0,000.00"},
    {"label": "Ticker Symbol", "field": "ticker"},
    {"label": "WKN", "field": "wkn"},
    {"label": "Security Name", "field": "name"},
    {"label": "Note", "field": "note"},
    {"label": "Cash Account", "field": "account"},
    {"label": "Securities Account", "field": "portfolio"}
  ]
}
`
)

// The header of each kind of file: its configuration's column labels, in
// their order, which is the order of each row's cells.
var (
	ppSecuritiesHeader = []string{"Ticker Symbol", "WKN", "Security Name", "Note", "Currency", "Date of Quote", "Quote"}
	ppAccountHeader    = []string{
		"Date", "Time", "Type", "Value", "Transaction Currency", "Fees", "Ticker Symbol", "WKN", "Security Name", "Note",
		"Cash Account", "Offset Account",
	}
	ppPortfolioHeader = []string{
		"Date", "Time", "Type", "Value", "Transaction Currency", "Shares", "Fees", "Ticker Symbol", "WKN", "Security Name", "Note",
		"Cash Account", "Securities Account",
	}
)

// A ppFile is a file of a PP export.
type ppFile struct {
	// name is its name in the export's folder.
	name string
	// kind is what it holds: ppSecurities, ppAccount, ppPortfolio or
	// ppConfiguration.
	kind string
	// configuration is the name of the configuration that reads a file of
	// securities or records; "" for a configuration.
	configuration string
	// account is the account whose records an account or a portfolio file
	// holds; the zero account for any other file.
	account toolAccount
	// rows is how many rows a file of securities or records holds below its
	// header.
	rows int
	// records are the records of an account or a portfolio file, in the
	// order of its rows; none for any other file. The replay of PP's
	// duplicate rule reads them (ppTicks).
	records []*toolEvent
	// data is its bytes.
	data []byte
}

// ppFiles returns the files of the PP export of in, whose tool is PP and
// whose records are events (toolEvents), in the order to import them (5.1
// of the design): securities.csv, which makes the securities before the
// records that name them, a security written on its own with its
// descriptions as its note, which PP updates on a security that it holds
// (CSVSecurityExtractor.java:155-157), and every other with none, which
// changes nothing there; then, for each account with a record, in the
// order of their ids, its account file and its portfolio file, each when it
// has a row; then the three configurations. securities.csv is written even
// with no row, as are the configurations.
func ppFiles(in *toolInput, events []toolEvent) []ppFile {
	var securities [][]string
	for _, l := range mergeListed(toolPP, listSecurities(in, events)) {
		ticker, wkn, name := ppIdentifiers(l.security)
		note := ppTrim(l.security.note)
		if len(l.prices) == 0 {
			securities = append(securities, []string{ticker, wkn, name, note, ppCurrency, "", ""})
		}
		for _, p := range l.prices {
			securities = append(securities, []string{ticker, wkn, name, note, ppCurrency, string(p.date), roundRat(p.price, 8)})
		}
	}
	files := []ppFile{{
		name: ppSecuritiesFile, kind: ppSecurities, configuration: ppSecuritiesConfig,
		rows: len(securities), data: ppCSV(ppSecuritiesHeader, securities),
	}}

	// Each account's records, by file, in the order of their days and
	// times.
	type file struct {
		account toolAccount
		records []*toolEvent
	}
	byName := map[string]*file{}
	for i := range events {
		e := &events[i]
		name := ppFileOf(e)
		if byName[name] == nil {
			byName[name] = &file{account: e.account}
		}
		byName[name].records = append(byName[name].records, e)
	}
	names := slices.SortedFunc(maps.Keys(byName), func(a, b string) int {
		fa, fb := byName[a], byName[b]
		// The account file before the portfolio file: "account-" sorts
		// before "portfolio-".
		return cmp.Or(cmp.Compare(fa.account.id, fb.account.id), strings.Compare(a, b))
	})
	for _, name := range names {
		f := byName[name]
		slices.SortStableFunc(f.records, func(a, b *toolEvent) int {
			return cmp.Or(cmp.Compare(a.day, b.day), a.time.Compare(b.time))
		})
		kind, _ := f.records[0].kind.ppType()
		header, configuration := ppAccountHeader, ppAccountConfig
		if kind == ppPortfolio {
			header, configuration = ppPortfolioHeader, ppPortfolioConfig
		}
		rows := make([][]string, len(f.records))
		for j, e := range f.records {
			rows[j] = ppRecordOf(e).cells()
		}
		files = append(files, ppFile{
			name: name, kind: kind, configuration: configuration, account: f.account,
			rows: len(rows), records: f.records, data: ppCSV(header, rows),
		})
	}

	for _, c := range []struct{ name, data string }{
		{ppSecuritiesConfig, ppSecuritiesConfiguration},
		{ppAccountConfig, ppAccountConfiguration},
		{ppPortfolioConfig, ppPortfolioConfiguration},
	} {
		files = append(files, ppFile{name: c.name, kind: ppConfiguration, data: []byte(c.data)})
	}
	return files
}

// ppFileOf returns the name of the file that writes record e: its
// account's account file, for a record that moves only cash, or portfolio
// file, for one that moves shares, as account-transactions-A1-135.csv.
func ppFileOf(e *toolEvent) string {
	file, _ := e.kind.ppType()
	return file + "-transactions-" + e.account.suffix() + ".csv"
}

// A ppRecord is the row of a PP file that writes a record: its cells as
// written, which are what PP keeps of the record.
type ppRecord struct {
	// file is ppAccount or ppPortfolio, as for the record's kind.
	file string
	// date and time are its Date and Time; typ its Type, as the
	// configurations label it; value, shares and fees its Value, Shares and
	// Fees.
	date, time, typ, value, shares, fees string
	// ticker, wkn and name are its Ticker Symbol, WKN and Security Name.
	ticker, wkn, name string
	// note is its Note.
	note string
	// cash is its Cash Account, offset its Offset Account, and securities
	// its Securities Account.
	cash, offset, securities string
}

// ppRecordOf returns the row of a PP file that writes record e (5.3 and 5.4
// of the design). Value is unsigned, as the Type says which way the cash
// goes; Time is the time of New York's clocks, when the record has one on
// its day; Fees are written with a dividend, a buy or a sale that has some,
// the only records that PP takes fees on; the security's identifiers with a
// record that names one; the account's name as the Cash Account of a record
// that moves cash and as the Securities Account of one that moves shares;
// and the other account's name as a transfer's Offset Account.
func ppRecordOf(e *toolEvent) ppRecord {
	file, typ := e.kind.ppType()
	r := ppRecord{file: file, date: string(e.day), typ: typ, value: formatCents(e.value), note: ppNote(e)}
	if seconds, ok := e.clock(); ok {
		r.time = fmt.Sprintf("%02d:%02d", seconds/3600, seconds%3600/60)
	}
	if e.fees > 0 {
		r.fees = formatCents(e.fees)
	}
	if e.security != nil {
		r.ticker, r.wkn, r.name = ppIdentifiers(e.security)
	}
	name := e.account.name()
	switch file {
	case ppPortfolio:
		r.shares, r.securities = formatMicros(e.shares), name
		// A delivery moves no cash, and PP takes no account with it.
		if e.kind == evBuy || e.kind == evSell {
			r.cash = name
		}
	default:
		r.cash = name
		if e.kind == evTransferOut {
			r.offset = e.pair.name()
		}
	}
	return r
}

// cells returns the record's cells in the order of its file's header.
func (r ppRecord) cells() []string {
	if r.file == ppPortfolio {
		return []string{
			r.date, r.time, r.typ, r.value, ppCurrency, r.shares, r.fees, r.ticker, r.wkn, r.name, r.note,
			r.cash, r.securities,
		}
	}
	return []string{r.date, r.time, r.typ, r.value, ppCurrency, r.fees, r.ticker, r.wkn, r.name, r.note, r.cash, r.offset}
}

// ppIdentifiers returns the Ticker Symbol, WKN and Security Name that PP
// gets for security s (3.5 of the design), by which PP finds the security
// or makes it: its PP ticker, its CUSIP and its name, which every security
// of the store that the tools take for one with it has alike (join). A
// security written on its own (unsure) has a name alone, made from its key
// (safeName), which PP finds no other security by, and which a description
// learned later does not change (I-6 of the consolidation). A security
// that would have none of the three, having no description, no CUSIP and
// a ticker kept from PP for its dot, is named by its latest symbol, else
// its key: PP refuses a record of no security.
func ppIdentifiers(s *toolSecurity) (ticker, wkn, name string) {
	if s.unsure {
		return "", "", ppTrim(s.safeName())
	}
	ticker, wkn, name = ppTrim(s.ppTicker()), ppTrim(s.cusip), ppTrim(s.name)
	if ticker == "" && wkn == "" && name == "" {
		name = ppTrim(cmp.Or(s.symbol, s.key))
	}
	return ticker, wkn, name
}

// ppNote returns the Note of record e (5.3.1 of the design): schwabtx, the
// account's label and the row's ref, and the leg of a trade or journal of
// several; a transfer's paying side and then its receiving side, with "to"
// between; a two-row cash event's cash row and then, after "with", its
// shares row; then the row's text, and what each of the record's caveats
// says, in their order. PP compares no notes when it looks for duplicates,
// so a note may change between two exports of one row.
func ppNote(e *toolEvent) string {
	var head string
	switch {
	case e.kind == evTransferOut:
		head = ppTransferHead(e)
	case e.with != "":
		head = e.account.label() + " " + e.ref + " with " + e.with
	default:
		head = e.account.label() + " " + e.ref
		if e.leg.Valid {
			head += " leg " + strconv.FormatInt(e.leg.Int64, 10)
		}
	}
	note := "schwabtx " + head + ": " + ppTrim(e.text)
	var words []string
	for _, c := range e.caveats {
		if w := ppCaveatWords(e, c); !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	if len(words) > 0 {
		note += "; " + strings.Join(words, "; ")
	}
	return ppTrim(note)
}

// ppTransferHead returns the head of the Note of record e, a transfer: its
// paying side's label and ref, "to", and its receiving side's, as "A2 ...398
// T61001201 to A1 ...135 T61001202".
func ppTransferHead(e *toolEvent) string {
	return e.account.label() + " " + e.ref + " to " + e.pair.label() + " " + e.pairRef
}

// ppCaveatWords returns what the Note of record e says of its caveat
// (5.3.1 of the design); the caveat's code for one that no PP record
// carries. Each of the three caveats of a journal written apart from its
// partner names the partner, and a sale whose fees PP gets in a Fees row of
// their own says so in both records.
func ppCaveatWords(e *toolEvent, caveat string) string {
	switch caveat {
	case "value_unknown":
		return "value unknown: set it"
	case "value_from_snapshot":
		return "value from the snapshot"
	case "assumed_multiplier":
		return "multiplier 100 assumed"
	case "cash_not_written":
		return "its cash of " + formatCents(e.amount) + " is not in this file"
	case "fees_unknown":
		return "fees unknown, in the price"
	case "pair_amounts_differ", "pair_not_journal", "pair_outside":
		return "paired with " + e.pair.label() + " " + e.pairRef + ", written apart"
	case "return_of_capital":
		return "return of capital"
	case "option_closed_at_zero":
		return "closed at 0"
	case "fees_apart":
		if e.kind == evFees {
			return "fees of the sale"
		}
		return "fees in a Fees row"
	}
	return caveat
}

// ppTrim returns s without the characters that PP trims from both ends of
// every cell it reads (TextUtil.trim): Unicode's spaces and line and
// paragraph separators, the tab, line feed, vertical tab, form feed and
// carriage return, the four ASCII information separators, and the byte
// order mark. A cell so trimmed is what PP keeps of it.
func ppTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.In(r, unicode.Z) || '\t' <= r && r <= '\r' || 0x1c <= r && r <= 0x1f || r == 0xfeff
	})
}

// ppCSV returns a file of header and rows as PP's CSV import reads it with
// the configurations (5.2 of the design): UTF-8 without a byte order mark,
// commas, quotes as RFC 4180 has them, LF line ends.
func ppCSV(header []string, rows [][]string) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	// Writing to a bytes.Buffer cannot fail.
	_ = w.Write(header)
	_ = w.WriteAll(rows)
	return b.Bytes()
}
