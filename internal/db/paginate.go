package db

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Bidirectional keyset (cursor) pagination shared by every list endpoint.
//
// sqlc generates static SQL, so a dynamic ORDER BY / cursor predicate cannot be a
// query parameter (a column name or ASC/DESC is never a bind value). This file is
// a small query builder: a list wrapper declares a `paginator` once (its table,
// columns, the index-backed sortable columns, and the filterable columns), adds
// filters by column+value through typed methods, and calls build(). The builder
// emits the whole page query (`SELECT … FROM … WHERE … ORDER BY … LIMIT ?`) plus a
// matching `SELECT COUNT(*)` for the total. Column names and operators come only
// from the paginator's whitelists; every caller value is a bound ? placeholder, so
// there is no injection surface. ? runs through db.exec, which rewrites it to $N on
// Postgres, so one statement works on both engines with no dialect branch.
//
// Navigation is bidirectional: `After` pages forward, `Before` pages backward
// (scanned in reverse, then flipped back to display order). Each page reports
// total_items (a separate COUNT), has_next/has_previous, and the cursors to move
// either way.

// colKind tells the cursor codec how to decode a key column's value.
type colKind int

const (
	kindText colKind = iota
	kindInt
)

// keyCol is one column of a sort order: a TRUSTED SQL column name (whitelist
// only, never user input) plus its value type for cursor encoding.
type keyCol struct {
	col  string
	kind colKind
}

// sortMode is the ordered column list for one named sort. The columns together
// must be UNIQUE (the last is the tiebreaker) so the keyset cursor never skips or
// repeats a row. Direction applies uniformly to all columns, keeping the cursor
// predicate a simple OR-chain — no mixed ASC/DESC (which row-value tuples can't
// express on SQLite). Every column should be index-backed so the seek stays cheap.
type sortMode []keyCol

// paginator is the configure-once policy for one listing: where to read, what to
// select, the sortable (index-backed) and filterable column whitelists, and
// defaults. baseWhere is an optional TRUSTED constant predicate (no placeholders)
// always ANDed in — e.g. a literal an engine needs for partial-index matching.
// Declare instances as package vars next to the wrapper that uses them.
type paginator struct {
	table      string // FROM clause (trusted constant), e.g. "process_instances"
	columns    string // SELECT list (trusted constant)
	baseWhere  string // always-applied constant predicate, no ? placeholders ("" = none)
	sorts      map[string]sortMode
	filterCols []string // columns the wrapper may filter on (whitelist)
	defSort    string   // default sort-mode key (PageReq.Sort == "")
	defDesc    bool     // default direction (PageReq.Desc == nil)
	defLimit   int
	maxLimit   int
}

func (pg paginator) allowsFilter(col string) bool {
	for _, c := range pg.filterCols {
		if c == col {
			return true
		}
	}
	return false
}

// PageReq is the decoded pagination input from the API layer. After/Before are
// opaque cursors from a previous page; Before pages backward. At most one should
// be set (Before wins if both are). A nil Desc uses the paginator's default
// direction; an empty Sort selects the default sort.
type PageReq struct {
	Sort   string
	Desc   *bool
	Limit  int
	After  string
	Before string
}

// PageInfo is the navigation metadata returned alongside a page of items.
// ItemsBefore/ItemsAfter are the counts outside the page in display order, so the
// caller can render "showing N–M of total". NextCursor/PreviousCursor are set only
// in a direction that has more rows (NextCursor iff ItemsAfter>0, PreviousCursor
// iff ItemsBefore>0), so cursor presence is itself the has-more signal and a
// page-to-end loop terminates when the cursor is absent.
type PageInfo struct {
	Size           int    `json:"size"`
	TotalItems     int64  `json:"total_items"`
	ItemsBefore    int64  `json:"items_before"`
	ItemsAfter     int64  `json:"items_after"`
	NextCursor     string `json:"next_cursor,omitempty"`
	PreviousCursor string `json:"previous_cursor,omitempty"`
}

// built is the assembled, ready-to-run plan for one request: the page statement
// plus the scaffolding countQuery needs to assemble the combined count (the agg
// list is injected between countLeading and countSource once the page's boundary
// rows are known) and the context orient needs to flip a backward page.
type built struct {
	pageSQL  string
	pageArgs []any

	// Count scaffolding. The final count is:
	//   countLeading + "<aggs>" + countSource
	// with args countPrefixArgs + <agg args> + countFilterArgs (matching the
	// placeholder order: any CTE seed in countLeading, then the agg CASE values in
	// the SELECT list, then the filter values in countSource's WHERE).
	countLeading    string // "[<CTE>] SELECT " — everything up to the agg list
	countSource     string // " FROM <source> [WHERE <filters>]" — after the agg list
	countPrefixArgs []any
	countFilterArgs []any

	mode     sortMode
	sort     string
	desc     bool
	limit    int
	backward bool
}

// listQuery accumulates the dynamic filters of one request. Build it with
// paginator.query(req); add filters with Eq/EqIf/Gte/GteIf; finish with build (or
// buildSource for a custom static prefix such as a recursive CTE). A filter on an
// undeclared column is a programming error surfaced as a build error.
type listQuery struct {
	pg    paginator
	req   PageReq
	conds []string
	args  []any
	err   error
}

func (pg paginator) query(req PageReq) *listQuery {
	return &listQuery{pg: pg, req: req}
}

// Eq adds "col = ?" binding value. col must be a declared filter column.
func (q *listQuery) Eq(col string, value any) *listQuery { return q.cond(col, "=", value) }

// EqIf adds "col = ?" only when include is true (optional/skip-when-empty filters).
func (q *listQuery) EqIf(col string, value any, include bool) *listQuery {
	if include {
		return q.cond(col, "=", value)
	}
	return q
}

// Gte adds "col >= ?".
func (q *listQuery) Gte(col string, value any) *listQuery { return q.cond(col, ">=", value) }

// GteIf adds "col >= ?" only when include is true.
func (q *listQuery) GteIf(col string, value any, include bool) *listQuery {
	if include {
		return q.cond(col, ">=", value)
	}
	return q
}

func (q *listQuery) cond(col, op string, value any) *listQuery {
	if q.err != nil {
		return q
	}
	if !q.pg.allowsFilter(col) {
		q.err = fmt.Errorf("paginate: %q is not a configured filter column", col)
		return q
	}
	q.conds = append(q.conds, col+" "+op+" ?")
	q.args = append(q.args, value)
	return q
}

// build assembles the page + count plan for the paginator's own table:
// "SELECT <columns> FROM <table>" for the page and "SELECT <aggs> FROM <table>"
// for the count.
func (q *listQuery) build() (built, error) {
	return q.buildSource(
		"SELECT "+q.pg.columns+" FROM "+q.pg.table,
		"SELECT ",
		" FROM "+q.pg.table,
		nil,
	)
}

// buildSource is build for callers needing a custom static prefix (e.g. a
// recursive CTE). All three fragments are TRUSTED constants:
//   - pagePrefix:   "[<CTE>] SELECT <cols> FROM <source>" (the page query, before WHERE)
//   - countLeading: "[<CTE>] SELECT " (the count, up to its agg list)
//   - countFrom:    " FROM <source>" (the count, after its agg list, before WHERE)
//
// prefixArgs bind any ? in the CTE (shared by both queries) and are placed first.
// The generated WHERE / ORDER BY / LIMIT is appended to the page; the count's
// WHERE (filters only, never the cursor predicate) is baked into countSource so
// countQuery can later splice the agg list in the middle.
func (q *listQuery) buildSource(pagePrefix, countLeading, countFrom string, prefixArgs []any) (built, error) {
	if q.err != nil {
		return built{}, q.err
	}
	pg := q.pg

	key := q.req.Sort
	if key == "" {
		key = pg.defSort
	}
	mode, ok := pg.sorts[key]
	if !ok {
		return built{}, fmt.Errorf("unknown sort %q", q.req.Sort)
	}
	desc := pg.defDesc
	if q.req.Desc != nil {
		desc = *q.req.Desc
	}
	limit := q.req.Limit
	if limit <= 0 {
		limit = pg.defLimit
	}
	if limit > pg.maxLimit {
		limit = pg.maxLimit
	}

	backward := q.req.Before != ""
	tok := q.req.After
	if backward {
		tok = q.req.Before
	}

	// Scan direction: forward follows the display order, backward reverses it (then
	// orient flips the slice back). cmp/dir derive from the scan direction.
	scanAsc := !desc
	if backward {
		scanAsc = !scanAsc
	}
	cmp, dir := ">", "ASC"
	if !scanAsc {
		cmp, dir = "<", "DESC"
	}

	// WHERE: baseWhere + filters (shared by count), then the cursor predicate (page
	// only — the count must reflect the grand total, not the post-cursor remainder).
	conds := make([]string, 0, len(q.conds)+2)
	if pg.baseWhere != "" {
		conds = append(conds, pg.baseWhere)
	}
	conds = append(conds, q.conds...)
	countConds := append([]string(nil), conds...)

	var predArgs []any
	if tok != "" {
		vals, err := decodeCursor(tok, key, desc, mode)
		if err != nil {
			return built{}, err
		}
		pred, pArgs := keysetPredicate(mode, cmp, vals)
		conds = append(conds, pred)
		predArgs = pArgs
	}

	orderCols := make([]string, len(mode))
	for i, kc := range mode {
		orderCols[i] = kc.col + " " + dir
	}
	orderBy := strings.Join(orderCols, ", ")

	pageSQL := pagePrefix + whereClause(conds) + " ORDER BY " + orderBy + " LIMIT ?"
	pageArgs := make([]any, 0, len(prefixArgs)+len(q.args)+len(predArgs)+1)
	pageArgs = append(pageArgs, prefixArgs...)
	pageArgs = append(pageArgs, q.args...)
	pageArgs = append(pageArgs, predArgs...)
	pageArgs = append(pageArgs, int64(limit))

	return built{
		pageSQL:         pageSQL,
		pageArgs:        pageArgs,
		countLeading:    countLeading,
		countSource:     countFrom + whereClause(countConds),
		countPrefixArgs: prefixArgs,
		countFilterArgs: q.args,
		mode:            mode,
		sort:            key,
		desc:            desc,
		limit:           limit,
		backward:        backward,
	}, nil
}

// countQuery assembles the combined count over the filtered set: the grand total,
// the number of rows strictly before the first row, and strictly after the last
// row — both in display order. first/last are the page's boundary key values (nil
// for an empty page → only the total is counted). One scan yields all three.
func (b built) countQuery(first, last []any) (string, []any) {
	// "before the first row" / "after the last row" in display order.
	beforeCmp, afterCmp := "<", ">"
	if b.desc {
		beforeCmp, afterCmp = ">", "<"
	}
	aggs := []string{"COUNT(*)"}
	var aggArgs []any
	addAgg := func(vals []any, cmp string) {
		if len(vals) == 0 {
			aggs = append(aggs, "0")
			return
		}
		pred, args := keysetPredicate(b.mode, cmp, vals)
		aggs = append(aggs, "COALESCE(SUM(CASE WHEN "+pred+" THEN 1 ELSE 0 END), 0)")
		aggArgs = append(aggArgs, args...)
	}
	addAgg(first, beforeCmp)
	addAgg(last, afterCmp)

	sql := b.countLeading + strings.Join(aggs, ", ") + b.countSource
	args := make([]any, 0, len(b.countPrefixArgs)+len(aggArgs)+len(b.countFilterArgs))
	args = append(args, b.countPrefixArgs...)
	args = append(args, aggArgs...)
	args = append(args, b.countFilterArgs...)
	return sql, args
}

func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// orient restores display order for a backward page (which was scanned in reverse)
// and returns the page plus its boundary key values — the first and last rows'
// sort-key values, in display order — which the count query and cursors are built
// from. Both boundaries are nil for an empty page. valsOf returns a row's
// key-column values for the active sort.
func orient[T any](b built, rows []T, valsOf func(sort string, row T) []any) (items []T, first, last []any) {
	if b.backward {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	if len(rows) == 0 {
		return rows, nil, nil
	}
	return rows, valsOf(b.sort, rows[0]), valsOf(b.sort, rows[len(rows)-1])
}

// keysetPredicate builds the lexicographic OR-chain that selects rows strictly
// after the cursor under comparator cmp (> for an ascending scan, < for
// descending). For columns (a, b, c):
//
//	(a cmp ?) OR (a = ? AND b cmp ?) OR (a = ? AND b = ? AND c cmp ?)
//
// Spelled out (not row-value syntax) so it runs identically on SQLite and Postgres.
func keysetPredicate(mode sortMode, cmp string, vals []any) (string, []any) {
	ors := make([]string, 0, len(mode))
	args := make([]any, 0, len(mode)*(len(mode)+1)/2)
	for i := range mode {
		terms := make([]string, 0, i+1)
		for j := 0; j < i; j++ {
			terms = append(terms, mode[j].col+" = ?")
			args = append(args, vals[j])
		}
		terms = append(terms, mode[i].col+" "+cmp+" ?")
		args = append(args, vals[i])
		ors = append(ors, "("+strings.Join(terms, " AND ")+")")
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// cursorToken is the opaque, URL-safe-base64-encoded JSON payload of a cursor. It
// carries the sort key and direction it was minted under, so a token is rejected
// if reused under a different sort. It does NOT encode the filters — like all
// keyset pagination it assumes stable filters across pages.
type cursorToken struct {
	K string `json:"k"` // sort-mode key
	D bool   `json:"d"` // desc
	V []any  `json:"v"` // key-column values, in mode order
}

// encodeCursor mints the opaque cursor for a boundary row.
func encodeCursor(sort string, desc bool, mode sortMode, vals []any) (string, error) {
	if len(vals) != len(mode) {
		return "", fmt.Errorf("cursor: got %d values, want %d", len(vals), len(mode))
	}
	b, err := json.Marshal(cursorToken{K: sort, D: desc, V: vals})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor parses tok and returns its key-column values, coerced to each
// column's Go type. It rejects a token minted under a different sort/direction and
// any malformed token.
func decodeCursor(tok, sort string, desc bool, mode sortMode) ([]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep kindInt values exact (avoid float64 round-trip)
	var c cursorToken
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	if c.K != sort || c.D != desc {
		return nil, fmt.Errorf("cursor does not match the requested sort")
	}
	if len(c.V) != len(mode) {
		return nil, fmt.Errorf("invalid cursor")
	}
	out := make([]any, len(mode))
	for i, kc := range mode {
		switch kc.kind {
		case kindInt:
			n, ok := c.V[i].(json.Number)
			if !ok {
				return nil, fmt.Errorf("invalid cursor")
			}
			iv, err := n.Int64()
			if err != nil {
				return nil, fmt.Errorf("invalid cursor")
			}
			out[i] = iv
		default:
			s, ok := c.V[i].(string)
			if !ok {
				return nil, fmt.Errorf("invalid cursor")
			}
			out[i] = s
		}
	}
	return out, nil
}
