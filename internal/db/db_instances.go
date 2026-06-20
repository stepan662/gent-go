package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

// instancePaginator is the pagination policy for ListInstances. Only index-backed
// sorts are offered: created keys on (created_at, id) — backed by
// idx_instances_status_created_at and the UUIDv7 PK tiebreaker. Add more sort keys
// here (with a matching index) to extend.
var instancePaginator = paginator{
	table:   "process_instances",
	columns: instanceColumns,
	sorts: map[string]sortMode{
		"created": {{"created_at", kindInt}, {"id", kindText}},
	},
	filterCols: []string{"status"},
	defSort:    "created",
	defDesc:    true, // newest first, preserving the previous fixed order
	defLimit:   20,
	maxLimit:   100,
}

// instanceCursorVals returns the active sort mode's key-column values for inst,
// matching instancePaginator's column order for that mode.
func instanceCursorVals(sort string, inst *model.ProcessInstance) []any {
	switch sort {
	case "updated": // external-task queue
		return []any{inst.UpdatedAt.UnixMilli(), inst.ID}
	default: // created
		return []any{inst.CreatedAt.UnixMilli(), inst.ID}
	}
}

// instanceColumns is the full process_instances column list, in the order
// scanInstance reads them. Shared by the hand-written ClaimInstances and
// RetryProcess queries so adding a column touches one place.
const instanceColumns = `id, process_name, process_version, task_queue, context_data, parent_id,
	call_stack, retry_count, wake_at, status, error,
	created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id`

// scanInstance reads one process_instances row (in instanceColumns order) from a
// *sql.Row or *sql.Rows.
func scanInstance(s interface{ Scan(...any) error }) (dbgen.ProcessInstance, error) {
	var r dbgen.ProcessInstance
	err := s.Scan(
		&r.ID, &r.ProcessName, &r.ProcessVersion,
		&r.TaskQueue, &r.ContextData, &r.ParentID,
		&r.CallStack, &r.RetryCount, &r.WakeAt,
		&r.Status, &r.Error, &r.CreatedAt, &r.UpdatedAt,
		&r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
	)
	return r, err
}

// marshalInstanceState serialises the two JSON blobs every instance write needs.
func marshalInstanceState(inst *model.ProcessInstance) (taskQueue, contextData string, err error) {
	queue, err := json.Marshal(inst.TaskQueue)
	if err != nil {
		return "", "", err
	}
	ctx, err := json.Marshal(inst.ContextData)
	if err != nil {
		return "", "", err
	}
	return string(queue), string(ctx), nil
}

// updateInstanceParams builds the params for the UpdateInstance query from inst,
// stamping updated_at with now.
func updateInstanceParams(inst *model.ProcessInstance, now int64) (dbgen.UpdateInstanceParams, error) {
	queue, ctx, err := marshalInstanceState(inst)
	if err != nil {
		return dbgen.UpdateInstanceParams{}, err
	}
	return dbgen.UpdateInstanceParams{
		ID:          inst.ID,
		TaskQueue:   queue,
		ContextData: ctx,
		RetryCount:  int64(inst.RetryCount),
		WakeAt: fromTimePtr(inst.WakeAt),
		Status:      string(inst.Status),
		WaitState:   string(inst.WaitState),
		Error:       inst.Error,
		UpdatedAt:   now,
	}, nil
}

// insertInstanceParams builds the params for the InsertInstance query. status is
// passed explicitly so callers can override it (e.g. spawned children inherit the
// parent's status); created/updated timestamps are passed for the same reason.
func insertInstanceParams(inst *model.ProcessInstance, status string, createdAt, updatedAt int64) (dbgen.InsertInstanceParams, error) {
	queue, ctx, err := marshalInstanceState(inst)
	if err != nil {
		return dbgen.InsertInstanceParams{}, err
	}
	callStack, err := json.Marshal(inst.CallStack)
	if err != nil {
		return dbgen.InsertInstanceParams{}, err
	}
	return dbgen.InsertInstanceParams{
		ID:             inst.ID,
		ProcessName:    inst.ProcessName,
		ProcessVersion: int64(inst.ProcessVersion),
		TaskQueue:      queue,
		ContextData:    ctx,
		ParentID:       inst.ParentID,
		SpawnTaskID:    inst.SpawnTaskID,
		CallStack:      string(callStack),
		RetryCount:     int64(inst.RetryCount),
		WakeAt:    fromTimePtr(inst.WakeAt),
		Status:         status,
		WaitState:      string(inst.WaitState),
		Error:          inst.Error,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	}, nil
}

func (db *DB) SaveInstance(inst *model.ProcessInstance) error {
	now := nowMillis()
	params, err := insertInstanceParams(inst, string(inst.Status), now, now)
	if err != nil {
		return err
	}
	return db.q.InsertInstance(context.Background(), params)
}

func (db *DB) UpdateInstance(inst *model.ProcessInstance) error {
	params, err := updateInstanceParams(inst, nowMillis())
	if err != nil {
		return err
	}
	return db.q.UpdateInstance(context.Background(), params)
}

// UpdateInstanceProgress writes the mutable task state (queue, context, retry
// counters, wait_state) without touching status or error. Used after a task
// completes mid-process so that a concurrent CancelProcess or FailAncestors
// result is preserved in the DB for the next tick. wait_state IS written: it is
// owned exclusively by the lease-holding worker (SetParentCollecting only fires
// while the DB row says 'waiting', which is never the case mid-claim), and the
// post-collect reset to ” must be persisted or the stale 'collecting' would
// make the next spawn task skip phase 1 entirely.
func (db *DB) UpdateInstanceProgress(inst *model.ProcessInstance) error {
	queue, ctx, err := marshalInstanceState(inst)
	if err != nil {
		return err
	}
	return db.q.UpdateInstanceProgress(context.Background(), dbgen.UpdateInstanceProgressParams{
		ID:          inst.ID,
		TaskQueue:   queue,
		ContextData: ctx,
		RetryCount:  int64(inst.RetryCount),
		WakeAt: fromTimePtr(inst.WakeAt),
		WaitState:   string(inst.WaitState),
		UpdatedAt:   nowMillis(),
	})
}

func (db *DB) GetInstance(id string) (*model.ProcessInstance, error) {
	r, err := db.q.GetInstance(context.Background(), id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("instance not found")
	}
	if err != nil {
		return nil, err
	}
	return toInstance(r)
}

// ListInstances returns a page of instances, optionally filtered by status
// (empty = all), sorted and paged per req. It returns the page items and the
// navigation metadata (before/after counts and cursors).
func (db *DB) ListInstances(status string, req PageReq) ([]*model.ProcessInstance, PageInfo, error) {
	b, err := instancePaginator.query(req).EqIf("status", status, status != "").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	return db.queryInstancePage(b)
}

// queryInstancePage runs a built instance-listing query (page + count) and returns
// the scanned page plus its PageInfo. Shared by ListInstances and
// ListExternalTasks, which select the same columns and cursor on the same keys.
func (db *DB) queryInstancePage(b built) ([]*model.ProcessInstance, PageInfo, error) {
	rows, err := db.exec.QueryContext(context.Background(), b.pageSQL, b.pageArgs...)
	if err != nil {
		return nil, PageInfo{}, err
	}
	defer rows.Close()
	var out []*model.ProcessInstance
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			return nil, PageInfo{}, err
		}
		inst, err := toInstance(r)
		if err != nil {
			return nil, PageInfo{}, err
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, PageInfo{}, err
	}
	items, first, last := orient(b, out, instanceCursorVals)
	info, err := db.pageInfo(b, first, last)
	if err != nil {
		return nil, PageInfo{}, err
	}
	return items, info, nil
}

// ChildrenForTask returns all child instances spawned by the given task of a
// parent, as model instances. Used by the engine to collect child outputs.
func (db *DB) ChildrenForTask(ctx context.Context, parentID, spawnTaskID string) ([]*model.ProcessInstance, error) {
	rows, err := db.q.GetChildrenForTask(ctx, dbgen.GetChildrenForTaskParams{
		ParentID:    parentID,
		SpawnTaskID: spawnTaskID,
	})
	if err != nil {
		return nil, fmt.Errorf("get children for task: %w", err)
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

// ── row → model conversion ────────────────────────────────────────────────────

func toInstance(r dbgen.ProcessInstance) (*model.ProcessInstance, error) {
	inst := &model.ProcessInstance{
		ID:             r.ID,
		ProcessName:    r.ProcessName,
		ProcessVersion: int(r.ProcessVersion),
		ParentID:       r.ParentID,
		SpawnTaskID:    r.SpawnTaskID,
		RetryCount:     int(r.RetryCount),
		Status:         model.Status(r.Status),
		WaitState:      model.WaitState(r.WaitState),
		Error:          r.Error,
		CreatedAt:      toTime(r.CreatedAt),
		UpdatedAt:      toTime(r.UpdatedAt),
		WakeAt:    toTimePtr(r.WakeAt),
		WorkerID:       nullStringPtr(r.WorkerID),
		LeaseExpiresAt: toTimePtr(r.LeaseExpiresAt),
	}
	if err := json.Unmarshal([]byte(r.TaskQueue), &inst.TaskQueue); err != nil {
		return nil, fmt.Errorf("unmarshal task_queue: %w", err)
	}
	if err := json.Unmarshal([]byte(r.ContextData), &inst.ContextData); err != nil {
		return nil, fmt.Errorf("unmarshal context_data: %w", err)
	}
	if err := json.Unmarshal([]byte(r.CallStack), &inst.CallStack); err != nil {
		return nil, fmt.Errorf("unmarshal call_stack: %w", err)
	}
	return inst, nil
}
