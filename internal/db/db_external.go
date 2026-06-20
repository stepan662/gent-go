package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

// externalPaginator is the pagination policy for the external-task queue.
// baseWhere keeps wait_state='external' a literal so Postgres matches the partial
// idx_external_queue index (a bound parameter would not); that index's trailing
// updated_at column also backs the sort. Keys on park time (updated_at) with the
// UUIDv7 id tiebreaker, oldest first.
var externalPaginator = paginator{
	table:      "process_instances",
	columns:    instanceColumns,
	baseWhere:  "wait_state = 'external'",
	filterCols: []string{"process_name", "process_version"},
	sorts: map[string]sortMode{
		"updated": {{"updated_at", kindInt}, {"id", kindText}},
	},
	defSort:  "updated",
	defDesc:  false,
	defLimit: 20,
	maxLimit: 100,
}

// ListExternalTasks returns a page of instances parked on an external task,
// filtered by process name/version (empty/0 = any), sorted and paged per pg. It
// returns the page and the cursor for the next page ("" on the final page).
//
// task_id filtering is left to the caller (it lives in the JSON task queue, not a
// column), so a caller post-filtering task_id may see fewer than the page size
// even when more rows match — the returned cursor still advances correctly.
func (db *DB) ListExternalTasks(processName string, processVersion int, req PageReq) ([]*model.ProcessInstance, PageInfo, error) {
	b, err := externalPaginator.query(req).
		EqIf("process_name", processName, processName != "").
		EqIf("process_version", int64(processVersion), processVersion != 0).
		build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return db.queryInstancePage(b)
}

// ResolveExternalTask atomically delivers a submitted result to an instance parked on
// an external task and un-parks it so the engine resumes. It is the single place the
// resolve API writes instance state, and it must stay mutually exclusive with the
// engine (a timeout claim), a concurrent cancel, and a double/stale submit:
//
//   - The row is locked (FOR UPDATE on Postgres; SQLite serialises via its single
//     writer) so the read-then-write is atomic.
//   - status='running' AND wait_state='external' rejects already-resolved, timed-out,
//     cancelled, or not-yet-parked instances.
//   - The live-lease guard (worker_id set with an unexpired lease) rejects a submit that
//     races a timeout claim already in flight on a worker — the timeout wins.
//   - context._external.token == token rejects a stale token from a prior arming (e.g. a
//     looping task that re-armed) — the exact-occurrence guarantee.
//
// The result is stored under _external_result; the engine consumes it on the next claim
// (runExternal phase 2). Returns a descriptive error when the task is no longer waiting.
func (db *DB) ResolveExternalTask(ctx context.Context, instanceID, token string, result any) error {
	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lock := ""
	if db.dialect == "postgres" {
		lock = " FOR UPDATE"
	}

	var status, waitState, contextData string
	var workerID sql.NullString
	var leaseExpiresAt sql.NullInt64
	err = raw.QueryRowContext(ctx,
		`SELECT status, wait_state, context_data, worker_id, lease_expires_at
		   FROM process_instances WHERE id = ?`+lock, instanceID).
		Scan(&status, &waitState, &contextData, &workerID, &leaseExpiresAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("external task not found")
	}
	if err != nil {
		return fmt.Errorf("lock instance: %w", err)
	}

	if status != string(model.StatusRunning) || model.WaitState(waitState) != model.WaitStateExternal {
		return fmt.Errorf("task is not waiting for an external result")
	}
	// A live lease means a worker already claimed this instance (a timeout firing); the
	// timeout wins, so reject the submit rather than racing its advance.
	if workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis() {
		return fmt.Errorf("external task is being processed; try again")
	}

	var cd map[string]any
	if err := json.Unmarshal([]byte(contextData), &cd); err != nil {
		return fmt.Errorf("unmarshal context: %w", err)
	}
	ext, _ := cd[model.CtxExternal].(map[string]any)
	storedToken, _ := ext["token"].(string)
	if storedToken == "" || storedToken != token {
		return fmt.Errorf("token does not match the waiting task (it may have already been resolved or re-armed)")
	}

	cd[model.CtxExternalResult] = result
	newCtx, err := json.Marshal(cd)
	if err != nil {
		return fmt.Errorf("marshal context: %w", err)
	}
	// The status/wait_state/token/lease checks above ran under the row lock, so the
	// un-park is unconditional here.
	if err := qtx.SetExternalResult(ctx, dbgen.SetExternalResultParams{
		ContextData: string(newCtx),
		UpdatedAt:   nowMillis(),
		ID:          instanceID,
	}); err != nil {
		return fmt.Errorf("resolve external task: %w", err)
	}
	return tx.Commit()
}
