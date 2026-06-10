package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	dbgen "gent/internal/db/gen"
)

// pgRewriter wraps a DBTX and translates SQLite-style placeholders (?N / ?)
// to PostgreSQL-style ($N) before executing queries.
// sqlc generates ?1, ?2, … for SQLite (positional) and $1, $2, … for Postgres.
// At runtime we compile one binary using the SQLite-generated package, so
// we must rewrite before sending queries to a Postgres connection.
type pgRewriter struct{ dbgen.DBTX }

func (r pgRewriter) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return r.DBTX.ExecContext(ctx, rewritePlaceholders(q), args...)
}
func (r pgRewriter) PrepareContext(ctx context.Context, q string) (*sql.Stmt, error) {
	return r.DBTX.PrepareContext(ctx, rewritePlaceholders(q))
}
func (r pgRewriter) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return r.DBTX.QueryContext(ctx, rewritePlaceholders(q), args...)
}
func (r pgRewriter) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return r.DBTX.QueryRowContext(ctx, rewritePlaceholders(q), args...)
}

// rewritePlaceholders converts SQLite placeholder syntax to PostgreSQL:
//   - ?N  (named positional, e.g. ?1) → $N   (same index, parameter reused)
//   - ?   (plain positional)          → $N   (auto-incremented counter)
func rewritePlaceholders(query string) string {
	var b strings.Builder
	b.Grow(len(query))
	n := 0
	for i := 0; i < len(query); {
		if query[i] != '?' {
			b.WriteByte(query[i])
			i++
			continue
		}
		j := i + 1
		for j < len(query) && query[j] >= '0' && query[j] <= '9' {
			j++
		}
		b.WriteByte('$')
		if j > i+1 {
			b.WriteString(query[i+1 : j]) // ?N → $N
		} else {
			n++
			fmt.Fprintf(&b, "%d", n) // ? → $counter
		}
		i = j
	}
	return b.String()
}

// beginTx starts a transaction and returns the raw *sql.Tx alongside a
// *dbgen.Queries already wrapped in pgRewriter when running on Postgres.
func (db *DB) beginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, *dbgen.Queries, error) {
	tx, err := db.sqldb.BeginTx(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	var dbtx dbgen.DBTX = tx
	if db.dialect == "postgres" {
		dbtx = pgRewriter{dbtx}
	}
	return tx, dbgen.New(dbtx), nil
}
