package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

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
	now := time.Now().UTC()
	row := &instanceRow{
		ID:             inst.ID,
		ProcessName:    inst.ProcessName,
		ProcessVersion: inst.ProcessVersion,
		StepQueue:      string(queue),
		ContextData:    string(ctx),
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

// ── row → model conversion ────────────────────────────────────────────────────

func toInstance(r instanceRow) (*model.ProcessInstance, error) {
	inst := &model.ProcessInstance{
		ID:             r.ID,
		ProcessName:    r.ProcessName,
		ProcessVersion: r.ProcessVersion,
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
