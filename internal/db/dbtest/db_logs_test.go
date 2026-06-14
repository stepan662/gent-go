package dbtest

import (
	"testing"
	"time"

	dbpkg "gent/internal/db"
	"gent/internal/model"
)

// appendLog writes one entry with an explicit timestamp so ordering and
// time-window assertions are deterministic.
func appendLog(t *testing.T, db *dbpkg.DB, instanceID string, level model.LogLevel, event string, atMillis int64) {
	t.Helper()
	err := db.AppendLog(&model.LogEntry{
		InstanceID: instanceID,
		Level:      level,
		Event:      event,
		StepID:     "s1",
		Message:    event + " message",
		Detail:     map[string]any{"event": event},
		CreatedAt:  time.UnixMilli(atMillis),
	})
	if err != nil {
		t.Fatalf("AppendLog(%s): %v", event, err)
	}
}

// spawnInstance saves a bare instance row with the given parent, so the subtree
// recursive CTE in ListTreeLogs has a real parent_id chain to walk.
func spawnInstance(t *testing.T, db *dbpkg.DB, id, parentID string) {
	t.Helper()
	if err := db.SaveInstance(&model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		StepQueue:      []*model.Step{},
		ContextData:    map[string]any{},
		ParentID:       parentID,
		Status:         model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance(%s): %v", id, err)
	}
}

func TestListLogs_OrderAndFilters(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventStepStarted, 1000)
			appendLog(t, b.db, "inst-1", model.LogWarn, model.EventRetryScheduled, 2000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventStepSucceeded, 3000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventInstanceDone, 4000)
			// A different instance must not leak into inst-1's logs.
			appendLog(t, b.db, "inst-2", model.LogInfo, model.EventStepStarted, 1500)

			all, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListLogs: %v", err)
			}
			if len(all) != 4 {
				t.Fatalf("expected 4 logs for inst-1, got %d", len(all))
			}
			wantOrder := []string{
				model.EventStepStarted, model.EventRetryScheduled,
				model.EventStepSucceeded, model.EventInstanceDone,
			}
			for i, w := range wantOrder {
				if all[i].Event != w {
					t.Errorf("entry %d: want %q, got %q", i, w, all[i].Event)
				}
			}
			// Detail round-trips through JSON.
			if all[0].Detail["event"] != model.EventStepStarted {
				t.Errorf("detail not preserved: %v", all[0].Detail)
			}

			// Level filter.
			warns, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{Level: string(model.LogWarn)})
			if err != nil {
				t.Fatalf("ListLogs(level): %v", err)
			}
			if len(warns) != 1 || warns[0].Event != model.EventRetryScheduled {
				t.Fatalf("level filter: want 1 retry_scheduled, got %+v", warns)
			}

			// Since filter (inclusive).
			recent, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{Since: 3000})
			if err != nil {
				t.Fatalf("ListLogs(since): %v", err)
			}
			if len(recent) != 2 {
				t.Fatalf("since filter: want 2, got %d", len(recent))
			}
		})
	}
}

func TestListLogs_CursorPagination(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			for i := int64(1); i <= 5; i++ {
				appendLog(t, b.db, "inst-1", model.LogInfo, model.EventStepCompleted, i*1000)
			}

			page1, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{Limit: 2})
			if err != nil {
				t.Fatalf("page1: %v", err)
			}
			if len(page1) != 2 {
				t.Fatalf("page1: want 2, got %d", len(page1))
			}
			last := page1[len(page1)-1]
			page2, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{
				Limit:   2,
				AfterTs: last.CreatedAt.UnixMilli(),
				AfterID: last.ID,
			})
			if err != nil {
				t.Fatalf("page2: %v", err)
			}
			if len(page2) != 2 {
				t.Fatalf("page2: want 2, got %d", len(page2))
			}
			// Pages must not overlap and must stay ordered.
			if !page2[0].CreatedAt.After(last.CreatedAt) {
				t.Errorf("cursor did not advance: page1 last=%v page2 first=%v",
					last.CreatedAt, page2[0].CreatedAt)
			}
		})
	}
}

func TestListTreeLogs_AggregatesSubtree(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			// Build a real parent chain so the recursive CTE has edges to walk:
			//   root → child-a → grandchild
			//   root → child-b
			// plus an unrelated tree (other) that must never leak in.
			spawnInstance(t, b.db, "root", "")
			spawnInstance(t, b.db, "child-a", "root")
			spawnInstance(t, b.db, "child-b", "root")
			spawnInstance(t, b.db, "grandchild", "child-a")
			spawnInstance(t, b.db, "other", "")

			appendLog(t, b.db, "root", model.LogInfo, model.EventChildrenSpawned, 1000)
			appendLog(t, b.db, "child-a", model.LogInfo, model.EventChildrenSpawned, 2000)
			appendLog(t, b.db, "child-b", model.LogInfo, model.EventStepSucceeded, 3000)
			appendLog(t, b.db, "grandchild", model.LogInfo, model.EventStepSucceeded, 4000)
			appendLog(t, b.db, "other", model.LogInfo, model.EventStepStarted, 2500)

			// Subtree from the root: root + a + b + grandchild = 4 (not "other").
			fromRoot, err := b.db.ListTreeLogs("root", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListTreeLogs(root): %v", err)
			}
			if len(fromRoot) != 4 {
				t.Fatalf("subtree(root): want 4 entries, got %d", len(fromRoot))
			}

			// Subtree from a mid-tree node: child-a + grandchild = 2. Works from any
			// node, not just the root — the win over the old root_id column.
			fromChildA, err := b.db.ListTreeLogs("child-a", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListTreeLogs(child-a): %v", err)
			}
			if len(fromChildA) != 2 {
				t.Fatalf("subtree(child-a): want 2 entries, got %d", len(fromChildA))
			}

			// Per-instance view stays scoped to one instance.
			single, err := b.db.ListLogs("child-a", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListLogs(child-a): %v", err)
			}
			if len(single) != 1 {
				t.Fatalf("child-a: want 1 entry, got %d", len(single))
			}
		})
	}
}

func TestPruneLogs_DeletesOlderThanCutoff(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventStepStarted, 1000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventStepSucceeded, 2000)
			appendLog(t, b.db, "inst-1", model.LogInfo, model.EventInstanceDone, 3000)

			n, err := b.db.PruneLogs(2500)
			if err != nil {
				t.Fatalf("PruneLogs: %v", err)
			}
			if n != 2 {
				t.Fatalf("PruneLogs: want 2 deleted, got %d", n)
			}
			remaining, err := b.db.ListLogs("inst-1", dbpkg.LogQuery{})
			if err != nil {
				t.Fatalf("ListLogs: %v", err)
			}
			if len(remaining) != 1 || remaining[0].Event != model.EventInstanceDone {
				t.Fatalf("after prune: want only instance_completed, got %+v", remaining)
			}
		})
	}
}
