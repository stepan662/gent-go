package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
	"github.com/xeipuuv/gojsonschema"

	"gent/internal/model"
)

//go:embed migrations/*.sql
var sqlMigrations embed.FS

// DB wraps a bun.DB and implements Store for both SQLite and PostgreSQL.
type DB struct {
	bun *bun.DB
}

// OpenSQLite opens (or creates) the SQLite database at path and runs migrations.
func OpenSQLite(path string) (*DB, error) {
	dsn := path + "?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=ON&_busy_timeout=5000"
	sqldb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1) // SQLite supports only one writer at a time.
	return open(bun.NewDB(sqldb, sqlitedialect.New()))
}

// OpenPostgres opens a PostgreSQL connection using the given DSN and runs migrations.
// DSN format: postgres://user:password@host:port/database?sslmode=disable
func OpenPostgres(dsn string) (*DB, error) {
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	sqldb.SetMaxOpenConns(50)
	sqldb.SetMaxIdleConns(25)
	return open(bun.NewDB(sqldb, pgdialect.New()))
}

// Open is an alias for OpenSQLite kept for backward compatibility.
func Open(path string) (*DB, error) { return OpenSQLite(path) }

func open(bundb *bun.DB) (*DB, error) {
	db := &DB{bun: bundb}
	if err := db.migrate(); err != nil {
		bundb.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	ctx := context.Background()
	subFS, err := fs.Sub(sqlMigrations, "migrations")
	if err != nil {
		return fmt.Errorf("migrations fs: %w", err)
	}
	ms := migrate.NewMigrations()
	if err := ms.Discover(subFS); err != nil {
		return fmt.Errorf("discover migrations: %w", err)
	}
	migrator := migrate.NewMigrator(db.bun, ms)
	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

func (db *DB) Close() error { return db.bun.Close() }

// ── bun row models ────────────────────────────────────────────────────────────

type definitionRow struct {
	bun.BaseModel `bun:"table:process_definitions"`
	Name          string    `bun:"name,pk"`
	Version       int       `bun:"version,pk"`
	Definition    string    `bun:"definition,notnull"`
	CreatedAt     time.Time `bun:"created_at,notnull"`
}

type instanceRow struct {
	bun.BaseModel  `bun:"table:process_instances"`
	ID             string     `bun:"id,pk"`
	ProcessName    string     `bun:"process_name,notnull"`
	ProcessVersion int        `bun:"process_version,notnull"`
	StepQueue      string     `bun:"step_queue,notnull"`
	ContextData    string     `bun:"context_data,notnull"`
	ParentID       string     `bun:"parent_id,notnull"`
	CallStack      string     `bun:"call_stack,notnull"`
	RetryCount     int        `bun:"retry_count,notnull"`
	NextRetryAt    *time.Time `bun:"next_retry_at"`
	Status         string     `bun:"status,notnull"`
	Error          string     `bun:"error,notnull"`
	CreatedAt      time.Time  `bun:"created_at,notnull"`
	UpdatedAt      time.Time  `bun:"updated_at,notnull"`
	WorkerID       *string    `bun:"worker_id"`
	LeaseExpiresAt *time.Time `bun:"lease_expires_at"`
}

// ── Process Definitions ───────────────────────────────────────────────────────

func (db *DB) SaveDefinition(def *model.ProcessDefinition) error {
	data, err := json.Marshal(def)
	if err != nil {
		return err
	}
	row := &definitionRow{
		Name:       def.Name,
		Version:    def.Version,
		Definition: string(data),
		CreatedAt:  time.Now().UTC(),
	}
	_, err = db.bun.NewInsert().
		Model(row).
		On("CONFLICT (name, version) DO UPDATE SET definition = EXCLUDED.definition").
		Exec(context.Background())
	return err
}

func (db *DB) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	var row definitionRow
	err := db.bun.NewSelect().
		Model(&row).
		Where("name = ? AND version = ?", name, version).
		Scan(context.Background())
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
	var v sql.NullInt64
	err := db.bun.NewSelect().
		TableExpr("process_definitions").
		ColumnExpr("MAX(version)").
		Where("name = ?", name).
		Scan(context.Background(), &v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, fmt.Errorf("no definitions found for %q", name)
	}
	return int(v.Int64), nil
}

func (db *DB) ListDefinitions() ([]model.ProcessDefinition, error) {
	var rows []definitionRow
	err := db.bun.NewSelect().
		Model(&rows).
		OrderExpr("name, version").
		Scan(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]model.ProcessDefinition, len(rows))
	for i, r := range rows {
		if err := json.Unmarshal([]byte(r.Definition), &out[i]); err != nil {
			return nil, err
		}
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
	now := time.Now().UTC()
	row := &instanceRow{
		ID:             inst.ID,
		ProcessName:    inst.ProcessName,
		ProcessVersion: inst.ProcessVersion,
		StepQueue:      string(queue),
		ContextData:    string(ctx),
		ParentID:       inst.ParentID,
		CallStack:      string(callStack),
		RetryCount:     inst.RetryCount,
		NextRetryAt:    inst.NextRetryAt,
		Status:         string(inst.Status),
		Error:          inst.Error,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	_, err = db.bun.NewInsert().Model(row).Exec(context.Background())
	return err
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
	_, err = db.bun.NewUpdate().
		TableExpr("process_instances").
		Set("step_queue = ?", string(queue)).
		Set("context_data = ?", string(ctx)).
		Set("retry_count = ?", inst.RetryCount).
		Set("next_retry_at = ?", inst.NextRetryAt).
		Set("status = ?", string(inst.Status)).
		Set("error = ?", inst.Error).
		Set("updated_at = ?", time.Now().UTC()).
		Set("worker_id = NULL").
		Set("lease_expires_at = NULL").
		Where("id = ?", inst.ID).
		Exec(context.Background())
	return err
}

func (db *DB) RenewWorkerLeases(workerID string, leaseDur time.Duration) error {
	expiry := time.Now().UTC().Add(leaseDur)
	_, err := db.bun.NewUpdate().
		TableExpr("process_instances").
		Set("lease_expires_at = ?", expiry).
		Where("worker_id = ?", workerID).
		Exec(context.Background())
	return err
}

func (db *DB) ClaimInstances(workerID string, leaseDur time.Duration, limit int) ([]*model.ProcessInstance, error) {
	now := time.Now().UTC()
	leaseExpiry := now.Add(leaseDur)

	subq := db.bun.NewSelect().
		TableExpr("process_instances").
		Column("id").
		Where("status = 'running'").
		Where("(next_retry_at IS NULL OR next_retry_at <= ?)", now).
		Where("(worker_id IS NULL OR lease_expires_at <= ?)", now).
		Limit(limit)

	// PostgreSQL needs FOR UPDATE SKIP LOCKED to prevent concurrent workers
	// from racing to claim the same instance. SQLite's single-writer model
	// makes this unnecessary there.
	if db.bun.Dialect().Name() == dialect.PG {
		subq = subq.For("UPDATE SKIP LOCKED")
	}

	var rows []instanceRow
	_, err := db.bun.NewUpdate().
		TableExpr("process_instances").
		Set("worker_id = ?", workerID).
		Set("lease_expires_at = ?", leaseExpiry).
		Where("id IN (?)", subq).
		Returning("*").
		Exec(context.Background(), &rows)
	if err != nil {
		return nil, err
	}
	return toInstances(rows)
}

func (db *DB) GetInstance(id string) (*model.ProcessInstance, error) {
	var row instanceRow
	err := db.bun.NewSelect().
		Model(&row).
		Where("id = ?", id).
		Scan(context.Background())
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("instance not found")
	}
	if err != nil {
		return nil, err
	}
	return toInstance(row)
}

func (db *DB) ListInstances(status string) ([]*model.ProcessInstance, error) {
	var rows []instanceRow
	q := db.bun.NewSelect().Model(&rows).OrderExpr("created_at DESC")
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Scan(context.Background()); err != nil {
		return nil, err
	}
	return toInstances(rows)
}

// TryWakeParent is called when a child instance finishes (completed or failed).
// It checks whether all siblings are done and, if so, either cascades failure to
// all waiting ancestors in one query or wakes the direct parent with merged outputs.
//
// spawnStepID and spawnOrder are derived from the child's own context
// (_spawn_step_id key) and the parent's stored context (the placeholder ID list).
func (db *DB) TryWakeParent(child *model.ProcessInstance) error {
	ctx := context.Background()

	// Derive the spawn step ID from the child's own context.
	spawnStepID, _ := child.ContextData["_spawn_step_id"].(string)
	if spawnStepID == "" {
		return fmt.Errorf("child %q missing _spawn_step_id in context", child.ID)
	}

	// Count siblings still in progress.
	var remaining int
	err := db.bun.NewSelect().
		TableExpr("process_instances").
		ColumnExpr("COUNT(*)").
		Where("parent_id = ?", child.ParentID).
		Where("status NOT IN ('completed', 'failed')").
		Scan(ctx, &remaining)
	if err != nil {
		return fmt.Errorf("count siblings: %w", err)
	}
	if remaining > 0 {
		return nil
	}

	// All siblings done — read their final state.
	var siblingRows []instanceRow
	if err := db.bun.NewSelect().
		Model(&siblingRows).
		Where("parent_id = ?", child.ParentID).
		Scan(ctx); err != nil {
		return fmt.Errorf("read siblings: %w", err)
	}

	// Index siblings by ID for ordered output building.
	siblingByID := make(map[string]instanceRow, len(siblingRows))
	for _, s := range siblingRows {
		siblingByID[s.ID] = s
	}

	// Check for any failure.
	var failedID, failedErr string
	for _, s := range siblingRows {
		if s.Status == string(model.StatusFailed) {
			failedID, failedErr = s.ID, s.Error
			break
		}
	}

	if failedID != "" {
		// Cascade failure to all waiting ancestors in one UPDATE.
		reason := fmt.Sprintf("child process %q failed: %s", failedID, failedErr)
		ancestors := child.CallStack
		if len(ancestors) == 0 {
			return nil
		}
		_, err := db.bun.NewUpdate().
			TableExpr("process_instances").
			Set("status = ?", string(model.StatusFailed)).
			Set("error = ?", reason).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id IN (?)", bun.In(ancestors)).
			Where("status = ?", string(model.StatusWaiting)).
			Exec(ctx)
		return err
	}

	// All succeeded — fetch the parent to get spawn order, child output schema, and patch its context.
	var parentRow instanceRow
	if err := db.bun.NewSelect().
		Model(&parentRow).
		Where("id = ?", child.ParentID).
		Scan(ctx); err != nil {
		return fmt.Errorf("fetch parent: %w", err)
	}
	var parentCtx map[string]any
	if err := json.Unmarshal([]byte(parentRow.ContextData), &parentCtx); err != nil {
		return fmt.Errorf("unmarshal parent context: %w", err)
	}

	// Recover spawn order from the placeholder stored at spawn time.
	var spawnOrder []string
	if outputs, ok := parentCtx["outputs"].(map[string]any); ok {
		switch v := outputs[spawnStepID].(type) {
		case []string:
			spawnOrder = v
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					spawnOrder = append(spawnOrder, s)
				}
			}
		}
	}

	// Read child_output_schema if the parent declared one.
	var childOutputSchema map[string]any
	if s, ok := parentCtx["_spawn_child_output_schema"]; ok {
		childOutputSchema, _ = s.(map[string]any)
	}

	// Build the step output array in spawn order.
	// Output is only included per child when child_output_schema is declared and validates.
	type childResult struct {
		ID     string `json:"id"`
		Output any    `json:"output,omitempty"`
	}
	results := make([]childResult, 0, len(spawnOrder))
	for _, id := range spawnOrder {
		row, ok := siblingByID[id]
		if !ok {
			continue
		}
		result := childResult{ID: id}
		if len(childOutputSchema) > 0 {
			var ctxData map[string]any
			if err := json.Unmarshal([]byte(row.ContextData), &ctxData); err != nil {
				return fmt.Errorf("unmarshal child context: %w", err)
			}
			output := ctxData["output"]
			if err := validateChildOutput(childOutputSchema, output); err != nil {
				// Treat schema violation as a child failure — cascade to all waiting ancestors.
				reason := fmt.Sprintf("child process %q output validation: %v", id, err)
				ancestors := child.CallStack
				if len(ancestors) > 0 {
					_, uerr := db.bun.NewUpdate().
						TableExpr("process_instances").
						Set("status = ?", string(model.StatusFailed)).
						Set("error = ?", reason).
						Set("updated_at = ?", time.Now().UTC()).
						Where("id IN (?)", bun.In(ancestors)).
						Where("status = ?", string(model.StatusWaiting)).
						Exec(ctx)
					if uerr != nil {
						return uerr
					}
				}
				return nil
			}
			result.Output = output
		}
		results = append(results, result)
	}

	if parentCtx["outputs"] == nil {
		parentCtx["outputs"] = map[string]any{}
	}
	parentCtx["outputs"].(map[string]any)[spawnStepID] = results

	patchedCtx, err := json.Marshal(parentCtx)
	if err != nil {
		return fmt.Errorf("marshal parent context: %w", err)
	}

	// WHERE status='waiting' makes this idempotent if two siblings race here.
	_, err = db.bun.NewUpdate().
		TableExpr("process_instances").
		Set("status = ?", string(model.StatusRunning)).
		Set("context_data = ?", string(patchedCtx)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", child.ParentID).
		Where("status = ?", string(model.StatusWaiting)).
		Exec(ctx)
	return err
}

// ── row → model conversion ────────────────────────────────────────────────────

func toInstance(r instanceRow) (*model.ProcessInstance, error) {
	inst := &model.ProcessInstance{
		ID:             r.ID,
		ProcessName:    r.ProcessName,
		ProcessVersion: r.ProcessVersion,
		ParentID:       r.ParentID,
		RetryCount:     r.RetryCount,
		NextRetryAt:    r.NextRetryAt,
		Status:         model.Status(r.Status),
		Error:          r.Error,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		WorkerID:       r.WorkerID,
		LeaseExpiresAt: r.LeaseExpiresAt,
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

func toInstances(rows []instanceRow) ([]*model.ProcessInstance, error) {
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
