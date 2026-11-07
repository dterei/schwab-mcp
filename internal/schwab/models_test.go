package schwab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// readTestdata returns the contents of testdata/name.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
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
		t.Errorf("futures fill: %d named and %d unnamed fee legs, fees %v, legs' cost %v against %v", named, unnamed, futuresFees, cost, futures.NetAmount)
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

// census is what a walk of the testdata found: for each struct type, the
// keys of each of its records, and the keys that no field takes.
type census struct {
	records map[reflect.Type][]map[string]bool
	unknown []string
}

func (c *census) walk(t reflect.Type, v any) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == reflect.TypeFor[time.Time]():
	case t.Kind() == reflect.Slice:
		for _, e := range v.([]any) {
			c.walk(t.Elem(), e)
		}
	case t.Kind() == reflect.Struct:
		fields := jsonFields(t)
		keys := map[string]bool{}
		for k, x := range v.(map[string]any) {
			keys[k] = true
			f, ok := fields[k]
			if !ok {
				c.unknown = append(c.unknown, t.Name()+"."+k)
				continue
			}
			c.walk(f.t, x)
		}
		c.records[t] = append(c.records[t], keys)
	}
}

func testdataCensus(t *testing.T) *census {
	t.Helper()
	c := &census{records: map[reflect.Type][]map[string]bool{}}
	for file, typ := range map[string]reflect.Type{
		"account_numbers.json": reflect.TypeFor[[]AccountNumber](),
		"accounts.json":        reflect.TypeFor[[]Account](),
		"transactions.json":    reflect.TypeFor[[]Transaction](),
		"transaction.json":     reflect.TypeFor[Transaction](),
		"user_preference.json": reflect.TypeFor[UserPreference](),
	} {
		var v any
		if err := json.Unmarshal(readTestdata(t, file), &v); err != nil {
			t.Fatal(err)
		}
		c.walk(typ, v)
	}
	return c
}

// The structs take every key in the testdata, every key seen live is both a
// field and in the testdata, and a field whose tag does not mark it as
// optional is in every record of its kind, as in Schwab's answers.
func TestModelsCoverEveryKey(t *testing.T) {
	c := testdataCensus(t)
	if len(c.unknown) > 0 {
		slices.Sort(c.unknown)
		t.Errorf("testdata keys that no field takes: %v", slices.Compact(c.unknown))
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

// No account number is part of another value in the testdata, so that a
// test that looks for leaked account numbers can search for them as text.
func TestTestdataAccountNumbersStandAlone(t *testing.T) {
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
			if key == "accountNumber" {
				numbers = append(numbers, v)
			} else {
				values = append(values, v)
			}
		default:
			b, _ := json.Marshal(v)
			values = append(values, string(b))
		}
	}
	for _, f := range []string{"account_numbers.json", "accounts.json", "transactions.json", "transaction.json", "user_preference.json"} {
		var v any
		if err := json.Unmarshal(readTestdata(t, f), &v); err != nil {
			t.Fatal(err)
		}
		walk("", v)
	}
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	for _, n := range numbers {
		for _, v := range values {
			if strings.Contains(v, n) {
				t.Errorf("account number %s is in the value %q", n, v)
			}
		}
		for _, m := range numbers {
			if m != n && strings.Contains(m, n) {
				t.Errorf("account number %s is in account number %s", n, m)
			}
		}
	}
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
			json: `{"activityId":1,"time":"2024-04-04T15:56:07+0000","type":"TRADE","status":"VALID","netAmount":"12.5","description":"kept"}`,
			check: func(t *testing.T, tx Transaction) {
				want(t, tx.Description == "kept" && tx.NetAmount == 0, "fields: %+v", tx)
			},
			problems: []string{"netAmount: Schwab sent a string where a number was expected"},
		},
		{
			name:     "an id written as a fraction",
			json:     `{"activityId":1.0,"time":"2024-04-04T15:56:07+0000","type":"TRADE","status":"VALID","netAmount":1}`,
			problems: []string{"activityId: Schwab sent the number 1.0 where a whole number was expected"},
		},
		{
			name: "every key that Schwab always sends missing",
			json: `{"accountNumber":"1"}`,
			problems: []string{
				"activityId: missing, though Schwab sends it on every record of its kind",
				"time: missing, though Schwab sends it on every record of its kind",
				"type: missing, though Schwab sends it on every record of its kind",
				"status: missing, though Schwab sends it on every record of its kind",
				"netAmount: missing, though Schwab sends it on every record of its kind",
			},
		},
		{
			name: "null where values belong",
			json: `{"activityId":null,"time":null,"type":null,"status":"VALID","netAmount":null,"transferItems":null,"settlementDate":null}`,
			problems: []string{
				"activityId: missing, though Schwab sends it on every record of its kind",
				"time: missing, though Schwab sends it on every record of its kind",
				"type: missing, though Schwab sends it on every record of its kind",
				"netAmount: missing, though Schwab sends it on every record of its kind",
			},
		},
		{
			name:     "a time that is not one",
			json:     `{"activityId":1,"time":"2024-13-45","type":"TRADE","status":"VALID","netAmount":1}`,
			problems: []string{`time: Schwab sent "2024-13-45", which is not a time`},
		},
		{
			name:     "a time sent as a number",
			json:     `{"activityId":1,"time":1712246167,"type":"TRADE","status":"VALID","netAmount":1}`,
			problems: []string{"time: Schwab sent a number where a time was expected"},
		},
		{
			name: "a strange leg",
			json: `{"activityId":1,"time":"2024-04-04T15:56:07+0000","type":"TRADE","status":"VALID","netAmount":1,` +
				`"transferItems":[{"instrument":{"assetType":"OPTION","symbol":"X","expirationDate":"soon"},"amount":2},` +
				`{"instrument":{"assetType":"CURRENCY","symbol":"CURRENCY_USD","closingPrice":"x"},"amount":3}]}`,
			check: func(t *testing.T, tx Transaction) {
				want(t, len(tx.TransferItems) == 2 && tx.TransferItems[1].Amount == 3, "legs: %+v", tx.TransferItems)
			},
			problems: []string{
				`transferItems.instrument.expirationDate: Schwab sent "soon", which is not a time`,
				"transferItems.instrument.closingPrice: Schwab sent a string where a number was expected",
			},
		},
		{
			name:     "legs sent as an object",
			json:     `{"activityId":1,"time":"2024-04-04T15:56:07+0000","type":"TRADE","status":"VALID","netAmount":1,"transferItems":{}}`,
			problems: []string{"transferItems: Schwab sent an object where an array was expected"},
		},
		{
			name:     "a number too large",
			json:     `{"activityId":1,"time":"2024-04-04T15:56:07+0000","type":"TRADE","status":"VALID","netAmount":1e400}`,
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
			json: `{"ActivityId":7,"Time":"2024-04-04T15:56:07Z","type":"TRADE","status":"VALID","netAmount":0,"futureField":{"a":[1]}}`,
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
	body := `[{"activityId":1,"netAmount":"x"},null,7,{"activityId":2,"time":"2024-04-04T15:56:07+0000","type":"TRADE","status":"VALID","netAmount":3}]`
	got, err := decodeList[Transaction]([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3].ActivityID != 2 || got[3].NetAmount != 3 || len(got[3].Problems) != 0 {
		t.Fatalf("decoded %+v", got)
	}
	if got[1].Problems == nil || got[2].Problems == nil || string(got[1].Raw) != "null" {
		t.Errorf("the strange records: %+v, %+v", got[1], got[2])
	}
}

func TestDecodeAccountLeniently(t *testing.T) {
	body := `{"securitiesAccount":{"type":"MARGIN","accountNumber":"61803398",` +
		`"currentBalances":{"isInCall":0,"cashBalance":12.5},` +
		`"positions":[{"longQuantity":1,"shortQuantity":0,"instrument":{"assetType":"EQUITY","symbol":"X"}},` +
		`{"longQuantity":1,"shortQuantity":0,"marketValue":5,"instrument":null}]}}`
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
	if got, want := problemStrings(positions[0].Problems), []string{
		"marketValue: missing, though Schwab sends it on every record of its kind",
	}; !slices.Equal(got, want) {
		t.Errorf("first position's problems:\n got %q\nwant %q", got, want)
	}
	if got, want := problemStrings(positions[1].Problems), []string{
		"instrument: Schwab sent null where an object was expected",
	}; !slices.Equal(got, want) {
		t.Errorf("second position's problems:\n got %q\nwant %q", got, want)
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
	} {
		ps = nil
		if got, present := readTime(json.RawMessage(in), "time", &ps); !got.IsZero() || !present || !slices.Equal(problemStrings(ps), []string{problem}) {
			t.Errorf("readTime(%s) = %v, %v, %q; want the zero time and %q", in, got, present, ps, problem)
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
		// An evening execution belongs to its day in New York, not to the next
		// day in UTC, in winter and in summer.
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

func TestJSONPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "",
		"plain":                               "",
		"plain.netAmount":                     "netAmount",
		"netAmount":                           "netAmount",
		"plain.securitiesAccount.items.price": "securitiesAccount.items.price",
		"securitiesAccount.items.1.price":     "securitiesAccount.items.price",
		"transferItems.12.instrument.symbol":  "transferItems.instrument.symbol",
	} {
		if got := jsonPath(in); got != want {
			t.Errorf("jsonPath(%q) = %q, want %q", in, got, want)
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
}

// recordTypes are the types that decode themselves leniently.
var recordTypes = []reflect.Type{
	reflect.TypeFor[AccountNumber](), reflect.TypeFor[Account](), reflect.TypeFor[Position](),
	reflect.TypeFor[Instrument](), reflect.TypeFor[Transaction](), reflect.TypeFor[UserPreference](),
}

// Every time field is read by hand. On Go 1.26, a time field that a
// record's UnmarshalJSON does not read itself stops the decode at its key,
// on Schwab's offsets, and loses the rest of the record; so each time,
// filled as Schwab writes times, must come back, and so must a key after it.
func TestEveryTimeFieldIsReadByHand(t *testing.T) {
	for _, typ := range recordTypes {
		fields := jsonFields(typ)
		obj := map[string]any{}
		var times []string
		for name, f := range fields {
			if f.t == reflect.TypeFor[time.Time]() {
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
		if ps := problemsField(v.Elem()); len(ps) > 0 {
			for _, p := range ps {
				if strings.Contains(p.What, "time") {
					t.Errorf("%s: %v", typ.Name(), p)
				}
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
	body := `{"activityId":5,"time":"2024-04-04T15:56:07+0000","type":"TRADE","status":"VALID","netAmount":1,` +
		`"SUBACCOUNT":7,"qualifiedDividend":"true",` +
		`"transferItems":[{"amount":"1","cost":true,"Price":{},"instrument":{"assetType":"EQUITY","symbol":"X","strikePrice":"9"}}]}`
	var tx Transaction
	if err := json.Unmarshal([]byte(body), &tx); err != nil {
		t.Fatal(err)
	}
	want := []string{
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
	if tx.QualifiedDividend != nil {
		t.Errorf("qualifiedDividend = %v, want nil since it could not be read", *tx.QualifiedDividend)
	}

	var a Account
	if err := json.Unmarshal([]byte(`{"securitiesAccount":{"accountNumber":"1","isPortfolioMargin":1,`+
		`"CurrentBalances":{"IsInCall":0,"cashBalance":"x","liquidationValue":9}},"aggregatedBalance":{"liquidationValue":[1]}}`), &a); err != nil {
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

// A duplicated scalar takes its last value, but a duplicated object is
// merged into the first, as the package doc says.
func TestDuplicateKeys(t *testing.T) {
	var a Account
	if err := json.Unmarshal([]byte(`{"securitiesAccount":{"accountNumber":"1","type":"CASH","type":"MARGIN",`+
		`"currentBalances":{"cashBalance":100,"liquidationValue":500},"currentBalances":{"cashBalance":7}}}`), &a); err != nil {
		t.Fatal(err)
	}
	sa := a.SecuritiesAccount
	if sa.Type != "MARGIN" || *sa.CurrentBalances.CashBalance != 7 || sa.CurrentBalances.LiquidationValue == nil || *sa.CurrentBalances.LiquidationValue != 500 {
		t.Errorf("decoded %+v", sa)
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
