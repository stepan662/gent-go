package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

// instanceColumns is the full process_instances column list, in the order
// scanInstance reads them. Shared by the hand-written ClaimInstances and
// RetryProcess queries so adding a column touches one place.
const instanceColumns = `id, process_name, process_version, step_queue, context_data, parent_id,
	call_stack, retry_count, wake_at, status, error,
	created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_step_id`

// scanInstance reads one process_instances row (in instanceColumns order) from a
// *sql.Row or *sql.Rows.
func scanInstance(s interface{ Scan(...any) error }) (dbgen.ProcessInstance, error) {
	var r dbgen.ProcessInstance
	err := s.Scan(
		&r.ID, &r.ProcessName, &r.ProcessVersion,
		&r.StepQueue, &r.ContextData, &r.ParentID,
		&r.CallStack, &r.RetryCount, &r.WakeAt,
		&r.Status, &r.Error, &r.CreatedAt, &r.UpdatedAt,
		&r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnStepID,
	)
	return r, err
}

// marshalInstanceState serialises the two JSON blobs every instance write needs.
func marshalInstanceState(inst *model.ProcessInstance) (stepQueue, contextData string, err error) {
	queue, err := json.Marshal(inst.StepQueue)
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
		StepQueue:   queue,
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
		StepQueue:      queue,
		ContextData:    ctx,
		ParentID:       inst.ParentID,
		SpawnStepID:    inst.SpawnStepID,
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

// UpdateInstanceProgress writes the mutable step state (queue, context, retry
// counters, wait_state) without touching status or error. Used after a step
// completes mid-process so that a concurrent CancelProcess or FailAncestors
// result is preserved in the DB for the next tick. wait_state IS written: it is
// owned exclusively by the lease-holding worker (SetParentCollecting only fires
// while the DB row says 'waiting', which is never the case mid-claim), and the
// post-collect reset to ” must be persisted or the stale 'collecting' would
// make the next spawn step skip phase 1 entirely.
func (db *DB) UpdateInstanceProgress(inst *model.ProcessInstance) error {
	queue, ctx, err := marshalInstanceState(inst)
	if err != nil {
		return err
	}
	return db.q.UpdateInstanceProgress(context.Background(), dbgen.UpdateInstanceProgressParams{
		ID:          inst.ID,
		StepQueue:   queue,
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

func (db *DB) ListInstances(status string) ([]*model.ProcessInstance, error) {
	rows, err := db.q.ListInstances(context.Background(), status)
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

// ChildrenForStep returns all child instances spawned by the given step of a
// parent, as model instances. Used by the engine to collect child outputs.
func (db *DB) ChildrenForStep(ctx context.Context, parentID, spawnStepID string) ([]*model.ProcessInstance, error) {
	rows, err := db.q.GetChildrenForStep(ctx, dbgen.GetChildrenForStepParams{
		ParentID:    parentID,
		SpawnStepID: spawnStepID,
	})
	if err != nil {
		return nil, fmt.Errorf("get children for step: %w", err)
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
		SpawnStepID:    r.SpawnStepID,
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
	if err := json.Unmarshal([]byte(r.StepQueue), &inst.StepQueue); err != nil {
		return nil, fmt.Errorf("unmarshal step_queue: %w", err)
	}
	if err := json.Unmarshal([]byte(r.ContextData), &inst.ContextData); err != nil {
		return nil, fmt.Errorf("unmarshal context_data: %w", err)
	}
	if err := json.Unmarshal([]byte(r.CallStack), &inst.CallStack); err != nil {
		return nil, fmt.Errorf("unmarshal call_stack: %w", err)
	}
	return inst, nil
}
