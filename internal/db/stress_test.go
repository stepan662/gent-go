package db

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/lib/pq"

	"gent/internal/model"
)

// pgDeadlock reports whether err is a PostgreSQL deadlock error (SQLSTATE 40P01).
func pgDeadlock(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "40P01"
}

// TestStress_CancelProcess_vs_FailInstanceAndAncestors runs CancelProcess and
// FailInstanceAndAncestors concurrently many times to confirm that the known
// structural deadlock occurs in practice and that PostgreSQL always resolves it
// without leaving the DB in an inconsistent state.
//
// Each iteration fires both operations against the same parent-child pair:
//   - CancelProcess locks parent (running) then descends to child (running) — top-down.
//   - FailInstanceAndAncestors locks child first (UpdateInstance has no status filter),
//     then tries to lock parent — bottom-up.
//
// This is the only real deadlock in the codebase; the others tested by barrier tests
// above cannot occur in practice because their WHERE conditions are mutually exclusive.
func TestStress_CancelProcess_vs_FailInstanceAndAncestors(t *testing.T) {
	if sharedPgDB == nil {
		t.Skip("PostgreSQL not available (set POSTGRES_DSN)")
	}
	ctx := context.Background()
	db := sharedPgDB

	const iterations = 30
	var deadlockCount, successCount int

	for i := 0; i < iterations; i++ {
		db.sqldb.ExecContext(ctx, "DELETE FROM process_instances")
		insertInst(t, db, "parent", model.StatusRunning, "", nil, "")
		insertInst(t, db, "child", model.StatusRunning, "parent", []string{"parent"}, "")

		child, err := db.GetInstance("child")
		if err != nil {
			t.Fatalf("iteration %d: GetInstance child: %v", i, err)
		}
		child.Status = model.StatusFailed
		child.Error = "stress error"

		var wg sync.WaitGroup
		errs := make(chan error, 2)

		wg.Add(2)
		go func() { defer wg.Done(); errs <- db.CancelProcess(ctx, "parent") }()
		go func() { defer wg.Done(); errs <- db.FailInstanceAndAncestors(child) }()
		wg.Wait()
		close(errs)

		for err := range errs {
			switch {
			case err == nil:
				successCount++
			case pgDeadlock(err):
				deadlockCount++
			default:
				t.Errorf("iteration %d: unexpected error: %v", i, err)
			}
		}

		// Verify neither row is stuck as 'running' — regardless of which operation
		// won, both must have reached a terminal or cancelling state.
		for _, id := range []string{"parent", "child"} {
			inst, err := db.GetInstance(id)
			if err != nil {
				t.Errorf("iteration %d: %s not queryable after concurrent ops: %v", i, id, err)
				continue
			}
			if inst.Status == model.StatusRunning {
				t.Errorf("iteration %d: %s still 'running' — inconsistent state", i, id)
			}
		}
	}

	total := iterations * 2
	t.Logf("ran %d iterations (%d total operations)", iterations, total)
	t.Logf("  success:  %d/%d (%.0f%%)", successCount, total, 100*float64(successCount)/float64(total))
	t.Logf("  deadlock: %d/%d (%.0f%%)", deadlockCount, total, 100*float64(deadlockCount)/float64(total))
	if deadlockCount == 0 {
		t.Log("  note: no deadlocks observed — scheduling may not have produced the exact interleave")
	}
}
