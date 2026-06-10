package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"github.com/xeipuuv/gojsonschema"

	"gent/internal/db/gen"
	"gent/internal/model"
)

//go:embed migrations/*.sql
var sqlMigrations embed.FS

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

func nowUnix() int64 { return time.Now().UTC().Unix() }

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

func (db *DB) SaveDefinition(def *model.ProcessDefinition, version int, deps []DependencyRow, hash string) error {
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
// This query is hand-written because it requires a dynamic-length IN clause.
func (db *DB) FindParentsOf(channel string, childVersions map[string]int) (stale, current []VersionedDef, err error) {
	if len(childVersions) == 0 {
		return nil, nil, nil
	}
	args := []any{channel}
	placeholders := make([]string, 0, len(childVersions))
	for name := range childVersions {
		args = append(args, name)
		placeholders = append(placeholders, db.ph(len(args)))
	}
	query := fmt.Sprintf(`
		SELECT pd.parent_name, pc.version AS parent_version, pd.child_name, pd.child_version AS baked_version
		FROM process_dependencies pd
		JOIN process_channels pc ON pc.name = pd.parent_name AND pc.channel = %s
		WHERE pd.parent_version = pc.version
		  AND pd.child_name IN (%s)
	`, db.ph(1), strings.Join(placeholders, ", "))

	rows, err := db.sqldb.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type entry struct {
		version int
		isStale bool
	}
	byName := make(map[string]*entry)
	for rows.Next() {
		var parentName, childName string
		var parentVersion, bakedVersion int
		if err := rows.Scan(&parentName, &parentVersion, &childName, &bakedVersion); err != nil {
			return nil, nil, err
		}
		e := byName[parentName]
		if e == nil {
			e = &entry{version: parentVersion}
			byName[parentName] = e
		}
		if bakedVersion != childVersions[childName] {
			e.isStale = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
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
		CallStack:      string(callStack),
		RetryCount:     int64(inst.RetryCount),
		NextRetryAt:    fromTimePtr(inst.NextRetryAt),
		Status:         string(inst.Status),
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
		Error:       inst.Error,
		UpdatedAt:   nowUnix(),
	})
}

// UpdateInstanceProgress writes the mutable step state (queue, context, retry counters)
// without touching status or error. Used after a step completes mid-process so that a
// concurrent CancelProcess or FailAncestors result is preserved in the DB for the next tick.
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
				SELECT pi.id FROM process_instances pi
				WHERE pi.status IN ('running', 'cancelling')
				  AND (pi.next_retry_at IS NULL OR pi.next_retry_at <= $3)
				  AND (pi.worker_id IS NULL OR pi.lease_expires_at <= $4)
				LIMIT $5
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, process_name, process_version, step_queue, context_data, parent_id,
			          call_stack, retry_count, next_retry_at, status, error,
			          created_at, updated_at, worker_id, lease_expires_at`
	} else {
		query = `
			UPDATE process_instances
			SET worker_id = ?, lease_expires_at = ?
			WHERE id IN (
				SELECT pi.id FROM process_instances pi
				WHERE pi.status IN ('running', 'cancelling')
				  AND (pi.next_retry_at IS NULL OR pi.next_retry_at <= ?)
				  AND (pi.worker_id IS NULL OR pi.lease_expires_at <= ?)
				LIMIT ?
			)
			RETURNING id, process_name, process_version, step_queue, context_data, parent_id,
			          call_stack, retry_count, next_retry_at, status, error,
			          created_at, updated_at, worker_id, lease_expires_at`
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
			&r.WorkerID, &r.LeaseExpiresAt,
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

// TryWakeParent is called when a child instance completes or is cancelled.
// For failed children, use FailAncestors instead.
// It checks whether all siblings are in a terminal state and, if so, either wakes
// the parent with merged outputs (success path) or transitions a cancelling parent
// to cancelled (cancellation path).
func (db *DB) TryWakeParent(child *model.ProcessInstance) error {
	ctx := context.Background()

	spawnStepID, _ := child.ContextData["_spawn_step_id"].(string)
	if spawnStepID == "" {
		return fmt.Errorf("child %q missing _spawn_step_id in context", child.ID)
	}

	remaining, err := db.q.CountActiveSiblings(ctx, child.ParentID)
	if err != nil {
		return fmt.Errorf("count siblings: %w", err)
	}
	if remaining > 0 {
		return nil
	}

	parentRow, err := db.q.GetInstance(ctx, child.ParentID)
	if err != nil {
		return fmt.Errorf("fetch parent: %w", err)
	}
	parentStatus := model.Status(parentRow.Status)

	// Parent already failed (FailAncestors ran when a sibling failed). Nothing to do.
	if parentStatus == model.StatusFailed {
		return nil
	}

	siblingRows, err := db.q.GetSiblings(ctx, child.ParentID)
	if err != nil {
		return fmt.Errorf("read siblings: %w", err)
	}

	// Safety check: if any sibling failed, FailAncestors will handle (or already handled)
	// the parent. This guards against a race where TryWakeParent observes a failed sibling
	// before FailAncestors has committed.
	for _, s := range siblingRows {
		if s.Status == string(model.StatusFailed) {
			return nil
		}
	}

	// Cancelling parent: all siblings are completed or cancelled, none failed.
	// Transition parent to cancelled and propagate up the tree.
	if parentStatus == model.StatusCancelling {
		parentInst, err := toInstance(parentRow)
		if err != nil {
			return fmt.Errorf("parse parent: %w", err)
		}
		parentInst.Status = model.StatusCancelled
		if err := db.UpdateInstance(parentInst); err != nil {
			return err
		}
		if parentInst.ParentID != "" {
			return db.TryWakeParent(parentInst)
		}
		return nil
	}

	// Success path: parent is waiting, all siblings completed. Merge outputs and wake.
	var parentCtx map[string]any
	if err := json.Unmarshal([]byte(parentRow.ContextData), &parentCtx); err != nil {
		return fmt.Errorf("unmarshal parent context: %w", err)
	}

	callType, _ := child.ContextData["_spawn_call_type"].(string)

	if parentCtx["outputs"] == nil {
		parentCtx["outputs"] = map[string]any{}
	}

	var wakeErr string
	switch callType {
	case string(model.CallTypeChild):
		wakeErr = db.buildSingleChildOutput(parentCtx, spawnStepID, siblingRows)
	default:
		wakeErr = db.buildParallelChildOutput(parentCtx, spawnStepID, siblingRows)
	}

	if wakeErr != "" {
		// Output validation failed — fail the parent directly.
		parentInst, err := toInstance(parentRow)
		if err != nil {
			return fmt.Errorf("parse parent: %w", err)
		}
		parentInst.Status = model.StatusFailed
		parentInst.Error = wakeErr
		if err := db.UpdateInstance(parentInst); err != nil {
			return err
		}
		if parentInst.CallStack != nil {
			return db.FailAncestors(parentInst)
		}
		return nil
	}

	patchedCtx, err := json.Marshal(parentCtx)
	if err != nil {
		return fmt.Errorf("marshal parent context: %w", err)
	}
	return db.q.WakeParent(ctx, dbgen.WakeParentParams{
		ID:          child.ParentID,
		ContextData: string(patchedCtx),
		UpdatedAt:   nowUnix(),
	})
}

// FailAncestors marks all ancestor processes in the child's call stack as failed.
// This is a single bulk UPDATE — O(1) queries regardless of tree depth.
// It targets ancestors in 'running', 'waiting', or 'cancelling' state so that
// errors always take precedence over cancellation.
// Hand-written because it requires a dynamic-length IN clause.
func (db *DB) FailAncestors(child *model.ProcessInstance) error {
	if len(child.CallStack) == 0 {
		return nil
	}
	args := make([]any, 0, len(child.CallStack)+2)
	args = append(args, child.Error, nowUnix())
	placeholders := make([]string, len(child.CallStack))
	for i, id := range child.CallStack {
		args = append(args, id)
		placeholders[i] = db.ph(len(args))
	}
	query := fmt.Sprintf(
		`UPDATE process_instances SET status = 'failed', error = %s, updated_at = %s
		 WHERE id IN (%s) AND status IN ('running', 'waiting', 'cancelling')`,
		db.ph(1), db.ph(2), strings.Join(placeholders, ", "),
	)
	_, err := db.sqldb.ExecContext(context.Background(), query, args...)
	return err
}

// CancelProcess atomically marks an entire process tree as cancelling.
// Downward: all running/waiting descendants of id are marked cancelling via recursive CTE.
// Upward: all ancestor processes (from the target's call_stack) are marked cancelling.
// Both operations run in a single transaction.
// Hand-written because it uses a recursive CTE and a dynamic IN clause.
func (db *DB) CancelProcess(ctx context.Context, id string) error {
	// Read the target's call_stack before the transaction so we can build the ancestor list.
	row, err := db.q.GetInstance(ctx, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	var callStack []string
	if err := json.Unmarshal([]byte(row.CallStack), &callStack); err != nil {
		return fmt.Errorf("unmarshal call_stack: %w", err)
	}

	tx, err := db.sqldb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := nowUnix()

	// Descendants: recursive CTE from the target downward.
	var descQuery string
	if db.dialect == "postgres" {
		descQuery = `
			WITH RECURSIVE descendants AS (
				SELECT id FROM process_instances WHERE id = $1
				UNION ALL
				SELECT p.id FROM process_instances p
				JOIN descendants d ON p.parent_id = d.id
			)
			UPDATE process_instances SET status = 'cancelling', updated_at = $2
			WHERE id IN (SELECT id FROM descendants)
			  AND status IN ('running', 'waiting')`
	} else {
		descQuery = `
			WITH RECURSIVE descendants AS (
				SELECT id FROM process_instances WHERE id = ?
				UNION ALL
				SELECT p.id FROM process_instances p
				JOIN descendants d ON p.parent_id = d.id
			)
			UPDATE process_instances SET status = 'cancelling', updated_at = ?
			WHERE id IN (SELECT id FROM descendants)
			  AND status IN ('running', 'waiting')`
	}
	if _, err := tx.ExecContext(ctx, descQuery, id, now); err != nil {
		return fmt.Errorf("cancel descendants: %w", err)
	}

	// Ancestors: mark the parent chain as cancelling (only the target's own parents,
	// not siblings of those parents).
	if len(callStack) > 0 {
		args := []any{now}
		placeholders := make([]string, len(callStack))
		for i, ancestorID := range callStack {
			args = append(args, ancestorID)
			placeholders[i] = db.ph(len(args))
		}
		ancQuery := fmt.Sprintf(
			`UPDATE process_instances SET status = 'cancelling', updated_at = %s
			 WHERE id IN (%s) AND status = 'running'`,
			db.ph(1), strings.Join(placeholders, ", "),
		)
		if _, err := tx.ExecContext(ctx, ancQuery, args...); err != nil {
			return fmt.Errorf("cancel ancestors: %w", err)
		}
	}

	return tx.Commit()
}

// RetryProcess resumes a failed or cancelled process from its current step.
// It transitions the process to 'running' and walks the call_stack to unblock
// ancestor processes that were waiting only on this process.
// Returns an error if the current step is marked only_once (force-retry not supported yet).
func (db *DB) RetryProcess(ctx context.Context, id string) error {
	instRow, err := db.q.GetInstance(ctx, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}

	status := model.Status(instRow.Status)
	if status != model.StatusFailed && status != model.StatusCancelled {
		return fmt.Errorf("process is not retryable (status: %s)", status)
	}

	inst, err := toInstance(instRow)
	if err != nil {
		return err
	}

	// Reject retry if the pending step is only_once.
	if len(inst.StepQueue) > 0 {
		step := inst.StepQueue[0]
		if step.OnlyOnce != nil && *step.OnlyOnce {
			return fmt.Errorf("step %q is marked only_once and has already been attempted; use force-retry to override", step.ID)
		}
	}

	tx, qtx, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Transition the process back to running.
	inst.Status = model.StatusRunning
	inst.Error = ""
	inst.RetryCount = 0
	inst.NextRetryAt = nil
	if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
		ID:          inst.ID,
		StepQueue:   instRow.StepQueue,
		ContextData: instRow.ContextData,
		RetryCount:  0,
		NextRetryAt: sql.NullInt64{},
		Status:      string(model.StatusRunning),
		Error:       "",
		UpdatedAt:   nowUnix(),
	}); err != nil {
		return fmt.Errorf("update instance: %w", err)
	}

	// Walk the call_stack nearest-first, unblocking ancestors that were only
	// blocked by this process.
	for i := len(inst.CallStack) - 1; i >= 0; i-- {
		ancestorID := inst.CallStack[i]
		ancestorRow, err := qtx.GetInstance(ctx, ancestorID)
		if err != nil {
			return fmt.Errorf("get ancestor %q: %w", ancestorID, err)
		}
		if model.Status(ancestorRow.Status) != model.StatusFailed {
			break
		}

		failingCount, err := qtx.CountFailingChildren(ctx, ancestorID)
		if err != nil {
			return fmt.Errorf("count failing children of %q: %w", ancestorID, err)
		}

		if failingCount == 0 {
			// This ancestor's only failing child was the one we just retried —
			// transition it back to waiting (it's still suspended for its children).
			if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
				ID:          ancestorID,
				StepQueue:   ancestorRow.StepQueue,
				ContextData: ancestorRow.ContextData,
				RetryCount:  ancestorRow.RetryCount,
				NextRetryAt: ancestorRow.NextRetryAt,
				Status:      string(model.StatusWaiting),
				Error:       "",
				UpdatedAt:   nowUnix(),
			}); err != nil {
				return fmt.Errorf("unblock ancestor %q: %w", ancestorID, err)
			}
		} else {
			// Other children are still failing — update the error message to
			// reflect the next blocker, and stop walking up.
			nextChild, err := qtx.GetFirstFailingChild(ctx, ancestorID)
			if err != nil {
				return fmt.Errorf("get next failing child of %q: %w", ancestorID, err)
			}
			if err := qtx.UpdateInstance(ctx, dbgen.UpdateInstanceParams{
				ID:          ancestorID,
				StepQueue:   ancestorRow.StepQueue,
				ContextData: ancestorRow.ContextData,
				RetryCount:  ancestorRow.RetryCount,
				NextRetryAt: ancestorRow.NextRetryAt,
				Status:      string(model.StatusFailed),
				Error:       nextChild.Error,
				UpdatedAt:   nowUnix(),
			}); err != nil {
				return fmt.Errorf("update ancestor error %q: %w", ancestorID, err)
			}
			break
		}
	}

	return tx.Commit()
}

// SpawnChildrenAndWait atomically inserts child instances and transitions the parent
// to waiting. It first checks (under a transaction lock) that the parent is still
// 'running' — if not, it rolls back and returns ErrParentNotRunning so the caller
// can handle the race gracefully.
// Hand-written because it requires coordinating multiple INSERT statements with a
// conditional parent status check in a single transaction.
func (db *DB) SpawnChildrenAndWait(ctx context.Context, parent *model.ProcessInstance, children []*model.ProcessInstance) error {
	tx, err := db.sqldb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock and verify parent status.
	var lockQuery string
	if db.dialect == "postgres" {
		lockQuery = `SELECT status FROM process_instances WHERE id = $1 FOR UPDATE`
	} else {
		lockQuery = `SELECT status FROM process_instances WHERE id = ?`
	}
	var currentStatus string
	if err := tx.QueryRowContext(ctx, lockQuery, parent.ID).Scan(&currentStatus); err != nil {
		return fmt.Errorf("lock parent: %w", err)
	}
	if currentStatus != string(model.StatusRunning) {
		return ErrParentNotRunning
	}

	// Insert all children.
	for _, child := range children {
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
		now := nowUnix()

		var insertQuery string
		var args []any
		if db.dialect == "postgres" {
			insertQuery = `INSERT INTO process_instances
				(id, process_name, process_version, step_queue, context_data, parent_id,
				 call_stack, retry_count, next_retry_at, status, error, created_at, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
		} else {
			insertQuery = `INSERT INTO process_instances
				(id, process_name, process_version, step_queue, context_data, parent_id,
				 call_stack, retry_count, next_retry_at, status, error, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
		}
		args = []any{
			child.ID, child.ProcessName, int64(child.ProcessVersion),
			string(queue), string(ctxData), child.ParentID,
			string(callStack), int64(child.RetryCount), sql.NullInt64{},
			string(child.Status), child.Error, now, now,
		}
		if _, err := tx.ExecContext(ctx, insertQuery, args...); err != nil {
			return fmt.Errorf("insert child: %w", err)
		}
	}

	// Update parent to waiting.
	parentQueue, err := json.Marshal(parent.StepQueue)
	if err != nil {
		return err
	}
	parentCtx, err := json.Marshal(parent.ContextData)
	if err != nil {
		return err
	}
	var updateQuery string
	var updateArgs []any
	if db.dialect == "postgres" {
		updateQuery = `UPDATE process_instances
			SET step_queue=$1, context_data=$2, retry_count=$3, next_retry_at=$4,
			    status=$5, error=$6, updated_at=$7, worker_id=NULL, lease_expires_at=NULL
			WHERE id=$8`
	} else {
		updateQuery = `UPDATE process_instances
			SET step_queue=?, context_data=?, retry_count=?, next_retry_at=?,
			    status=?, error=?, updated_at=?, worker_id=NULL, lease_expires_at=NULL
			WHERE id=?`
	}
	updateArgs = []any{
		string(parentQueue), string(parentCtx), int64(parent.RetryCount), sql.NullInt64{},
		string(model.StatusWaiting), parent.Error, nowUnix(), parent.ID,
	}
	if _, err := tx.ExecContext(ctx, updateQuery, updateArgs...); err != nil {
		return fmt.Errorf("update parent: %w", err)
	}

	return tx.Commit()
}

// ErrParentNotRunning is returned by SpawnChildrenAndWait when the parent process
// is no longer in 'running' state at spawn time (e.g. it was concurrently cancelled).
var ErrParentNotRunning = fmt.Errorf("parent is no longer running")

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
		RetryCount:     int(r.RetryCount),
		Status:         model.Status(r.Status),
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
