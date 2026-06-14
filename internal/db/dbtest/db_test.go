package dbtest

import (
	"context"
	"database/sql"
	"log"
	"os"
	"testing"
	"time"

	dbpkg "gent/internal/db"
	"gent/internal/model"
)

// sharedPgDB is opened once in TestMain and reused across all tests.
// nil when POSTGRES_DSN is not set. sharedPgRaw is a plain connection to the
// same database, used only to wipe tables between tests (the db package keeps
// its connection unexported, so the black-box tests open their own).
var (
	sharedPgDB  *dbpkg.DB
	sharedPgRaw *sql.DB
)

func TestMain(m *testing.M) {
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		pg, err := dbpkg.OpenPostgres(dsn, 0)
		if err != nil {
			log.Fatalf("open postgres for tests: %v", err)
		}
		sharedPgDB = pg
		defer pg.Close()

		raw, err := sql.Open("postgres", dsn)
		if err != nil {
			log.Fatalf("open raw postgres for tests: %v", err)
		}
		sharedPgRaw = raw
		defer raw.Close()
	}
	os.Exit(m.Run())
}

type backend struct {
	db   *dbpkg.DB
	name string
}

// testBackends returns one backend per available driver.
// SQLite always runs using a fresh temp file.
// PostgreSQL runs when POSTGRES_DSN is set; tables are wiped between tests.
func testBackends(t *testing.T) []backend {
	t.Helper()

	f, err := os.CreateTemp("", "gent-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	sqlite, err := dbpkg.OpenSQLite(f.Name())
	if err != nil {
		os.Remove(f.Name())
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlite.Close(); os.Remove(f.Name()) })

	out := []backend{{sqlite, "sqlite"}}

	if sharedPgDB != nil {
		ctx := context.Background()
		for _, table := range []string{"process_logs", "process_instances", "process_channels", "process_definitions"} {
			if _, err := sharedPgRaw.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				t.Fatalf("reset %s: %v", table, err)
			}
		}
		out = append(out, backend{sharedPgDB, "postgres"})
	}

	return out
}

func insertRunning(t *testing.T, db *dbpkg.DB, id string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		StepQueue:      []*model.Step{},
		ContextData:    map[string]any{},
		Status:         model.StatusRunning,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
}

// TestClaimInstances_Basic verifies that an unclaimed instance is returned with
// the claiming worker's ID and a set lease expiry (RETURNING gives post-update state).
func TestClaimInstances_Basic(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			got, err := b.db.ClaimInstances("worker-A", 10*time.Second, 10)
			if err != nil {
				t.Fatalf("ClaimInstances: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 instance, got %d", len(got))
			}
			if got[0].WorkerID == nil || *got[0].WorkerID != "worker-A" {
				t.Errorf("expected WorkerID=worker-A, got %v", got[0].WorkerID)
			}
			if got[0].LeaseExpiresAt == nil {
				t.Error("expected lease_expires_at to be set")
			}
		})
	}
}

// TestClaimInstances_SkipsLiveLease verifies that a second worker cannot steal
// an instance whose lease has not yet expired.
func TestClaimInstances_SkipsLiveLease(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 10*time.Second, 10); err != nil {
				t.Fatalf("first claim: %v", err)
			}

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10)
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("expected 0 instances (lease still live), got %d", len(got))
			}
		})
	}
}

// TestClaimInstances_ReclaimsExpiredLease verifies that after a lease expires a new
// worker can reclaim the instance.
func TestClaimInstances_ReclaimsExpiredLease(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 10*time.Millisecond, 10); err != nil {
				t.Fatalf("first claim: %v", err)
			}

			time.Sleep(20 * time.Millisecond) // let the lease expire

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10)
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 reclaimed instance, got %d", len(got))
			}
			if got[0].WorkerID == nil || *got[0].WorkerID != "worker-B" {
				t.Errorf("expected WorkerID=worker-B after reclaim, got %v", got[0].WorkerID)
			}
		})
	}
}

// TestRenewLease_Extends verifies that a successful renewal pushes the expiry
// far enough forward that a competing worker cannot reclaim the instance.
func TestRenewLease_Extends(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 30*time.Millisecond, 10); err != nil {
				t.Fatalf("claim: %v", err)
			}

			time.Sleep(20 * time.Millisecond)

			if err := b.db.RenewWorkerLeases("worker-A", time.Second); err != nil {
				t.Fatalf("RenewWorkerLeases: %v", err)
			}

			time.Sleep(20 * time.Millisecond) // original lease would have expired here

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10)
			if err != nil {
				t.Fatalf("competitor claim: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("expected 0 instances after successful renewal, got %d", len(got))
			}
		})
	}
}

// TestRenewLease_WrongWorker verifies that renewal by a non-owner is a no-op.
func TestRenewLease_WrongWorker(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			if _, err := b.db.ClaimInstances("worker-A", 30*time.Millisecond, 10); err != nil {
				t.Fatalf("claim: %v", err)
			}

			if err := b.db.RenewWorkerLeases("worker-Z", time.Second); err != nil {
				t.Fatalf("RenewWorkerLeases (wrong worker): %v", err)
			}

			time.Sleep(40 * time.Millisecond)

			got, err := b.db.ClaimInstances("worker-B", 10*time.Second, 10)
			if err != nil {
				t.Fatalf("reclaim: %v", err)
			}
			if len(got) != 1 {
				t.Errorf("expected 1 instance after bad renewal, got %d", len(got))
			}
		})
	}
}

// TestUpdateInstance_ClearsLease verifies that UpdateInstance always releases the
// lease so the next worker can reclaim freely.
func TestUpdateInstance_ClearsLease(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertRunning(t, b.db, "inst-1")

			claimed, err := b.db.ClaimInstances("worker-A", 10*time.Second, 10)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim: err=%v, count=%d", err, len(claimed))
			}

			inst := claimed[0]
			inst.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(inst); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}

			row, err := b.db.GetInstance("inst-1")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if row.WorkerID != nil {
				t.Errorf("expected worker_id=NULL after UpdateInstance, got %q", *row.WorkerID)
			}
			if row.LeaseExpiresAt != nil {
				t.Errorf("expected lease_expires_at=NULL after UpdateInstance, got %v", row.LeaseExpiresAt)
			}
		})
	}
}
