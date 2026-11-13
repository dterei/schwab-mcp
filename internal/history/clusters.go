package history

import (
	"cmp"
	"crypto/sha256"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A fileInput is what the derivation reads of a file of schwab.com
// imported for the account: a History or a Realized Gain/Loss export.
type fileInput struct {
	id int64
	// sha is the hex of its SHA-256, and kind what it is (history_csv,
	// history_json or realized_csv).
	sha, kind string
	// ref is its ref (F and a prefix of sha), and label its name with its
	// free text masked.
	ref, label string
	// forgotten says that your forget decision set it aside: it takes no
	// part in the derivation but for where your decisions on its rows go.
	forgotten bool
	// exportedAt is when it was exported, else when it was imported: the
	// newest export of a transaction shows it.
	exportedAt time.Time
	// span is what its coverage is worked out from, and from and to the
	// range you gave for a file that states none: your file_range
	// decision's, else the import's.
	span     schwabfile.Span
	from, to nytime.Date
	// nameDigits are the account's last digits as its name gives them.
	nameDigits string
	// completeness is your completeness decision on it, or "".
	completeness string
	// flags are the flags that its content gives by itself (layer 1), as
	// the store holds them.
	flags flagSet
	// rows are a History file's rows, and lots a Realized file's, in the
	// order of their ordinals.
	rows []*historyRowInput
	lots []*lotRowInput
}

// exported returns the New York date of the file's export, or of its
// import when it says none.
func (f *fileInput) exported() nytime.Date { return nytime.DateOf(f.exportedAt) }

// isHistory reports whether the file is a History export.
func (f *fileInput) isHistory() bool { return f.kind != "realized_csv" }

// A rowProblem is an entry of history_row.problems and realized_row.problems:
// what Read could not read of the row (What), or a value that the store
// notes: a number off the grid (Flag inexact), a placeholder in place of a
// number (Flag placeholder), or a lot whose columns disagree (Flag
// columns_disagree), with the column (Path) and the cell (Value).
type rowProblem struct {
	What  string `json:"what,omitempty"`
	Path  string `json:"path,omitempty"`
	Flag  string `json:"flag,omitempty"`
	Value string `json:"value,omitempty"`
}

// A historyRowInput is what the derivation reads of a row of a History
// file.
type historyRowInput struct {
	id   int64
	file *fileInput
	// ordinal is its place among the file's rows, from 1.
	ordinal int
	// posted and asOf are its dates; a row without posted is undated and
	// makes no transaction.
	posted, asOf nytime.Date
	// action, symbol and description are its cells, the description
	// masked.
	action, symbol, description string
	// quantity is signed by the action (signedQuantity); quantityText and
	// priceText are the exact values in one spelling, "" when blank.
	quantity               sql.NullInt64
	quantityText           string
	price                  sql.NullFloat64
	priceText              string
	fees, amount           sql.NullInt64
	feesBlank, amountBlank bool
	// coarse is its coarse identity (coarseHistory).
	coarse [32]byte
	// xKey is its ItemIssueId's key, and fileKey its symbol's, each "" when
	// it has none: its security is the first's, else the second's.
	xKey, fileKey string
	problems      []rowProblem
}

// event returns the day the row's transaction happened: its as-of date,
// else its posted date.
func (r *historyRowInput) event() nytime.Date { return cmp.Or(r.asOf, r.posted) }

// class returns what the rules for file transactions read of the row.
func (r *historyRowInput) class() fileClass {
	amount := r.amount
	if r.amountBlank {
		amount = sql.NullInt64{Valid: true}
	}
	return fileClass{action: normalAction(r.action), hasQuantity: r.quantityText != "", quantity: signOf(r.quantity), amount: amount}
}

// A lotRowInput is what the derivation reads of a lot of a Realized
// Gain/Loss file.
type lotRowInput struct {
	id       int64
	file     *fileInput
	ordinal  int
	closed   nytime.Date
	symbol   string
	name     string // masked
	quantity string // exact
	proceeds sql.NullInt64
	coarse   [32]byte
	fine     [32]byte
	fileKey  string
	problems []rowProblem
}

// coarseHistory returns the coarse identity of a History row (14.5 of the
// design): what a later export does not restate, its dates, its action as
// letters and digits, and its Fees & Comm and Amount in cents (or the
// placeholder, or empty). A JSON export and the CSV of the same rows
// agree on it.
func coarseHistory(posted, asOf nytime.Date, action string, fees, amount schwabfile.Decimal) [32]byte {
	return digest("h1", string(posted), string(asOf), normalAction(action), decimalCents(fees), decimalCents(amount))
}

// coarseLot returns the coarse identity of a lot: what a wash sale's
// later adjustments leave alone, its closed date, its opened date as
// written, its exact quantity and its proceeds in cents.
func coarseLot(closed nytime.Date, opened string, quantity, proceeds schwabfile.Decimal) [32]byte {
	return digest("l1", string(closed), opened, quantity.Canon(), decimalCents(proceeds))
}

// decimalCents writes a number of a file as cents, a placeholder as
// itself in lower case, a number off the cent grid exactly, and a blank as
// "".
func decimalCents(d schwabfile.Decimal) string {
	if c, ok := d.Cents(); ok {
		return strconv.FormatInt(c, 10)
	}
	if d.Placeholder != "" {
		return strings.ToLower(d.Placeholder)
	}
	return d.Canon()
}

// digest returns the SHA-256 of parts joined by NUL bytes.
func digest(parts ...string) [32]byte {
	return sha256.Sum256([]byte(strings.Join(parts, "\x00")))
}

// fineSymbol returns a symbol as the fine fields compare it: upper case,
// its white space one space.
func fineSymbol(s string) string { return strings.ToUpper(strings.Join(strings.Fields(s), " ")) }

// fineText returns text as the fine fields compare it: its white space one
// space.
func fineText(s string) string { return strings.Join(strings.Fields(s), " ") }

// A secInfo is what the counting reads of the security of a key: its id,
// and whether it is definite (holds a key with an id of Schwab's).
type secInfo struct {
	id       int64
	definite bool
}

// securities are what the counting reads of the store's securities: each
// key's, the periods in which each security's names were seen, and the
// pairs that your differ decisions keep apart.
type securities struct {
	keys    map[string]secInfo
	periods map[int64][][2]time.Time
	differ  map[[2]int64]bool
}

// of returns the security of a row by its keys: its ItemIssueId's, else
// its symbol's; false when it has neither.
func (s *securities) of(xKey, fileKey string) (secInfo, bool) {
	for _, k := range []string{xKey, fileKey} {
		if k == "" {
			continue
		}
		if i, ok := s.keys[k]; ok {
			return i, true
		}
	}
	return secInfo{}, false
}

// atOnce reports whether securities a and b existed at once: some name of
// each was seen within symbolSlack of a name of the other.
func (s *securities) atOnce(a, b int64) bool {
	for _, p := range s.periods[a] {
		for _, q := range s.periods[b] {
			if overlapWithin(p[0], p[1], q[0], q[1], symbolSlack) {
				return true
			}
		}
	}
	return false
}

// A rowSide is what knownDifferent reads of one row: its symbol and its
// keys.
type rowSide struct {
	symbol        string
	xKey, fileKey string
}

// contract returns the option contract that a symbol names, by either
// spelling: the OCC symbol or schwab.com's "XYZ 09/18/2026 50.00 C".
func contract(symbol string) (schwab.OCC, bool) {
	if o, ok := schwabfile.ParseOption(fineSymbol(symbol)); ok {
		return o, true
	}
	return schwab.ParseOCC(symbol)
}

// knownDifferent reports whether two rows of files are known to be two
// transactions (14.5 of the design): both securities definite, different,
// and existing at once; two options of different contracts (expiration,
// put or call, strike: the root is left out, since a ticker change renames
// it); two different CUSIPs; an option and a symbol that is not one; or
// your differ decision on their securities. Two options or CUSIPs that one
// security holds are not different. Anything else, such as equal
// symbols, an empty symbol or two tickers that nothing resolves, is not
// known to differ.
func knownDifferent(a, b rowSide, secs *securities) bool {
	sa, okA := secs.of(a.xKey, a.fileKey)
	sb, okB := secs.of(b.xKey, b.fileKey)
	same := okA && okB && sa.id == sb.id
	if okA && okB && !same {
		if secs.differ[[2]int64{min(sa.id, sb.id), max(sa.id, sb.id)}] {
			return true
		}
		if sa.definite && sb.definite && secs.atOnce(sa.id, sb.id) {
			return true
		}
	}
	if a.symbol == "" || b.symbol == "" {
		return false
	}
	ca, optA := contract(a.symbol)
	cb, optB := contract(b.symbol)
	switch {
	case optA && optB:
		return !same && (ca.Expiration != cb.Expiration || ca.PutCall != cb.PutCall || ca.StrikeThousandths != cb.StrikeThousandths)
	case optA != optB:
		return true
	}
	if schwab.IsCUSIP(a.symbol) && schwab.IsCUSIP(b.symbol) {
		return !same && fineSymbol(a.symbol) != fineSymbol(b.symbol)
	}
	return false
}

// A clustered row is a row of a file that clusters reads: History rows
// and Realized lots alike.
type clustered interface {
	fileOf() *fileInput
	ord() int
	coarseOf() [32]byte
	side() rowSide
}

func (r *historyRowInput) fileOf() *fileInput { return r.file }
func (r *historyRowInput) ord() int           { return r.ordinal }
func (r *historyRowInput) coarseOf() [32]byte { return r.coarse }
func (r *historyRowInput) side() rowSide      { return rowSide{r.symbol, r.xKey, r.fileKey} }
func (r *lotRowInput) fileOf() *fileInput     { return r.file }
func (r *lotRowInput) ord() int               { return r.ordinal }
func (r *lotRowInput) coarseOf() [32]byte     { return r.coarse }
func (r *lotRowInput) side() rowSide          { return rowSide{r.symbol, "", r.fileKey} }

// A cluster is the rows of different files that are one transaction (or
// one lot): the first opened it.
type cluster[R clustered] struct {
	rows []R
	// split says that it was opened only because its first row was known
	// different from a cluster that held no row of its file.
	split bool
}

// opener returns the row that opened the cluster.
func (c *cluster[R]) opener() R { return c.rows[0] }

// clusterRows clusters rows, of files in the order of their import, by
// their coarse identity (14.5 of the design): within a file every row is a
// transaction of its own; a row of a later file joins a cluster of its
// coarse identity that holds no row of its file and none known different
// from it, the pairs taken greedily by score (score), then by the
// cluster's age and the row's ordinal; rows left over open clusters of
// their own. So the transactions of a coarse identity are as many as the
// most rows of it that one file holds, raised only where a row is known
// different from every cluster it could join. The clusters come in the
// order they were opened.
func clusterRows[R clustered](rows []R, secs *securities, score func(a, b R) int) []*cluster[R] {
	byCoarse := map[[32]byte][]R{}
	var order [][32]byte
	for _, r := range rows {
		c := r.coarseOf()
		if _, ok := byCoarse[c]; !ok {
			order = append(order, c)
		}
		byCoarse[c] = append(byCoarse[c], r)
	}
	var all []*cluster[R]
	for _, c := range order {
		group := byCoarse[c]
		slices.SortStableFunc(group, func(a, b R) int {
			return cmp.Or(cmp.Compare(a.fileOf().id, b.fileOf().id), cmp.Compare(a.ord(), b.ord()))
		})
		var clusters []*cluster[R]
		for i := 0; i < len(group); {
			j := i
			for j < len(group) && group[j].fileOf() == group[i].fileOf() {
				j++
			}
			clusters = joinFile(clusters, group[i:j], secs, score)
			i = j
		}
		all = append(all, clusters...)
	}
	return all
}

// joinFile adds the rows of one file, of one coarse identity, to its
// clusters (clusterRows).
func joinFile[R clustered](clusters []*cluster[R], rows []R, secs *securities, score func(a, b R) int) []*cluster[R] {
	type pair struct{ k, r, score int }
	var pairs []pair
	differs := make([][]bool, len(rows)) // row -> cluster: known different from a row of it
	for ri, r := range rows {
		differs[ri] = make([]bool, len(clusters))
		for k, c := range clusters {
			for _, o := range c.rows {
				if knownDifferent(r.side(), o.side(), secs) {
					differs[ri][k] = true
					break
				}
			}
			if !differs[ri][k] {
				pairs = append(pairs, pair{k, ri, score(r, c.opener())})
			}
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.k, b.k), cmp.Compare(rows[a.r].ord(), rows[b.r].ord()))
	})
	n := len(clusters)
	takenK, takenR := make([]bool, n), make([]bool, len(rows))
	for _, p := range pairs {
		if takenK[p.k] || takenR[p.r] {
			continue
		}
		takenK[p.k], takenR[p.r] = true, true
		clusters[p.k].rows = append(clusters[p.k].rows, rows[p.r])
	}
	for ri, r := range rows {
		if takenR[ri] {
			continue
		}
		split := false
		for k := range n {
			split = split || !takenK[k] && differs[ri][k]
		}
		clusters = append(clusters, &cluster[R]{rows: []R{r}, split: split})
	}
	return clusters
}

// historyScore scores a row against a cluster's first row: equal symbols
// count most, then equal quantities and prices, then equal descriptions.
func historyScore(r, o *historyRowInput) int {
	s := 0
	if fineSymbol(r.symbol) == fineSymbol(o.symbol) {
		s += 4
	}
	if r.quantityText == o.quantityText {
		s += 2
	}
	if r.priceText == o.priceText {
		s += 2
	}
	if fineText(r.description) == fineText(o.description) {
		s++
	}
	return s
}

// lotScore scores a lot against a cluster's first lot: equal symbols count
// most, then equal names.
func lotScore(r, o *lotRowInput) int {
	s := 0
	if fineSymbol(r.symbol) == fineSymbol(o.symbol) {
		s += 4
	}
	if fineText(r.name) == fineText(o.name) {
		s++
	}
	return s
}

// shown returns the row of a cluster that its entry shows: the newest
// export's, by when it was exported and then by its file's id.
func shown[R clustered](c *cluster[R]) R {
	best := c.rows[0]
	for _, r := range c.rows[1:] {
		f, b := r.fileOf(), best.fileOf()
		if cmp.Or(f.exportedAt.Compare(b.exportedAt), cmp.Compare(f.id, b.id)) > 0 {
			best = r
		}
	}
	return best
}

// hexRef returns the shortest prefix of sha, of 8 hex digits or more, that
// no other of shas starts with.
func hexRef(sha string, shas []string) string {
	n := 8
	for _, o := range shas {
		if o == sha {
			continue
		}
		common := 0
		for common < len(sha) && common < len(o) && sha[common] == o[common] {
			common++
		}
		n = max(n, common+1)
	}
	return sha[:min(n, len(sha))]
}
