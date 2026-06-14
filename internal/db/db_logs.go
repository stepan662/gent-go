package db

import (
	"context"
	"encoding/json"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"

	"github.com/google/uuid"
)

// LogQuery holds the optional filters shared by ListLogs and ListTreeLogs.
// The zero value (empty Level, zero Since, empty AfterID) returns the first
// page from the beginning. AfterTs/AfterID form a keyset cursor: pass the
// CreatedAt/ID of the last row of the previous page to fetch the next.
type LogQuery struct {
	Level   string
	Since   int64 // unix millis; 0 = from the start
	AfterTs int64
	AfterID string
	Limit   int
}

const defaultLogLimit = 200

func (q LogQuery) limit() int64 {
	if q.Limit <= 0 {
		return defaultLogLimit
	}
	return int64(q.Limit)
}

// AppendLog writes one audit-trail row. It is best-effort by contract: callers
// (the engine) must not let a failure here abort an instance advance. A blank
// entry.ID is filled with a fresh uuid; a zero CreatedAt is stamped with the
// DB clock.
func (db *DB) AppendLog(entry *model.LogEntry) error {
	detail := []byte("{}")
	if len(entry.Detail) > 0 {
		b, err := json.Marshal(entry.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	id := entry.ID
	if id == "" {
		// UUIDv7 is time-ordered and monotonic within a millisecond, so the
		// (created_at, id) sort preserves insertion order even when several
		// events of one advance() share the same millisecond timestamp.
		if v7, err := uuid.NewV7(); err == nil {
			id = v7.String()
		} else {
			id = uuid.NewString()
		}
	}
	createdAt := nowMillis()
	if !entry.CreatedAt.IsZero() {
		createdAt = entry.CreatedAt.UnixMilli()
	}
	return db.q.InsertLog(context.Background(), dbgen.InsertLogParams{
		ID:         id,
		InstanceID: entry.InstanceID,
		RootID:     entry.RootID,
		Level:      string(entry.Level),
		Event:      entry.Event,
		StepID:     entry.StepID,
		Message:    entry.Message,
		Code:       entry.Code,
		Detail:     string(detail),
		CreatedAt:  createdAt,
	})
}

// ListLogs returns one instance's audit trail oldest-first, applying the
// filters and cursor in opts.
func (db *DB) ListLogs(instanceID string, opts LogQuery) ([]*model.LogEntry, error) {
	rows, err := db.q.ListLogs(context.Background(), dbgen.ListLogsParams{
		InstanceID: instanceID,
		Level:      opts.Level,
		Since:      opts.Since,
		AfterTs:    opts.AfterTs,
		AfterID:    opts.AfterID,
		Lim:        opts.limit(),
	})
	if err != nil {
		return nil, err
	}
	return toLogEntries(rows)
}

// ListTreeLogs returns every log for a whole process subtree, keyed on the root
// instance id, oldest-first.
func (db *DB) ListTreeLogs(rootID string, opts LogQuery) ([]*model.LogEntry, error) {
	rows, err := db.q.ListLogsByRoot(context.Background(), dbgen.ListLogsByRootParams{
		RootID:  rootID,
		Level:   opts.Level,
		Since:   opts.Since,
		AfterTs: opts.AfterTs,
		AfterID: opts.AfterID,
		Lim:     opts.limit(),
	})
	if err != nil {
		return nil, err
	}
	return toLogEntries(rows)
}

// PruneLogs deletes every log older than the given cutoff (unix millis) and
// returns how many rows were removed.
func (db *DB) PruneLogs(before int64) (int64, error) {
	return db.q.DeleteLogsBefore(context.Background(), before)
}

func toLogEntries(rows []dbgen.ProcessLog) ([]*model.LogEntry, error) {
	out := make([]*model.LogEntry, len(rows))
	for i, r := range rows {
		e, err := toLogEntry(r)
		if err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

func toLogEntry(r dbgen.ProcessLog) (*model.LogEntry, error) {
	e := &model.LogEntry{
		ID:         r.ID,
		InstanceID: r.InstanceID,
		RootID:     r.RootID,
		Level:      model.LogLevel(r.Level),
		Event:      r.Event,
		StepID:     r.StepID,
		Message:    r.Message,
		Code:       r.Code,
		CreatedAt:  toTime(r.CreatedAt),
	}
	if r.Detail != "" && r.Detail != "{}" {
		if err := json.Unmarshal([]byte(r.Detail), &e.Detail); err != nil {
			return nil, err
		}
	}
	return e, nil
}
