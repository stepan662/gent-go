package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"gent/internal/model"

	_ "github.com/mattn/go-sqlite3"
)

// DB wraps a SQLite connection and provides all persistence operations.
type DB struct {
	conn *sql.DB
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer; WAL allows concurrent readers.
	conn.SetMaxOpenConns(1)

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	_, err := db.conn.Exec(`
		PRAGMA journal_mode=WAL;
		PRAGMA synchronous=NORMAL;
		PRAGMA foreign_keys=ON;

		CREATE TABLE IF NOT EXISTS process_definitions (
			name       TEXT    NOT NULL,
			version    INTEGER NOT NULL,
			definition TEXT    NOT NULL,
			created_at TEXT    NOT NULL,
			PRIMARY KEY (name, version)
		);

		CREATE TABLE IF NOT EXISTS process_instances (
			id              TEXT    NOT NULL PRIMARY KEY,
			process_name    TEXT    NOT NULL,
			process_version INTEGER NOT NULL,
			step_queue      TEXT    NOT NULL DEFAULT '[]',
			context_data    TEXT    NOT NULL DEFAULT '{}',
			retry_count     INTEGER NOT NULL DEFAULT 0,
			next_retry_at   TEXT,
			status          TEXT    NOT NULL DEFAULT 'running',
			error           TEXT    NOT NULL DEFAULT '',
			created_at      TEXT    NOT NULL,
			updated_at      TEXT    NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_instances_pending
			ON process_instances (status, next_retry_at);
	`)
	return err
}

// Close closes the underlying database connection.
func (db *DB) Close() error { return db.conn.Close() }

// --- Process Definitions ---

// SaveDefinition inserts or replaces a process definition.
func (db *DB) SaveDefinition(def *model.ProcessDefinition) error {
	data, err := json.Marshal(def)
	if err != nil {
		return err
	}
	_, err = db.conn.Exec(
		`INSERT OR REPLACE INTO process_definitions (name, version, definition, created_at) VALUES (?, ?, ?, ?)`,
		def.Name, def.Version, string(data), time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// GetDefinition loads a specific version of a process definition.
func (db *DB) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	row := db.conn.QueryRow(
		`SELECT definition FROM process_definitions WHERE name = ? AND version = ?`,
		name, version,
	)
	var raw string
	if err := row.Scan(&raw); err == sql.ErrNoRows {
		return nil, fmt.Errorf("definition %q v%d not found", name, version)
	} else if err != nil {
		return nil, err
	}
	var def model.ProcessDefinition
	return &def, json.Unmarshal([]byte(raw), &def)
}

// LatestVersion returns the highest registered version for a process name.
func (db *DB) LatestVersion(name string) (int, error) {
	row := db.conn.QueryRow(
		`SELECT MAX(version) FROM process_definitions WHERE name = ?`, name,
	)
	var v sql.NullInt64
	if err := row.Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, fmt.Errorf("no definitions found for %q", name)
	}
	return int(v.Int64), nil
}

// ListDefinitions returns a summary (name + version) of every registered definition.
func (db *DB) ListDefinitions() ([]model.ProcessDefinition, error) {
	rows, err := db.conn.Query(
		`SELECT definition FROM process_definitions ORDER BY name, version`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ProcessDefinition
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var def model.ProcessDefinition
		if err := json.Unmarshal([]byte(raw), &def); err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, rows.Err()
}

// --- Process Instances ---

// SaveInstance inserts a new process instance.
func (db *DB) SaveInstance(inst *model.ProcessInstance) error {
	queue, err := json.Marshal(inst.StepQueue)
	if err != nil {
		return err
	}
	ctx, err := json.Marshal(inst.ContextData)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.conn.Exec(
		`INSERT INTO process_instances
		 (id, process_name, process_version, step_queue, context_data, retry_count, next_retry_at, status, error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inst.ID, inst.ProcessName, inst.ProcessVersion,
		string(queue), string(ctx),
		inst.RetryCount, nullTime(inst.NextRetryAt),
		string(inst.Status), inst.Error, now, now,
	)
	return err
}

// UpdateInstance persists the mutable fields of an instance after a step executes.
func (db *DB) UpdateInstance(inst *model.ProcessInstance) error {
	queue, err := json.Marshal(inst.StepQueue)
	if err != nil {
		return err
	}
	ctx, err := json.Marshal(inst.ContextData)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.conn.Exec(
		`UPDATE process_instances
		 SET step_queue=?, context_data=?, retry_count=?, next_retry_at=?, status=?, error=?, updated_at=?
		 WHERE id=?`,
		string(queue), string(ctx),
		inst.RetryCount, nullTime(inst.NextRetryAt),
		string(inst.Status), inst.Error, now,
		inst.ID,
	)
	return err
}

// GetInstance loads a single instance by ID.
func (db *DB) GetInstance(id string) (*model.ProcessInstance, error) {
	row := db.conn.QueryRow(
		`SELECT id, process_name, process_version, step_queue, context_data,
		        retry_count, next_retry_at, status, error, created_at, updated_at
		 FROM process_instances WHERE id = ?`, id,
	)
	return scanInstance(row)
}

// ListInstances returns all instances, optionally filtered by status (empty = all).
func (db *DB) ListInstances(status string) ([]*model.ProcessInstance, error) {
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = db.conn.Query(
			`SELECT id, process_name, process_version, step_queue, context_data,
			        retry_count, next_retry_at, status, error, created_at, updated_at
			 FROM process_instances ORDER BY created_at DESC`,
		)
	} else {
		rows, err = db.conn.Query(
			`SELECT id, process_name, process_version, step_queue, context_data,
			        retry_count, next_retry_at, status, error, created_at, updated_at
			 FROM process_instances WHERE status = ? ORDER BY created_at DESC`, status,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.ProcessInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// PendingInstances returns up to limit running instances whose next_retry_at is in the past (or null).
func (db *DB) PendingInstances(limit int) ([]*model.ProcessInstance, error) {
	rows, err := db.conn.Query(
		`SELECT id, process_name, process_version, step_queue, context_data,
		        retry_count, next_retry_at, status, error, created_at, updated_at
		 FROM process_instances
		 WHERE status = 'running'
		   AND (next_retry_at IS NULL OR next_retry_at <= ?)
		 LIMIT ?`,
		time.Now().UTC().Format(time.RFC3339Nano), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.ProcessInstance
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanInstance(s scanner) (*model.ProcessInstance, error) {
	var (
		inst       model.ProcessInstance
		queueRaw   string
		ctxRaw     string
		nextRetry  sql.NullString
		statusStr  string
		createdStr string
		updatedStr string
	)
	err := s.Scan(
		&inst.ID, &inst.ProcessName, &inst.ProcessVersion,
		&queueRaw, &ctxRaw,
		&inst.RetryCount, &nextRetry,
		&statusStr, &inst.Error,
		&createdStr, &updatedStr,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("instance not found")
	}
	if err != nil {
		return nil, err
	}

	inst.Status = model.Status(statusStr)

	if err := json.Unmarshal([]byte(queueRaw), &inst.StepQueue); err != nil {
		return nil, fmt.Errorf("unmarshal step_queue: %w", err)
	}
	if err := json.Unmarshal([]byte(ctxRaw), &inst.ContextData); err != nil {
		return nil, fmt.Errorf("unmarshal context_data: %w", err)
	}
	if nextRetry.Valid {
		t, _ := time.Parse(time.RFC3339Nano, nextRetry.String)
		inst.NextRetryAt = &t
	}
	inst.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	inst.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return &inst, nil
}

func nullTime(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
