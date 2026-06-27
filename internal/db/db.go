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
	// never share mutable Task pointers. SaveDefinition invalidates the key on
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
		if err := bootstrapPostgres(sqldb); err != nil {
			sqldb.Close()
			return nil, err
		}
	}
	var dbtx dbgen.DBTX = sqldb
	if dialect == "postgres" {
		dbtx = pgRewriter{dbtx}
	}
	return &DB{sqldb: sqldb, q: dbgen.New(dbtx), exec: dbtx, dialect: dialect}, nil
}

// pgBootstrapLockKey is the advisory-lock key that serializes bootstrapPostgres
// across concurrently-starting workers. Any fixed int64 works (it only needs to be
// the same for every worker); this one spells "gent".
const pgBootstrapLockKey int64 = 0x67656E74 // "gent"

// bootstrapPostgres runs the post-migration Postgres-only setup: the json_each
// helper function and aggressive autovacuum on process_instances. Both statements
// rewrite a system-catalog tuple (pg_proc, pg_class), so a fleet that starts several
// workers at once races on them and one fails with "tuple concurrently updated". A
// transaction-scoped advisory lock serializes the block — the first worker applies
// it, the rest wait and then re-apply it idempotently (CREATE OR REPLACE / SET to
// the same values) with no concurrent catalog write. golang-migrate already guards
// the migrations the same way; this covers the two statements that run after them.
func bootstrapPostgres(sqldb *sql.DB) error {
	ctx := context.Background()
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin postgres bootstrap: %w", err)
	}
	defer tx.Rollback()

	// Held until the transaction ends (commit below), so only one worker is inside
	// the bootstrap at a time.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, pgBootstrapLockKey); err != nil {
		return fmt.Errorf("acquire bootstrap lock: %w", err)
	}

	if _, err := tx.ExecContext(ctx, pgFunctionsSQL); err != nil {
		return fmt.Errorf("create json_each function: %w", err)
	}

	// process_instances is a high-churn queue table: every instance passes
	// through status='running' and then completes, leaving a dead tuple in the
	// runnable range of idx_process_instances_status_wait. The claim query
	// (run every poll by every worker) must skip those dead entries until they
	// are vacuumed, so under a burst of completions claims slow down until
	// autovacuum catches up. Make autovacuum aggressive and unthrottled on this
	// one table so dead tuples are reclaimed promptly. (SQLite updates in place
	// and has no MVCC dead tuples, so this is Postgres-only.)
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE process_instances SET (
			autovacuum_vacuum_scale_factor = 0.02,
			autovacuum_vacuum_threshold    = 50,
			autovacuum_vacuum_cost_delay   = 0
		)`); err != nil {
		return fmt.Errorf("tune process_instances autovacuum: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit postgres bootstrap: %w", err)
	}
	return nil
}

func (db *DB) Close() error { return db.sqldb.Close() }

// pageInfo runs the paginator's before/after counts for a page whose boundary key
// values are first/last (in display order; nil for an empty page) and assembles
// the PageInfo: the effective sort/order, the (capped) before/after counts, and a
// cursor for each direction that actually has more rows (Before only when there
// are rows before, After only when there are rows after — so cursor presence is
// the has-more signal).
func (db *DB) pageInfo(b built, first, last []any) (PageInfo, error) {
	query, args := b.countQuery(first, last)
	var before, after int64
	if err := db.exec.QueryRowContext(context.Background(), query, args...).Scan(&before, &after); err != nil {
		return PageInfo{}, err
	}
	order := "asc"
	if b.desc {
		order = "desc"
	}
	info := PageInfo{
		Size:        b.limit,
		ItemsBefore: before,
		ItemsAfter:  after,
		Sort:        b.sort,
		Order:       order,
	}
	var err error
	if before > 0 {
		if info.Before, err = encodeCursor(b.sort, b.desc, b.mode, first); err != nil {
			return PageInfo{}, err
		}
	}
	if after > 0 {
		if info.After, err = encodeCursor(b.sort, b.desc, b.mode, last); err != nil {
			return PageInfo{}, err
		}
	}
	return info, nil
}

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
