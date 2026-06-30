package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

// instancePaginator is the pagination policy for ListInstances. It selects only
// the summary columns (no context_data/task/call_stack — see
// instanceSummaryColumns) so listing many instances never fetches a potentially
// huge context. Two index-backed sorts: created keys on (created_at, id) and
// updated on (updated_at, id) — backed by idx_instances_updated_at — each with the
// UUIDv7 PK tiebreaker. The default is created: it is immutable, so a cursor walk
// stays stable even as the engine mutates rows; the CLI's "recent activity" view
// opts into updated explicitly. Add more sort keys here (with a matching index).
var instancePaginator = paginator{
	table:   "process_instances",
	columns: instanceSummaryColumns,
	sorts: map[string]sortMode{
		"created": {{"created_at", kindInt}, {"id", kindText}},
		"updated": {{"updated_at", kindInt}, {"id", kindText}},
	},
	filterCols: []string{"status"},
	defSort:    "created",
	defDesc:    true, // newest first
	defLimit:   20,
	maxLimit:   100,
}

// instanceCursorVals returns the active sort mode's key-column values for inst,
// matching externalPaginator's column order for that mode (the external-task queue
// keys on park time, updated_at).
func instanceCursorVals(sort string, inst *model.ProcessInstance) []any {
	switch sort {
	case "updated": // external-task queue
		return []any{inst.UpdatedAt.UnixMilli(), inst.ID}
	default: // created
		return []any{inst.CreatedAt.UnixMilli(), inst.ID}
	}
}

// instanceSummaryCursorVals is instanceCursorVals for the summary list path,
// matching instancePaginator's sort modes (updated / created).
func instanceSummaryCursorVals(sort string, s *model.InstanceSummary) []any {
	switch sort {
	case "created":
		return []any{s.CreatedAt.UnixMilli(), s.ID}
	default: // updated (default)
		return []any{s.UpdatedAt.UnixMilli(), s.ID}
	}
}

// instanceColumns is the full process_instances column list, in the order
// scanInstance reads them. Shared by the hand-written ClaimInstances and
// RetryProcess queries so adding a column touches one place.
const instanceColumns = `id, process_name, process_version, parent_id,
	call_stack, retry_count, wake_at, status, error,
	created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_task_id,
	input_data, outputs_data, output_data, error_data, external_data, engine_state, task`

// instanceSummaryColumns is the lightweight projection used by ListInstances: no
// context_data/task/call_stack, so a list never reads or unmarshals a
// potentially huge context blob. Order matches scanInstanceSummary.
const instanceSummaryColumns = `id, process_name, process_version, retry_count,
	status, wait_state, error, created_at, updated_at`

// scanInstanceSummary reads one process_instances row (in instanceSummaryColumns
// order) into a model.InstanceSummary.
func scanInstanceSummary(s interface{ Scan(...any) error }) (*model.InstanceSummary, error) {
	var (
		r                          model.InstanceSummary
		processVersion, retryCount int64
		status, waitState          string
		createdAt, updatedAt       int64
	)
	if err := s.Scan(
		&r.ID, &r.ProcessName, &processVersion, &retryCount,
		&status, &waitState, &r.Error, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	r.ProcessVersion = int(processVersion)
	r.RetryCount = int(retryCount)
	r.Status = model.Status(status)
	r.WaitState = model.WaitState(waitState)
	r.CreatedAt = toTime(createdAt)
	r.UpdatedAt = toTime(updatedAt)
	return &r, nil
}

// scanInstance reads one process_instances row (in instanceColumns order) from a
// *sql.Row or *sql.Rows.
func scanInstance(s interface{ Scan(...any) error }) (dbgen.ProcessInstance, error) {
	var r dbgen.ProcessInstance
	err := s.Scan(
		&r.ID, &r.ProcessName, &r.ProcessVersion, &r.ParentID,
		&r.CallStack, &r.RetryCount, &r.WakeAt, &r.Status, &r.Error,
		&r.CreatedAt, &r.UpdatedAt, &r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnTaskID,
		&r.InputData, &r.OutputsData, &r.OutputData, &r.ErrorData, &r.ExternalData, &r.EngineState, &r.Task,
	)
	return r, err
}

// contextCols holds the six decomposed context columns as serialized JSON, ready
// to drop into an Insert/Update params struct.
type contextCols struct {
	InputData, OutputsData, OutputData, ErrorData, ExternalData, EngineState string
}

// outputsColumn is the on-disk shape of outputs_data: the completion order plus the
// per-task output envelopes, each independently inline-or-externalized.
type outputsColumn struct {
	Order []string                   `json:"order,omitempty"`
	Items map[string]json.RawMessage `json:"items,omitempty"`
}

// CurrentTask resolves an instance's current task object from its (immutable,
// version-pinned) definition, or nil when the instance has no current task (Task == "",
// i.e. completed or drained). Only the current task is ever needed; its successors are
// implied by the definition's task order, so no queue is materialised. Used by callers
// that need the task's shape (action, only_once) rather than just its id.
func (db *DB) CurrentTask(inst *model.ProcessInstance) (*model.Task, error) {
	if inst.Task == "" {
		return nil, nil
	}
	def, err := db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil, err
	}
	for _, t := range def.Tasks {
		if t.ID == inst.Task {
			return t, nil
		}
	}
	return nil, fmt.Errorf("task %q not found in %s v%d", inst.Task, inst.ProcessName, inst.ProcessVersion)
}

// encodeValueSlot externalizes a value-bearing slot (input / a task output / the
// process output): big values become an object reference (appended to pending and
// recorded in referenced), small ones stay inline. An *model.ObjectRef (an
// unchanged, still-lazy slot) is re-emitted as its reference with no new object.
func encodeValueSlot(v any, pending *[]*pendingObject, referenced map[string]struct{}) (model.Envelope, error) {
	env, p, err := encodeContextValue(v)
	if err != nil {
		return model.Envelope{}, err
	}
	if p != nil {
		*pending = append(*pending, p)
	}
	if env.IsRef() {
		referenced[env.Refs[0].Ref] = struct{}{}
	}
	return env, nil
}

// encodeContext splits inst.ContextData into the six column strings, collecting the
// objects to write (pending) and the full set of object hashes the value-slots still
// reference (referenced) so the write transaction can pin new objects and dereference
// ones a slot no longer points at.
func encodeContext(inst *model.ProcessInstance) (cols contextCols, pending []*pendingObject, referenced map[string]struct{}, err error) {
	referenced = map[string]struct{}{}
	cd := inst.ContextData

	encodeSlot := func(v any) (string, error) {
		env, e := encodeValueSlot(v, &pending, referenced)
		if e != nil {
			return "", e
		}
		b, e := json.Marshal(env)
		return string(b), e
	}

	if v, ok := cd["input"]; ok {
		if cols.InputData, err = encodeSlot(v); err != nil {
			return
		}
	}
	if outs, ok := cd["outputs"].(map[string]any); ok {
		oc := outputsColumn{Order: toStringSlice(cd["output_order"]), Items: map[string]json.RawMessage{}}
		for k, v := range outs {
			env, e := encodeValueSlot(v, &pending, referenced)
			if e != nil {
				err = e
				return
			}
			b, e := json.Marshal(env)
			if e != nil {
				err = e
				return
			}
			oc.Items[k] = b
		}
		b, e := json.Marshal(oc)
		if e != nil {
			err = e
			return
		}
		cols.OutputsData = string(b)
	}
	if v, ok := cd["output"]; ok {
		if cols.OutputData, err = encodeSlot(v); err != nil {
			return
		}
	}
	if v, ok := cd["error"]; ok {
		b, e := json.Marshal(v)
		if e != nil {
			err = e
			return
		}
		cols.ErrorData = string(b)
	}
	if cols.ExternalData, err = encodeExternalData(cd); err != nil {
		return
	}
	cols.EngineState, err = encodeEngineState(cd)
	return
}

// encodeExternalData serialises the parked external-task bookkeeping (task_id, token,
// the inline input snapshot, and a submitted result) into external_data. Returns ""
// when no external state is present. External payloads stay inline in v1 (their own
// column already keeps them off the main runnable index).
func encodeExternalData(cd map[string]any) (string, error) {
	ext := map[string]any{}
	if e, ok := cd[model.CtxExternal].(map[string]any); ok {
		for k, v := range e {
			ext[k] = v
		}
	}
	if r, ok := cd[model.CtxExternalResult]; ok {
		ext["result"] = r
		ext["has_result"] = true
	}
	if len(ext) == 0 {
		return "", nil
	}
	b, err := json.Marshal(ext)
	return string(b), err
}

// withExternalResult writes a submitted/buffered result into an external_data column
// value (the {task_id,token,input,...} bookkeeping), marking has_result so the engine
// consumes it on the next claim. Used by the resolve/deliver paths that operate on the
// column string directly rather than the in-memory context map.
func withExternalResult(externalData string, result any) (string, error) {
	ext := map[string]any{}
	if externalData != "" {
		if err := json.Unmarshal([]byte(externalData), &ext); err != nil {
			return "", fmt.Errorf("decode external_data: %w", err)
		}
	}
	ext["result"] = result
	ext["has_result"] = true
	b, err := json.Marshal(ext)
	return string(b), err
}

// externalToken extracts the per-occurrence token from an external_data column value.
func externalToken(externalData string) (string, error) {
	if externalData == "" {
		return "", nil
	}
	var ext map[string]any
	if err := json.Unmarshal([]byte(externalData), &ext); err != nil {
		return "", fmt.Errorf("decode external_data: %w", err)
	}
	tok, _ := ext["token"].(string)
	return tok, nil
}

// encodeEngineState serialises the spawn/children bookkeeping into engine_state.
// Returns "" when none is present.
func encodeEngineState(cd map[string]any) (string, error) {
	es := map[string]any{}
	for ctxKey, col := range engineStateKeys {
		if v, ok := cd[ctxKey]; ok {
			es[col] = v
		}
	}
	if len(es) == 0 {
		return "", nil
	}
	b, err := json.Marshal(es)
	return string(b), err
}

// engineStateKeys maps the engine-internal context keys to their engine_state field
// names (and back, in decodeContext).
var engineStateKeys = map[string]string{
	"_children":            "children",
	"_spawn_action_type":   "spawn_action_type",
	"_spawn_child_key":     "spawn_child_key",
	"_spawn_result_schema": "spawn_result_schema",
}

func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// persistContext encodes inst's context, writes/dereferences the implied objects via
// qtx (inside the caller's transaction), and returns the column strings for the
// caller's Insert/Update params.
func (db *DB) persistContext(ctx context.Context, qtx *dbgen.Queries, inst *model.ProcessInstance, now int64) (contextCols, error) {
	cols, pending, referenced, err := encodeContext(inst)
	if err != nil {
		return contextCols{}, err
	}
	if err := db.applyContextObjectDiff(ctx, qtx, inst.ID, pending, inst.LoadedObjectHashes, referenced, now); err != nil {
		return contextCols{}, err
	}
	return cols, nil
}

// updateInstanceParams builds UpdateInstance params from inst + already-encoded
// columns, stamping updated_at with now.
func updateInstanceParams(inst *model.ProcessInstance, cols contextCols, now int64) (dbgen.UpdateInstanceParams, error) {
	return dbgen.UpdateInstanceParams{
		ID:           inst.ID,
		Task:         inst.Task,
		OutputsData:  cols.OutputsData,
		OutputData:   cols.OutputData,
		ErrorData:    cols.ErrorData,
		ExternalData: cols.ExternalData,
		EngineState:  cols.EngineState,
		RetryCount:   int64(inst.RetryCount),
		WakeAt:       fromTimePtr(inst.WakeAt),
		Status:       string(inst.Status),
		WaitState:    string(inst.WaitState),
		Error:        inst.Error,
		UpdatedAt:    now,
	}, nil
}

// insertInstanceParams builds InsertInstance params from inst + already-encoded
// columns. status is passed explicitly so callers can override it (e.g. spawned
// children inherit the parent's status); created/updated timestamps are passed for
// the same reason.
func insertInstanceParams(inst *model.ProcessInstance, cols contextCols, status string, createdAt, updatedAt int64) (dbgen.InsertInstanceParams, error) {
	callStack, err := json.Marshal(inst.CallStack)
	if err != nil {
		return dbgen.InsertInstanceParams{}, err
	}
	return dbgen.InsertInstanceParams{
		ID:             inst.ID,
		ProcessName:    inst.ProcessName,
		ProcessVersion: int64(inst.ProcessVersion),
		Task:           inst.Task,
		InputData:      cols.InputData,
		OutputsData:    cols.OutputsData,
		OutputData:     cols.OutputData,
		ErrorData:      cols.ErrorData,
		ExternalData:   cols.ExternalData,
		EngineState:    cols.EngineState,
		ParentID:       inst.ParentID,
		SpawnTaskID:    inst.SpawnTaskID,
		CallStack:      string(callStack),
		RetryCount:     int64(inst.RetryCount),
		WakeAt:         fromTimePtr(inst.WakeAt),
		Status:         status,
		WaitState:      string(inst.WaitState),
		Error:          inst.Error,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	}, nil
}

func (db *DB) SaveInstance(inst *model.ProcessInstance) error {
	ctx := context.Background()
	now := nowMillis()
	tx, qtx, _, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cols, err := db.persistContext(ctx, qtx, inst, now)
	if err != nil {
		return err
	}
	params, err := insertInstanceParams(inst, cols, string(inst.Status), now, now)
	if err != nil {
		return err
	}
	if err := qtx.InsertInstance(ctx, params); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) UpdateInstance(inst *model.ProcessInstance) error {
	ctx := context.Background()
	now := nowMillis()
	tx, qtx, _, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cols, err := db.persistContext(ctx, qtx, inst, now)
	if err != nil {
		return err
	}
	params, err := updateInstanceParams(inst, cols, now)
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstance(ctx, params); err != nil {
		return err
	}
	return tx.Commit()
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
	ctx := context.Background()
	now := nowMillis()
	tx, qtx, _, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cols, err := db.persistContext(ctx, qtx, inst, now)
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstanceProgress(ctx, dbgen.UpdateInstanceProgressParams{
		ID:           inst.ID,
		Task:         inst.Task,
		OutputsData:  cols.OutputsData,
		ErrorData:    cols.ErrorData,
		ExternalData: cols.ExternalData,
		EngineState:  cols.EngineState,
		RetryCount:   int64(inst.RetryCount),
		WakeAt:       fromTimePtr(inst.WakeAt),
		WaitState:    string(inst.WaitState),
		UpdatedAt:    now,
	}); err != nil {
		return err
	}
	return tx.Commit()
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

// ListInstances returns a page of instance summaries, optionally filtered by
// status (empty = all), sorted and paged per req. Summaries omit the context blob
// (use GetInstance for full detail). It returns the page items and the navigation
// metadata (before/after counts and cursors).
func (db *DB) ListInstances(status string, req PageReq) ([]*model.InstanceSummary, PageInfo, error) {
	b, err := instancePaginator.query(req).EqIf("status", status, status != "").build()
	if err != nil {
		return nil, PageInfo{}, err
	}
	rows, err := db.exec.QueryContext(context.Background(), b.pageSQL, b.pageArgs...)
	if err != nil {
		return nil, PageInfo{}, err
	}
	defer rows.Close()
	var out []*model.InstanceSummary
	for rows.Next() {
		s, err := scanInstanceSummary(rows)
		if err != nil {
			return nil, PageInfo{}, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, PageInfo{}, err
	}
	items, first, last := orient(b, out, instanceSummaryCursorVals)
	info, err := db.pageInfo(b, first, last)
	if err != nil {
		return nil, PageInfo{}, err
	}
	return items, info, nil
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
		Task:           r.Task,
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
	cd, loaded, err := decodeContext(r)
	if err != nil {
		return nil, err
	}
	inst.ContextData = cd
	inst.LoadedObjectHashes = loaded
	if err := json.Unmarshal([]byte(r.CallStack), &inst.CallStack); err != nil {
		return nil, fmt.Errorf("unmarshal call_stack: %w", err)
	}
	return inst, nil
}

// decodeContext reassembles the six context columns into the in-memory ContextData
// map. Externalized value-slots become *model.ObjectRef markers (resolved lazily on
// first access); loaded is the set of referenced object hashes, used at write time to
// dereference objects a slot stops pointing at.
func decodeContext(r dbgen.ProcessInstance) (map[string]any, map[string]struct{}, error) {
	cd := map[string]any{}
	loaded := map[string]struct{}{}

	decodeSlot := func(s, key string) error {
		if s == "" {
			return nil
		}
		var env model.Envelope
		if err := json.Unmarshal([]byte(s), &env); err != nil {
			return fmt.Errorf("decode %s envelope: %w", key, err)
		}
		if env.IsRef() {
			loaded[env.Refs[0].Ref] = struct{}{}
		}
		cd[key] = decodeEnvelope(env)
		return nil
	}

	if err := decodeSlot(r.InputData, "input"); err != nil {
		return nil, nil, err
	}
	if r.OutputsData != "" {
		var oc outputsColumn
		if err := json.Unmarshal([]byte(r.OutputsData), &oc); err != nil {
			return nil, nil, fmt.Errorf("decode outputs_data: %w", err)
		}
		items := make(map[string]any, len(oc.Items))
		for k, raw := range oc.Items {
			var env model.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				return nil, nil, fmt.Errorf("decode output %s envelope: %w", k, err)
			}
			if env.IsRef() {
				loaded[env.Refs[0].Ref] = struct{}{}
			}
			items[k] = decodeEnvelope(env)
		}
		cd["outputs"] = items
		if oc.Order != nil {
			cd["output_order"] = oc.Order
		}
	}
	if err := decodeSlot(r.OutputData, "output"); err != nil {
		return nil, nil, err
	}
	if r.ErrorData != "" {
		var ev any
		if err := json.Unmarshal([]byte(r.ErrorData), &ev); err != nil {
			return nil, nil, fmt.Errorf("decode error_data: %w", err)
		}
		cd["error"] = ev
	}
	if r.ExternalData != "" {
		var ext map[string]any
		if err := json.Unmarshal([]byte(r.ExternalData), &ext); err != nil {
			return nil, nil, fmt.Errorf("decode external_data: %w", err)
		}
		hasResult, _ := ext["has_result"].(bool)
		result := ext["result"]
		delete(ext, "has_result")
		delete(ext, "result")
		if len(ext) > 0 {
			cd[model.CtxExternal] = ext
		}
		if hasResult {
			cd[model.CtxExternalResult] = result
		}
	}
	if r.EngineState != "" {
		var es map[string]any
		if err := json.Unmarshal([]byte(r.EngineState), &es); err != nil {
			return nil, nil, fmt.Errorf("decode engine_state: %w", err)
		}
		for ctxKey, col := range engineStateKeys {
			if v, ok := es[col]; ok {
				cd[ctxKey] = v
			}
		}
	}
	return cd, loaded, nil
}
