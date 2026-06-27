package dbtest

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	dbpkg "gent/internal/db"
)

// TestConcurrentOpenPostgres reproduces the fleet cold-start race: several workers
// calling OpenPostgres against the same brand-new database at once. After migrations
// (which golang-migrate already serializes with its own advisory lock), each worker
// runs the post-migration bootstrap — the json_each helper function and the
// autovacuum ALTER on process_instances — both of which write a system-catalog tuple.
// Without the advisory lock that serializes the bootstrap, the concurrent CREATE
// FUNCTION / ALTER TABLE fail with "tuple concurrently updated" or a pg_proc
// duplicate-key violation, and a worker dies on launch. Runs against a throwaway
// database so the full cold-start path is exercised in isolation. Skipped without
// POSTGRES_DSN — the bootstrap is Postgres-only (SQLite has no such step).
func TestConcurrentOpenPostgres(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}

	freshDSN, dropDB := freshDatabase(t, dsn)
	defer dropDB()

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	dbs := make([]*dbpkg.DB, workers)
	release := make(chan struct{})

	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-release // unblock every goroutine at once to maximise the race window
			// Small pool so workers*pool stays well under max_connections.
			dbs[i], errs[i] = dbpkg.OpenPostgres(freshDSN, 2)
		}(i)
	}
	close(release)
	wg.Wait()

	for i, db := range dbs {
		if db != nil {
			db.Close()
		}
		if errs[i] != nil {
			t.Errorf("worker %d: concurrent OpenPostgres failed: %v", i, errs[i])
		}
	}
}

// freshDatabase creates a uniquely-named throwaway database on the same server as
// dsn and returns a DSN pointing at it plus a cleanup that drops it. CREATE/DROP
// DATABASE cannot run inside the target, so it connects to the "postgres"
// maintenance database. Skips the test if the role lacks CREATEDB.
func freshDatabase(t *testing.T, dsn string) (string, func()) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse POSTGRES_DSN: %v", err)
	}
	// time-based name; digits only, so no injection risk in the CREATE/DROP below.
	name := fmt.Sprintf("gent_bootstrap_%d", time.Now().UnixNano())

	admin := *u
	admin.Path = "/postgres"
	adminDSN := admin.String()

	adminDB, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("open maintenance db: %v", err)
	}
	defer adminDB.Close()
	if _, err := adminDB.Exec("CREATE DATABASE " + name); err != nil {
		t.Skipf("cannot create a throwaway database (role needs CREATEDB): %v", err)
	}

	fresh := *u
	fresh.Path = "/" + name
	return fresh.String(), func() {
		a, err := sql.Open("postgres", adminDSN)
		if err != nil {
			return
		}
		defer a.Close()
		// FORCE (Postgres 13+) terminates any lingering pooled connections so the
		// drop never blocks on a slow Close.
		a.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	}
}
