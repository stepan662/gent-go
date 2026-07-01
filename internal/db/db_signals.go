package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	dbgen "genroc/internal/db/gen"
	"genroc/internal/model"
)

// ArmExternalOrConsumeSignal is the engine's atomic entry into an external task. Under
// the instance row lock (which it shares with DeliverSignal, so the two never interleave)
// it either:
//
//   - consumes the oldest buffered signal for (instance, task) if one is waiting — writing
//     it as _external_result so advance() resumes immediately, and KEEPING this worker's
//     lease so the instance is never claimable mid-advance (the engine's own progress write
//     at the end of the call releases the lease); or
//   - parks the instance (wait_state='external', the per-occurrence token + input snapshot
//     in _external) and RELEASES the lease — the engine then returns outcomeNoop, exactly
//     like a child spawn parking its parent.
//
// Popping the signal and writing its result is one commit, so a crash after this returns
// (but before the engine's progress write) still resumes via runExternal phase 2 — the
// signal is never lost. Mirrors the two-writer coordination of FinishChild / SpawnChildren.
func (db *DB) ArmExternalOrConsumeSignal(ctx context.Context, inst *model.ProcessInstance, taskID, token string, input any, wakeAt *time.Time) (consumed bool, result any, err error) {
	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback()

	lock := ""
	if db.dialect == "postgres" {
		lock = " FOR UPDATE"
	}
	// Take the instance row lock first — the same lock DeliverSignal takes — so a signal
	// arriving during arming serializes either fully before (we pop it) or fully after
	// (it finds us parked and resolves directly). No lost signal, no deadlock. The FOR
	// UPDATE makes this read hand-written; everything else goes through sqlc.
	var one int
	switch err := raw.QueryRowContext(ctx, `SELECT 1 FROM process_instances WHERE id = ?`+lock, inst.ID).Scan(&one); err {
	case nil:
	case sql.ErrNoRows:
		return false, nil, fmt.Errorf("instance not found")
	default:
		return false, nil, fmt.Errorf("lock instance: %w", err)
	}

	// Pop the oldest buffered signal for this (instance, task), if any.
	resultStr, popErr := qtx.PopOldestSignal(ctx, dbgen.PopOldestSignalParams{InstanceID: inst.ID, TaskID: taskID})
	if popErr != nil && popErr != sql.ErrNoRows {
		return false, nil, fmt.Errorf("pop signal: %w", popErr)
	}

	now := nowMillis()

	if popErr == nil {
		// A buffered signal was waiting: consume it now. SetExternalResult writes the result
		// durably but leaves worker_id/lease untouched, so this worker keeps the lease and
		// the instance stays non-claimable until the engine finishes advancing and releases it.
		var p any
		if err := json.Unmarshal([]byte(resultStr), &p); err != nil {
			return false, nil, fmt.Errorf("decode buffered signal: %w", err)
		}
		cd := cloneContext(inst.ContextData)
		cd[model.CtxExternalResult] = p
		delete(cd, model.CtxExternal)
		extData, err := encodeExternalData(cd)
		if err != nil {
			return false, nil, err
		}
		if err := qtx.SetExternalResult(ctx, dbgen.SetExternalResultParams{
			ExternalData: extData,
			UpdatedAt:    now,
			ID:           inst.ID,
		}); err != nil {
			return false, nil, fmt.Errorf("consume buffered signal: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, nil, err
		}
		return true, p, nil
	}

	// No buffered signal: park. Snapshot the input + per-occurrence token under _external;
	// UpdateInstance writes the parked state and clears worker_id/lease (the parked instance
	// is non-runnable, so the engine returns noop).
	inst.ContextData[model.CtxExternal] = map[string]any{"task_id": taskID, "token": token, "input": input}
	delete(inst.ContextData, model.CtxExternalResult)
	inst.WaitState = model.WaitStateExternal
	inst.WakeAt = wakeAt
	cols, err := db.persistContext(ctx, qtx, inst, now)
	if err != nil {
		return false, nil, err
	}
	params, err := updateInstanceParams(inst, cols, now)
	if err != nil {
		return false, nil, err
	}
	if err := qtx.UpdateInstance(ctx, params); err != nil {
		return false, nil, fmt.Errorf("park external: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return false, nil, nil
}

// DeliverSignal delivers a signal addressed to (instance, external task). Under the
// instance row lock it resolves the task immediately when it is armed right now (and not
// mid-timeout-claim), otherwise it buffers the result FIFO for the task's next arming.
// Returns delivered=true when it resolved immediately, false when it was buffered. The
// caller validates the result against the task's result_schema before calling.
func (db *DB) DeliverSignal(ctx context.Context, instanceID, taskID, signalID string, result any) (delivered bool, err error) {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return false, fmt.Errorf("marshal result: %w", err)
	}

	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	lock := ""
	if db.dialect == "postgres" {
		lock = " FOR UPDATE"
	}
	var status, waitState, currentTask, externalData string
	var workerID sql.NullString
	var leaseExpiresAt sql.NullInt64
	switch err := raw.QueryRowContext(ctx,
		`SELECT status, wait_state, task, external_data, worker_id, lease_expires_at
		   FROM process_instances WHERE id = ?`+lock, instanceID).
		Scan(&status, &waitState, &currentTask, &externalData, &workerID, &leaseExpiresAt); err {
	case nil:
	case sql.ErrNoRows:
		return false, fmt.Errorf("instance not found")
	default:
		return false, fmt.Errorf("lock instance: %w", err)
	}
	if status != string(model.StatusRunning) {
		return false, fmt.Errorf("instance is not running (status %s); cannot signal", status)
	}

	// The `task` column is the current task id, so the instance is armed for this
	// signal iff it is parked on an external wait at exactly that task.
	armed := model.WaitState(waitState) == model.WaitStateExternal && currentTask == taskID
	// A live lease means a worker is mid-advance on this row (a timeout firing); don't race
	// it — buffer instead, and the signal is consumed if the task re-arms.
	liveLeased := workerID.Valid && leaseExpiresAt.Valid && leaseExpiresAt.Int64 > nowMillis()

	if armed && !liveLeased {
		newExt, err := withExternalResult(externalData, result)
		if err != nil {
			return false, err
		}
		// armed/lease checked above under the row lock, so the un-park is unconditional.
		if err := qtx.SetExternalResult(ctx, dbgen.SetExternalResultParams{
			ExternalData: newExt,
			UpdatedAt:    nowMillis(),
			ID:           instanceID,
		}); err != nil {
			return false, fmt.Errorf("deliver signal: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}

	if err := qtx.InsertSignal(ctx, dbgen.InsertSignalParams{
		ID:         signalID,
		InstanceID: instanceID,
		TaskID:     taskID,
		Result:     string(resultJSON),
		CreatedAt:  nowMillis(),
	}); err != nil {
		return false, fmt.Errorf("buffer signal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

// CountBufferedSignals returns how many signals are buffered for (instance, task).
// Used by tests and observability.
func (db *DB) CountBufferedSignals(instanceID, taskID string) (int, error) {
	n, err := db.q.CountBufferedSignals(context.Background(), dbgen.CountBufferedSignalsParams{
		InstanceID: instanceID,
		TaskID:     taskID,
	})
	return int(n), err
}

// cloneContext returns a shallow copy of a context map, so callers can add/remove a
// top-level key for a DB write without mutating the engine's in-memory instance.
func cloneContext(m map[string]any) map[string]any {
	c := make(map[string]any, len(m)+1)
	for k, v := range m {
		c[k] = v
	}
	return c
}
