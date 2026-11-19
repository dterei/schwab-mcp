package mcpserver

// What every answer shares (design 8.1, 8.3 and 10.0). A tool's output type
// embeds envelope, and page when it pages, and cuts when it is a fitter;
// addTool fills the warnings and the cuts.

// envelope is embedded by every tool's output: where the answer came from,
// as of when, and what the model should know of it.
type envelope struct {
	Source   Ident     `json:"source"`   // "schwab", "store", "schwab+store" or "local"
	AsOf     Instant   `json:"as_of"`    // when Schwab answered, or the store or files were read
	Warnings []warning `json:"warnings"` // [] when none, never null
}

// env returns e, so that addTool reaches the envelope of any output that
// embeds it.
func (e *envelope) env() *envelope { return e }

// An enveloped output embeds envelope.
type enveloped interface{ env() *envelope }

// A warning is something the model should know of an answer: its code,
// one of design 8.1's, and a sentence composed from masked pieces.
type warning struct {
	Code    Ident    `json:"code"`
	Message FreeText `json:"message"`
}

// The sources of an answer.
const (
	sourceSchwab      Ident = "schwab"
	sourceStore       Ident = "store"
	sourceSchwabStore Ident = "schwab+store"
	sourceLocal       Ident = "local"
)

// sources are the sources an answer may name.
var sources = []Ident{sourceSchwab, sourceStore, sourceSchwabStore, sourceLocal}

// page is embedded by the outputs of the tools that page.
type page struct {
	NextCursor *Cursor `json:"next_cursor"` // null on the last page
	Complete   bool    `json:"complete"`    // false when the budget cut the walk short
}

// cuts is embedded by each output that is a fitter, to name the lists that
// were cut to fit the cap.
type cuts struct {
	Cut []cutList `json:"cut"` // [] when nothing was cut, never null: addTool sets it
}

// cutRecord returns c, so that addTool reaches the cuts of any output that
// embeds them.
func (c *cuts) cutRecord() *cuts { return c }

// A cutter embeds cuts.
type cutter interface{ cutRecord() *cuts }

// cutList is a list that was cut to fit the cap.
type cutList struct {
	Path  Ident `json:"path"`
	Shown int   `json:"shown"` // its first items, in the list's order
	Total int   `json:"total"`
}

// fitter is an answer that step 8 of addTool may cut to fit the cap: it
// drops raw first, then halves the longest of its lists, until it fits.
// Each fitter embeds cuts.
type fitter interface {
	// dropRaw drops Schwab's raw JSON from the answer, and reports whether
	// there was any.
	dropRaw() bool
	// lists returns the lists that may be cut, as they stand.
	lists() []cuttable
}

// cuttable is one list that may be cut.
type cuttable struct {
	// path is how cut names the list: "order.fills",
	// "orders.0.detail.activities", "accounts.2.figures".
	path string
	// value is the list itself, for measuring its encoding.
	value any
	// count returns its length now.
	count func() int
	// keep cuts it to its first n items.
	keep func(n int)
}

// accountRef names an account in an answer.
type accountRef struct {
	Account     MaskedNumber `json:"account"`      // "...135"
	AccountHash *Ident       `json:"account_hash"` // null for an account only the store knows
	Nickname    *FreeText    `json:"nickname"`
}

// span is a run of days, both included.
type span struct {
	From Date `json:"from"`
	To   Date `json:"to"`
}

// problem is a place in a record that Schwab sent differently from before:
// its key path, and what was wrong, which may quote Schwab's value and so
// is masked as free text.
type problem struct {
	Path Ident    `json:"path"`
	What FreeText `json:"what"`
}
