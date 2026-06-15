package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

// FinishChild atomically saves the child as terminal and, if all siblings are
// now done, wakes the waiting parent. A healthy (running) parent wakes to
// 'collecting' — it will actually merge child outputs; a draining
// (failing/cancelling) parent wakes to ” and just settles. 'collecting'
// therefore strictly means "all children completed, outputs will be merged".
//
// The parent row is locked first (FOR UPDATE on PostgreSQL) to prevent race conditions
// between concurrent sibling completions. SQLite serialises naturally via single-writer.
//
// For root instances (no parent), only the child save is performed.
// For failed children, use FailAncestors instead; FinishChild is only for
// completed/cancelled terminal states.
func (db *DB) FinishChild(child *model.ProcessInstance) error {
	if child.ParentID == "" {
		return db.UpdateInstance(child)
	}

	ctx := context.Background()
	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Acquire row locks (oldest-first) and read the parent's wait_state in one shot.
	// The locking CTE keeps the same lock order as CancelProcess and
	// FailInstanceAndAncestors, preventing deadlocks; the FOR UPDATE is appended only
	// on PostgreSQL — SQLite serialises via its single writer and runs the CTE without it.
	lock := ""
	if db.dialect == "postgres" {
		lock = " FOR UPDATE"
	}
	var parentWaitState string
	err = raw.QueryRowContext(ctx, `
		WITH locked AS (
			SELECT id, wait_state FROM process_instances
			WHERE id IN (?, ?)
			ORDER BY created_at, id`+lock+`
		)
		SELECT wait_state FROM locked WHERE id = ?`,
		child.ID, child.ParentID, child.ParentID).Scan(&parentWaitState)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lock parent: %w", err)
	}
	parentFound := err == nil

	// Save child as terminal.
	childParams, err := updateInstanceParams(child, nowMillis())
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstance(ctx, childParams); err != nil {
		return fmt.Errorf("save child: %w", err)
	}

	// If the parent was found and is waiting, check whether all siblings are terminal.
	if parentFound && model.WaitState(parentWaitState) == model.WaitStateWaiting {
		active, err := qtx.CountActiveSiblings(ctx, dbgen.CountActiveSiblingsParams{
			ParentID:    child.ParentID,
			SpawnStepID: child.SpawnStepID,
		})
		if err != nil {
			return fmt.Errorf("count siblings: %w", err)
		}
		if active == 0 {
			if err := qtx.WakeParent(ctx, dbgen.WakeParentParams{
				ID:        child.ParentID,
				UpdatedAt: nowMillis(),
			}); err != nil {
				return fmt.Errorf("wake parent: %w", err)
			}
		}
	}

	return tx.Commit()
}

// FailInstanceAndAncestors atomically marks a child instance as failed,
// propagates 'failing' to all ancestors in its call stack, and — when the
// failed child was the last active member of its spawn batch — wakes the
// parent (to ”, the parent is failing by then) so the engine can settle it
// on the next tick. All in a single transaction; the safe replacement for
// calling UpdateInstance + FailAncestors separately.
func (db *DB) FailInstanceAndAncestors(child *model.ProcessInstance) error {
	ctx := context.Background()
	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowMillis()

	// Lock child and all ancestors oldest-first — consistent with FinishChild and
	// CancelProcess. This step exists only to take FOR UPDATE locks, so it runs on
	// PostgreSQL alone; SQLite serialises via its single writer. Ancestors are read
	// from the child's call_stack in the DB; no Go-side ID list needed.
	if db.dialect == "postgres" {
		lockRows, lockErr := raw.QueryContext(ctx, `
			SELECT id FROM process_instances
			WHERE id = ?
			   OR id IN (SELECT value FROM json_each(
			                 (SELECT call_stack FROM process_instances WHERE id = ?)))
			ORDER BY created_at, id FOR UPDATE`, child.ID, child.ID)
		if lockErr != nil {
			return fmt.Errorf("lock rows: %w", lockErr)
		}
		lockRows.Close()
	}

	childParams, err := updateInstanceParams(child, now)
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstance(ctx, childParams); err != nil {
		return err
	}

	// Bulk-mark all ancestors as failing in a single UPDATE via json_each.
	if len(child.CallStack) > 0 {
		idsJSON, err := json.Marshal(child.CallStack)
		if err != nil {
			return err
		}
		if err := qtx.FailAncestors(ctx, dbgen.FailAncestorsParams{
			Error:     child.Error,
			UpdatedAt: now,
			Ids:       string(idsJSON),
		}); err != nil {
			return err
		}
	}

	// If this failure settled the last active child of the batch, wake the
	// waiting parent (mirrors FinishChild) so the engine can claim it and
	// transition failing → failed. WakeParent picks '' here — the parent is
	// failing, so it must never enter the collect phase.
	if child.ParentID != "" {
		var parentWaitState string
		err := raw.QueryRowContext(ctx,
			"SELECT wait_state FROM process_instances WHERE id = ?",
			child.ParentID).Scan(&parentWaitState)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("read parent wait_state: %w", err)
		}
		if err == nil && model.WaitState(parentWaitState) == model.WaitStateWaiting {
			active, err := qtx.CountActiveSiblings(ctx, dbgen.CountActiveSiblingsParams{
				ParentID:    child.ParentID,
				SpawnStepID: child.SpawnStepID,
			})
			if err != nil {
				return fmt.Errorf("count siblings: %w", err)
			}
			if active == 0 {
				if err := qtx.WakeParent(ctx, dbgen.WakeParentParams{
					ID:        child.ParentID,
					UpdatedAt: now,
				}); err != nil {
					return fmt.Errorf("wake parent: %w", err)
				}
			}
		}
	}

	return tx.Commit()
}

// subtreeCTE is a `WITH RECURSIVE subtree(id) AS (...)` clause binding the CTE
// `subtree` to the instance with the bound id and every descendant, by walking
// process_instances.parent_id down (riding idx_instances_parent_step, indexed on
// both engines).
//
// The CTE is a plain SELECT and takes no row locks, so it can't deadlock. Callers
// that mutate the tree lock the enumerated rows in a SEPARATE step,
// ORDER BY created_at, id FOR UPDATE — the global order shared with FinishChild
// and FailInstanceAndAncestors, which is what prevents deadlocks. (Postgres also
// forbids FOR UPDATE inside a recursive CTE, so this split is mandatory.)
const subtreeCTE = `WITH RECURSIVE subtree(id) AS (
	SELECT id FROM process_instances WHERE id = ?
	UNION ALL
	SELECT pi.id FROM process_instances pi JOIN subtree s ON pi.parent_id = s.id
)`

// forUpdate is the lock clause appended to the subtree-locking SELECT on Postgres;
// SQLite serialises via its single writer and has no FOR UPDATE syntax.
func (db *DB) forUpdate() string {
	if db.dialect == "postgres" {
		return " FOR UPDATE"
	}
	return ""
}

// CancelProcess atomically marks an entire process tree as cancelling.
// It only accepts root instances — cancellation is a decision about the whole
// tree, so cancelling a descendant directly is rejected with the root's ID.
// All running instances of the tree (the root and every descendant) transition
// to 'cancelling'.
//
// The tree is enumerated with a recursive walk over parent_id (subtreeCTE). The
// locking CTE then takes every row lock in created_at, id order before the UPDATE
// — the same order as FinishChild and FailInstanceAndAncestors, eliminating
// deadlocks on PostgreSQL (SQLite serialises via its single writer; its lock
// clause is empty).
func (db *DB) CancelProcess(ctx context.Context, id string) error {
	row, err := db.q.GetInstance(ctx, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	if err := requireRoot(row, "cancel"); err != nil {
		return err
	}

	tx, _, exec, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The locked CTE pre-locks the subtree in created_at, id order before the UPDATE
	// (Postgres); on SQLite the ORDER BY is a harmless no-op (single writer, no
	// FOR UPDATE). One query for both engines — only the lock clause differs.
	if _, err := exec.ExecContext(ctx, subtreeCTE+`,
		locked AS (
			SELECT id FROM process_instances
			WHERE id IN (SELECT id FROM subtree)
			ORDER BY created_at, id`+db.forUpdate()+`
		)
		UPDATE process_instances SET status = 'cancelling', updated_at = ?
		WHERE id IN (SELECT id FROM locked) AND status = 'running'`,
		id, nowMillis()); err != nil {
		return fmt.Errorf("cancel process: %w", err)
	}

	return tx.Commit()
}

// requireRoot rejects operations on non-root instances, pointing the caller at
// the tree root (call_stack[0]) instead.
func requireRoot(row dbgen.ProcessInstance, op string) error {
	if row.ParentID == "" {
		return nil
	}
	var stack []string
	if err := json.Unmarshal([]byte(row.CallStack), &stack); err != nil || len(stack) == 0 {
		return fmt.Errorf("instance %q is not a root instance", row.ID)
	}
	return fmt.Errorf("instance %q is not a root instance; %s root instance %q instead", row.ID, op, stack[0])
}

// RetryProcess resumes a failed or cancelled root process from where its tree
// was interrupted. Failed/cancelled instances on the current execution path are
// revived in place: leaves re-run their pending step, parents are reconstructed
// as waiting (live children) or collecting (all children done). Completed work
// is never redone. force overrides the only_once protection on pending steps.
//
// Only root instances are accepted — like cancellation, retry is a decision
// about the whole tree. A root that is failed/cancelled implies the tree has
// fully settled (nodes only reach a terminal status once all their children
// are terminal); draining roots are rejected as failing/cancelling by the
// status check.
func (db *DB) RetryProcess(ctx context.Context, id string, force bool) error {
	rootRow, err := db.q.GetInstance(ctx, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	if err := requireRoot(rootRow, "retry"); err != nil {
		return err
	}
	status := model.Status(rootRow.Status)
	if status != model.StatusFailed && status != model.StatusCancelled {
		return fmt.Errorf("process is not retryable (status: %s)", status)
	}

	tx, qtx, exec, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock and load the whole tree (root + descendants) in created_at, id order —
	// the same lock order as CancelProcess/FinishChild/FailInstanceAndAncestors —
	// so concurrent cancels and child completions serialize against the revival.
	// The tree is enumerated with a recursive walk over parent_id (subtreeCTE); the
	// FOR UPDATE on the outer SELECT (Postgres only) locks the rows in that order.
	rows, err := exec.QueryContext(ctx, subtreeCTE+`
		SELECT `+instanceColumns+` FROM process_instances
		WHERE id IN (SELECT id FROM subtree)
		ORDER BY created_at, id`+db.forUpdate(), id)
	if err != nil {
		return fmt.Errorf("lock tree: %w", err)
	}
	defer rows.Close()

	nodes := make(map[string]*model.ProcessInstance)
	rawRows := make(map[string]dbgen.ProcessInstance)
	children := make(map[string]map[string][]*model.ProcessInstance) // parentID → spawnStepID → batch
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			return fmt.Errorf("scan tree row: %w", err)
		}
		inst, err := toInstance(r)
		if err != nil {
			return err
		}
		nodes[inst.ID] = inst
		rawRows[inst.ID] = r
		if inst.ParentID != "" {
			if children[inst.ParentID] == nil {
				children[inst.ParentID] = make(map[string][]*model.ProcessInstance)
			}
			children[inst.ParentID][inst.SpawnStepID] = append(children[inst.ParentID][inst.SpawnStepID], inst)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close() // release the connection for the updates below (SQLite single-conn)
	root, ok := nodes[id]
	if !ok {
		return fmt.Errorf("instance not found")
	}

	// Walk the tree top-down, reviving the interrupted path. Only the root and
	// the front-step children of revived nodes are visited, so completed steps
	// and finished side branches are never touched.
	var dirty []*model.ProcessInstance
	var revive func(node *model.ProcessInstance) error
	revive = func(node *model.ProcessInstance) error {
		switch node.Status {
		case model.StatusCompleted:
			return nil // finished work is kept
		case model.StatusRunning, model.StatusFailing, model.StatusCancelling:
			// Unreachable under a terminal root (the tree has settled);
			// kept as defense — a live node belongs to the engine.
			return nil
		}
		// node is failed or cancelled
		newWaitState := model.WaitStateNone
		if len(node.StepQueue) > 0 {
			front := node.StepQueue[0]
			kids := children[node.ID][front.ID]
			if len(kids) > 0 {
				// Interrupted inside this spawn step's wait/collect cycle —
				// revive the batch and reconstruct the wait state. (Kids exist
				// only for spawn steps: SpawnChildrenAndWait is atomic.)
				anyActive := false
				for _, k := range kids {
					if err := revive(k); err != nil {
						return err
					}
					if k.Status != model.StatusCompleted && k.Status != model.StatusFailed && k.Status != model.StatusCancelled {
						anyActive = true
					}
				}
				if anyActive {
					newWaitState = model.WaitStateWaiting
				} else {
					newWaitState = model.WaitStateCollecting // re-run the lost collect
				}
			} else if front.OnlyOnce != nil && *front.OnlyOnce && !force {
				// Reviving with wait_state none re-executes the front step.
				return fmt.Errorf("instance %q step %q is marked only_once and may have already been attempted; use force to override", node.ID, front.ID)
			}
		}
		// Empty queue: interrupted between the last step and the completed
		// write — advance() completes it on the next claim.
		node.Status = model.StatusRunning
		node.WaitState = newWaitState
		node.Error = ""
		node.RetryCount = 0
		node.NextRetryAt = nil
		dirty = append(dirty, node)
		return nil
	}
	if err := revive(root); err != nil {
		return err
	}

	now := nowMillis()
	for _, node := range dirty {
		raw := rawRows[node.ID]
		if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
			ID:          node.ID,
			StepQueue:   raw.StepQueue,
			ContextData: raw.ContextData,
			RetryCount:  0,
			NextRetryAt: sql.NullInt64{},
			Status:      string(node.Status),
			WaitState:   string(node.WaitState),
			Error:       "",
			UpdatedAt:   now,
		}); err != nil {
			return fmt.Errorf("revive instance %q: %w", node.ID, err)
		}
	}

	return tx.Commit()
}

// SpawnChildrenAndWait atomically inserts child instances and transitions the parent
// to wait_state='waiting'. Children inherit the parent's current status so that a
// concurrently-cancelled parent spawns cancelling children (they self-cancel and
// wake the parent via FinishChild).
// Zero children: no-op (parent continues without entering the waiting state).
// Hand-written because it requires coordinating multiple INSERTs with a parent update.
func (db *DB) SpawnChildrenAndWait(ctx context.Context, parent *model.ProcessInstance, children []*model.ProcessInstance) error {
	if len(children) == 0 {
		return nil
	}

	tx, qtx, raw, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock parent and read its current status to propagate to children. FOR UPDATE
	// is appended only on PostgreSQL; SQLite serialises via its single writer.
	lock := ""
	if db.dialect == "postgres" {
		lock = " FOR UPDATE"
	}
	var currentStatus, currentWaitState string
	if err := raw.QueryRowContext(ctx,
		`SELECT status, wait_state FROM process_instances WHERE id = ?`+lock,
		parent.ID).Scan(&currentStatus, &currentWaitState); err != nil {
		return fmt.Errorf("lock parent: %w", err)
	}
	if currentWaitState != "" {
		return fmt.Errorf("parent %q is already in wait_state %q", parent.ID, currentWaitState)
	}

	// Insert children with the parent's current status (propagates cancelling if needed).
	// Each child gets created_at = now+i so siblings have a strict ordering by definition
	// position — ClaimInstances (ORDER BY created_at) always processes them in spawn order.
	now := nowMillis()
	for i, child := range children {
		ts := now + int64(i)
		params, err := insertInstanceParams(child, currentStatus, ts, ts)
		if err != nil {
			return err
		}
		if err := qtx.InsertInstance(ctx, params); err != nil {
			return fmt.Errorf("insert child: %w", err)
		}
	}

	// Suspend parent: keep status, set wait_state='waiting'.
	parentQueue, parentCtx, err := marshalInstanceState(parent)
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
		ID:          parent.ID,
		StepQueue:   parentQueue,
		ContextData: parentCtx,
		RetryCount:  int64(parent.RetryCount),
		NextRetryAt: sql.NullInt64{},
		Status:      currentStatus,
		WaitState:   string(model.WaitStateWaiting),
		Error:       parent.Error,
		UpdatedAt:   now,
	}); err != nil {
		return fmt.Errorf("suspend parent: %w", err)
	}

	return tx.Commit()
}
