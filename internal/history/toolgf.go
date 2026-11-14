package history

import (
	"cmp"
	"encoding/json"
	"io"
	"maps"
	"math/big"
	"slices"
	"strconv"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// This file writes an export for Ghostfolio (GF): ghostfolio.json, which
// Ghostfolio's import reads, from the records that toolEvents makes for it
// and the input they come from. Its comments cite the tool exports' design
// by section, as toolmap.go's do.

// gfFileName is the name of an export's file for Ghostfolio.
const gfFileName = "ghostfolio.json"

// A gfFile is ghostfolio.json, in the shape of Ghostfolio's own export
// without its meta and user (6.2 of the design): the accounts with their
// balances, the MANUAL asset profiles with their prices, and the
// activities. Every key is one that Ghostfolio's DTOs declare, since its
// import refuses any other. The three lists are in the order of
// Ghostfolio's own export, and each object's keys in alphabetical order,
// as the design shows them; the import reads them in any order. Numbers
// are json.Number, written from exact text (7 of the design), never from
// a float.
type gfFile struct {
	Accounts      []gfAccount  `json:"accounts"`
	AssetProfiles []gfProfile  `json:"assetProfiles"`
	Activities    []gfActivity `json:"activities"`
}

// A gfAccount is an account of the file (6.3 of the design). Balances is
// absent for an account with no balance. PlatformID is always null: the
// key must be present, since a missing one fails Ghostfolio's IsString,
// and the export names no platform.
type gfAccount struct {
	Balances   []gfBalance `json:"balances,omitempty"`
	Comment    string      `json:"comment"`
	Currency   string      `json:"currency"`
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	PlatformID *string     `json:"platformId"`
}

// A gfBalance is an account's cash at midnight UTC of a market date, in
// dollars, signed.
type gfBalance struct {
	Date  string      `json:"date"`
	Value json.Number `json:"value"`
}

// A gfProfile is a MANUAL asset profile of the file (6.4 of the design):
// a cash profile, or a security that Ghostfolio gets no prices for. The
// optional keys are absent when they have no value.
type gfProfile struct {
	AssetClass    string    `json:"assetClass,omitempty"`
	AssetSubClass string    `json:"assetSubClass,omitempty"`
	Comment       string    `json:"comment,omitempty"`
	Currency      string    `json:"currency"`
	CUSIP         string    `json:"cusip,omitempty"`
	DataSource    string    `json:"dataSource"`
	MarketData    []gfPrice `json:"marketData,omitempty"`
	Name          string    `json:"name"`
	Symbol        string    `json:"symbol"`
}

// A gfPrice is a profile's price at midnight UTC of a market date, in
// dollars per share.
type gfPrice struct {
	Date        string      `json:"date"`
	MarketPrice json.Number `json:"marketPrice"`
}

// A gfActivity is an activity of the file (3.4 and 6.5 of the design).
// Its data source is always given, since the default for BUY, SELL and
// DIVIDEND is a setting of the instance.
type gfActivity struct {
	AccountID  string      `json:"accountId"`
	Comment    string      `json:"comment"`
	Currency   string      `json:"currency"`
	DataSource string      `json:"dataSource"`
	Date       string      `json:"date"`
	Fee        json.Number `json:"fee"`
	Quantity   json.Number `json:"quantity"`
	Symbol     string      `json:"symbol"`
	Type       string      `json:"type"`
	UnitPrice  json.Number `json:"unitPrice"`
}

// gfCashProfiles are the cash profiles (3.4.4 and 6.4 of the design), in
// the order that the file lists them: the MANUAL assets that the FEE and
// INTEREST activities of fees, interest and taxes are of, which no
// security carries in Ghostfolio. toolEvent.profile names one.
var gfCashProfiles = []struct{ profile, name string }{
	{"fees", "Schwab fees"},
	{"interest", "Schwab interest"},
	{"taxes", "Schwab taxes"},
}

// gfProfileSymbol returns the symbol of the cash profile profile: the
// name-based UUID of "profile " and the profile (6.7 of the design), the
// same in every export.
func gfProfileSymbol(profile string) string { return uuid8(toolNamespace, "profile "+profile) }

// A gfExport is an export for Ghostfolio: ghostfolio.json, and what the
// manifest and stderr say of it (6.4, 6.6 and 8 of the design).
type gfExport struct {
	// file is ghostfolio.json.
	file gfFile
	// assets are what file.AssetProfiles are, one for each, in their order.
	assets []gfAsset
	// yahoo are the symbols that Ghostfolio's import asks Yahoo about, each
	// once, in order; idle those of them that Yahoo may no longer know
	// (yahooIdle), which the manifest lists for you to look up first.
	yahoo []string
	idle  []toolIdle
}

// A gfAsset is what an asset profile of the file is: a cash profile, or a
// security's.
type gfAsset struct {
	// profile is the cash profile that it is (toolEvent.profile), "" for a
	// security's; security is the security, nil for a cash profile.
	profile  string
	security *toolSecurity
	// used says that an activity of the file is of it. Ghostfolio makes a
	// profile only for an activity that it imports, so one that no activity
	// uses is written for its prices alone, which Ghostfolio applies only
	// when it holds the asset already (prices_only in the manifest).
	used bool
}

// gfRender returns the export for Ghostfolio of in, whose records for
// Ghostfolio are events (toolEvents of in, with in.tool toolGF): its
// accounts, asset profiles and activities (6.2 to 6.5 of the design), and
// the symbols that its import asks Yahoo about (6.6).
func gfRender(in *toolInput, events []toolEvent) *gfExport {
	x := &gfExport{file: gfFile{Accounts: gfAccounts(in, events), Activities: []gfActivity{}}}
	x.file.AssetProfiles, x.assets = gfAssets(in, events)
	// The activities in the order of their day, time, account, ref and leg.
	// Every record for Ghostfolio is an activity: toolEvents gives none of
	// the cash rows, which the accounts' balances carry.
	records := slices.Clone(events)
	slices.SortStableFunc(records, func(a, b toolEvent) int {
		return cmp.Or(cmp.Compare(a.day, b.day), a.time.Compare(b.time), cmp.Compare(a.account.id, b.account.id),
			cmp.Compare(a.ref, b.ref), cmp.Compare(a.leg.Int64, b.leg.Int64))
	})
	yahoo := map[string]bool{}
	for i := range records {
		e := &records[i]
		x.file.Activities = append(x.file.Activities, gfActivityOf(e))
		if s := e.security; s != nil && e.profile == "" && s.manual == "" {
			yahoo[s.gfSymbol()] = true
		}
	}
	x.yahoo = slices.Sorted(maps.Keys(yahoo))
	x.idle = yahooIdle(in, events)
	return x
}

// gfAccounts returns the accounts of the file (6.3 of the design): each
// selected account with an activity or a snapshot, in the order of their
// ids, with every balance of toolBalances, whatever the export's last day,
// since Ghostfolio takes an account's balances only in the import that
// makes the account. An account is named, and has its id, as in every
// other export of it, so that Ghostfolio finds it again.
func gfAccounts(in *toolInput, events []toolEvent) []gfAccount {
	ids := map[int64]bool{}
	for _, e := range events {
		ids[e.account.id] = true
	}
	for _, s := range in.snapshots {
		if in.selected(s.account) && s.marketDate != "" {
			ids[s.account] = true
		}
	}
	balances := toolBalances(in)
	out := []gfAccount{}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		a := in.accountOf(id)
		account := gfAccount{Comment: "schwabtx " + a.label(), Currency: "USD", ID: a.gfID(), Name: a.name()}
		for _, b := range balances[id] {
			account.Balances = append(account.Balances, gfBalance{Date: gfMidnight(b.date), Value: json.Number(formatCents(b.cash))})
		}
		out = append(out, account)
	}
	return out
}

// gfAssets returns the asset profiles of the file and what each is (6.4 of
// the design): the cash profiles that an activity is of, in the order of
// gfCashProfiles, each with one price of 1 on the day of its first
// activity, so that Ghostfolio values it and takes its fees off net
// performance; then, in the order of listSecurities, each security that
// Ghostfolio gets as a MANUAL asset and that an activity is of or a
// selected account held, named from its identity alone, with its
// description in its comment and its snapshot prices.
func gfAssets(in *toolInput, events []toolEvent) ([]gfProfile, []gfAsset) {
	profiles, assets := []gfProfile{}, []gfAsset{}
	for _, c := range gfCashProfiles {
		first := nytime.Date("")
		for _, e := range events {
			if e.profile == c.profile && (first == "" || e.day < first) {
				first = e.day
			}
		}
		if first == "" {
			continue
		}
		profiles = append(profiles, gfProfile{
			Currency: "USD", DataSource: "MANUAL", MarketData: []gfPrice{{Date: gfMidnight(first), MarketPrice: "1"}},
			Name: c.name, Symbol: gfProfileSymbol(c.profile),
		})
		assets = append(assets, gfAsset{profile: c.profile, used: true})
	}
	for _, l := range listSecurities(in, events) {
		s := l.security
		if s.manual == "" {
			continue
		}
		p := gfProfile{Comment: s.name, Currency: "USD", CUSIP: s.cusip, DataSource: "MANUAL", Name: s.gfName(), Symbol: s.gfSymbol()}
		switch {
		case s.bond:
			p.AssetClass, p.AssetSubClass = "FIXED_INCOME", "BOND"
		case s.assetType == "EQUITY":
			p.AssetClass, p.AssetSubClass = "EQUITY", "STOCK"
		}
		for _, price := range l.prices {
			p.MarketData = append(p.MarketData, gfPrice{Date: gfMidnight(price.date), MarketPrice: json.Number(roundRat(price.price, 8))})
		}
		profiles = append(profiles, p)
		assets = append(assets, gfAsset{security: s, used: l.used})
	}
	return profiles, assets
}

// gfActivityOf returns the activity of record e, which Ghostfolio takes as
// an activity (3.4 of the design): a BUY or a SELL of its shares, at its
// gross over its shares, with its fees; a delivery's or an option close's
// BUY or SELL at its price, with no fee; a DIVIDEND of quantity 1 at its
// gross, with its fees; an INTEREST of quantity 1 at its cash; or a FEE of
// quantity 0 at 0, with its cash as the fee.
func gfActivityOf(e *toolEvent) gfActivity {
	a := gfActivity{
		AccountID: e.account.gfID(), Comment: gfComment(e), Currency: "USD",
		Date: e.gfDate(), Type: e.kind.gfType(),
	}
	if e.profile != "" {
		a.DataSource, a.Symbol = "MANUAL", gfProfileSymbol(e.profile)
	} else {
		a.DataSource, a.Symbol = e.security.gfDataSource(), e.security.gfSymbol()
	}
	switch e.kind {
	case evBuy, evSell:
		a.Quantity = json.Number(formatMicros(e.shares))
		a.UnitPrice = json.Number(roundRat(gfTradePrice(e.gross, e.shares), 10))
		a.Fee = json.Number(formatCents(e.fees))
	case evDeliveryIn, evDeliveryOut, evCloseIn, evCloseOut:
		// toolEvents prices every delivery, and closes an option at 0.
		a.Quantity = json.Number(formatMicros(e.shares))
		a.UnitPrice = json.Number(roundRat(e.price, 10))
		a.Fee = json.Number(formatCents(0))
	case evDividend:
		a.Quantity, a.UnitPrice, a.Fee = "1", json.Number(formatCents(e.gross)), json.Number(formatCents(e.fees))
	case evInterest, evFeesRefund, evTaxRefund:
		a.Quantity, a.UnitPrice, a.Fee = "1", json.Number(formatCents(e.value)), json.Number(formatCents(0))
	default:
		// Fees, an interest charge and taxes: a FEE.
		a.Quantity, a.UnitPrice, a.Fee = "0", "0", json.Number(formatCents(e.value))
	}
	return a
}

// gfComment returns the comment of record e's activity: "schwabtx", its
// account's label and its row's ref, and its leg when it is one leg of a
// trade of several (6.5 of the design). Ghostfolio compares comments when
// it looks for duplicates, so two equal trades of one day, or two accounts'
// equal dividends, are never taken for each other.
func gfComment(e *toolEvent) string {
	s := "schwabtx " + e.account.label() + " " + e.ref
	if e.leg.Valid {
		s += " leg " + strconv.FormatInt(e.leg.Int64, 10)
	}
	return s
}

// gfTradePrice returns a trade's unit price in dollars per share: its
// gross over its shares, which from |gross| in cents and shares in
// millionths, above 0, is |gross| × 10⁴ ÷ shares (7 of the design).
func gfTradePrice(gross, shares int64) *big.Rat {
	num := new(big.Int).Mul(new(big.Int).Abs(big.NewInt(gross)), big.NewInt(10_000))
	return new(big.Rat).SetFrac(num, big.NewInt(shares))
}

// gfMidnight returns midnight UTC of day, as Ghostfolio writes the dates of
// balances and prices.
func gfMidnight(day nytime.Date) string { return string(day) + "T00:00:00.000Z" }

// write writes f as ghostfolio.json: indented by two spaces, with &, < and
// > as they are (a description can hold &), and a newline at the end.
func (f *gfFile) write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}
