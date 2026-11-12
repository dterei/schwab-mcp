package history

import "strings"

// A columnKind is what a view's column holds, as far as privacy goes.
type columnKind string

const (
	// freeText is text that Schwab, a file or you wrote, stored through
	// mask.Text or mask.FileName, or composed of such text and of
	// identifiers (a flag's detail): a description, a nickname, a name, a
	// label, a detail.
	freeText columnKind = "free"
	// identifier is what cannot hold an account number: a ref, a key, an
	// ID, a symbol, a CUSIP, a hash, an amount, a quantity, a date or time,
	// a code, or the masked number itself.
	identifier columnKind = "id"
)

// A viewColumn is a column of one of the store's views and its kind.
type viewColumn struct {
	view, column string
	kind         columnKind
	// when, for a column of free text, is the condition on the view's row
	// under which it holds free text, or "" for every row; on the other
	// rows it holds identifiers.
	when string
}

// viewColumns are the views' columns, each of one kind (5.3 of the
// design), in the views' and their columns' order: the contract with what
// reads the store, which TestViewsHoldNoAccountNumber holds every view to
// and docs/store.md lists. A column of free text is marked with a *; a
// security name's value is free text only when it is a description, and an
// entry's kind only when it is a file's Action, which may be one that no
// rule knows (an API transaction's type is a code).
var viewColumns = func() []viewColumn {
	when := map[string]string{
		"security_names.value": "kind = 'description'",
		"entries.kind":         "source = 'file'",
		"ledger.kind":          "source = 'file'",
	}
	var cs []viewColumn
	for _, v := range []struct{ view, columns string }{
		{"accounts", "account_id ref account nickname* account_type kind* primary_account lot_method hash first_seen_at last_seen_at not_shared_since used"},
		{"securities", "security_key definite asset_type type symbol cusip description* put_call strike expiration_day multiplier underlying_symbol underlying_key first_seen_at last_seen_at"},
		{"security_names", "security_key key kind value* seen_in first_seen_at last_seen_at"},
		{"security_keys", "key security_key"},
		{"entries", "ref account_id account source day posted_date time kind* description* amount_cents fees_cents gross_cents quantity_micros quantity_as_of price symbol security_key category category_rule pair_kind pair_ref pair_account_id counted counted_why flags matched_with"},
		{"ledger", "ref account_id account source day time kind* description* amount_cents fees_cents gross_cents quantity_micros quantity_as_of price symbol security_key category pair_kind pair_ref pair_account_id flags"},
		{"transactions", "ref account_id account activity_id time trade_day settlement_day type status sub_account description* net_cents order_id position_id qualified_dividend category category_rule symbol security_key quantity_micros price gross_cents fees_cents revisions gone_since first_seen_at first_seen_by latest_seen_at counted counted_why flags"},
		{"revisions", "ref account_id seq first_seen_at last_seen_at first_seen_by changed"},
		{"legs", "ref account_id account day time source leg asset_type symbol security_key instrument_id item_issue_id quantity_micros cash_cents cost_cents price fee_type position_effect put_call strike expiration_day multiplier underlying_symbol txn_symbol txn_security_key quantity_as_of category counted"},
		{"file_transactions", "ref account_id account posted_date asof_date event_date action* symbol description* quantity_micros quantity_as_of price fees_cents amount_cents item_issue_id acctg_rule_cd files security_key category category_rule counted counted_why flags"},
		{"matches", "file_ref api_ref account_id account method score"},
		{"snapshots", "snapshot_id ref account_id account fetched_at market_date in_session_date source account_type current_liquidation_cents initial_liquidation_cents aggregated_liquidation_cents cash_cents positions flags"},
		{"balances", "snapshot_id account_id account fetched_at market_date in_session_date balance_set figure value_as_sent value_cents"},
		{"positions", "snapshot_id account_id account fetched_at market_date in_session_date position security_key asset_type symbol cusip description* long_micros short_micros settled_long_micros settled_short_micros average_price market_value long_open_pl short_open_pl cost_basis maintenance day_pl"},
		{"sessions", "date open_at close_at early verified"},
		{"snapshot_gaps", "account_id account market_date"},
		{"coverages", "account_id account source from_date to_date"},
		{"files", "ref account_id account kind label* layout imported_at exported_at covers_from covers_to coverage_source completeness complete_by lacks_rows row_count forgotten warnings"},
		{"realized_lots", "ref account_id account symbol name* security_key closed_date opened_date opened_text quantity_micros proceeds_cents cost_basis_cents gain_cents lt_gain_cents st_gain_cents term wash_sale disallowed_loss_cents unadjusted_cost_cents txn_cost_basis_cents txn_gain_cents versions files flags"},
		{"flags", "code severity meaning detail* subject_ref account_id"},
		{"reads", "read_id endpoint account_id window_start window_end all_types symbol sent_at answered_at outcome http_status row_count capped complete"},
	} {
		for c := range strings.FieldsSeq(v.columns) {
			name, free := strings.CutSuffix(c, "*")
			col := viewColumn{view: v.view, column: name, kind: identifier}
			if free {
				col.kind, col.when = freeText, when[v.view+"."+name]
			}
			cs = append(cs, col)
		}
	}
	return cs
}()
