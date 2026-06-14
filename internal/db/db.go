package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
	exec    dbgen.DBTX // rewrites ?→$N on Postgres; use for hand-written SQL
	dialect string     // "sqlite" | "postgres"

	// defCache memoises GetDefinition lookups, keyed by defKey → definition JSON.
	// Definitions are write-once per (name, version) in normal operation, so the
	// raw JSON is safe to cache; we re-unmarshal a fresh copy per call so callers
	// never share mutable Step pointers. SaveDefinition invalidates the key on
	// write to cover the ON CONFLICT DO UPDATE overwrite path. This is the engine's
	// hottest read (every spawn/goto/output resolves a definition) and on SQLite it
	// otherwise contends with writes for the single connection.
	defCache sync.Map // defKey → string
}

type defKey struct {
	name    string
	version int
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
	return &DB{sqldb: sqldb, q: dbgen.New(dbtx), exec: dbtx, dialect: dialect}, nil
}

func (db *DB) Close() error { return db.sqldb.Close() }

// ── time helpers ─────────────────────────────────────────────────────────────

// All DB timestamps are unix milliseconds (BIGINT columns).

// clockOffset (milliseconds) shifts this process's notion of "now" for all DB
// reads/writes. Only ever increased, via AdvanceClock (debug /tick endpoint),
// so tests can expire leases and retry timers without real waits.
var clockOffset atomic.Int64

func nowMillis() int64 { return time.Now().UnixMilli() + clockOffset.Load() }

// AdvanceClock shifts the DB clock forward by d and returns the new total
// offset. Testing only.
func AdvanceClock(d time.Duration) time.Duration {
	return time.Duration(clockOffset.Add(d.Milliseconds())) * time.Millisecond
}

// Now returns the current time as seen by the DB clock (including any test
// offset). Anything compared against DB timestamps must use this, not time.Now.
func Now() time.Time { return toTime(nowMillis()) }

func toTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

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
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
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
	tx, qtx, _, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowMillis()

	if err := qtx.InsertDefinition(ctx, dbgen.InsertDefinitionParams{
		Name:        def.Name,
		Version:     int64(version),
		Definition:  string(data),
		ContentHash: hash,
		CreatedAt:   now,
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
			UpdatedAt: now,
		}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Drop any stale cache entry: InsertDefinition uses ON CONFLICT DO UPDATE, so
	// re-registering an existing (name, version) can change its content.
	db.defCache.Delete(defKey{name: def.Name, version: version})
	return nil
}

func (db *DB) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	key := defKey{name: name, version: version}

	raw, ok := db.defCache.Load(key)
	if !ok {
		row, err := db.q.GetDefinition(context.Background(), dbgen.GetDefinitionParams{Name: name, Version: int64(version)})
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("definition %q v%d not found", name, version)
		}
		if err != nil {
			return nil, err
		}
		raw = row.Definition
		db.defCache.Store(key, raw)
	}

	// Unmarshal a fresh copy every call so callers never share mutable Step pointers.
	var def model.ProcessDefinition
	return &def, json.Unmarshal([]byte(raw.(string)), &def)
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
		def     string // raw definition JSON, carried by every row of this parent
		isStale bool
	}
	byName := make(map[string]*entry)
	for _, r := range rows {
		e := byName[r.ParentName]
		if e == nil {
			e = &entry{version: int(r.ParentVersion), def: r.ParentDefinition}
			byName[r.ParentName] = e
		}
		if int(r.BakedVersion) != childVersions[r.ChildName] {
			e.isStale = true
		}
	}

	for name, e := range byName {
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(e.def), &def); err != nil {
			return nil, nil, fmt.Errorf("unmarshal definition %q v%d: %w", name, e.version, err)
		}
		vd := VersionedDef{Version: e.version, Def: &def}
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
		UpdatedAt: nowMillis(),
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
	rows, err := db.q.LoadDefinitionsOnChannel(context.Background(), channel)
	if err != nil {
		return nil, err
	}
	out := make([]VersionedDef, 0, len(rows))
	for _, r := range rows {
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(r.Definition), &def); err != nil {
			return nil, err
		}
		out = append(out, VersionedDef{Version: int(r.Version), Def: &def})
	}
	return out, nil
}

// ── Process Instances ─────────────────────────────────────────────────────────

// instanceColumns is the full process_instances column list, in the order
// scanInstance reads them. Shared by the hand-written ClaimInstances and
// RetryProcess queries so adding a column touches one place.
const instanceColumns = `id, process_name, process_version, step_queue, context_data, parent_id,
	call_stack, retry_count, next_retry_at, status, error,
	created_at, updated_at, worker_id, lease_expires_at, wait_state, spawn_step_id`

// scanInstance reads one process_instances row (in instanceColumns order) from a
// *sql.Row or *sql.Rows.
func scanInstance(s interface{ Scan(...any) error }) (dbgen.ProcessInstance, error) {
	var r dbgen.ProcessInstance
	err := s.Scan(
		&r.ID, &r.ProcessName, &r.ProcessVersion,
		&r.StepQueue, &r.ContextData, &r.ParentID,
		&r.CallStack, &r.RetryCount, &r.NextRetryAt,
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
		NextRetryAt: fromTimePtr(inst.NextRetryAt),
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
		NextRetryAt:    fromTimePtr(inst.NextRetryAt),
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
		NextRetryAt: fromTimePtr(inst.NextRetryAt),
		WaitState:   string(inst.WaitState),
		UpdatedAt:   nowMillis(),
	})
}

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
	const where = `status IN ('running', 'failing', 'cancelling')
			  AND wait_state <> 'waiting'
			  AND (next_retry_at IS NULL OR next_retry_at <= ?)
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
				&r.StepQueue, &r.ContextData, &r.ParentID,
				&r.CallStack, &r.RetryCount, &r.NextRetryAt,
				&r.Status, &r.Error, &r.CreatedAt, &r.UpdatedAt,
				&r.WorkerID, &r.LeaseExpiresAt, &r.WaitState, &r.SpawnStepID,
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

// CancelProcess atomically marks an entire process tree as cancelling.
// It only accepts root instances — cancellation is a decision about the whole
// tree, so cancelling a descendant directly is rejected with the root's ID.
// All running instances of the tree (the root and every row whose call_stack
// contains it) transition to 'cancelling'.
//
// The descendant scan is dialect-specific by necessity: PostgreSQL matches via the
// JSONB `?` operator, backed by the call_stack GIN index (see open()), so it stays a
// raw query with explicit $N rather than going through the ?→$N rewriter. A locking
// CTE then takes every row lock in created_at, id order before the UPDATE — the same
// order as FinishChild and FailInstanceAndAncestors, eliminating deadlocks. SQLite
// uses json_each and serialises via its single writer.
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

	now := nowMillis()

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

	tx, qtx, _, err := db.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Lock and load the whole tree (root + descendants) in created_at, id order —
	// the same lock order as CancelProcess/FinishChild/FailInstanceAndAncestors —
	// so concurrent cancels and child completions serialize against the revival.
	//
	// The descendant match is dialect-specific by necessity: PostgreSQL uses the
	// JSONB `?` operator, which is backed by the call_stack GIN index (see open());
	// SQLite uses json_each. Because of that literal `?` operator this query bypasses
	// the placeholder rewriter and runs on the raw tx with explicit $N / ?.
	var query string
	if db.dialect == "postgres" {
		query = `SELECT ` + instanceColumns + ` FROM process_instances
			WHERE id = $1 OR call_stack::jsonb ? $1
			ORDER BY created_at, id FOR UPDATE`
	} else {
		query = `SELECT ` + instanceColumns + ` FROM process_instances
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
