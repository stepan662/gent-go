package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

// ListExternalTasks returns instances parked on an external task, filtered by process
// name/version (empty/0 = any) and capped at limit, ordered by park time. task_id
// filtering is left to the caller (it lives in the JSON task queue, not a column).
func (db *DB) ListExternalTasks(processName string, processVersion, limit int) ([]*model.ProcessInstance, error) {
	rows, err := db.q.ListExternalTasks(context.Background(), dbgen.ListExternalTasksParams{
		ProcessName:    processName,
		ProcessVersion: int64(processVersion),
		Lim:            int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]*model.ProcessInstance, len(rows))
	for i, r := range rows {
		inst, err := toInstance(r)
		if err != nil {
			return nil, err
		}
		out[i] = inst
	}
	return out, nil
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
	tx, _, raw, err := db.beginTx(ctx, nil)
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

	// The conditional WHERE re-asserts the parked state under the lock; with FOR UPDATE
	// held nothing can have changed, so 0 rows is purely defensive.
	res, err := raw.ExecContext(ctx,
		`UPDATE process_instances
		    SET context_data = ?, wait_state = '', wake_at = NULL, updated_at = ?
		  WHERE id = ? AND status = 'running' AND wait_state = 'external'`,
		string(newCtx), nowMillis(), instanceID)
	if err != nil {
		return fmt.Errorf("resolve external task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task is not waiting for an external result")
	}

	return tx.Commit()
}
