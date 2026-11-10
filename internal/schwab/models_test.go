package schwab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// readTestdata returns the fixture with the given name, from the fake
// Schwab's embedded testdata.
func readTestdata(t testing.TB, name string) []byte {
	t.Helper()
	return schwabtest.Fixture(t, name)
}

// elements returns the bytes of each element of the JSON array in b, as
// they are in b.
func elements(t *testing.T, b []byte) []json.RawMessage {
	t.Helper()
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		t.Fatal(err)
	}
	return raws
}

func noProblems[T any](t *testing.T, records []T, problems func(T) []Problem) {
	t.Helper()
	for i, r := range records {
		if ps := problems(r); len(ps) > 0 {
			t.Errorf("record %d has problems: %q", i, ps)
		}
	}
}

func TestDecodeAccountNumbers(t *testing.T) {
	b := readTestdata(t, "account_numbers.json")
	got, err := decodeList[AccountNumber](b)
	if err != nil {
		t.Fatal(err)
	}
	noProblems(t, got, func(a AccountNumber) []Problem { return a.Problems })
	raws := elements(t, b)
	if len(got) != 3 || got[1].AccountNumber != "61803398" || len(got[1].HashValue) != 64 {
		t.Fatalf("decoded %+v", got)
	}
	for i := range got {
		if !bytes.Equal(got[i].Raw, raws[i]) {
			t.Errorf("record %d: Raw is %s, want the bytes Schwab sent, %s", i, got[i].Raw, raws[i])
		}
	}
}

func TestDecodeAccounts(t *testing.T) {
	b := readTestdata(t, "accounts.json")
	got, err := decodeList[Account](b)
	if err != nil {
		t.Fatal(err)
	}
	noProblems(t, got, func(a Account) []Problem { return a.Problems })
	if len(got) != 3 {
		t.Fatalf("decoded %d accounts, want 3", len(got))
	}
	for _, a := range got {
		noProblems(t, a.SecuritiesAccount.Positions, func(p Position) []Problem { return p.Problems })
	}
	raws := elements(t, b)
	for i := range got {
		if !bytes.Equal(got[i].Raw, raws[i]) {
			t.Errorf("account %d: Raw is not the bytes Schwab sent", i)
		}
	}

	cash := got[0].SecuritiesAccount
	if cash.Type != "CASH" || cash.AccountNumber != "29470135" || len(cash.Positions) != 4 {
		t.Fatalf("first account: %+v", cash)
	}
	if v := cash.CurrentBalances.LiquidationValue; v == nil || *v != 6997.68 {
		t.Errorf("current liquidationValue = %v, want 6997.68", v)
	}
	// A real 0 is kept, and a figure Schwab did not send is nil.
	if v := cash.CurrentBalances.Savings; v == nil || *v != 0 {
		t.Errorf("savings = %v, want a present 0", v)
	}
	if cash.CurrentBalances.BuyingPower != nil || cash.ProjectedBalances.CashBalance != nil {
		t.Error("a cash account has figures that Schwab did not send")
	}
	if v := cash.InitialBalances.IsInCall; v == nil || *v {
		t.Errorf("isInCall = %v, want a present false", v)
	}
	if p := cash.IsPortfolioMargin; p == nil || *p {
		t.Errorf("isPortfolioMargin = %v, want a present false", p)
	}
	if cash.IsIntradayMargin != nil {
		t.Error("isIntradayMargin is set though Schwab did not send it")
	}
	if v := got[0].AggregatedBalance.CurrentIntradayBuyingPowerAmount; v == nil || *v != 1532.17 {
		t.Errorf("aggregatedBalance.currentIntradayBuyingPowerAmount = %v", v)
	}
	mmf := cash.Positions[3]
	if mmf.Instrument.AssetType != AssetMutualFund || mmf.Instrument.Type != "NO_LOAD_TAXABLE" || mmf.AgedQuantity != 1220.89 {
		t.Errorf("money market position: %+v", mmf)
	}
	if mmf.Raw == nil || !bytes.Contains(got[0].Raw, mmf.Raw) {
		t.Error("a position's Raw is not part of its account's")
	}
	if delisted := cash.Positions[2].Instrument; delisted.Symbol != delisted.CUSIP || delisted.NetChange != 0 {
		t.Errorf("delisted position's instrument: %+v", delisted)
	}

	margin := got[1].SecuritiesAccount
	put := margin.Positions[0]
	if put.Instrument.AssetType != AssetOption || put.Instrument.PutCall != "PUT" || put.Instrument.UnderlyingSymbol != "XYZ" {
		t.Errorf("short put's instrument: %+v", put.Instrument)
	}
	if put.ShortQuantity != 2 || put.SettledShortQuantity != -2 || put.MarketValue != -310 || put.LongOpenProfitLoss != 0 {
		t.Errorf("short put: %+v", put)
	}
	if v := margin.ProjectedBalances.IntradayBuyingPowerAmount; v == nil || *v != 8258.84 {
		t.Errorf("projected intradayBuyingPowerAmount = %v", v)
	}
	if long := margin.Positions[1]; long.LongQuantity != 1 || long.AveragePrice != 512 {
		t.Errorf("whole numbers written as integers: %+v", long)
	}

	old := got[2]
	if old.SecuritiesAccount.Positions != nil {
		t.Error("an account without positions has some")
	}
	if old.AggregatedBalance != (AggregatedBalance{}) || old.SecuritiesAccount.CurrentBalances.LiquidationValue != nil {
		t.Error("figures that a 2024 answer did not send are set")
	}
	if v := old.SecuritiesAccount.InitialBalances.LiquidationValue; v == nil || *v != 4855.5 {
		t.Errorf("initial liquidationValue = %v, want 4855.5", v)
	}
}

func TestDecodeTransactions(t *testing.T) {
	b := readTestdata(t, "transactions.json")
	got, err := decodeList[Transaction](b)
	if err != nil {
		t.Fatal(err)
	}
	noProblems(t, got, func(tx Transaction) []Problem { return tx.Problems })
	raws := elements(t, b)
	if len(got) != len(raws) {
		t.Fatalf("decoded %d transactions, want %d", len(got), len(raws))
	}
	byID := map[int64]Transaction{}
	for i, tx := range got {
		if !bytes.Equal(tx.Raw, raws[i]) {
			t.Errorf("transaction %d: Raw is not the bytes Schwab sent", tx.ActivityID)
		}
		byID[tx.ActivityID] = tx
	}

	sale := byID[73508261941]
	if sale.Type != TransactionTrade || sale.OrderID != 1004517638295 || sale.Description != "" || !sale.SettlementDate.IsZero() {
		t.Errorf("option sale: %+v", sale)
	}
	if want := time.Date(2026, 8, 4, 15, 22, 41, 0, time.UTC); sale.Time != want || sale.TradeDate != want {
		t.Errorf("option sale's time and tradeDate = %v, %v; want %v in UTC", sale.Time, sale.TradeDate, want)
	}
	if sale.TradeDay() != "2026-08-04" || sale.SettlementDay() != "" {
		t.Errorf("option sale's days = %q, %q", sale.TradeDay(), sale.SettlementDay())
	}
	leg := sale.TransferItems[4]
	if leg.Amount != -1 || leg.Cost != 312 || leg.Price != 3.12 || leg.PositionEffect != "CLOSING" {
		t.Errorf("option leg: %+v", leg)
	}
	call := leg.Instrument
	if call.ExpirationDate != time.Date(2026, 9, 18, 4, 0, 0, 0, time.UTC) || call.StrikePrice != 50 || call.OptionPremiumMultiplier != 100 {
		t.Errorf("call: %+v", call)
	}
	if d := call.OptionDeliverables; len(d) != 1 || d[0].Deliverable == nil || d[0].Deliverable.Symbol != "XYZ" || d[0].DeliverableUnits != 100 {
		t.Errorf("call's deliverables: %+v", d)
	}
	if DateInNewYork(call.ExpirationDate) != "2026-09-18" {
		t.Errorf("expiration day = %s", DateInNewYork(call.ExpirationDate))
	}
	var fees float64
	for _, it := range sale.TransferItems {
		if it.Instrument.AssetType == AssetCurrency {
			fees -= it.Cost
		}
	}
	if !near(fees, 0.69) {
		t.Errorf("fees = %v, want 0.69", fees)
	}

	if put := byID[73508261944].TransferItems[4].Instrument; put.UniformSymbol != "" || put.OptionDeliverables[0].Deliverable != nil {
		t.Errorf("a 2025 option has 2026 fields: %+v", put)
	}

	futures := byID[73508261945]
	var named, unnamed int
	var cost, futuresFees float64
	for _, it := range futures.TransferItems {
		cost += it.Cost
		if it.Instrument.AssetType == AssetCurrency {
			futuresFees -= it.Cost
			if it.FeeType == "" {
				unnamed++
			} else {
				named++
			}
		}
	}
	if named != 2 || unnamed != 6 || !near(futuresFees, 2.82) || !near(cost, futures.NetAmount) {
		t.Errorf("futures-option fill: %d named and %d unnamed fee legs, fees %v, legs' cost %v against %v", named, unnamed, futuresFees, cost, futures.NetAmount)
	}
	if futures.TradeDay() != "2025-01-14" {
		t.Errorf("an overnight fill at 00:15 in New York is on %s", futures.TradeDay())
	}

	if midnight := byID[73508261942]; midnight.TradeDay() != "2026-03-13" || midnight.Time.Equal(midnight.TradeDate) {
		t.Errorf("an execution booked at midnight: %+v", midnight)
	}
	if reinvest := byID[73508261946]; reinvest.SettlementDay() != "2024-04-08" || reinvest.OrderID != 0 || reinvest.PositionID == 0 {
		t.Errorf("reinvestment: %+v", reinvest)
	}
	interest := byID[73508261949]
	if interest.TradeDay() != "2024-04-30" || DateInNewYork(interest.Time) != "2024-04-30" || interest.Time.Format("01-02") != "05-01" {
		t.Errorf("month-end interest: posted %v, booked on %s", interest.Time, interest.TradeDay())
	}
	if q := byID[73508261948].QualifiedDividend; q == nil || *q {
		t.Errorf("qualifiedDividend = %v, want a present false", q)
	}
	if q := byID[73508261950].QualifiedDividend; q != nil {
		t.Errorf("qualifiedDividend = %v, though Schwab did not send it", *q)
	}
	if w := byID[73508261951]; w.TradeDay() != "2023-12-15" || w.TradeDate.Hour() != 5 {
		t.Errorf("a winter date: %v, %s", w.TradeDate, w.TradeDay())
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestDecodeOneTransaction(t *testing.T) {
	b := readTestdata(t, "transaction.json")
	got, err := decodeOne[Transaction](b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Problems) > 0 || !bytes.Equal(got.Raw, bytes.TrimSpace(b)) {
		t.Fatalf("problems %q, Raw %s", got.Problems, got.Raw)
	}
	// Its instrument names assetType twice; the last one counts.
	if in := got.TransferItems[0].Instrument; in.AssetType != AssetCollectiveInvestment || in.ClosingPrice != 72.04 {
		t.Errorf("instrument: %+v", in)
	}
}

func TestDecodeUserPreference(t *testing.T) {
	b := readTestdata(t, "user_preference.json")
	got, err := decodeOne[UserPreference](b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Problems) > 0 || len(got.Accounts) != 3 || len(got.StreamerInfo) != 1 || len(got.Offers) != 1 {
		t.Fatalf("decoded %+v", got)
	}
	if !bytes.Equal(got.Raw, bytes.TrimSpace(b)) {
		t.Errorf("Raw is not the bytes Schwab sent")
	}
	if a := got.Accounts[1]; a.NickName != "Options & Margin" || a.DisplayAcctID != "...398" || a.LotSelectionMethod != "FIFO" || a.PrimaryAccount {
		t.Errorf("second account: %+v", a)
	}
	if got.Accounts[2].LotSelectionMethod != "" {
		t.Error("lotSelectionMethod is set though Schwab did not send it")
	}
	if s := got.StreamerInfo[0]; s.StreamerSocketURL != "wss://streamer-api.schwab.com/ws" || s.SchwabClientFunctionID != "APIAPP" {
		t.Errorf("streamer info: %+v", s)
	}
}

// liveKeys are the keys seen in Schwab's real answers for each type,
// counted from the public captures the API notes cite. Each must be a field
// and appear in the testdata, so that a key missing from both fails.
var liveKeys = map[reflect.Type][]string{
	reflect.TypeFor[AccountNumber](): {"accountNumber", "hashValue"},
	reflect.TypeFor[Account]():       {"securitiesAccount", "aggregatedBalance"},
	reflect.TypeFor[AggregatedBalance](): {
		"currentLiquidationValue", "liquidationValue", "intradayBuyingPowerAmount", "currentIntradayBuyingPowerAmount",
	},
	reflect.TypeFor[SecuritiesAccount](): {
		"type", "accountNumber", "roundTrips", "isDayTrader", "isClosingOnlyRestricted", "pfcbFlag",
		"isPortfolioMargin", "isIntradayMargin", "positions", "initialBalances", "currentBalances", "projectedBalances",
	},
	reflect.TypeFor[Balances](): {
		"accountValue", "accruedInterest", "availableFunds", "availableFundsNonMarginableTrade", "bondValue",
		"buyingPower", "buyingPowerNonMarginableTrade", "cashAvailableForTrading", "cashAvailableForWithdrawal",
		"cashBalance", "cashCall", "cashDebitCallValue", "cashReceipts", "dayTradingBuyingPower",
		"dayTradingBuyingPowerCall", "dayTradingEquityCall", "equity", "equityPercentage",
		"intradayBuyingPowerAmount", "isInCall", "liquidationValue", "longMarginValue", "longMarketValue",
		"longNonMarginableMarketValue", "longOptionMarketValue", "longStockValue", "maintenanceCall",
		"maintenanceRequirement", "margin", "marginBalance", "marginEquity", "moneyMarketFund", "mutualFundValue",
		"pendingDeposits", "regTCall", "savings", "shortBalance", "shortMarginValue", "shortMarketValue",
		"shortOptionMarketValue", "shortStockValue", "sma", "stockBuyingPower", "totalCash", "unsettledCash",
	},
	reflect.TypeFor[Position](): {
		"instrument", "longQuantity", "shortQuantity", "settledLongQuantity", "settledShortQuantity",
		"previousSessionLongQuantity", "previousSessionShortQuantity", "agedQuantity", "averagePrice",
		"averageLongPrice", "averageShortPrice", "taxLotAverageLongPrice", "taxLotAverageShortPrice", "marketValue",
		"longOpenProfitLoss", "shortOpenProfitLoss", "maintenanceRequirement", "currentDayProfitLoss",
		"currentDayProfitLossPercentage", "currentDayCost",
	},
	reflect.TypeFor[Instrument](): {
		"assetType", "symbol", "uniformSymbol", "cusip", "description", "instrumentId", "type", "status",
		"netChange", "closingPrice", "putCall", "strikePrice", "expirationDate", "optionPremiumMultiplier",
		"underlyingSymbol", "underlyingCusip", "optionDeliverables",
	},
	reflect.TypeFor[OptionDeliverable](): {"rootSymbol", "strikePercent", "deliverableNumber", "deliverableUnits", "deliverable"},
	reflect.TypeFor[Deliverable](): {
		"assetType", "symbol", "uniformSymbol", "description", "instrumentId", "type", "status", "closingPrice",
	},
	reflect.TypeFor[Transaction](): {
		"activityId", "time", "tradeDate", "settlementDate", "accountNumber", "type", "status", "subAccount",
		"description", "netAmount", "orderId", "positionId", "qualifiedDividend", "transferItems",
	},
	reflect.TypeFor[TransferItem]():   {"instrument", "amount", "cost", "price", "feeType", "positionEffect"},
	reflect.TypeFor[UserPreference](): {"accounts", "streamerInfo", "offers"},
	reflect.TypeFor[PreferenceAccount](): {
		"accountNumber", "primaryAccount", "type", "nickName", "displayAcctId", "autoPositionEffect",
		"accountColor", "lotSelectionMethod",
	},
	reflect.TypeFor[StreamerInfo](): {
		"streamerSocketUrl", "schwabClientCustomerId", "schwabClientCorrelId", "schwabClientChannel",
		"schwabClientFunctionId",
	},
	reflect.TypeFor[Offer](): {"level2Permissions", "mktDataPermission"},
}

// field is a struct field as JSON sees it.
type field struct {
	t        reflect.Type
	optional bool // tagged omitempty or omitzero
}

// jsonFields returns the fields of struct type t by JSON key.
func jsonFields(t reflect.Type) map[string]field {
	fields := map[string]field{}
	for f := range t.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "-" || name == "" {
			continue
		}
		fields[name] = field{t: f.Type, optional: strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")}
	}
	return fields
}

// testdataCensus walks every record of the testdata, each fixture that
// answers an account endpoint read as that endpoint's records, and returns
// the census and the keys that no field takes, each after the file that
// holds it.
func testdataCensus(t *testing.T) (c *census, unknown []string) {
	t.Helper()
	c = newCensus()
	for _, a := range testdataAnswers(t) {
		for _, r := range a.records {
			var v any
			if err := json.Unmarshal(r, &v); err != nil {
				t.Fatalf("%s: %v", a.file, err)
			}
			keys, _ := c.add(a.typ, v)
			for _, k := range keys {
				unknown = append(unknown, a.file+": "+k.String())
			}
		}
	}
	return c, unknown
}

// The structs take every key in the testdata, every key seen live is both a
// field and in the testdata, and a field whose tag does not mark it as
// optional is in every record of its kind, as in Schwab's answers.
func TestModelsCoverEveryKey(t *testing.T) {
	c, unknown := testdataCensus(t)
	if len(unknown) > 0 {
		slices.Sort(unknown)
		t.Errorf("testdata keys that no field takes: %v", slices.Compact(unknown))
	}
	for typ, keys := range liveKeys {
		fields := jsonFields(typ)
		seen := map[string]bool{}
		for _, r := range c.records[typ] {
			for k := range r {
				seen[k] = true
			}
		}
		for _, k := range keys {
			if _, ok := fields[k]; !ok {
				t.Errorf("%s has no field for %q, which Schwab sends", typ.Name(), k)
			}
			if !seen[k] {
				t.Errorf("no %s in the testdata has %q, which Schwab sends", typ.Name(), k)
			}
		}
	}
	for typ, records := range c.records {
		for k, f := range jsonFields(typ) {
			if f.optional {
				continue
			}
			for i, r := range records {
				if !r[k] {
					t.Errorf("%s %d lacks %q, whose tag says every %s has it", typ.Name(), i, k, typ.Name())
				}
			}
		}
	}
}

// standAloneProblems returns where an account number of files, the value
// of an accountNumber key in any case, as a string or a whole number, as
// Schwab may send it, is part of another value of theirs, or of another
// account number, so that a test that looks for leaked account numbers can
// search for them as text; and how many account numbers they hold. It
// reads every file that holds JSON.
func standAloneProblems(t *testing.T, files []indexedFile) (problems []string, accounts int) {
	t.Helper()
	var numbers []string
	var values []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				walk(k, x)
			}
		case []any:
			for _, x := range v {
				walk(key, x)
			}
		case string:
			if strings.EqualFold(key, "accountNumber") {
				numbers = append(numbers, v)
			} else {
				values = append(values, v)
			}
		default:
			b, _ := json.Marshal(v)
			if n, ok := v.(float64); ok && strings.EqualFold(key, "accountNumber") && n == float64(int64(n)) {
				numbers = append(numbers, string(b))
			} else {
				values = append(values, string(b))
			}
		}
	}
	for _, f := range files {
		if schwabtest.ExtensionFor(f.body) != ".json" {
			continue
		}
		var v any
		if err := json.Unmarshal(f.body, &v); err != nil {
			t.Fatalf("%s: %v", f.entry.File, err)
		}
		walk("", v)
	}
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	for _, n := range numbers {
		for _, v := range values {
			if strings.Contains(v, n) {
				problems = append(problems, fmt.Sprintf("account number %s is in the value %q", n, v))
			}
		}
		for _, m := range numbers {
			if m != n && strings.Contains(m, n) {
				problems = append(problems, fmt.Sprintf("account number %s is in account number %s", n, m))
			}
		}
	}
	return problems, len(numbers)
}

// No account number is part of another value in the testdata, in any file
// of the fake's index that holds JSON, a recording as well as the hand-made
// ones, so that a test that looks for leaked account numbers can search for
// them as text. An account number sent as a number is one too, at its key
// in any case.
func TestTestdataAccountNumbersStandAlone(t *testing.T) {
	problems, accounts := standAloneProblems(t, indexedFiles(t))
	if accounts < len(schwabtest.FixtureAccounts) {
		t.Errorf("the JSON files hold %d account numbers, fewer than the fixtures' %d", accounts, len(schwabtest.FixtureAccounts))
	}
	for _, p := range problems {
		t.Error(p)
	}
	numberAt := `[{"accountNumber":` + schwabtest.FixtureAccounts[0].Number + `,"x":"` + schwabtest.FixtureAccounts[1].Number + `"},` +
		`{"AccountNumber":"` + schwabtest.FixtureAccounts[1].Number + `","y":1` + schwabtest.FixtureAccounts[0].Number + `}]`
	if problems, accounts := standAloneProblems(t, []indexedFile{{body: []byte(numberAt)}}); accounts != 2 || len(problems) != 2 {
		t.Errorf("account numbers as a number and at a key in another case: %d, %q", accounts, problems)
	}
}

// Records as Schwab sends them, with the keys that it sends on every record
// of their kind and no others, for tests to change.
const (
	baseTransaction = `{"activityId":1,"time":"2024-04-04T15:56:07+0000","accountNumber":"29470135","type":"TRADE",` +
		`"status":"VALID","subAccount":"CASH","tradeDate":"2024-04-04T15:56:07+0000","netAmount":1,"transferItems":[]}`
	baseLeg      = `{"instrument":{"assetType":"EQUITY","symbol":"X"},"amount":1,"cost":-1}`
	basePosition = `{"instrument":{"assetType":"EQUITY","symbol":"X"},"longQuantity":1,"shortQuantity":0,` +
		`"settledLongQuantity":1,"settledShortQuantity":0,"averagePrice":5,"marketValue":5,"maintenanceRequirement":0,` +
		`"currentDayProfitLoss":0,"currentDayProfitLossPercentage":0,"currentDayCost":0}`
	baseSecuritiesAccount = `{"type":"MARGIN","accountNumber":"61803398","roundTrips":0,"isDayTrader":false,` +
		`"isClosingOnlyRestricted":false,"pfcbFlag":false,"initialBalances":{"liquidationValue":5},` +
		`"currentBalances":{"liquidationValue":5},"projectedBalances":{"buyingPower":5}}`
)

// withKeys returns obj, a JSON object, with changes made: each is a key and
// the raw JSON to give it, or "" to take the key out. A key that obj lacks
// is added at the end; the others keep their places.
func withKeys(obj string, changes ...string) string {
	if len(changes)%2 != 0 {
		panic("withKeys takes keys and values in pairs")
	}
	type member struct{ key, value string }
	var ms []member
	dec := json.NewDecoder(strings.NewReader(obj))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		panic("withKeys takes an object: " + obj)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			panic(err)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			panic(err)
		}
		ms = append(ms, member{key.(string), string(value)})
	}
	for i := 0; i < len(changes); i += 2 {
		key, value := changes[i], changes[i+1]
		j := slices.IndexFunc(ms, func(m member) bool { return m.key == key })
		switch {
		case j >= 0 && value == "":
			ms = slices.Delete(ms, j, j+1)
		case j >= 0:
			ms[j].value = value
		case value != "":
			ms = append(ms, member{key, value})
		}
	}
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = strconv.Quote(m.key) + ":" + m.value
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// missing returns the problem of each key being missing.
func missing(keys ...string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + ": missing, though Schwab sends it on every record of its kind"
	}
	return out
}

func TestDecodeLeniently(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		check    func(t *testing.T, tx Transaction)
		problems []string
	}{
		{
			name: "a number sent as a string",
			json: withKeys(baseTransaction, "netAmount", `"12.5"`, "description", `"kept"`),
			check: func(t *testing.T, tx Transaction) {
				want(t, tx.Description == "kept" && tx.NetAmount == 0, "fields: %+v", tx)
			},
			problems: []string{"netAmount: Schwab sent a string where a number was expected"},
		},
		{
			name:     "an id written as a fraction",
			json:     withKeys(baseTransaction, "activityId", "1.0"),
			problems: []string{"activityId: Schwab sent the number 1.0 where a whole number was expected"},
		},
		{
			name: "every key that Schwab always sends missing",
			json: `{"description":"all there is"}`,
			problems: missing("accountNumber", "type", "status", "subAccount", "transferItems",
				"activityId", "time", "tradeDate", "netAmount"),
		},
		{
			name: "null where values belong",
			json: withKeys(baseTransaction, "activityId", "null", "time", "null", "type", "null", "netAmount", "null",
				"transferItems", "null", "settlementDate", "null"),
			problems: missing("type", "transferItems", "activityId", "time", "netAmount"),
		},
		{
			name:     "nothing where values belong",
			json:     withKeys(baseTransaction, "activityId", "0", "type", `""`, "status", `""`),
			problems: missing("activityId", "type", "status"),
		},
		{
			name:     "a time that is not one",
			json:     withKeys(baseTransaction, "time", `"2024-13-45"`),
			problems: []string{`time: Schwab sent "2024-13-45", which is not a time`},
		},
		{
			name:     "a time sent as a number",
			json:     withKeys(baseTransaction, "time", "1712246167"),
			problems: []string{"time: Schwab sent a number where a time was expected"},
		},
		{
			// The third leg's closing price has the second's problem, which is
			// named once.
			name: "strange legs",
			json: withKeys(baseTransaction, "transferItems", "["+
				withKeys(baseLeg, "instrument", `{"assetType":"OPTION","symbol":"X","expirationDate":"soon"}`, "amount", "2")+","+
				withKeys(baseLeg, "instrument", `{"assetType":"CURRENCY","symbol":"CURRENCY_USD","closingPrice":"x"}`, "amount", "3")+","+
				withKeys(baseLeg, "instrument", `{"assetType":"CURRENCY","symbol":"CURRENCY_USD","closingPrice":"y"}`, "cost", "")+"]"),
			check: func(t *testing.T, tx Transaction) {
				want(t, len(tx.TransferItems) == 3 && tx.TransferItems[1].Amount == 3, "legs: %+v", tx.TransferItems)
			},
			problems: []string{
				"transferItems.cost: missing, though Schwab sends it on every record of its kind",
				`transferItems.instrument.expirationDate: Schwab sent "soon", which is not a time`,
				"transferItems.instrument.closingPrice: Schwab sent a string where a number was expected",
			},
		},
		{
			name:     "legs sent as an object",
			json:     withKeys(baseTransaction, "transferItems", "{}"),
			problems: []string{"transferItems: Schwab sent an object where an array was expected"},
		},
		{
			name:     "a number too large",
			json:     withKeys(baseTransaction, "netAmount", "1e400"),
			problems: []string{"netAmount: Schwab sent the number 1e400, which is too large"},
		},
		{
			name:     "null instead of a transaction",
			json:     `null`,
			problems: []string{"Schwab sent null where an object was expected"},
		},
		{
			name:     "a number instead of a transaction",
			json:     `5`,
			problems: []string{"Schwab sent a number where an object was expected"},
		},
		{
			name: "keys Schwab adds later, and keys in another case",
			json: withKeys(baseTransaction, "activityId", "", "time", "", "ActivityId", "7", "Time", `"2024-04-04T15:56:07Z"`,
				"futureField", `{"a":[1]}`),
			check: func(t *testing.T, tx Transaction) {
				want(t, tx.ActivityID == 7 && !tx.Time.IsZero() && bytes.Contains(tx.Raw, []byte("futureField")), "fields: %+v", tx)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tx Transaction
			if err := json.Unmarshal([]byte(tt.json), &tx); err != nil {
				t.Fatalf("UnmarshalJSON failed: %v", err)
			}
			if got := problemStrings(tx.Problems); !slices.Equal(got, tt.problems) {
				t.Errorf("problems:\n got %q\nwant %q", got, tt.problems)
			}
			if string(tx.Raw) != tt.json {
				t.Errorf("Raw = %s, want %s", tx.Raw, tt.json)
			}
			if tt.check != nil {
				tt.check(t, tx)
			}
		})
	}
}

// problemStrings returns each problem as text.
func problemStrings(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

func want(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}

// One record that cannot be read costs itself, not the records after it.
func TestDecodeKeepsTheRestOfTheList(t *testing.T) {
	body := "[" + withKeys(baseTransaction, "netAmount", `"x"`) + ",null,7," +
		withKeys(baseTransaction, "activityId", "2", "netAmount", "3") + "]"
	got, err := decodeList[Transaction]([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3].ActivityID != 2 || got[3].NetAmount != 3 || len(got[3].Problems) != 0 {
		t.Fatalf("decoded %+v", got)
	}
	if ps, want := problemStrings(got[0].Problems), []string{"netAmount: Schwab sent a string where a number was expected"}; !slices.Equal(ps, want) {
		t.Errorf("first transaction's problems:\n got %q\nwant %q", ps, want)
	}
	if got[1].Problems == nil || got[2].Problems == nil || string(got[1].Raw) != "null" {
		t.Errorf("the strange records: %+v, %+v", got[1], got[2])
	}
}

// The records other than transactions name what they lack, and a key that
// they read by hand is named once.
func TestDecodeOtherRecordsLeniently(t *testing.T) {
	var n AccountNumber
	if err := json.Unmarshal([]byte(`{"accountNumber":"","hashValue":""}`), &n); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(n.Problems), missing("accountNumber", "hashValue"); !slices.Equal(got, want) {
		t.Errorf("account number's problems:\n got %q\nwant %q", got, want)
	}
	var a Account
	if err := json.Unmarshal([]byte(`{"securitiesAccount":`+withKeys(baseSecuritiesAccount, "accountNumber", `""`)+"}"), &a); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(a.Problems), missing("securitiesAccount.accountNumber"); !slices.Equal(got, want) {
		t.Errorf("account's problems:\n got %q\nwant %q", got, want)
	}
	var p Position
	if err := json.Unmarshal([]byte(withKeys(basePosition, "instrument", `{"assetType":"","symbol":"X"}`,
		"longQuantity", `"1"`, "marketValue", `"x"`)), &p); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(p.Problems), []string{
		"longQuantity: Schwab sent a string where a number was expected",
		"marketValue: Schwab sent a string where a number was expected",
		"instrument.assetType: missing, though Schwab sends it on every record of its kind",
	}; !slices.Equal(got, want) {
		t.Errorf("position's problems:\n got %q\nwant %q", got, want)
	}
	var u UserPreference
	b := `{"accounts":{},"offers":[{"level2Permissions":"yes"}]}`
	if err := json.Unmarshal([]byte(b), &u); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(u.Problems), []string{
		"accounts: Schwab sent an object where an array was expected",
		"streamerInfo: missing, though Schwab sends it on every record of its kind",
		"offers.level2Permissions: Schwab sent a string where a boolean was expected",
		"offers.mktDataPermission: missing, though Schwab sends it on every record of its kind",
	}; !slices.Equal(got, want) || string(u.Raw) != b {
		t.Errorf("preferences' problems:\n got %q\nwant %q", got, want)
	}
}

// Records read one at a time from a stream keep their own bytes, though the
// decoder reuses its buffer.
func TestRawSurvivesAStream(t *testing.T) {
	var buf bytes.Buffer
	var want []string
	for i := range 100 {
		s := fmt.Sprintf(`{"accountNumber":"%08d","hashValue":"%064d"}`, i, i)
		want = append(want, s)
		buf.WriteString(s + "\n")
	}
	dec := json.NewDecoder(&buf)
	var got []AccountNumber
	for dec.More() {
		var a AccountNumber
		if err := dec.Decode(&a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d records, want %d", len(got), len(want))
	}
	for i, a := range got {
		if string(a.Raw) != want[i] {
			t.Errorf("record %d: Raw is %s, want %s", i, a.Raw, want[i])
		}
	}
}

func TestDecodeAccountLeniently(t *testing.T) {
	body := `{"securitiesAccount":` + withKeys(baseSecuritiesAccount,
		"currentBalances", `{"isInCall":0,"cashBalance":12.5}`,
		"positions", "["+withKeys(basePosition, "marketValue", "")+","+withKeys(basePosition, "instrument", "null")+"]") + "}"
	var a Account
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(a.Problems), []string{
		"securitiesAccount.currentBalances.isInCall: Schwab sent a number where a boolean was expected",
	}; !slices.Equal(got, want) {
		t.Errorf("account's problems:\n got %q\nwant %q", got, want)
	}
	if v := a.SecuritiesAccount.CurrentBalances.IsInCall; v != nil {
		t.Errorf("isInCall = %v, want nil, since Schwab's value could not be read", *v)
	}
	if v := a.SecuritiesAccount.CurrentBalances.CashBalance; v == nil || *v != 12.5 {
		t.Errorf("cashBalance = %v, want 12.5 though isInCall could not be read", v)
	}
	positions := a.SecuritiesAccount.Positions
	if len(positions) != 2 {
		t.Fatalf("positions: %+v", positions)
	}
	if got, want := problemStrings(positions[0].Problems), missing("marketValue"); !slices.Equal(got, want) {
		t.Errorf("first position's problems:\n got %q\nwant %q", got, want)
	}
	// A null instrument is missing, once, not also an instrument without an
	// asset type.
	if got, want := problemStrings(positions[1].Problems), missing("instrument"); !slices.Equal(got, want) {
		t.Errorf("second position's problems:\n got %q\nwant %q", got, want)
	}
}

// decodeAny decodes b as JSON into maps, slices and json.Numbers, so that
// it encodes again with its numbers as written.
func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// alwaysPresent returns the paths in v, a record of type t decoded as any,
// of the keys that t, or a type within it, takes in a field without an omit
// option. A path is the keys and array indexes that lead to the key. It does
// not look into an account's positions, which are records of their own.
func alwaysPresent(t reflect.Type, v any, path []any) [][]any {
	t = derefType(t)
	var paths [][]any
	switch {
	case t.Kind() == reflect.Slice:
		s, _ := v.([]any)
		for i, e := range s {
			paths = append(paths, alwaysPresent(t.Elem(), e, append(slices.Clip(path), i))...)
		}
	case t.Kind() == reflect.Struct && t != timeType:
		m, _ := v.(map[string]any)
		fields := jsonFields(t)
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			x, ok := m[name]
			if !ok {
				continue
			}
			p := append(slices.Clip(path), name)
			if !fields[name].optional {
				paths = append(paths, p)
			}
			if fields[name].t != reflect.TypeFor[[]Position]() {
				paths = append(paths, alwaysPresent(fields[name].t, x, p)...)
			}
		}
	}
	return paths
}

// gone, given to changed, takes a key out.
var gone = new(int)

// changed returns v, JSON decoded as any, with the value at path set to to,
// or taken out if to is gone. v itself is left as it was.
func changed(v any, path []any, to any) any {
	if i, ok := path[0].(int); ok {
		s := slices.Clone(v.([]any))
		s[i] = changed(s[i], path[1:], to)
		return s
	}
	m := maps.Clone(v.(map[string]any))
	switch key := path[0].(string); {
	case len(path) > 1:
		m[key] = changed(m[key], path[1:], to)
	case to == gone:
		delete(m, key)
	default:
		m[key] = to
	}
	return m
}

// keyPath returns the problem path of path: its keys, without the indexes.
func keyPath(path []any) string {
	var keys []string
	for _, k := range path {
		if s, ok := k.(string); ok {
			keys = append(keys, s)
		}
	}
	return strings.Join(keys, ".")
}

// testdataRecords returns the records of the testdata by type, the positions
// of the accounts among them: those of each fixture that answers an account
// endpoint, read as that endpoint's records, in testdataAnswers' order.
func testdataRecords(t testing.TB) map[reflect.Type][]json.RawMessage {
	t.Helper()
	records := map[reflect.Type][]json.RawMessage{}
	positions := reflect.TypeFor[Position]()
	for _, a := range testdataAnswers(t) {
		records[a.typ] = append(records[a.typ], a.records...)
		if a.typ != reflect.TypeFor[Account]() {
			continue
		}
		for _, account := range a.records {
			var held struct {
				SecuritiesAccount struct {
					Positions []json.RawMessage `json:"positions"`
				} `json:"securitiesAccount"`
			}
			if err := json.Unmarshal(account, &held); err != nil {
				t.Fatal(err)
			}
			records[positions] = append(records[positions], held.SecuritiesAccount.Positions...)
		}
	}
	return records
}

// Taking out, or making null, any one key of a testdata record that Schwab
// sends on every record of its kind, at any depth, gives the record that
// key's absence as its one problem.
func TestDropAnyAlwaysPresentKey(t *testing.T) {
	seen := map[string]bool{}
	for typ, records := range testdataRecords(t) {
		for i, raw := range records {
			record := decodeAny(t, raw)
			for _, path := range alwaysPresent(typ, record, nil) {
				for _, to := range []any{gone, nil} {
					b, err := json.Marshal(changed(record, path, to))
					if err != nil {
						t.Fatal(err)
					}
					v := reflect.New(typ)
					if err := json.Unmarshal(b, v.Interface()); err != nil {
						t.Fatal(err)
					}
					if got, want := problemStrings(problemsField(v.Elem())), missing(keyPath(path)); !slices.Equal(got, want) {
						how := "taken out"
						if to == nil {
							how = "null"
						}
						t.Errorf("%s %d with %v %s: problems %q, want %q", typ.Name(), i, path, how, got, want)
					}
				}
				seen[keyPath(path)] = true
			}
		}
	}
	// The cases reach through arrays, pointers and nested records.
	for _, path := range []string{
		"securitiesAccount.currentBalances", "instrument.assetType", "accounts.nickName",
		"transferItems.instrument.optionDeliverables.deliverable.symbol",
	} {
		if !seen[path] {
			t.Errorf("no case takes out %s", path)
		}
	}
}

func TestReadTime(t *testing.T) {
	at := time.Date(2024, 3, 28, 21, 10, 42, 0, time.UTC)
	for _, in := range []string{
		"2024-03-28T21:10:42+0000",
		"2024-03-28T21:10:42.000+0000",
		"2024-03-28T21:10:42Z",
		"2024-03-28T21:10:42.000Z",
		"2024-03-28T21:10:42.000+00:00",
		"2024-03-28T17:10:42-04:00",
		"2024-03-28T17:10:42-0400",
	} {
		var ps []Problem
		got, present := readTime(json.RawMessage(`"`+in+`"`), "time", &ps)
		if got != at || !present || ps != nil {
			t.Errorf("readTime(%q) = %v, %v, %q; want %v in UTC", in, got, present, ps, at)
		}
	}
	// A date alone is midnight in New York, as Schwab writes dates.
	var ps []Problem
	if got, _ := readTime(json.RawMessage(`"2024-03-28"`), "tradeDate", &ps); got != time.Date(2024, 3, 28, 4, 0, 0, 0, time.UTC) || ps != nil {
		t.Errorf("a date alone read as %v, %q", got, ps)
	}
	for _, in := range []string{``, `null`} {
		ps = nil
		if got, present := readTime(json.RawMessage(in), "time", &ps); !got.IsZero() || present || ps != nil {
			t.Errorf("readTime(%q) = %v, %v, %q; want nothing", in, got, present, ps)
		}
	}
	for in, problem := range map[string]string{
		`""`:                    "time: Schwab sent an empty string, which is not a time",
		`"garbage"`:             `time: Schwab sent "garbage", which is not a time`,
		`"2024-03-28T21:10:42"`: `time: Schwab sent "2024-03-28T21:10:42", which is not a time`,
		`true`:                  "time: Schwab sent a boolean where a time was expected",
		`{}`:                    "time: Schwab sent an object where a time was expected",
		// An offset out of range is named as such, though it would also take
		// the instant outside the years 0 to 9999.
		`"9999-12-31T23:59:59-2400"`:  `time: Schwab sent "9999-12-31T23:59:59-2400", whose offset from UTC is out of range`,
		`"0000-01-01T00:00:00+24:00"`: `time: Schwab sent "0000-01-01T00:00:00+24:00", whose offset from UTC is out of range`,
	} {
		ps = nil
		if got, present := readTime(json.RawMessage(in), "time", &ps); !got.IsZero() || !present || !slices.Equal(problemStrings(ps), []string{problem}) {
			t.Errorf("readTime(%s) = %v, %v, %q; want the zero time and %q", in, got, present, ps, problem)
		}
	}

	// The years that time.Time encodes, 0 to 9999 in UTC, are kept to their
	// last second, and an instant that its offset takes outside them is
	// refused, since no record holding it could be encoded.
	for _, tt := range []struct {
		in   string
		want time.Time // the zero time for one refused
	}{
		{"0000-01-01T00:00:00Z", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"0000-01-01T01:00:00+0100", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"9999-12-31T23:59:59Z", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
		{"9999-12-31T22:59:59.999-01:00", time.Date(9999, 12, 31, 23, 59, 59, 999e6, time.UTC)},
		{"0000-01-01T00:00:00+0100", time.Time{}},
		{"0000-01-01T00:59:59.999+01:00", time.Time{}},
		{"9999-12-31T23:59:59-2359", time.Time{}},
		{"9999-12-31T23:59:59-0001", time.Time{}},
	} {
		ps = nil
		got, present := readTime(json.RawMessage(`"`+tt.in+`"`), "time", &ps)
		if tt.want.IsZero() {
			want := []string{fmt.Sprintf("time: Schwab sent %q, which is outside the years 0 to 9999 in UTC", tt.in)}
			if !got.IsZero() || !present || !slices.Equal(problemStrings(ps), want) {
				t.Errorf("readTime(%q) = %v, %v, %q; want the zero time and %q", tt.in, got, present, ps, want)
			}
			continue
		}
		if got != tt.want || !present || ps != nil {
			t.Errorf("readTime(%q) = %v, %v, %q; want %v", tt.in, got, present, ps, tt.want)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Errorf("readTime(%q) = %v, which does not encode: %v", tt.in, got, err)
		}
	}
}

func TestDateInNewYork(t *testing.T) {
	for _, tt := range []struct {
		at   time.Time
		want Date
	}{
		{time.Time{}, ""},
		// Midnight in New York, written in UTC, on both sides of each change of
		// the clocks in 2024.
		{time.Date(2024, 3, 8, 5, 0, 0, 0, time.UTC), "2024-03-08"},
		{time.Date(2024, 3, 11, 4, 0, 0, 0, time.UTC), "2024-03-11"},
		{time.Date(2024, 11, 1, 4, 0, 0, 0, time.UTC), "2024-11-01"},
		{time.Date(2024, 11, 4, 5, 0, 0, 0, time.UTC), "2024-11-04"},
		// An evening execution has its date in New York, not the next day's in
		// UTC, in winter and in summer.
		{time.Date(2024, 3, 1, 1, 30, 0, 0, time.UTC), "2024-02-29"},
		{time.Date(2024, 7, 2, 3, 59, 0, 0, time.UTC), "2024-07-01"},
		{time.Date(2024, 7, 2, 4, 0, 0, 0, time.UTC), "2024-07-02"},
		// The hour that happens twice when the clocks go back.
		{time.Date(2024, 11, 3, 5, 30, 0, 0, time.UTC), "2024-11-03"},
		{time.Date(2024, 11, 3, 6, 30, 0, 0, time.UTC), "2024-11-03"},
		// An instant in another zone is converted first.
		{time.Date(2024, 3, 28, 9, 0, 0, 0, time.FixedZone("Tokyo", 9*3600)), "2024-03-27"},
	} {
		if got := DateInNewYork(tt.at); got != tt.want {
			t.Errorf("DateInNewYork(%v) = %q, want %q", tt.at, got, tt.want)
		}
	}
}

func TestTransactionDays(t *testing.T) {
	booked := Transaction{
		Time:      time.Date(2024, 5, 1, 1, 18, 44, 0, time.UTC),
		TradeDate: time.Date(2024, 4, 30, 4, 0, 0, 0, time.UTC),
	}
	if booked.TradeDay() != "2024-04-30" || booked.SettlementDay() != "" {
		t.Errorf("days = %q, %q", booked.TradeDay(), booked.SettlementDay())
	}
	if noDate := (Transaction{Time: booked.Time}); noDate.TradeDay() != "2024-04-30" {
		t.Errorf("without a tradeDate, the trade day is %q", noDate.TradeDay())
	}
	if (Transaction{}).TradeDay() != "" {
		t.Error("a transaction without times has a trade day")
	}
	// An execution at 20:30 in New York has that day's date there, which in
	// UTC is the next; which day Schwab books such a fill on is an open
	// question.
	evening := time.Date(2025, 6, 3, 0, 30, 27, 0, time.UTC)
	if d := (Transaction{Time: evening, TradeDate: evening}).TradeDay(); d != "2025-06-02" {
		t.Errorf("an execution at 20:30 in New York is on %s", d)
	}
	// The trade date counts, not when the row was posted.
	late := Transaction{Time: time.Date(2024, 4, 2, 14, 0, 0, 0, time.UTC), TradeDate: time.Date(2024, 3, 28, 4, 0, 0, 0, time.UTC)}
	if d := late.TradeDay(); d != "2024-03-28" {
		t.Errorf("a row booked on 2024-03-28 and posted later is on %s", d)
	}
}

// The time zone built in for systems without one gives the offsets of New
// York, including in years past the end of its table of changes.
func TestBuiltInNewYorkZone(t *testing.T) {
	loc, err := time.LoadLocationFromTZData("America/New_York", newYorkZone)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		at     time.Time
		offset int
	}{
		{time.Date(2024, 3, 10, 6, 59, 59, 0, time.UTC), -5},
		{time.Date(2024, 3, 10, 7, 0, 0, 0, time.UTC), -4},
		{time.Date(2024, 11, 3, 5, 59, 59, 0, time.UTC), -4},
		{time.Date(2024, 11, 3, 6, 0, 0, 0, time.UTC), -5},
		{time.Date(2051, 3, 12, 7, 0, 0, 0, time.UTC), -4},
		{time.Date(2051, 11, 5, 6, 0, 0, 0, time.UTC), -5},
		{time.Date(1999, 7, 1, 0, 0, 0, 0, time.UTC), -4},
	} {
		if _, off := tt.at.In(loc).Zone(); off != tt.offset*3600 {
			t.Errorf("at %v the built-in zone is %+d hours from UTC, want %+d", tt.at, off/3600, tt.offset)
		}
	}
}

func TestDecodeBodies(t *testing.T) {
	for _, body := range []string{` [ ] `, "[]\n"} {
		got, err := decodeList[Transaction]([]byte(body))
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("decodeList(%q) = %v, %v; want an empty list", body, got, err)
		}
	}
	for body, msg := range map[string]string{
		`{"activityId":1}`: "the answer is an object, not a list",
		`null`:             "the answer is null, not a list",
		``:                 "the answer is nothing, not a list",
		`[{"a":1}`:         "unexpected end of JSON input",
		`[] []`:            "invalid character '[' after top-level value",
		// White space that JSON does not allow around a value.
		"\f[]":       "the answer is something else, not a list",
		"\u00a0[]\n": "the answer is something else, not a list",
		"[]\f":       `invalid character '\f' after top-level value`,
	} {
		if _, err := decodeList[Transaction]([]byte(body)); err == nil || err.Error() != msg {
			t.Errorf("decodeList(%q) = %v, want %q", body, err, msg)
		}
	}

	for _, body := range []string{`{"activityId":5}`, `[{"activityId":5}]`, " \n{\"activityId\":5}\n"} {
		got, err := decodeOne[Transaction]([]byte(body))
		if err != nil || got.ActivityID != 5 {
			t.Errorf("decodeOne(%q) = %+v, %v", body, got, err)
		}
	}
	for body, msg := range map[string]string{
		`[]`:                 "the answer is a list of 0, where one was expected",
		`[{"a":1},{"a":2}]`:  "the answer is a list of 2, where one was expected",
		`null`:               "the answer is null, not an object",
		`5`:                  "the answer is a number, not an object",
		`"a"`:                "the answer is a string, not an object",
		`{"activityId":1} x`: "invalid character 'x' after top-level value",
		// White space that JSON does not allow around a value.
		"\v{}":       "the answer is something else, not an object",
		"\u3000[{}]": "the answer is something else, not an object",
	} {
		if _, err := decodeOne[Transaction]([]byte(body)); err == nil || err.Error() != msg {
			t.Errorf("decodeOne(%q) = %v, want %q", body, err, msg)
		}
	}
}

// A typed record encodes to the keys Schwab sent, without inventing values
// for the ones it did not, such as zero dates.
func TestEncodingKeepsToWhatSchwabSent(t *testing.T) {
	txs, err := decodeList[Transaction](readTestdata(t, "transactions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tx := range txs {
		b, err := json.Marshal(tx)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("0001-01-01")) {
			t.Errorf("transaction %d encodes a zero time: %s", tx.ActivityID, b)
		}
		var back Transaction
		if err := json.Unmarshal(b, &back); err != nil || len(back.Problems) > 0 {
			t.Errorf("transaction %d does not decode again: %v, %q", tx.ActivityID, err, back.Problems)
		}
		back.Raw, tx.Raw = nil, nil
		if !reflect.DeepEqual(back, tx) {
			t.Errorf("transaction %d changed on the way through JSON:\n got %+v\nwant %+v", tx.ActivityID, back, tx)
		}
	}
	accounts, err := decodeList[Account](readTestdata(t, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(accounts[0].SecuritiesAccount.ProjectedBalances)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"cashAvailableForTrading":1532.17,"cashAvailableForWithdrawal":1532.17}`; string(b) != want {
		t.Errorf("projected balances encode as %s, want %s", b, want)
	}

	// Every key of every record's encoding is one that Schwab sent.
	for typ, records := range testdataRecords(t) {
		for i, raw := range records {
			v := reflect.New(typ)
			if err := json.Unmarshal(raw, v.Interface()); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(v.Interface())
			if err != nil {
				t.Fatal(err)
			}
			if extra := extraKeys(decodeAny(t, b), decodeAny(t, raw), ""); len(extra) > 0 {
				t.Errorf("%s %d encodes keys that Schwab did not send: %v", typ.Name(), i, extra)
			}
		}
	}
}

// extraKeys returns the paths of the keys in a that b lacks, where a and b
// are JSON decoded as any; arrays are compared element by element.
func extraKeys(a, b any, path string) []string {
	var extra []string
	switch a := a.(type) {
	case map[string]any:
		bm, _ := b.(map[string]any)
		for _, k := range slices.Sorted(maps.Keys(a)) {
			at := joinPath(path, k)
			if bv, ok := bm[k]; ok {
				extra = append(extra, extraKeys(a[k], bv, at)...)
			} else {
				extra = append(extra, at)
			}
		}
	case []any:
		bs, _ := b.([]any)
		for i, e := range a {
			if i < len(bs) {
				extra = append(extra, extraKeys(e, bs[i], path)...)
			}
		}
	}
	return extra
}

// A list that Schwab sends empty is still there, and empty, after the
// record's encoding, and one that Schwab does not send is still absent.
func TestEmptyListsSurviveEncoding(t *testing.T) {
	for _, tt := range []struct {
		list string // the list sent, or "" for none
		key  string // how the encoding holds it, or "" for not at all
	}{
		{"[]", "[]"},
		{"", ""},
	} {
		account := `{"securitiesAccount":` + withKeys(baseSecuritiesAccount, "positions", tt.list) + "}"
		a, back, enc := roundTrip[Account](t, account)
		if len(a.Problems) > 0 || encodedList(enc, "positions") != tt.key ||
			(a.SecuritiesAccount.Positions == nil) != (tt.list == "") || (back.SecuritiesAccount.Positions == nil) != (tt.list == "") {
			t.Errorf("positions %q: decoded as %#v, %q; encoded as %s; decoded again as %#v",
				tt.list, a.SecuritiesAccount.Positions, a.Problems, enc, back.SecuritiesAccount.Positions)
		}

		in := withKeys(`{"assetType":"OPTION","symbol":"X"}`, "optionDeliverables", tt.list)
		tx, txBack, enc := roundTrip[Transaction](t, withKeys(baseTransaction, "transferItems", "["+withKeys(baseLeg, "instrument", in)+"]"))
		sent, got := tx.TransferItems[0].Instrument.OptionDeliverables, txBack.TransferItems[0].Instrument.OptionDeliverables
		if len(tx.Problems) > 0 || encodedList(enc, "optionDeliverables") != tt.key || (sent == nil) != (tt.list == "") || (got == nil) != (tt.list == "") {
			t.Errorf("optionDeliverables %q: decoded as %#v, %q; encoded as %s; decoded again as %#v", tt.list, sent, tx.Problems, enc, got)
		}
	}
}

// roundTrip decodes body as a T, encodes the record and decodes that, and
// returns both records and the encoding.
func roundTrip[T any](t *testing.T, body string) (sent, back T, enc []byte) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	enc, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(enc, &back); err != nil {
		t.Fatal(err)
	}
	return sent, back, enc
}

// encodedList returns the empty list that enc holds under key, "[]", or ""
// if enc does not hold key at all, or "?" if it holds something else there.
func encodedList(enc []byte, key string) string {
	switch {
	case bytes.Contains(enc, []byte(`"`+key+`":[]`)):
		return "[]"
	case bytes.Contains(enc, []byte(`"`+key+`"`)):
		return "?"
	}
	return ""
}

// baseRecords are a record of each of the five types that Schwab's answers
// hold, without problems, for tests to change.
var baseRecords = []struct {
	typ  reflect.Type
	body string
}{
	{reflect.TypeFor[AccountNumber](), `{"accountNumber":"29470135","hashValue":"H"}`},
	{reflect.TypeFor[Account](), `{"securitiesAccount":` + baseSecuritiesAccount + "}"},
	{reflect.TypeFor[Position](), basePosition},
	{reflect.TypeFor[Transaction](), baseTransaction},
	{reflect.TypeFor[UserPreference](), `{"accounts":[],"streamerInfo":[],"offers":[]}`},
}

// A problems key from Schwab is a key that no field takes, whatever it holds
// and in whatever case: it fills no field and gives no problem, on every Go
// version, and Raw keeps it.
func TestSchwabsOwnProblemsKey(t *testing.T) {
	for _, base := range baseRecords {
		want := reflect.New(base.typ).Elem()
		if err := json.Unmarshal([]byte(base.body), want.Addr().Interface()); err != nil {
			t.Fatal(err)
		}
		want.FieldByName("Raw").SetZero()
		for _, key := range []string{"problems", "Problems", "PROBLEMS"} {
			for _, value := range []string{`5`, `"x"`, `true`, `null`, `{}`, `[]`, `[{}]`, `[{"what":"x","path":7}]`, `[{"what":"x"}]`} {
				body := withKeys(base.body, key, value)
				got := reflect.New(base.typ).Elem()
				if err := json.Unmarshal([]byte(body), got.Addr().Interface()); err != nil {
					t.Fatal(err)
				}
				if ps := problemsField(got); len(ps) > 0 {
					t.Errorf("%s with %q: %s: problems %q", base.typ.Name(), key, value, ps)
				}
				if raw := got.FieldByName("Raw").Bytes(); string(raw) != body {
					t.Errorf("%s with %q: %s: Raw is %s, want %s", base.typ.Name(), key, value, raw, body)
				}
				got.FieldByName("Raw").SetZero()
				if !reflect.DeepEqual(got.Interface(), want.Interface()) {
					t.Errorf("%s with %q: %s decoded as %+v, want %+v", base.typ.Name(), key, value, got, want)
				}
			}
		}
	}
}

// nonJSONSpace is white space that JSON does not allow around a value: a
// vertical tab, a form feed, and Unicode's next line, no-break, line
// separator and ideographic spaces, all of which bytes.TrimSpace trims.
var nonJSONSpace = []string{"\v", "\f", "\u0085", "\u00a0", "\u2028", "\u3000"}

// Only JSON's white space, spaces, tabs and line ends, may be around a
// record, and Raw leaves it out. Other white space makes the bytes no JSON,
// as encoding/json has it, so a record given them directly has a problem:
// before the record, it is not an object; after it, the JSON could not be
// read. An instrument, which a record holds, is null only with JSON's white
// space around.
func TestOnlyJSONWhiteSpaceAround(t *testing.T) {
	for _, base := range baseRecords {
		decode := func(b string) (rec reflect.Value, raw string) {
			v := reflect.New(base.typ)
			if err := v.Interface().(json.Unmarshaler).UnmarshalJSON([]byte(b)); err != nil {
				t.Fatalf("%s from %q: %v", base.typ.Name(), b, err)
			}
			return v.Elem(), string(v.Elem().FieldByName("Raw").Bytes())
		}
		for _, space := range []string{" ", "\t", "\r\n", " \t\r\n"} {
			if rec, raw := decode(space + base.body + space); len(problemsField(rec)) > 0 || raw != base.body {
				t.Errorf("%s in %q: problems %q, Raw %q; want none, and the record alone", base.typ.Name(), space, problemsField(rec), raw)
			}
		}
		for _, space := range nonJSONSpace {
			want := []string{"Schwab sent something else where an object was expected"}
			if rec, raw := decode(space + base.body); !slices.Equal(problemStrings(problemsField(rec)), want) || raw != space+base.body {
				t.Errorf("%s after %q: problems %q, Raw %q; want %q, and the bytes sent", base.typ.Name(), space, problemsField(rec), raw, want)
			}
			rec, raw := decode(base.body + space)
			if ps := problemsField(rec); len(ps) != 1 || ps[0].Path != "" || !strings.HasPrefix(ps[0].What, "the JSON could not be read: ") || raw != base.body+space {
				t.Errorf("%s before %q: problems %q, Raw %q; want that the JSON could not be read, and the bytes sent", base.typ.Name(), space, ps, raw)
			}
		}
	}

	for _, b := range []string{"null", " null", "\tnull\r\n"} {
		var in Instrument
		if err := in.UnmarshalJSON([]byte(b)); err != nil || !reflect.DeepEqual(in, Instrument{}) {
			t.Errorf("an instrument from %q: %+v, %v; want the zero instrument", b, in, err)
		}
	}
	for _, space := range nonJSONSpace {
		var in Instrument
		want := []string{"Schwab sent something else where an object was expected"}
		if err := in.UnmarshalJSON([]byte(space + "null")); err != nil || !slices.Equal(problemStrings(in.problems), want) {
			t.Errorf("an instrument from %q: problems %q, %v; want %q", space+"null", in.problems, err, want)
		}
	}
}

// recordTypes are the types that decode themselves leniently.
var recordTypes = []reflect.Type{
	reflect.TypeFor[AccountNumber](), reflect.TypeFor[Account](), reflect.TypeFor[Position](),
	reflect.TypeFor[Instrument](), reflect.TypeFor[Transaction](), reflect.TypeFor[UserPreference](),
}

// reachable adds t, if it is a struct, and the struct types that it holds,
// through fields, slices and pointers, to seen.
func reachable(t reflect.Type, seen map[reflect.Type]bool) {
	t = derefType(t)
	switch {
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		reachable(t.Elem(), seen)
	case t.Kind() == reflect.Struct && t != timeType && !seen[t]:
		seen[t] = true
		for f := range t.Fields() {
			if f.IsExported() {
				reachable(f.Type, seen)
			}
		}
	}
}

// holdsTime reports whether t is a time, or a pointer to or slice of them.
func holdsTime(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	return t == timeType
}

// Every time field is read by hand. On Go 1.26, a time field that a
// record's UnmarshalJSON does not read itself stops the decode at its key,
// on Schwab's offsets, and loses the rest of the object; so each time sits
// in a type with an UnmarshalJSON of its own, and each, filled as Schwab
// writes times, must come back, and so must a key after it.
func TestEveryTimeFieldIsReadByHand(t *testing.T) {
	types := map[reflect.Type]bool{}
	for _, typ := range recordTypes {
		reachable(typ, types)
	}
	for typ := range types {
		for f := range typ.Fields() {
			if holdsTime(f.Type) && !reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) {
				t.Errorf("%s.%s is a time in a type that does not decode itself, so nothing reads it by hand", typ.Name(), f.Name)
			}
			// The records read a time.Time by hand; time.Time itself would
			// decode a pointer to one, or a list of them.
			if holdsTime(f.Type) && f.Type != timeType {
				t.Errorf("%s.%s holds times by pointer or in a list, which no record reads by hand", typ.Name(), f.Name)
			}
		}
	}

	for _, typ := range recordTypes {
		fields := jsonFields(typ)
		obj := map[string]any{}
		var times []string
		for name, f := range fields {
			if f.t == timeType {
				obj[name] = "2024-04-04T15:56:07+0000"
				times = append(times, name)
			}
		}
		// A string field after the times, in the order JSON objects keep.
		b, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		trailer := ""
		for name, f := range fields {
			if f.t.Kind() == reflect.String && name != "status" {
				trailer = name
				break
			}
		}
		if trailer != "" {
			b = append(b[:len(b)-1], []byte(`,"`+trailer+`":"kept"}`)...)
			if len(obj) == 0 {
				b = []byte(`{"` + trailer + `":"kept"}`)
			}
		}
		v := reflect.New(typ)
		if err := json.Unmarshal(b, v.Interface()); err != nil {
			t.Fatalf("%s: %v", typ.Name(), err)
		}
		at := time.Date(2024, 4, 4, 15, 56, 7, 0, time.UTC)
		for _, name := range times {
			got := v.Elem().FieldByIndex(fieldIndex(typ, name)).Interface().(time.Time)
			if got != at {
				t.Errorf("%s.%s decoded as %v, want %v in UTC", typ.Name(), name, got, at)
			}
		}
		if trailer != "" {
			if got := v.Elem().FieldByIndex(fieldIndex(typ, trailer)).Interface(); fmt.Sprint(got) != "kept" {
				t.Errorf("%s.%s after the times decoded as %v", typ.Name(), trailer, got)
			}
		}
		for _, p := range problemsField(v.Elem()) {
			if strings.Contains(p.What, "time") {
				t.Errorf("%s: %v", typ.Name(), p)
			}
		}
	}
}

// fieldIndex returns the index of the field of struct type typ with JSON
// key name.
func fieldIndex(typ reflect.Type, name string) []int {
	for f := range typ.Fields() {
		if jsonName(f) == name {
			return f.Index
		}
	}
	panic("no field " + name)
}

// problemsField returns the problems of v, a record, whether exported or not.
func problemsField(v reflect.Value) []Problem {
	if f := v.FieldByName("Problems"); f.IsValid() {
		return f.Interface().([]Problem)
	}
	return v.Addr().Interface().(*Instrument).problems
}

// Every value of the wrong kind is named, at the path of its tag even when
// Schwab writes the key in another case, and the same on every Go version;
// a pointer such a value would have left at zero is nil, as for absent.
func TestEveryMismatchIsNamed(t *testing.T) {
	// The first type is of the wrong kind and the second, sent in another
	// case, is not.
	body := `{"TYPE":5,` + withKeys(baseTransaction, "subAccount", "", "SUBACCOUNT", "7", "qualifiedDividend", `"true"`,
		"transferItems", `[{"amount":"1","cost":true,"Price":{},"instrument":{"assetType":"EQUITY","symbol":"X","strikePrice":"9"}}]`)[1:]
	var tx Transaction
	if err := json.Unmarshal([]byte(body), &tx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"type: Schwab sent a number where a string was expected",
		"subAccount: Schwab sent a number where a string was expected",
		"qualifiedDividend: Schwab sent a string where a boolean was expected",
		"transferItems.amount: Schwab sent a string where a number was expected",
		"transferItems.cost: Schwab sent a boolean where a number was expected",
		"transferItems.price: Schwab sent an object where a number was expected",
		"transferItems.instrument.strikePrice: Schwab sent a string where a number was expected",
	}
	if got := problemStrings(tx.Problems); !slices.Equal(got, want) {
		t.Errorf("problems:\n got %q\nwant %q", got, want)
	}
	if tx.Type != TransactionTrade {
		t.Errorf("type = %q, want the one that could be read", tx.Type)
	}
	if tx.QualifiedDividend != nil {
		t.Errorf("qualifiedDividend = %v, want nil since it could not be read", *tx.QualifiedDividend)
	}

	var a Account
	if err := json.Unmarshal([]byte(`{"securitiesAccount":`+withKeys(baseSecuritiesAccount, "isPortfolioMargin", "1",
		"currentBalances", "", "CurrentBalances", `{"IsInCall":0,"cashBalance":"x","liquidationValue":9}`)+
		`,"aggregatedBalance":{"liquidationValue":[1]}}`), &a); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"securitiesAccount.isPortfolioMargin: Schwab sent a number where a boolean was expected",
		"securitiesAccount.currentBalances.cashBalance: Schwab sent a string where a number was expected",
		"securitiesAccount.currentBalances.isInCall: Schwab sent a number where a boolean was expected",
		"aggregatedBalance.liquidationValue: Schwab sent an array where a number was expected",
	}
	if got := problemStrings(a.Problems); !slices.Equal(got, want) {
		t.Errorf("problems:\n got %q\nwant %q", got, want)
	}
	cb := a.SecuritiesAccount.CurrentBalances
	if a.SecuritiesAccount.IsPortfolioMargin != nil || cb.CashBalance != nil || cb.IsInCall != nil || a.AggregatedBalance.LiquidationValue != nil {
		t.Errorf("pointers that Schwab's values could not fill are not nil: %+v", a)
	}
	if v := cb.LiquidationValue; v == nil || *v != 9 {
		t.Errorf("liquidationValue = %v, want 9", v)
	}

	// Objects, and objects in a list, of the wrong kind.
	a = Account{}
	if err := json.Unmarshal([]byte(`{"securitiesAccount":`+withKeys(baseSecuritiesAccount, "currentBalances", `"x"`)+
		`,"aggregatedBalance":5}`), &a); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"securitiesAccount.currentBalances: Schwab sent a string where an object was expected",
		"aggregatedBalance: Schwab sent a number where an object was expected",
	}
	if got := problemStrings(a.Problems); !slices.Equal(got, want) {
		t.Errorf("problems:\n got %q\nwant %q", got, want)
	}
	tx = Transaction{}
	if err := json.Unmarshal([]byte(withKeys(baseTransaction, "transferItems", "["+baseLeg+",5]")), &tx); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(tx.Problems), []string{"transferItems: Schwab sent a number where an object was expected"}; !slices.Equal(got, want) {
		t.Errorf("problems:\n got %q\nwant %q", got, want)
	}
}

// An instrument that is not an object is named once, as itself, in a
// position and in a leg; the same problem in two legs is named once, and two
// problems at one path are both named.
func TestProblemsNamedOnce(t *testing.T) {
	var p Position
	if err := json.Unmarshal([]byte(withKeys(basePosition, "instrument", `"X"`)), &p); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(p.Problems), []string{"instrument: Schwab sent a string where an object was expected"}; !slices.Equal(got, want) {
		t.Errorf("position's problems:\n got %q\nwant %q", got, want)
	}
	var tx Transaction
	if err := json.Unmarshal([]byte(withKeys(baseTransaction, "transferItems", "["+withKeys(baseLeg, "instrument", "7")+"]")), &tx); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(tx.Problems), []string{"transferItems.instrument: Schwab sent a number where an object was expected"}; !slices.Equal(got, want) {
		t.Errorf("transaction's problems:\n got %q\nwant %q", got, want)
	}
	legs := "[" + withKeys(baseLeg, "cost", `"x"`) + "," + withKeys(baseLeg, "cost", `"y"`) + "," +
		withKeys(baseLeg, "instrument", `{"assetType":"EQUITY","symbol":"X","closingPrice":"x"}`) + "," +
		withKeys(baseLeg, "instrument", `{"assetType":"EQUITY","symbol":"X","closingPrice":true}`) + "]"
	tx = Transaction{}
	if err := json.Unmarshal([]byte(withKeys(baseTransaction, "transferItems", legs)), &tx); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(tx.Problems), []string{
		"transferItems.cost: Schwab sent a string where a number was expected",
		"transferItems.instrument.closingPrice: Schwab sent a string where a number was expected",
		"transferItems.instrument.closingPrice: Schwab sent a boolean where a number was expected",
	}; !slices.Equal(got, want) {
		t.Errorf("transaction's problems:\n got %q\nwant %q", got, want)
	}
}

// null in a list of objects is not taken for an object without keys.
func TestNullElements(t *testing.T) {
	var tx Transaction
	in := `{"assetType":"OPTION","symbol":"X","optionDeliverables":[null]}`
	if err := json.Unmarshal([]byte(withKeys(baseTransaction, "transferItems",
		"["+baseLeg+",null,"+withKeys(baseLeg, "instrument", in, "cost", `"x"`)+"]")), &tx); err != nil {
		t.Fatal(err)
	}
	// The walk goes on past the null, to the leg after it.
	if got, want := problemStrings(tx.Problems), []string{
		"transferItems: Schwab sent null where an object was expected",
		"transferItems.cost: Schwab sent a string where a number was expected",
		"transferItems.instrument.optionDeliverables: Schwab sent null where an object was expected",
	}; !slices.Equal(got, want) {
		t.Errorf("transaction's problems:\n got %q\nwant %q", got, want)
	}
	// A null position is a problem of the position, not of its account.
	var a Account
	if err := json.Unmarshal([]byte(`{"securitiesAccount":`+withKeys(baseSecuritiesAccount, "positions", "["+basePosition+",null]")+"}"), &a); err != nil {
		t.Fatal(err)
	}
	if ps := a.SecuritiesAccount.Positions; len(a.Problems) != 0 || len(ps) != 2 ||
		!slices.Equal(problemStrings(ps[1].Problems), []string{"Schwab sent null where an object was expected"}) {
		t.Errorf("a null position: account's problems %q, positions %+v", a.Problems, ps)
	}
	var u UserPreference
	if err := json.Unmarshal([]byte(`{"accounts":[null],"streamerInfo":[null],"offers":[null]}`), &u); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(u.Problems), []string{
		"accounts: Schwab sent null where an object was expected",
		"streamerInfo: Schwab sent null where an object was expected",
		"offers: Schwab sent null where an object was expected",
	}; !slices.Equal(got, want) {
		t.Errorf("preferences' problems:\n got %q\nwant %q", got, want)
	}
}

func TestOffsetsOutOfRange(t *testing.T) {
	for _, in := range []string{
		"2024-04-04T15:56:07+2400", "2024-04-04T15:56:07-2460", "2024-04-04T15:56:07+0060",
		"2024-04-04T15:56:07+24:00", "2024-04-04T15:56:07+00:60",
	} {
		var ps []Problem
		if got, _ := readTime(json.RawMessage(`"`+in+`"`), "time", &ps); !got.IsZero() || len(ps) != 1 {
			t.Errorf("readTime(%q) = %v, %v; want the zero time and a problem", in, got, ps)
		}
	}
	var ps []Problem
	if got, _ := readTime(json.RawMessage(`"2024-04-04T15:56:07+2359"`), "time", &ps); got.IsZero() || ps != nil {
		t.Errorf("an offset of 23:59 was refused: %v", ps)
	}
}

// A list of one that holds something other than an object is not a record.
func TestDecodeOneRefusesAListOfNonObjects(t *testing.T) {
	for _, body := range []string{`[null]`, `[5]`, `[[{"activityId":1}]]`, `["x"]`} {
		if got, err := decodeOne[Transaction]([]byte(body)); err == nil {
			t.Errorf("decodeOne(%s) = %+v, want an error", body, got)
		}
	}
}

// A number, string or boolean sent twice takes its last value, but an
// object or array sent twice is a problem: what its fields hold then
// depends on how encoding/json combines the copies.
func TestDuplicateKeys(t *testing.T) {
	sentTwice := func(path string) string {
		return path + ": Schwab sent it more than once, so what it holds is uncertain"
	}
	var a Account
	sa := `{"type":"CASH","currentBalances":{"cashBalance":100,"liquidationValue":500},` + baseSecuritiesAccount[1:]
	if err := json.Unmarshal([]byte(`{"securitiesAccount":`+sa+`}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.SecuritiesAccount.Type != "MARGIN" {
		t.Errorf("type = %q, want the last one sent", a.SecuritiesAccount.Type)
	}
	if got, want := problemStrings(a.Problems), []string{sentTwice("securitiesAccount.currentBalances")}; !slices.Equal(got, want) {
		t.Errorf("account's problems:\n got %q\nwant %q", got, want)
	}

	// The last copy of a number counts even when it cannot be read, or is
	// null.
	var p Position
	if err := json.Unmarshal([]byte(basePosition[:len(basePosition)-1]+`,"averagePrice":"x"}`), &p); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(p.Problems), []string{"averagePrice: Schwab sent a string where a number was expected"}; !slices.Equal(got, want) || p.AveragePrice != 0 {
		t.Errorf("averagePrice = %v, with problems %q; want 0 and %q", p.AveragePrice, got, want)
	}
	if err := json.Unmarshal([]byte(basePosition[:len(basePosition)-1]+`,"averagePrice":null}`), &p); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(p.Problems), missing("averagePrice"); !slices.Equal(got, want) || p.AveragePrice != 0 {
		t.Errorf("averagePrice = %v, with problems %q; want 0 and %q", p.AveragePrice, got, want)
	}

	// An instrument decodes itself, so its second copy replaces the first.
	if err := json.Unmarshal([]byte(`{"instrument":{"assetType":"OPTION","symbol":"Y","cusip":"1"},`+basePosition[1:]), &p); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(p.Problems), []string{sentTwice("instrument")}; !slices.Equal(got, want) {
		t.Errorf("position's problems:\n got %q\nwant %q", got, want)
	}

	// An array sent twice, and an instrument sent twice in a leg.
	var tx Transaction
	leg := `{"instrument":{"assetType":"EQUITY","symbol":"Y"},` + baseLeg[1:]
	if err := json.Unmarshal([]byte(`{"transferItems":[`+baseLeg+","+baseLeg+"],"+
		withKeys(baseTransaction, "transferItems", "["+leg+"]")[1:]), &tx); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(tx.Problems), []string{
		sentTwice("transferItems"), sentTwice("transferItems.instrument"),
	}; !slices.Equal(got, want) {
		t.Errorf("transaction's problems:\n got %q\nwant %q", got, want)
	}

	// An earlier copy of the wrong kind is named, in any place in its
	// object and through a pointer, and an earlier null is no problem.
	a = Account{}
	if err := json.Unmarshal([]byte(`{"securitiesAccount":`+withKeys(baseSecuritiesAccount,
		"currentBalances", `{"cashBalance":1,"liquidationValue":"x","liquidationValue":5}`)+"}"), &a); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(a.Problems), []string{
		"securitiesAccount.currentBalances.liquidationValue: Schwab sent a string where a number was expected",
	}; !slices.Equal(got, want) {
		t.Errorf("account's problems:\n got %q\nwant %q", got, want)
	}
	if v := a.SecuritiesAccount.CurrentBalances.LiquidationValue; v == nil || *v != 5 {
		t.Errorf("liquidationValue = %v, want the last copy, 5", v)
	}
	tx = Transaction{}
	if err := json.Unmarshal([]byte(`{"subAccount":null,`+baseTransaction[1:]), &tx); err != nil {
		t.Fatal(err)
	}
	if len(tx.Problems) != 0 || tx.SubAccount != "CASH" {
		t.Errorf("an earlier null: subAccount %q, problems %q", tx.SubAccount, tx.Problems)
	}

	// Each problem is the same on every Go version, with none of
	// encoding/json's own text, and the keys that are missing are named too:
	// a bad copy then null, in either case; an object or array whose earlier
	// copy holds a bad value, or is not an object at all; and one whose last
	// copy is null, which leaves it missing rather than uncertain.
	inside := func(obj string) string { return obj[1 : len(obj)-1] } // an object's members, without its braces
	cases := []struct {
		name string
		rec  any
		body string
		want []string
	}{
		{
			"a number, then null", &Transaction{},
			`{"subAccount":5,` + inside(withKeys(baseTransaction, "subAccount", "", "activityId", "0")) + `,"subAccount":null}`,
			[]string{
				"subAccount: Schwab sent a number where a string was expected", missing("subAccount")[0], missing("activityId")[0],
			},
		},
		{
			"a number in another case, then null", &Transaction{},
			`{"SUBACCOUNT":5,` + inside(withKeys(baseTransaction, "subAccount", "", "status", `""`)) + `,"subAccount":null}`,
			[]string{
				"subAccount: Schwab sent a number where a string was expected", missing("subAccount")[0], missing("status")[0],
			},
		},
		{
			"a list with a bad leg, then a good one", &Transaction{},
			`{"transferItems":[` + withKeys(baseLeg, "amount", `"x"`) + `],` + withKeys(baseTransaction, "type", `""`,
				"transferItems", "["+baseLeg+"]")[1:],
			[]string{sentTwice("transferItems"), missing("type")[0]},
		},
		{
			"a list with a bad leg, then null", &Transaction{},
			`{"transferItems":[` + withKeys(baseLeg, "cost", `"x"`) + `],` + withKeys(baseTransaction, "transferItems", "null")[1:],
			missing("transferItems"),
		},
		{
			"a number where a list belongs, then null", &Transaction{},
			`{"transferItems":5,` + withKeys(baseTransaction, "transferItems", "null", "type", `""`)[1:],
			[]string{
				"transferItems: Schwab sent a number where an array was expected", missing("transferItems")[0], missing("type")[0],
			},
		},
		{
			"a number where an object belongs, then null, for an object Schwab sends only sometimes", &Account{},
			`{"securitiesAccount":` + baseSecuritiesAccount + `,"aggregatedBalance":5,"aggregatedBalance":null}`,
			[]string{"aggregatedBalance: Schwab sent a number where an object was expected"},
		},
		{
			"a number where a list belongs, then a list", &Transaction{},
			`{"transferItems":5,` + withKeys(baseTransaction, "transferItems", "["+baseLeg+"]")[1:],
			[]string{"transferItems: Schwab sent a number where an array was expected", sentTwice("transferItems")},
		},
		{
			// An instrument decodes itself, but only the walk of its position
			// sees an earlier copy.
			"a number where an instrument belongs, then null", &Position{},
			`{"instrument":5,"instrument":null,` + withKeys(basePosition, "instrument", "")[1:],
			[]string{"instrument: Schwab sent a number where an object was expected", missing("instrument")[0]},
		},
		{
			"a number where an instrument belongs, then an instrument", &Position{},
			`{"instrument":5,` + basePosition[1:],
			[]string{"instrument: Schwab sent a number where an object was expected", sentTwice("instrument")},
		},
		{
			"a number, then a boolean, then a string", &Transaction{},
			`{"subAccount":5,"subAccount":true,` + baseTransaction[1:],
			[]string{
				"subAccount: Schwab sent a number where a string was expected", "subAccount: Schwab sent a boolean where a string was expected",
			},
		},
		{
			"two balances, then null", &Account{},
			`{"securitiesAccount":{"currentBalances":{"liquidationValue":7},"currentBalances":{"cashBalance":1},` +
				withKeys(baseSecuritiesAccount, "currentBalances", "null")[1:] + "}",
			missing("securitiesAccount.currentBalances"),
		},
		{
			"balances with a bad value, then good ones", &Account{},
			`{"securitiesAccount":{"currentBalances":{"cashBalance":"x"},` + withKeys(baseSecuritiesAccount, "accountNumber", `""`)[1:] + "}",
			[]string{sentTwice("securitiesAccount.currentBalances"), missing("securitiesAccount.accountNumber")[0]},
		},
		{
			"balances with a bad value, then null", &Account{},
			`{"securitiesAccount":{"currentBalances":{"cashBalance":"x"},` + withKeys(baseSecuritiesAccount, "currentBalances", "null")[1:] + "}",
			missing("securitiesAccount.currentBalances"),
		},
	}
	for _, c := range cases {
		if err := json.Unmarshal([]byte(c.body), c.rec); err != nil {
			t.Fatal(err)
		}
		if got := problemStrings(problemsField(reflect.ValueOf(c.rec).Elem())); !slices.Equal(got, c.want) {
			t.Errorf("%s: problems\n got %q\nwant %q", c.name, got, c.want)
		}
	}

	// The last copy counts for an object too: one of the wrong kind leaves
	// it zero, and a null before the only object is no second copy.
	a = Account{}
	if err := json.Unmarshal([]byte(`{"securitiesAccount":{"currentBalances":{"liquidationValue":7},`+
		withKeys(baseSecuritiesAccount, "currentBalances", "5")[1:]+"}"), &a); err != nil {
		t.Fatal(err)
	}
	if v := a.SecuritiesAccount.CurrentBalances.LiquidationValue; v != nil {
		t.Errorf("liquidationValue = %v, though the last copy of its balances was a number", *v)
	}
	a = Account{}
	if err := json.Unmarshal([]byte(`{"securitiesAccount":{"currentBalances":{"liquidationValue":7},`+
		withKeys(baseSecuritiesAccount, "currentBalances", "null")[1:]+`,"aggregatedBalance":{"liquidationValue":1},"aggregatedBalance":null}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.SecuritiesAccount.CurrentBalances.LiquidationValue != nil || a.AggregatedBalance.LiquidationValue != nil {
		t.Errorf("balances = %+v, %+v, though the last copy of each was null", a.SecuritiesAccount.CurrentBalances, a.AggregatedBalance)
	}
	if got, want := problemStrings(a.Problems), missing("securitiesAccount.currentBalances"); !slices.Equal(got, want) {
		t.Errorf("account's problems:\n got %q\nwant %q", got, want)
	}
	tx = Transaction{}
	if err := json.Unmarshal([]byte(`{"transferItems":[`+baseLeg+`],`+withKeys(baseTransaction, "transferItems", "{}")[1:]), &tx); err != nil {
		t.Fatal(err)
	}
	if tx.TransferItems != nil {
		t.Errorf("legs = %+v, though the last copy of transferItems was an object", tx.TransferItems)
	}
	tx = Transaction{}
	in := `{"assetType":"OPTION","symbol":"X","optionDeliverables":[{"deliverableUnits":100,` +
		`"deliverable":null,"deliverable":{"assetType":"EQUITY","symbol":"A"}}]}`
	if err := json.Unmarshal([]byte(`{"transferItems":null,`+withKeys(baseTransaction, "transferItems",
		"["+withKeys(baseLeg, "instrument", in)+"]")[1:]), &tx); err != nil {
		t.Fatal(err)
	}
	if len(tx.Problems) != 0 {
		t.Errorf("a null before an object or list: problems %q", tx.Problems)
	}

	// A deliverable, a pointer to an object, sent twice is a problem.
	tx = Transaction{}
	in = `{"assetType":"OPTION","symbol":"X","optionDeliverables":[{"deliverableUnits":100,` +
		`"deliverable":{"assetType":"EQUITY","symbol":"A"},"deliverable":{"assetType":"EQUITY","symbol":"B"}}]}`
	if err := json.Unmarshal([]byte(withKeys(baseTransaction, "transferItems", "["+withKeys(baseLeg, "instrument", in)+"]")), &tx); err != nil {
		t.Fatal(err)
	}
	if got, want := problemStrings(tx.Problems), []string{sentTwice("transferItems.instrument.optionDeliverables.deliverable")}; !slices.Equal(got, want) {
		t.Errorf("transaction's problems:\n got %q\nwant %q", got, want)
	}
}

// Times come back in UTC, whatever the offset they were written with. On a
// machine whose own zone is UTC, as in CI, time.Parse gives "+0000"
// time.Local rather than time.UTC, and elsewhere a zone without a name, so
// a time that was not turned into UTC fails here wherever the test runs.
func TestTimesAreUTC(t *testing.T) {
	for _, in := range []string{"2024-01-04T15:56:07+0000", "2024-01-04T15:56:07Z", "2024-01-04T10:56:07-0500", "2024-01-04T15:56:07.000+00:00"} {
		var ps []Problem
		got, _ := readTime(json.RawMessage(`"`+in+`"`), "time", &ps)
		if want := time.Date(2024, 1, 4, 15, 56, 7, 0, time.UTC); got != want || got.Location() != time.UTC {
			t.Errorf("%q decoded as %v (%v), want %v", in, got, got.Location(), want)
		}
	}
}

// The built-in zone agrees with the system's, where there is one, hour by
// hour. When it stops agreeing, the tz database has changed New York's
// rules, and newyork.tzif needs to be taken again from Go's
// lib/time/zoneinfo.zip.
func TestBuiltInNewYorkZoneMatchesTheSystems(t *testing.T) {
	sys, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no system time zone database:", err)
	}
	built, err := time.LoadLocationFromTZData("America/New_York", newYorkZone)
	if err != nil {
		t.Fatal(err)
	}
	for at := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC); at.Year() < 2100; at = at.Add(time.Hour) {
		_, a := at.In(built).Zone()
		_, b := at.In(sys).Zone()
		if a != b {
			t.Fatalf("at %v the built-in zone is %+ds from UTC and the system's %+ds", at, a, b)
		}
	}
}
