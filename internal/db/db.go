package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"

	dbgen "gent/internal/db/gen"
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
//
// maxOpenConns caps the connection pool; idle connections are kept at half that.
// A fleet of workers must be sized so workers*maxOpenConns < the server's
// max_connections. Values <= 0 fall back to the default of 50.
func OpenPostgres(dsn string, maxOpenConns int) (*DB, error) {
	sqldb, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if maxOpenConns <= 0 {
		maxOpenConns = 50
	}
	sqldb.SetMaxOpenConns(maxOpenConns)
	sqldb.SetMaxIdleConns(max(maxOpenConns/2, 1))
	return open(sqldb, "postgres")
}

func open(sqldb *sql.DB, dialect string) (*DB, error) {
	if err := runMigrations(sqldb, dialect); err != nil {
		sqldb.Close()
		return nil, err
	}
	if dialect == "postgres" {
		if _, err := sqldb.ExecContext(context.Background(), pgFunctionsSQL); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("create json_each function: %w", err)
		}
		// process_instances is a high-churn queue table: every instance passes
		// through status='running' and then completes, leaving a dead tuple in the
		// runnable range of idx_process_instances_status_wait. The claim query
		// (run every poll by every worker) must skip those dead entries until they
		// are vacuumed, so under a burst of completions claims slow down until
		// autovacuum catches up. Make autovacuum aggressive and unthrottled on this
		// one table so dead tuples are reclaimed promptly. (SQLite updates in place
		// and has no MVCC dead tuples, so this is Postgres-only.)
		if _, err := sqldb.ExecContext(context.Background(),
			`ALTER TABLE process_instances SET (
				autovacuum_vacuum_scale_factor = 0.02,
				autovacuum_vacuum_threshold    = 50,
				autovacuum_vacuum_cost_delay   = 0
			)`); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("tune process_instances autovacuum: %w", err)
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
