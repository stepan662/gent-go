package db

import (
	"context"
	"encoding/json"

	dbgen "gent/internal/db/gen"
	"gent/internal/idgen"
	"gent/internal/model"
)

// LogQuery holds the optional filters shared by ListLogs and ListTreeLogs plus
// the pagination request. The zero value (empty Level, zero Since, zero Page)
// returns the first page from the beginning.
type LogQuery struct {
	Level string
	Since int64 // unix millis; 0 = from the start
	Page  PageReq
}

// logPaginator is the pagination policy for logs. Only time order is offered: the
// (created_at, id) keyset preserves insertion order because UUIDv7 ids are
// monotonic within a millisecond, and is index-backed (idx_process_logs_instance
// for the flat query, idx_process_logs_created for the subtree query). table +
// columns are pl.-qualified so build() serves the flat query; the subtree-CTE
// query supplies its own prefixes via buildSource.
var logPaginator = paginator{
	table:      "process_logs pl",
	columns:    logColumns,
	filterCols: []string{"pl.instance_id", "pl.level", "pl.created_at"},
	sorts: map[string]sortMode{
		"created": {{"pl.created_at", kindInt}, {"pl.id", kindText}},
	},
	defSort:  "created",
	defDesc:  false, // oldest first
	defLimit: 20,
	maxLimit: 100,
}

func logCursorVals(_ string, e *model.LogEntry) []any {
	return []any{e.CreatedAt.UnixMilli(), e.ID}
}

// AppendLog writes one audit-trail row. It is best-effort by contract: callers
// (the engine) must not let a failure here abort an instance advance. A blank
// entry.ID is filled with a fresh uuid; a zero CreatedAt is stamped with the
// DB clock.
func (db *DB) AppendLog(entry *model.LogEntry) error {
	id := entry.ID
	if id == "" {
		// UUIDv7 is time-ordered and monotonic within a millisecond, so the
		// (created_at, id) sort preserves insertion order even when several
		// events of one advance() share the same millisecond timestamp.
		id = idgen.New()
	}
	createdAt := nowMillis()
	if !entry.CreatedAt.IsZero() {
		createdAt = entry.CreatedAt.UnixMilli()
	}
	// meta is structured (and small), so it is stored as JSON; data is the raw,
	// possibly-truncated body and is stored verbatim.
	meta := ""
	if len(entry.Meta) > 0 {
		b, err := json.Marshal(entry.Meta)
		if err != nil {
			return err
		}
		meta = string(b)
	}
	return db.q.InsertLog(context.Background(), dbgen.InsertLogParams{
		ID:         id,
		InstanceID: entry.InstanceID,
		Level:      string(entry.Level),
		Event:      entry.Event,
		TaskID:     entry.TaskID,
		Message:    entry.Message,
		Code:       entry.Code,
		Data:       entry.Data, // raw payload snippet (input/output/request/response body), or ""
		Meta:       meta,
		CreatedAt:  createdAt,
	})
}

// logColumns is the pl.-qualified SELECT list shared by both log queries (the
// flat query aliases process_logs pl; the subtree query joins it as pl).
const logColumns = `pl.id, pl.instance_id, pl.level, pl.event, pl.task_id, pl.message, pl.code, pl.data, pl.meta, pl.created_at`

// logSubtreeCTE walks process_instances.parent_id from a seed id (the single ?
// placeholder) down, tagging each node with its depth from the seed. Hand-written
// because sqlc's SQLite grammar can't parse WITH RECURSIVE; both runtime drivers
// support it. treeLogsPrefix is the page SELECT; treeLogsCountInner is the count's
// inner row source (one row per matching log) the paginator wraps in COUNT(*).
const logSubtreeCTE = `
WITH RECURSIVE subtree(id, depth) AS (
    SELECT id, 0 FROM process_instances WHERE id = ?
    UNION ALL
    SELECT pi.id, s.depth + 1 FROM process_instances pi JOIN subtree s ON pi.parent_id = s.id
)`

const treeLogsJoin = `
FROM process_logs pl
JOIN subtree st ON st.id = pl.instance_id`

const treeLogsPrefix = logSubtreeCTE + `
SELECT ` + logColumns + `, st.depth` + treeLogsJoin

const treeLogsCountInner = logSubtreeCTE + `
SELECT 1` + treeLogsJoin

// ListLogs returns a page of one instance's audit trail, applying the filters and
// pagination in opts, plus the navigation metadata.
func (db *DB) ListLogs(instanceID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	b, err := logPaginator.query(opts.Page).
		Eq("pl.instance_id", instanceID).
		EqIf("pl.level", opts.Level, opts.Level != "").
		GteIf("pl.created_at", opts.Since, opts.Since > 0).
		build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	rows, err := db.exec.QueryContext(context.Background(), b.pageSQL, b.pageArgs...)
	if err != nil {
		return nil, PageInfo{}, err
	}
	out, err := scanLogPage(rows, false)
	if err != nil {
		return nil, PageInfo{}, err
	}
	items, first, last := orient(b, out, logCursorVals)
	info, err := db.pageInfo(b, first, last)
	if err != nil {
		return nil, PageInfo{}, err
	}
	return items, info, nil
}

// ListTreeLogs returns a page of every log for the subtree rooted at the given
// instance (the node itself and all descendants). rootID may be any node. Each
// entry's Depth is its instance's distance from rootID (rootID = 0), plus the
// navigation metadata. The CTE prefixes are trusted constants; the filters/cursor
// and ORDER BY are generated by the shared paginator via buildSource.
func (db *DB) ListTreeLogs(rootID string, opts LogQuery) ([]*model.LogEntry, PageInfo, error) {
	b, err := logPaginator.query(opts.Page).
		EqIf("pl.level", opts.Level, opts.Level != "").
		GteIf("pl.created_at", opts.Since, opts.Since > 0).
		buildSource(treeLogsPrefix, treeLogsCountInner, []any{rootID})
	if err != nil {
		return nil, PageInfo{}, err
	}
	rows, err := db.exec.QueryContext(context.Background(), b.pageSQL, b.pageArgs...)
	if err != nil {
		return nil, PageInfo{}, err
	}
	out, err := scanLogPage(rows, true)
	if err != nil {
		return nil, PageInfo{}, err
	}
	items, first, last := orient(b, out, logCursorVals)
	info, err := db.pageInfo(b, first, last)
	if err != nil {
		return nil, PageInfo{}, err
	}
	return items, info, nil
}

// scanLogPage scans a log page. When withDepth, each row carries a trailing
// st.depth column (the subtree query); otherwise it is the flat column list.
func scanLogPage(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}, withDepth bool) ([]*model.LogEntry, error) {
	defer rows.Close()
	var out []*model.LogEntry
	for rows.Next() {
		var r dbgen.ProcessLog
		var depth int64
		dest := []any{&r.ID, &r.InstanceID, &r.Level, &r.Event, &r.TaskID, &r.Message, &r.Code, &r.Data, &r.Meta, &r.CreatedAt}
		if withDepth {
			dest = append(dest, &depth)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		e, err := toLogEntry(r)
		if err != nil {
			return nil, err
		}
		e.Depth = int(depth)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneLogs deletes every log older than the given cutoff (unix millis) and
// returns how many rows were removed.
func (db *DB) PruneLogs(before int64) (int64, error) {
	return db.q.DeleteLogsBefore(context.Background(), before)
}

func toLogEntry(r dbgen.ProcessLog) (*model.LogEntry, error) {
	e := &model.LogEntry{
		ID:         r.ID,
		InstanceID: r.InstanceID,
		Level:      model.LogLevel(r.Level),
		Event:      r.Event,
		TaskID:     r.TaskID,
		Message:    r.Message,
		Code:       r.Code,
		Data:       r.Data,
		CreatedAt:  toTime(r.CreatedAt),
	}
	if r.Meta != "" && r.Meta != "{}" {
		if err := json.Unmarshal([]byte(r.Meta), &e.Meta); err != nil {
			return nil, err
		}
	}
	return e, nil
}
