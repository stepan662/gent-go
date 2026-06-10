package db

import (
	"context"
	"testing"

	"gent/internal/model"
)

// insertInst inserts an instance with the given status, parent, call stack, and error.
func insertInst(t *testing.T, db *DB, id string, status model.Status, parentID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		StepQueue:      []*model.Step{{ID: "step1"}},
		ContextData:    map[string]any{},
		Status:         status,
		ParentID:       parentID,
		CallStack:      callStack,
		Error:          errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertInst %q: %v", id, err)
	}
}

func mustStatus(t *testing.T, db *DB, id string) model.Status {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.Status
}

func mustError(t *testing.T, db *DB, id string) string {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.Error
}

// TestCancelProcess_SingleInstance verifies a running instance becomes cancelling.
func TestCancelProcess_SingleInstance(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")

			if err := b.db.CancelProcess(context.Background(), "root"); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusCancelling {
				t.Errorf("expected cancelling, got %q", got)
			}
		})
	}
}

// TestCancelProcess_Descendants verifies that all descendants of a root are marked cancelling.
func TestCancelProcess_Descendants(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "child1", model.StatusRunning, "root", []string{"root"}, "")
			insertInst(t, b.db, "child2", model.StatusWaiting, "root", []string{"root"}, "")
			// grandchild of root via child1
			insertInst(t, b.db, "gc1", model.StatusRunning, "child1", []string{"root", "child1"}, "")

			if err := b.db.CancelProcess(context.Background(), "root"); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}

			for id, want := range map[string]model.Status{
				"root":   model.StatusCancelling,
				"child1": model.StatusCancelling,
				"child2": model.StatusCancelling,
				"gc1":    model.StatusCancelling,
			} {
				if got := mustStatus(t, b.db, id); got != want {
					t.Errorf("%q: expected %q, got %q", id, want, got)
				}
			}
		})
	}
}

// TestCancelProcess_SkipsTerminalDescendants verifies completed/failed children are untouched.
func TestCancelProcess_SkipsTerminalDescendants(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "c-running", model.StatusRunning, "root", []string{"root"}, "")
			insertInst(t, b.db, "c-completed", model.StatusCompleted, "root", []string{"root"}, "")
			insertInst(t, b.db, "c-failed", model.StatusFailed, "root", []string{"root"}, "err")

			if err := b.db.CancelProcess(context.Background(), "root"); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "c-running"); got != model.StatusCancelling {
				t.Errorf("c-running: expected cancelling, got %q", got)
			}
			if got := mustStatus(t, b.db, "c-completed"); got != model.StatusCompleted {
				t.Errorf("c-completed: should stay completed, got %q", got)
			}
			if got := mustStatus(t, b.db, "c-failed"); got != model.StatusFailed {
				t.Errorf("c-failed: should stay failed, got %q", got)
			}
		})
	}
}

// TestCancelProcess_LeafCancelsAncestors verifies that cancelling a leaf marks its ancestors
// as cancelling but leaves siblings of those ancestors untouched.
func TestCancelProcess_LeafCancelsAncestors(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "grand", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "parent", model.StatusRunning, "grand", []string{"grand"}, "")
			insertInst(t, b.db, "sibling", model.StatusRunning, "grand", []string{"grand"}, "")
			insertInst(t, b.db, "leaf", model.StatusRunning, "parent", []string{"grand", "parent"}, "")

			if err := b.db.CancelProcess(context.Background(), "leaf"); err != nil {
				t.Fatalf("CancelProcess: %v", err)
			}

			// leaf and its ancestor chain become cancelling
			for _, id := range []string{"leaf", "parent", "grand"} {
				if got := mustStatus(t, b.db, id); got != model.StatusCancelling {
					t.Errorf("%q: expected cancelling, got %q", id, got)
				}
			}
			// sibling of the leaf's parent is NOT affected
			if got := mustStatus(t, b.db, "sibling"); got != model.StatusRunning {
				t.Errorf("sibling: expected running (untouched), got %q", got)
			}
		})
	}
}

// TestFailAncestors_OverridesCancelling verifies that a child failure overrides
// the 'cancelling' status of ancestor processes (error wins over cancellation).
func TestFailAncestors_OverridesCancelling(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "grand", model.StatusCancelling, "", nil, "")
			insertInst(t, b.db, "parent", model.StatusCancelling, "grand", []string{"grand"}, "")
			// leaf is already failed and triggers FailAncestors
			leaf := &model.ProcessInstance{
				ID:        "leaf",
				CallStack: []string{"grand", "parent"},
				Error:     "boom",
			}

			if err := b.db.FailAncestors(leaf); err != nil {
				t.Fatalf("FailAncestors: %v", err)
			}

			for _, id := range []string{"grand", "parent"} {
				if got := mustStatus(t, b.db, id); got != model.StatusFailed {
					t.Errorf("%q: expected failed, got %q", id, got)
				}
				if msg := mustError(t, b.db, id); msg != "boom" {
					t.Errorf("%q: expected error \"boom\", got %q", id, msg)
				}
			}
		})
	}
}

// TestFailAncestors_AlreadyFailed verifies that already-failed ancestors are not overwritten.
func TestFailAncestors_AlreadyFailed(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "original error")
			leaf := &model.ProcessInstance{
				ID:        "leaf",
				CallStack: []string{"parent"},
				Error:     "new error",
			}

			if err := b.db.FailAncestors(leaf); err != nil {
				t.Fatalf("FailAncestors: %v", err)
			}

			// Failed ancestors are excluded from the UPDATE (status IN condition)
			if msg := mustError(t, b.db, "parent"); msg != "original error" {
				t.Errorf("parent error should be unchanged, got %q", msg)
			}
		})
	}
}

// TestSpawnChildrenAndWait_RunningParent verifies normal spawn: parent → waiting.
func TestSpawnChildrenAndWait_RunningParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusRunning, "", nil, "")

			parent, err := b.db.GetInstance("parent")
			if err != nil {
				t.Fatalf("GetInstance parent: %v", err)
			}
			child := &model.ProcessInstance{
				ID:          "child",
				ProcessName: "test",
				StepQueue:   []*model.Step{{ID: "step1"}},
				ContextData: map[string]any{},
				ParentID:    "parent",
				CallStack:   []string{"parent"},
				Status:      model.StatusRunning,
			}

			if err := b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child}); err != nil {
				t.Fatalf("SpawnChildrenAndWait: %v", err)
			}

			if got := mustStatus(t, b.db, "parent"); got != model.StatusWaiting {
				t.Errorf("parent: expected waiting, got %q", got)
			}
			if got := mustStatus(t, b.db, "child"); got != model.StatusRunning {
				t.Errorf("child: expected running, got %q", got)
			}
		})
	}
}

// TestSpawnChildrenAndWait_CancellingParent verifies that a concurrent cancel prevents spawn.
func TestSpawnChildrenAndWait_CancellingParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusCancelling, "", nil, "")

			parent, err := b.db.GetInstance("parent")
			if err != nil {
				t.Fatalf("GetInstance parent: %v", err)
			}
			child := &model.ProcessInstance{
				ID:          "child",
				ProcessName: "test",
				StepQueue:   []*model.Step{{ID: "step1"}},
				ContextData: map[string]any{},
				ParentID:    "parent",
				CallStack:   []string{"parent"},
				Status:      model.StatusRunning,
			}

			err = b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child})
			if err != ErrParentNotRunning {
				t.Fatalf("expected ErrParentNotRunning, got %v", err)
			}

			// child must not have been inserted
			if _, err := b.db.GetInstance("child"); err == nil {
				t.Error("child should not exist after rejected spawn")
			}
		})
	}
}

// TestRetryProcess_OnlyBlocker verifies that retrying the sole failing child
// transitions its parent from failed → waiting.
func TestRetryProcess_OnlyBlocker(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "child failed")
			insertInst(t, b.db, "child-ok", model.StatusCompleted, "parent", []string{"parent"}, "")
			insertInst(t, b.db, "child-bad", model.StatusFailed, "parent", []string{"parent"}, "something broke")

			if err := b.db.RetryProcess(context.Background(), "child-bad"); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "child-bad"); got != model.StatusRunning {
				t.Errorf("child-bad: expected running, got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusWaiting {
				t.Errorf("parent: expected waiting (unblocked), got %q", got)
			}
		})
	}
}

// TestRetryProcess_MultipleBlockers verifies that retrying one of several failing children
// leaves the parent failed but updates its error to the next blocker.
func TestRetryProcess_MultipleBlockers(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "first child error")
			insertInst(t, b.db, "child-bad-1", model.StatusFailed, "parent", []string{"parent"}, "first child error")
			insertInst(t, b.db, "child-bad-2", model.StatusFailed, "parent", []string{"parent"}, "second child error")

			if err := b.db.RetryProcess(context.Background(), "child-bad-1"); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "child-bad-1"); got != model.StatusRunning {
				t.Errorf("child-bad-1: expected running, got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusFailed {
				t.Errorf("parent: should stay failed, got %q", got)
			}
			// parent error updated to the remaining blocker
			if msg := mustError(t, b.db, "parent"); msg != "second child error" {
				t.Errorf("parent error: expected %q, got %q", "second child error", msg)
			}
		})
	}
}

// TestRetryProcess_OnlyOnce verifies that retrying a process whose pending step
// is marked only_once is rejected.
func TestRetryProcess_OnlyOnce(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			trueVal := true
			inst := &model.ProcessInstance{
				ID:          "locked",
				ProcessName: "test",
				StepQueue:   []*model.Step{{ID: "step1", OnlyOnce: &trueVal}},
				ContextData: map[string]any{},
				Status:      model.StatusFailed,
				Error:       "failed on only_once step",
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			err := b.db.RetryProcess(context.Background(), "locked")
			if err == nil {
				t.Fatal("expected error for only_once step, got nil")
			}
			if mustStatus(t, b.db, "locked") != model.StatusFailed {
				t.Error("status should remain failed after rejected retry")
			}
		})
	}
}
