package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"github.com/xeipuuv/gojsonschema"

	dbgen "gent/internal/db/gen"
	"gent/internal/model"
)

//go:embed migrations/*.sql
var sqlMigrations embed.FS

//go:embed pg_functions.sql
var pgFunctionsSQL string

// DB wraps a *sql.DB and implements all persistence for both SQLite and PostgreSQL.
type DB struct {
	sqldb   *sql.DB
	q       *dbgen.Queries
	dialect string // "sqlite" | "postgres"
}

// OpenSQLite opens (or creates) the SQLite database at path and runs migrations.
func OpenSQLite(path string) (*DB, error) {
	dsn := path + "?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000"
	sqldb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1) // SQLite supports only one writer at a time.
	return open(sqldb, "sqlite")
}

// OpenPostgres opens a PostgreSQL connection and runs migrations.
// DSN format: postgres://user:password@host:port/database?sslmode=disable
func OpenPostgres(dsn string) (*DB, error) {
	sqldb, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	sqldb.SetMaxOpenConns(50)
	sqldb.SetMaxIdleConns(25)
	return open(sqldb, "postgres")
}

func open(sqldb *sql.DB, dialect string) (*DB, error) {
	if err := runMigrations(sqldb, dialect); err != nil {
		sqldb.Close()
		return nil, err
	}
	if dialect == "postgres" {
		if _, err := sqldb.ExecContext(context.Background(),
			`CREATE INDEX IF NOT EXISTS idx_instances_call_stack_gin
			     ON process_instances USING GIN ((call_stack::jsonb))`); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("create call_stack GIN index: %w", err)
		}
		if _, err := sqldb.ExecContext(context.Background(), pgFunctionsSQL); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("create json_each function: %w", err)
		}
	}
	var dbtx dbgen.DBTX = sqldb
	if dialect == "postgres" {
		dbtx = pgRewriter{dbtx}
	}
	return &DB{sqldb: sqldb, q: dbgen.New(dbtx), dialect: dialect}, nil
}

func (db *DB) Close() error { return db.sqldb.Close() }

// ph returns the positional placeholder for parameter n (1-indexed).
func (db *DB) ph(n int) string {
	if db.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// ── time helpers ─────────────────────────────────────────────────────────────

// clockOffset shifts this process's notion of "now" for all DB reads/writes.
// Only ever increased, via AdvanceClock (debug /clock/advance endpoint), so
// tests can expire leases and retry timers without real waits.
var clockOffset atomic.Int64

func nowUnix() int64 { return time.Now().UTC().Unix() + clockOffset.Load() }

// AdvanceClock shifts the DB clock forward by the given number of seconds and
// returns the new total offset. Testing only.
func AdvanceClock(seconds int64) int64 { return clockOffset.Add(seconds) }

// Now returns the current time as seen by the DB clock (including any test
// offset). Anything compared against DB timestamps must use this, not time.Now.
func Now() time.Time { return toTime(nowUnix()) }

func toTime(unix int64) time.Time { return time.Unix(unix, 0).UTC() }

func toTimePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := toTime(n.Int64)
	return &t
}

func fromTimePtr(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func nullStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// ── Public types ──────────────────────────────────────────────────────────────

// DependencyRow represents a row in process_dependencies.
type DependencyRow struct {
	ParentName    string
	ParentVersion int
	StepID        string
	ChildKey      string
	ChildName     string
	ChildVersion  int
}

// StaleRefRow is returned by FindStaleRefs.
type StaleRefRow struct {
	ParentName     string
	ParentVersion  int
	StepID         string
	ChildName      string
	BakedVersion   int
	ChannelVersion int
}

// VersionedDef pairs a ProcessDefinition with its server-assigned version number.
type VersionedDef struct {
	Version int
	Def     *model.ProcessDefinition
}

// ── Process Definitions ───────────────────────────────────────────────────────

// SaveDefinition persists a new process definition version with its dependencies.
// If channel is non-empty, the channel pointer is updated in the same transaction
// so a crash cannot leave a definition saved without a channel pointing to it.
func (db *DB) SaveDefinition(def *model.ProcessDefinition, version int, deps []DependencyRow, hash string, channel string) error {
	data, err := json.Marshal(def)
	if err != nil {
		return err
	}
	ctx := context.Background()
	tx, qtx, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := qtx.InsertDefinition(ctx, dbgen.InsertDefinitionParams{
		Name:        def.Name,
		Version:     int64(version),
		Definition:  string(data),
		ContentHash: hash,
		CreatedAt:   nowUnix(),
	}); err != nil {
		return err
	}
	if err := qtx.DeleteDependencies(ctx, dbgen.DeleteDependenciesParams{
		ParentName:    def.Name,
		ParentVersion: int64(version),
	}); err != nil {
		return err
	}
	for _, d := range deps {
		if err := qtx.InsertDependency(ctx, dbgen.InsertDependencyParams{
			ParentName:    d.ParentName,
			ParentVersion: int64(d.ParentVersion),
			StepID:        d.StepID,
			ChildKey:      d.ChildKey,
			ChildName:     d.ChildName,
			ChildVersion:  int64(d.ChildVersion),
		}); err != nil {
			return err
		}
	}
	if channel != "" {
		if err := qtx.UpsertChannel(ctx, dbgen.UpsertChannelParams{
			Name:      def.Name,
			Channel:   channel,
			Version:   int64(version),
			UpdatedAt: nowUnix(),
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	ctx := context.Background()
	row, err := db.q.GetDefinition(ctx, dbgen.GetDefinitionParams{Name: name, Version: int64(version)})
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("definition %q v%d not found", name, version)
	}
	if err != nil {
		return nil, err
	}
	var def model.ProcessDefinition
	return &def, json.Unmarshal([]byte(row.Definition), &def)
}

func (db *DB) LatestVersion(name string) (int, error) {
	v, err := db.q.LatestVersion(context.Background(), name)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, fmt.Errorf("no definitions found for %q", name)
	}
	return int(v.(int64)), nil
}

func (db *DB) ListDefinitions() ([]VersionedDef, error) {
	rows, err := db.q.ListDefinitions(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]VersionedDef, len(rows))
	for i, r := range rows {
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(r.Definition), &def); err != nil {
			return nil, err
		}
		out[i] = VersionedDef{Version: int(r.Version), Def: &def}
	}
	return out, nil
}

func (db *DB) GetDefinitionRaw(name string, version int) ([]byte, error) {
	raw, err := db.q.GetDefinitionRaw(context.Background(), dbgen.GetDefinitionRawParams{
		Name:    name,
		Version: int64(version),
	})
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("definition %q v%d not found", name, version)
	}
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

func (db *DB) FindVersionByHash(name, hash string) (int, error) {
	v, err := db.q.FindVersionByHash(context.Background(), dbgen.FindVersionByHashParams{
		Name:        name,
		ContentHash: hash,
	})
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, fmt.Errorf("no version found for %q with given hash", name)
	}
	return int(v.(int64)), nil
}

func (db *DB) GetDependencies(name string, version int) ([]DependencyRow, error) {
	rows, err := db.q.GetDependencies(context.Background(), dbgen.GetDependenciesParams{
		ParentName:    name,
		ParentVersion: int64(version),
	})
	if err != nil {
		return nil, err
	}
	out := make([]DependencyRow, len(rows))
	for i, r := range rows {
		out[i] = DependencyRow{
			ParentName:    r.ParentName,
			ParentVersion: int(r.ParentVersion),
			StepID:        r.StepID,
			ChildKey:      r.ChildKey,
			ChildName:     r.ChildName,
			ChildVersion:  int(r.ChildVersion),
		}
	}
	return out, nil
}

func (db *DB) GetDependencyVersion(parentName string, parentVersion int, stepID string, childKey string) (int, error) {
	v, err := db.q.GetDependencyVersion(context.Background(), dbgen.GetDependencyVersionParams{
		ParentName:    parentName,
		ParentVersion: int64(parentVersion),
		StepID:        stepID,
		ChildKey:      childKey,
	})
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("dependency not found for %q v%d step %q child %q", parentName, parentVersion, stepID, childKey)
	}
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// FindParentsOf returns all processes on channel that have deps referencing any
// of the given children. stale = dep version doesn't match the target; current = matches.
// A parent is stale if ANY of its relevant deps are mismatched.
func (db *DB) FindParentsOf(channel string, childVersions map[string]int) (stale, current []VersionedDef, err error) {
	if len(childVersions) == 0 {
		return nil, nil, nil
	}
	names := make([]string, 0, len(childVersions))
	for name := range childVersions {
		names = append(names, name)
	}
	namesJSON, err := json.Marshal(names)
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.q.FindParentsOf(context.Background(), dbgen.FindParentsOfParams{
		Channel: channel,
		Names:   string(namesJSON),
	})
	if err != nil {
		return nil, nil, err
	}

	type entry struct {
		version int
		isStale bool
	}
	byName := make(map[string]*entry)
	for _, r := range rows {
		e := byName[r.ParentName]
		if e == nil {
			e = &entry{version: int(r.ParentVersion)}
			byName[r.ParentName] = e
		}
		if int(r.BakedVersion) != childVersions[r.ChildName] {
			e.isStale = true
		}
	}

	for name, e := range byName {
		def, defErr := db.GetDefinition(name, e.version)
		if defErr != nil {
			return nil, nil, defErr
		}
		vd := VersionedDef{Version: e.version, Def: def}
		if e.isStale {
			stale = append(stale, vd)
		} else {
			current = append(current, vd)
		}
	}
	return stale, current, nil
}

func (db *DB) FindStaleRefs(channel string) ([]StaleRefRow, error) {
	rows, err := db.q.FindStaleRefs(context.Background(), channel)
	if err != nil {
		return nil, err
	}
	out := make([]StaleRefRow, len(rows))
	for i, r := range rows {
		out[i] = StaleRefRow{
			ParentName:     r.ParentName,
			ParentVersion:  int(r.ParentVersion),
			StepID:         r.StepID,
			ChildName:      r.ChildName,
			BakedVersion:   int(r.BakedVersion),
			ChannelVersion: int(r.ChannelVersion),
		}
	}
	return out, nil
}

// ── Channels ──────────────────────────────────────────────────────────────────

func (db *DB) SaveChannel(name, channel string, version int) error {
	return db.q.UpsertChannel(context.Background(), dbgen.UpsertChannelParams{
		Name:      name,
		Channel:   channel,
		Version:   int64(version),
		UpdatedAt: nowUnix(),
	})
}

func (db *DB) GetChannel(name, channel string) (int, error) {
	v, err := db.q.GetChannel(context.Background(), dbgen.GetChannelParams{Name: name, Channel: channel})
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("process %q has no channel %q", name, channel)
	}
	return int(v), err
}

func (db *DB) DeleteChannel(name, channel string) error {
	return db.q.DeleteChannel(context.Background(), dbgen.DeleteChannelParams{Name: name, Channel: channel})
}

func (db *DB) ListChannels(name string) (map[string]int, error) {
	rows, err := db.q.ListChannels(context.Background(), name)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		out[r.Channel] = int(r.Version)
	}
	return out, nil
}

func (db *DB) LoadDefinitionsOnChannel(channel string) ([]VersionedDef, error) {
	rows, err := db.q.ListChannelsForChannel(context.Background(), channel)
	if err != nil {
		return nil, err
	}
	out := make([]VersionedDef, 0, len(rows))
	for _, r := range rows {
		def, err := db.GetDefinition(r.Name, int(r.Version))
		if err != nil {
			return nil, err
		}
		out = append(out, VersionedDef{Version: int(r.Version), Def: def})
	}
	return out, nil
}

// ── Process Instances ─────────────────────────────────────────────────────────

func (db *DB) SaveInstance(inst *model.ProcessInstance) error {
	queue, err := json.Marshal(inst.StepQueue)
	if err != nil {
		return err
	}
	ctx, err := json.Marshal(inst.ContextData)
	if err != nil {
		return err
	}
	callStack, err := json.Marshal(inst.CallStack)
	if err != nil {
		return err
	}
	now := nowUnix()
	return db.q.InsertInstance(context.Background(), dbgen.InsertInstanceParams{
		ID:             inst.ID,
		ProcessName:    inst.ProcessName,
		ProcessVersion: int64(inst.ProcessVersion),
		StepQueue:      string(queue),
		ContextData:    string(ctx),
		ParentID:       inst.ParentID,
		SpawnStepID:    inst.SpawnStepID,
		CallStack:      string(callStack),
		RetryCount:     int64(inst.RetryCount),
		NextRetryAt:    fromTimePtr(inst.NextRetryAt),
		Status:         string(inst.Status),
		WaitState:      string(inst.WaitState),
		Error:          inst.Error,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
}

func (db *DB) UpdateInstance(inst *model.ProcessInstance) error {
	queue, err := json.Marshal(inst.StepQueue)
	if err != nil {
		return err
	}
	ctx, err := json.Marshal(inst.ContextData)
	if err != nil {
		return err
	}
	return db.q.UpdateInstance(context.Background(), dbgen.UpdateInstanceParams{
		ID:          inst.ID,
		StepQueue:   string(queue),
		ContextData: string(ctx),
		RetryCount:  int64(inst.RetryCount),
		NextRetryAt: fromTimePtr(inst.NextRetryAt),
		Status:      string(inst.Status),
		WaitState:   string(inst.WaitState),
		Error:       inst.Error,
		UpdatedAt:   nowUnix(),
	})
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
	queue, err := json.Marshal(inst.StepQueue)
	if err != nil {
		return err
	}
	ctx, err := json.Marshal(inst.ContextData)
	if err != nil {
		return err
	}
	return db.q.UpdateInstanceProgress(context.Background(), dbgen.UpdateInstanceProgressParams{
		ID:          inst.ID,
		StepQueue:   string(queue),
		ContextData: string(ctx),
		RetryCount:  int64(inst.RetryCount),
		NextRetryAt: fromTimePtr(inst.NextRetryAt),
		WaitState:   string(inst.WaitState),
		UpdatedAt:   nowUnix(),
	})
}

func (db *DB) RenewWorkerLeases(workerID string, leaseDur time.Duration) error {
	return db.q.RenewWorkerLeases(context.Background(), dbgen.RenewWorkerLeasesParams{
		WorkerID:       sql.NullString{String: workerID, Valid: true},
		LeaseExpiresAt: sql.NullInt64{Int64: nowUnix() + int64(leaseDur.Seconds()), Valid: true},
	})
}

// ClaimInstances is hand-written because SQLite and PostgreSQL require different
// locking strategies: PostgreSQL uses FOR UPDATE SKIP LOCKED to safely handle
// concurrent workers, while SQLite's single-writer model makes this unnecessary.
//
// wait_state <> 'waiting' excludes parents suspended for children.
// Both ” (none) and 'collecting' are claimable.
func (db *DB) ClaimInstances(workerID string, leaseDur time.Duration, limit int) ([]*model.ProcessInstance, error) {
	now := nowUnix()
	leaseExpiry := now + int64(leaseDur.Seconds())
	ctx := context.Background()

	var query string
	if db.dialect == "postgres" {
		query = `
			UPDATE process_instances
			SET worker_id = $1, lease_expires_at = $2
			WHERE id IN (
				SELECT id FROM process_instances
				WHERE status IN ('running', 'failing', 'cancelling')
				  AND wait_state <> 'waiting'
				  AND (next_retry_at IS NULL OR next_retry_at <= $3)
				  AND (worker_id IS NULL OR lease_expires_at <= $4)
				ORDER BY created_at ASC, id ASC
				LIMIT $5
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, process_name, process_version, step_queue, context_data, parent_id,
			          call_stack, retry_count, next_retry_at, status, error,
			          created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_step_id`
	} else {
		query = `
			UPDATE process_instances
			SET worker_id = ?, lease_expires_at = ?
			WHERE id IN (
				SELECT id FROM process_instances
				WHERE status IN ('running', 'failing', 'cancelling')
				  AND wait_state <> 'waiting'
				  AND (next_retry_at IS NULL OR next_retry_at <= ?)
				  AND (worker_id IS NULL OR lease_expires_at <= ?)
				ORDER BY created_at ASC, id ASC
				LIMIT ?
			)
			RETURNING id, process_name, process_version, step_queue, context_data, parent_id,
			          call_stack, retry_count, next_retry_at, status, error,
			          created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_step_id`
	}

	rows, err := db.sqldb.QueryContext(ctx, query, workerID, leaseExpiry, now, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*model.ProcessInstance
	for rows.Next() {
		var r dbgen.ProcessInstance
		if err := rows.Scan(
			&r.ID, &r.ProcessName, &r.ProcessVersion,
			&r.StepQueue, &r.ContextData, &r.ParentID,
			&r.CallStack, &r.RetryCount, &r.NextRetryAt,
			&r.Status, &r.Error, &r.CreatedAt, &r.UpdatedAt,
			&r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnStepID,
		); err != nil {
			return nil, err
		}
		inst, err := toInstance(r)
		if err != nil {
			return nil, err
		}
		result = append(result, inst)
	}
	return result, rows.Err()
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
	ctx := context.Background()
	var rows []dbgen.ProcessInstance
	var err error
	if status == "" {
		rows, err = db.q.ListInstances(ctx)
	} else {
		rows, err = db.q.ListInstancesByStatus(ctx, status)
	}
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
	tx, qtx, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Acquire row locks (oldest-first) and read the parent's wait_state in one shot.
	// The locking CTE (PostgreSQL) ensures the same lock order as CancelProcess and
	// FailInstanceAndAncestors, preventing deadlocks. SQLite needs no explicit locking.
	var parentWaitState string
	if db.dialect == "postgres" {
		err = tx.QueryRowContext(ctx, `
			WITH locked AS (
				SELECT id, wait_state FROM process_instances
				WHERE id IN ($1, $2)
				ORDER BY created_at, id FOR UPDATE
			)
			SELECT wait_state FROM locked WHERE id = $2`,
			child.ID, child.ParentID).Scan(&parentWaitState)
	} else {
		err = tx.QueryRowContext(ctx,
			"SELECT wait_state FROM process_instances WHERE id = ?",
			child.ParentID).Scan(&parentWaitState)
	}
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("lock parent: %w", err)
	}
	parentFound := err == nil

	// Save child as terminal.
	queue, err := json.Marshal(child.StepQueue)
	if err != nil {
		return err
	}
	childCtx, err := json.Marshal(child.ContextData)
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
		ID:          child.ID,
		StepQueue:   string(queue),
		ContextData: string(childCtx),
		RetryCount:  int64(child.RetryCount),
		NextRetryAt: fromTimePtr(child.NextRetryAt),
		Status:      string(child.Status),
		WaitState:   string(child.WaitState),
		Error:       child.Error,
		UpdatedAt:   nowUnix(),
	}); err != nil {
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
				UpdatedAt: nowUnix(),
			}); err != nil {
				return fmt.Errorf("wake parent: %w", err)
			}
		}
	}

	return tx.Commit()
}

// CollectChildOutputs is called when a parent instance is in WaitStateCollecting.
// It reads all child instances and merges their outputs into inst.ContextData.
// On success, inst.ContextData["outputs"][step.ID] holds the merged result.
//
// Collecting is only valid when every child of the batch completed — a failed
// or cancelled child makes the parent failing/cancelling, which exits advance()
// before the collect phase. The guard below enforces this rather than silently
// merging nil outputs if that invariant is ever broken.
func (db *DB) CollectChildOutputs(ctx context.Context, inst *model.ProcessInstance, step *model.Step) error {
	siblings, err := db.q.GetChildrenForStep(ctx, dbgen.GetChildrenForStepParams{
		ParentID:    inst.ID,
		SpawnStepID: step.ID,
	})
	if err != nil {
		return fmt.Errorf("get children for step: %w", err)
	}
	for _, row := range siblings {
		if model.Status(row.Status) != model.StatusCompleted {
			return fmt.Errorf("child %q is %s; outputs can only be collected when all children completed", row.ID, row.Status)
		}
	}
	if inst.ContextData["outputs"] == nil {
		inst.ContextData["outputs"] = map[string]any{}
	}
	var wakeErr string
	switch step.Call.Type {
	case model.CallTypeChild:
		wakeErr = db.buildSingleChildOutput(inst.ContextData, step.ID, siblings)
	default:
		wakeErr = db.buildParallelChildOutput(inst.ContextData, step.ID, siblings)
	}
	if wakeErr != "" {
		return fmt.Errorf("%s", wakeErr)
	}
	return nil
}

// FailAncestors marks all ancestor processes in the child's call stack as
// 'failing' — doomed but still draining. Ancestors keep their wait_state
// ('waiting'), so they stay unclaimable until their children settle; the engine
// then transitions them to 'failed' one level per tick, mirroring cancellation.
// This is a single bulk UPDATE — O(1) queries regardless of tree depth.
// It targets ancestors in 'running' or 'cancelling' state, so errors always
// take precedence over cancellation and the first error wins.
func (db *DB) FailAncestors(child *model.ProcessInstance) error {
	if len(child.CallStack) == 0 {
		return nil
	}
	idsJSON, err := json.Marshal(child.CallStack)
	if err != nil {
		return err
	}
	return db.q.FailAncestors(context.Background(), dbgen.FailAncestorsParams{
		Error:     child.Error,
		UpdatedAt: nowUnix(),
		Ids:       string(idsJSON),
	})
}

// FailInstanceAndAncestors atomically marks a child instance as failed,
// propagates 'failing' to all ancestors in its call stack, and — when the
// failed child was the last active member of its spawn batch — wakes the
// parent (to ”, the parent is failing by then) so the engine can settle it
// on the next tick. All in a single transaction; the safe replacement for
// calling UpdateInstance + FailAncestors separately.
func (db *DB) FailInstanceAndAncestors(child *model.ProcessInstance) error {
	ctx := context.Background()
	tx, qtx, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock child and all ancestors oldest-first (PostgreSQL) — consistent with
	// FinishChild and CancelProcess. Ancestors are read from the child's call_stack
	// in the DB; no Go-side ID list needed. SQLite needs no explicit locking.
	if db.dialect == "postgres" {
		lockRows, lockErr := tx.QueryContext(ctx, `
			SELECT id FROM process_instances
			WHERE id = $1
			   OR id IN (SELECT jsonb_array_elements_text(
			                 (SELECT call_stack::jsonb FROM process_instances WHERE id = $1)
			             ))
			ORDER BY created_at, id FOR UPDATE`, child.ID)
		if lockErr != nil {
			return fmt.Errorf("lock rows: %w", lockErr)
		}
		lockRows.Close()
	}

	queue, err := json.Marshal(child.StepQueue)
	if err != nil {
		return err
	}
	ctxData, err := json.Marshal(child.ContextData)
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
		ID:          child.ID,
		StepQueue:   string(queue),
		ContextData: string(ctxData),
		RetryCount:  int64(child.RetryCount),
		NextRetryAt: fromTimePtr(child.NextRetryAt),
		Status:      string(child.Status),
		WaitState:   string(child.WaitState),
		Error:       child.Error,
		UpdatedAt:   nowUnix(),
	}); err != nil {
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
			UpdatedAt: nowUnix(),
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
		err := tx.QueryRowContext(ctx,
			"SELECT wait_state FROM process_instances WHERE id = "+db.ph(1),
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
					UpdatedAt: nowUnix(),
				}); err != nil {
					return fmt.Errorf("wake parent: %w", err)
				}
			}
		}
	}

	return tx.Commit()
}

// CancelProcess atomically marks an entire process tree as cancelling.
// It only accepts root instances — cancellation is a decision about the whole
// tree, so cancelling a descendant directly is rejected with the root's ID.
// All running instances of the tree (the root and every row whose call_stack
// contains it) transition to 'cancelling'.
//
// On PostgreSQL a single locking CTE acquires every row lock in created_at, id order
// before the UPDATE runs — the same order used by FinishChild and FailInstanceAndAncestors,
// eliminating deadlocks. Descendants are found via the JSONB ? operator.
func (db *DB) CancelProcess(ctx context.Context, id string) error {
	row, err := db.q.GetInstance(ctx, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	if err := requireRoot(row, "cancel"); err != nil {
		return err
	}

	tx, err := db.sqldb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := nowUnix()

	var qErr error
	if db.dialect == "postgres" {
		_, qErr = tx.ExecContext(ctx, `
			WITH locked AS (
				SELECT id FROM process_instances
				WHERE id = $1 OR call_stack::jsonb ? $1
				ORDER BY created_at, id FOR UPDATE
			)
			UPDATE process_instances SET status = 'cancelling', updated_at = $2
			WHERE id IN (SELECT id FROM locked) AND status = 'running'`,
			id, now)
	} else {
		_, qErr = tx.ExecContext(ctx, `
			UPDATE process_instances SET status = 'cancelling', updated_at = ?
			WHERE status = 'running'
			  AND (id = ?
			       OR EXISTS (SELECT 1 FROM json_each(call_stack) WHERE value = ?))`,
			now, id, id)
	}
	if qErr != nil {
		return fmt.Errorf("cancel process: %w", qErr)
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

	tx, qtx, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock and load the whole tree (root + descendants) in created_at, id order —
	// the same lock order as CancelProcess/FinishChild/FailInstanceAndAncestors —
	// so concurrent cancels and child completions serialize against the revival.
	cols := `id, process_name, process_version, step_queue, context_data, parent_id,
	         call_stack, retry_count, next_retry_at, status, error,
	         created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_step_id`
	var query string
	if db.dialect == "postgres" {
		query = `SELECT ` + cols + ` FROM process_instances
			WHERE id = $1 OR call_stack::jsonb ? $1
			ORDER BY created_at, id FOR UPDATE`
	} else {
		query = `SELECT ` + cols + ` FROM process_instances
			WHERE id = ? OR EXISTS (SELECT 1 FROM json_each(call_stack) WHERE value = ?)
			ORDER BY created_at, id`
	}
	args := []any{id}
	if db.dialect != "postgres" {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("lock tree: %w", err)
	}
	defer rows.Close()

	nodes := make(map[string]*model.ProcessInstance)
	rawRows := make(map[string]dbgen.ProcessInstance)
	children := make(map[string]map[string][]*model.ProcessInstance) // parentID → spawnStepID → batch
	for rows.Next() {
		var r dbgen.ProcessInstance
		if err := rows.Scan(
			&r.ID, &r.ProcessName, &r.ProcessVersion,
			&r.StepQueue, &r.ContextData, &r.ParentID,
			&r.CallStack, &r.RetryCount, &r.NextRetryAt,
			&r.Status, &r.Error, &r.CreatedAt, &r.UpdatedAt,
			&r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnStepID,
		); err != nil {
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

	now := nowUnix()
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

	tx, qtx, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock parent and read its current status to propagate to children.
	var lockQuery string
	if db.dialect == "postgres" {
		lockQuery = `SELECT status, wait_state FROM process_instances WHERE id = $1 FOR UPDATE`
	} else {
		lockQuery = `SELECT status, wait_state FROM process_instances WHERE id = ?`
	}
	var currentStatus, currentWaitState string
	if err := tx.QueryRowContext(ctx, lockQuery, parent.ID).Scan(&currentStatus, &currentWaitState); err != nil {
		return fmt.Errorf("lock parent: %w", err)
	}
	if currentWaitState != "" {
		return fmt.Errorf("parent %q is already in wait_state %q", parent.ID, currentWaitState)
	}

	// Insert children with the parent's current status (propagates cancelling if needed).
	// Each child gets created_at = now+i so siblings have a strict ordering by definition
	// position — ClaimInstances (ORDER BY created_at) always processes them in spawn order.
	now := nowUnix()
	for i, child := range children {
		queue, err := json.Marshal(child.StepQueue)
		if err != nil {
			return err
		}
		ctxData, err := json.Marshal(child.ContextData)
		if err != nil {
			return err
		}
		callStack, err := json.Marshal(child.CallStack)
		if err != nil {
			return err
		}
		ts := now + int64(i)
		if err := qtx.InsertInstance(ctx, dbgen.InsertInstanceParams{
			ID:             child.ID,
			ProcessName:    child.ProcessName,
			ProcessVersion: int64(child.ProcessVersion),
			StepQueue:      string(queue),
			ContextData:    string(ctxData),
			ParentID:       child.ParentID,
			SpawnStepID:    child.SpawnStepID,
			CallStack:      string(callStack),
			RetryCount:     int64(child.RetryCount),
			NextRetryAt:    sql.NullInt64{},
			Status:         currentStatus,
			WaitState:      string(model.WaitStateNone),
			Error:          child.Error,
			CreatedAt:      ts,
			UpdatedAt:      ts,
		}); err != nil {
			return fmt.Errorf("insert child: %w", err)
		}
	}

	// Suspend parent: keep status, set wait_state='waiting'.
	parentQueue, err := json.Marshal(parent.StepQueue)
	if err != nil {
		return err
	}
	parentCtx, err := json.Marshal(parent.ContextData)
	if err != nil {
		return err
	}
	if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
		ID:          parent.ID,
		StepQueue:   string(parentQueue),
		ContextData: string(parentCtx),
		RetryCount:  int64(parent.RetryCount),
		NextRetryAt: sql.NullInt64{},
		Status:      currentStatus,
		WaitState:   string(model.WaitStateWaiting),
		Error:       parent.Error,
		UpdatedAt:   nowUnix(),
	}); err != nil {
		return fmt.Errorf("suspend parent: %w", err)
	}

	return tx.Commit()
}

// buildSingleChildOutput writes the single child's output directly to
// parentCtx["outputs"][stepID]. Returns a non-empty error message on validation failure.
func (db *DB) buildSingleChildOutput(parentCtx map[string]any, stepID string, siblings []dbgen.ProcessInstance) string {
	if len(siblings) == 0 {
		return ""
	}
	row := siblings[0]
	var childCtx map[string]any
	if err := json.Unmarshal([]byte(row.ContextData), &childCtx); err != nil {
		return fmt.Sprintf("unmarshal child context: %v", err)
	}
	output := childCtx["output"]
	if schemaRaw, _ := childCtx["_spawn_output_schema"].(string); schemaRaw != "" {
		var schema map[string]any
		json.Unmarshal([]byte(schemaRaw), &schema) //nolint:errcheck
		if err := validateChildOutput(schema, output); err != nil {
			return fmt.Sprintf("child process %q (%s) output validation: %v", row.ID, row.ProcessName, err)
		}
	}
	parentCtx["outputs"].(map[string]any)[stepID] = output
	return ""
}

// buildParallelChildOutput writes each sibling's output to parentCtx["outputs"][stepID][key].
// Returns a non-empty error message on the first validation failure.
func (db *DB) buildParallelChildOutput(parentCtx map[string]any, stepID string, siblings []dbgen.ProcessInstance) string {
	result := make(map[string]any, len(siblings))
	for _, row := range siblings {
		var childCtx map[string]any
		if err := json.Unmarshal([]byte(row.ContextData), &childCtx); err != nil {
			return fmt.Sprintf("unmarshal child context: %v", err)
		}
		key, _ := childCtx["_spawn_child_key"].(string)
		output := childCtx["output"]
		if schemaRaw, _ := childCtx["_spawn_output_schema"].(string); schemaRaw != "" {
			var schema map[string]any
			json.Unmarshal([]byte(schemaRaw), &schema) //nolint:errcheck
			if err := validateChildOutput(schema, output); err != nil {
				return fmt.Sprintf("child process %q (%s) output validation: %v", row.ID, row.ProcessName, err)
			}
		}
		result[key] = output
	}
	parentCtx["outputs"].(map[string]any)[stepID] = result
	return ""
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
		NextRetryAt:    toTimePtr(r.NextRetryAt),
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

func validateChildOutput(schema map[string]any, output any) error {
	result, err := gojsonschema.Validate(
		gojsonschema.NewGoLoader(schema),
		gojsonschema.NewGoLoader(output),
	)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	if !result.Valid() {
		msgs := make([]string, len(result.Errors()))
		for i, e := range result.Errors() {
			msgs[i] = e.String()
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}
