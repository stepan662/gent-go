package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

// renewChunkSize bounds how many leases a single renewal transaction touches.
// Small chunks keep each transaction's lock set tiny, so a row locked by an
// in-flight advance stalls only its chunk rather than every lease at once (a
// single bulk UPDATE would block all renewals behind one contended row).
const renewChunkSize = 100

// RenewWorkerLeases re-stamps all of this worker's leases to now+leaseDur, in
// small chunks (soonest-to-expire first). Each chunk is its own transaction, so
// renewals make progress even while in-flight advances hold row locks.
func (db *DB) RenewWorkerLeases(workerID string, leaseDur time.Duration) error {
	newExpiry := sql.NullInt64{Int64: nowMillis() + leaseDur.Milliseconds(), Valid: true}
	worker := sql.NullString{String: workerID, Valid: true}
	for {
		n, err := db.q.RenewWorkerLeasesChunk(context.Background(), dbgen.RenewWorkerLeasesChunkParams{
			NewExpiry: newExpiry,
			WorkerID:  worker,
			ChunkSize: renewChunkSize,
		})
		if err != nil {
			return err
		}
		// Fewer than a full chunk renewed → no eligible leases remain. Renewed rows
		// are stamped to newExpiry, so they no longer match the chunk's predicate;
		// the eligible set shrinks each pass, guaranteeing termination.
		if n < renewChunkSize {
			return nil
		}
	}
}

// ClaimInstances atomically leases up to limit runnable instances to workerID.
// PostgreSQL appends FOR UPDATE SKIP LOCKED so concurrent workers never block on
// each other; SQLite's single-writer model needs no such clause. db.exec rewrites
// the ? placeholders to $N on Postgres.
//
// wait_state <> 'waiting' excludes parents suspended for children.
// Both ” (none) and 'collecting' are claimable.
func (db *DB) ClaimInstances(workerID string, leaseDur time.Duration, limit int) ([]*model.ProcessInstance, error) {
	now := nowMillis()
	leaseExpiry := now + leaseDur.Milliseconds()

	ctx := context.Background()

	// Shared claimable predicate. The two `?` are both `now` (retry timer, lease expiry).
	//
	// A doomed instance ('failing'/'cancelling') is drained immediately, ignoring
	// wake_at: it will never run its pending task again, so there is no point
	// waiting out a delay or retry-backoff timer before settling it. Only a healthy
	// 'running' instance honours its timer. This is what lets a cancel take effect
	// promptly on an instance parked in a delay, without mutating wake_at.
	const where = `status IN ('running', 'failing', 'cancelling')
			  AND wait_state <> 'waiting'
			  AND (status IN ('failing', 'cancelling') OR wake_at IS NULL OR wake_at <= ?)
			  AND (worker_id IS NULL OR lease_expires_at <= ?)`

	if db.dialect == "postgres" {
		// One statement: a CTE captures the prior worker_id (to flag lease takeovers)
		// and FOR UPDATE SKIP LOCKED lets concurrent workers avoid blocking.
		query := `
			WITH cand AS (
				SELECT id AS cand_id, worker_id AS prev_worker
				FROM process_instances
				WHERE ` + where + `
				ORDER BY created_at ASC, id ASC
				LIMIT ? FOR UPDATE SKIP LOCKED
			)
			UPDATE process_instances
			SET worker_id = ?, lease_expires_at = ?
			FROM cand
			WHERE process_instances.id = cand.cand_id
			RETURNING ` + instanceColumns + `, cand.prev_worker`

		rows, err := db.exec.QueryContext(ctx, query, now, now, limit, workerID, leaseExpiry)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []*model.ProcessInstance
		for rows.Next() {
			var r dbgen.ProcessInstance
			var prevWorker sql.NullString
			if err := rows.Scan(
				&r.ID, &r.ProcessName, &r.ProcessVersion,
				&r.TaskQueue, &r.ContextData, &r.ParentID,
				&r.CallStack, &r.RetryCount, &r.WakeAt,
				&r.Status, &r.Error, &r.CreatedAt, &r.UpdatedAt,
				&r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
				&prevWorker,
			); err != nil {
				return nil, err
			}
			inst, err := toInstance(r)
			if err != nil {
				return nil, err
			}
			inst.ReclaimedExpired = prevWorker.Valid && prevWorker.String != ""
			result = append(result, inst)
		}
		return result, rows.Err()
	}

	// SQLite can't reference a FROM table in RETURNING, so it selects-then-updates
	// in one transaction. Its single-writer model makes that atomic (no FOR UPDATE);
	// the selected worker_id is the prior owner, before we overwrite it.
	tx, _, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	selectQ := `SELECT ` + instanceColumns + `
		FROM process_instances
		WHERE ` + where + `
		ORDER BY created_at ASC, id ASC
		LIMIT ?`
	rows, err := raw.QueryContext(ctx, selectQ, now, now, limit)
	if err != nil {
		return nil, err
	}
	var result []*model.ProcessInstance
	ids := make([]string, 0, limit)
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		inst, err := toInstance(r)
		if err != nil {
			rows.Close()
			return nil, err
		}
		inst.ReclaimedExpired = inst.WorkerID != nil // prior worker present => takeover
		result = append(result, inst)
		ids = append(ids, inst.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // must close the cursor before the UPDATE on the single connection
	if len(result) == 0 {
		return nil, tx.Commit()
	}

	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE process_instances SET worker_id = ?, lease_expires_at = ?
		 WHERE id IN (SELECT value FROM json_each(?))`,
		workerID, leaseExpiry, string(idsJSON)); err != nil {
		return nil, err
	}

	// Reflect the new lease state on the returned instances.
	newLease := toTime(leaseExpiry)
	w := workerID
	for _, inst := range result {
		inst.WorkerID = &w
		inst.LeaseExpiresAt = &newLease
	}
	return result, tx.Commit()
}
