package dbtest

import (
	"context"
	"strings"
	"testing"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// insertInst inserts an instance with the given status, parent, call stack, and error.
func insertInst(t *testing.T, db *dbpkg.DB, id string, status model.Status, parentID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
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

func mustStatus(t *testing.T, db *dbpkg.DB, id string) model.Status {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.Status
}

func mustError(t *testing.T, db *dbpkg.DB, id string) string {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.Error
}

func mustWaitState(t *testing.T, db *dbpkg.DB, id string) model.WaitState {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.WaitState
}

// insertInstW inserts an instance with an explicit wait_state (for testing waiting parents).
func insertInstW(t *testing.T, db *dbpkg.DB, id string, status model.Status, waitState model.WaitState, parentID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
		ContextData:    map[string]any{},
		Status:         status,
		WaitState:      waitState,
		ParentID:       parentID,
		CallStack:      callStack,
		Error:          errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertInstW %q: %v", id, err)
	}
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
			insertInstW(t, b.db, "child2", model.StatusRunning, model.WaitStateWaiting, "root", []string{"root"}, "")
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

// TestCancelProcess_NonRootRejected verifies that cancelling a descendant directly
// is rejected with an error naming the tree root, leaving the tree untouched.
func TestCancelProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusRunning, "", nil, "")
			insertInst(t, b.db, "mid", model.StatusRunning, "root", []string{"root"}, "")
			insertInst(t, b.db, "leaf", model.StatusRunning, "mid", []string{"root", "mid"}, "")

			err := b.db.CancelProcess(context.Background(), "leaf")
			if err == nil {
				t.Fatal("expected error for non-root cancel, got nil")
			}
			if !strings.Contains(err.Error(), `"root"`) {
				t.Errorf("error should name the root instance, got %q", err)
			}
			for _, id := range []string{"root", "mid", "leaf"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running (untouched), got %q", id, got)
				}
			}
		})
	}
}

// TestFailInstanceAndAncestors_OverridesCancelling verifies that a child failure marks
// 'cancelling' ancestors as 'failing' (error wins over cancellation) while
// preserving their wait_state so they keep draining until children settle.
func TestFailInstanceAndAncestors_OverridesCancelling(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "grand", model.StatusCancelling, model.WaitStateWaiting, "", nil, "")
			insertInstW(t, b.db, "parent", model.StatusCancelling, model.WaitStateWaiting, "grand", []string{"grand"}, "")
			// leaf is already failed and triggers ancestor failure propagation
			leaf := &model.ProcessInstance{
				ID:        "leaf",
				CallStack: []string{"grand", "parent"},
				Error:     "boom",
			}

			if err := b.db.FailInstanceAndAncestors(leaf); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			for _, id := range []string{"grand", "parent"} {
				if got := mustStatus(t, b.db, id); got != model.StatusFailing {
					t.Errorf("%q: expected failing, got %q", id, got)
				}
				if msg := mustError(t, b.db, id); msg != "boom" {
					t.Errorf("%q: expected error \"boom\", got %q", id, msg)
				}
				if got := mustWaitState(t, b.db, id); got != model.WaitStateWaiting {
					t.Errorf("%q: wait_state should be preserved, got %q", id, got)
				}
			}
		})
	}
}

// TestFailInstanceAndAncestors_AlreadyFailed verifies that already-failed ancestors are not overwritten.
func TestFailInstanceAndAncestors_AlreadyFailed(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "original error")
			leaf := &model.ProcessInstance{
				ID:        "leaf",
				CallStack: []string{"parent"},
				Error:     "new error",
			}

			if err := b.db.FailInstanceAndAncestors(leaf); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
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
				Task:        "step1",
				ContextData: map[string]any{},
				ParentID:    "parent",
				CallStack:   []string{"parent"},
				Status:      model.StatusRunning,
			}

			if err := b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child}); err != nil {
				t.Fatalf("SpawnChildrenAndWait: %v", err)
			}

			if got := mustStatus(t, b.db, "parent"); got != model.StatusRunning {
				t.Errorf("parent: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
			if got := mustStatus(t, b.db, "child"); got != model.StatusRunning {
				t.Errorf("child: expected running, got %q", got)
			}
		})
	}
}

// TestSpawnChildrenAndWait_CancellingParent verifies that a cancelling parent spawns
// cancelling children (they self-cancel and wake the parent via FinishChild).
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
				Task:        "step1",
				ContextData: map[string]any{},
				ParentID:    "parent",
				CallStack:   []string{"parent"},
				Status:      model.StatusRunning,
			}

			if err := b.db.SpawnChildrenAndWait(context.Background(), parent, []*model.ProcessInstance{child}); err != nil {
				t.Fatalf("SpawnChildrenAndWait: %v", err)
			}

			// parent keeps cancelling status but enters wait_state=waiting
			if got := mustStatus(t, b.db, "parent"); got != model.StatusCancelling {
				t.Errorf("parent: expected cancelling, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
			// child is spawned as cancelling (inherits parent status)
			if got := mustStatus(t, b.db, "child"); got != model.StatusCancelling {
				t.Errorf("child: expected cancelling, got %q", got)
			}
		})
	}
}

// insertChild inserts a child instance spawned by the given parent task.
func insertChild(t *testing.T, db *dbpkg.DB, id string, status model.Status, parentID, spawnTaskID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		Task:           "step1",
		ContextData:    map[string]any{},
		Status:         status,
		ParentID:       parentID,
		SpawnTaskID:    spawnTaskID,
		CallStack:      callStack,
		Error:          errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertChild %q: %v", id, err)
	}
}

// TestRetryProcess_NonRetryableStatuses verifies that only failed and cancelled
// instances can be retried. In particular a still-draining ('failing' or
// 'cancelling') tree must settle before a retry is accepted.
func TestRetryProcess_NonRetryableStatuses(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			for _, status := range []model.Status{
				model.StatusRunning,
				model.StatusFailing,
				model.StatusCancelling,
				model.StatusCompleted,
			} {
				id := "inst-" + string(status)
				insertInst(t, b.db, id, status, "", nil, "")

				err := b.db.RetryProcess(context.Background(), id, false)
				if err == nil {
					t.Fatalf("%s: expected error, got nil", status)
				}
				if !strings.Contains(err.Error(), "not retryable") {
					t.Errorf("%s: expected 'not retryable' error, got %q", status, err)
				}
				if got := mustStatus(t, b.db, id); got != status {
					t.Errorf("%s: status should be unchanged, got %q", status, got)
				}
			}
		})
	}
}

// TestRetryProcess_NonRootRejected verifies that retrying a descendant directly is
// rejected with an error naming the tree root, leaving the tree untouched.
func TestRetryProcess_NonRootRejected(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "child failed")
			insertChild(t, b.db, "child-bad", model.StatusFailed, "root", "step1", []string{"root"}, "boom")

			err := b.db.RetryProcess(context.Background(), "child-bad", false)
			if err == nil {
				t.Fatal("expected error for non-root retry, got nil")
			}
			if !strings.Contains(err.Error(), `"root"`) {
				t.Errorf("error should name the root instance, got %q", err)
			}
			if got := mustStatus(t, b.db, "child-bad"); got != model.StatusFailed {
				t.Errorf("child-bad: expected failed (untouched), got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: expected failed (untouched), got %q", got)
			}
		})
	}
}

// TestRetryProcess_FailedTree_RevivesOnlyFailedLeaf verifies that retrying the
// root of a failed tree revives only the failed leaf and reconstructs the root
// as running+waiting, leaving completed siblings untouched.
func TestRetryProcess_FailedTree_RevivesOnlyFailedLeaf(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "child failed")
			insertChild(t, b.db, "child-ok", model.StatusCompleted, "parent", "step1", []string{"parent"}, "")
			insertChild(t, b.db, "child-bad", model.StatusFailed, "parent", "step1", []string{"parent"}, "something broke")

			if err := b.db.RetryProcess(context.Background(), "parent", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "child-bad"); got != model.StatusRunning {
				t.Errorf("child-bad: expected running, got %q", got)
			}
			if got := mustError(t, b.db, "child-bad"); got != "" {
				t.Errorf("child-bad: error should be cleared, got %q", got)
			}
			if got := mustStatus(t, b.db, "child-ok"); got != model.StatusCompleted {
				t.Errorf("child-ok: expected completed (untouched), got %q", got)
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusRunning {
				t.Errorf("parent: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
		})
	}
}

// TestRetryProcess_FailedTree_RevivesAllFailedChildren verifies that a root
// retry revives every failed child of the pending spawn task in one pass.
func TestRetryProcess_FailedTree_RevivesAllFailedChildren(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusFailed, "", nil, "first child error")
			insertChild(t, b.db, "child-bad-1", model.StatusFailed, "parent", "step1", []string{"parent"}, "first child error")
			insertChild(t, b.db, "child-bad-2", model.StatusFailed, "parent", "step1", []string{"parent"}, "second child error")

			if err := b.db.RetryProcess(context.Background(), "parent", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			for _, id := range []string{"child-bad-1", "child-bad-2"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
			}
			if got := mustStatus(t, b.db, "parent"); got != model.StatusRunning {
				t.Errorf("parent: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateWaiting {
				t.Errorf("parent: expected wait_state=waiting, got %q", got)
			}
		})
	}
}

// TestRetryProcess_FailedTree_DeepChain verifies revival of a multi-level failed
// tree: the origin leaf re-runs, every intermediate ancestor returns to
// running+waiting.
func TestRetryProcess_FailedTree_DeepChain(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "boom")
			insertChild(t, b.db, "mid", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
			insertChild(t, b.db, "leaf", model.StatusFailed, "mid", "step1", []string{"root", "mid"}, "boom")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "leaf"); got != model.StatusRunning {
				t.Errorf("leaf: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "leaf"); got != model.WaitStateNone {
				t.Errorf("leaf: expected wait_state none, got %q", got)
			}
			for _, id := range []string{"root", "mid"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
				if got := mustWaitState(t, b.db, id); got != model.WaitStateWaiting {
					t.Errorf("%q: expected wait_state=waiting, got %q", id, got)
				}
				if got := mustError(t, b.db, id); got != "" {
					t.Errorf("%q: error should be cleared, got %q", id, got)
				}
			}
		})
	}
}

// TestRetryProcess_CancelledTree_ReconstructsWaiting verifies that retrying a
// cancelled root whose spawn task has an unfinished child revives the child and
// reconstructs the root as waiting.
func TestRetryProcess_CancelledTree_ReconstructsWaiting(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusCancelled, "", nil, "")
			insertChild(t, b.db, "c-done", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-cancelled", model.StatusCancelled, "root", "step1", []string{"root"}, "")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "c-cancelled"); got != model.StatusRunning {
				t.Errorf("c-cancelled: expected running, got %q", got)
			}
			if got := mustStatus(t, b.db, "c-done"); got != model.StatusCompleted {
				t.Errorf("c-done: expected completed (untouched), got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: expected wait_state=waiting, got %q", got)
			}
		})
	}
}

// TestRetryProcess_CancelledTree_ReconstructsCollecting verifies that retrying a
// cancelled root whose spawn-task children all completed revives it straight to
// collecting, so the engine re-runs the lost collect.
func TestRetryProcess_CancelledTree_ReconstructsCollecting(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusCancelled, "", nil, "")
			insertChild(t, b.db, "c1", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c2", model.StatusCompleted, "root", "step1", []string{"root"}, "")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateCollecting {
				t.Errorf("root: expected wait_state=collecting, got %q", got)
			}
			for _, id := range []string{"c1", "c2"} {
				if got := mustStatus(t, b.db, id); got != model.StatusCompleted {
					t.Errorf("%q: expected completed (untouched), got %q", id, got)
				}
			}
		})
	}
}

// TestRetryProcess_Cancelled_RerunsPendingStep verifies that a cancelled
// instance whose pending task spawned nothing simply re-runs it (wait_state none).
func TestRetryProcess_Cancelled_RerunsPendingStep(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusCancelled, "", nil, "")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateNone {
				t.Errorf("root: expected wait_state none, got %q", got)
			}
		})
	}
}

// TestRetryProcess_EmptyQueue verifies that an instance interrupted between its
// last task and the completed write revives cleanly; advance() finishes it.
func TestRetryProcess_EmptyQueue(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := &model.ProcessInstance{
				ID:          "root",
				ProcessName: "test",
				Task:        "",
				ContextData: map[string]any{},
				Status:      model.StatusCancelled,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusRunning {
				t.Errorf("root: expected running, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateNone {
				t.Errorf("root: expected wait_state none, got %q", got)
			}
		})
	}
}

// TestFailInstanceAndAncestors_LastActiveChild_WakesParent verifies that when
// the failing child is the last active member of its spawn batch, the parent
// is marked failing AND woken (wait_state ”) so the engine can settle it —
// never 'collecting': that state is reserved for all-completed batches.
func TestFailInstanceAndAncestors_LastActiveChild_WakesParent(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			insertChild(t, b.db, "c-done", model.StatusCompleted, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.Error = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "c-bad"); got != model.StatusFailed {
				t.Errorf("c-bad: expected failed, got %q", got)
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: expected failing, got %q", got)
			}
			// All batch children terminal → parent woken so the engine can
			// claim it and settle failing → failed. The wake is to '' (not
			// 'collecting') because a failing parent never merges outputs.
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateNone {
				t.Errorf("root: expected wait_state none, got %q", got)
			}
			if msg := mustError(t, b.db, "root"); msg != "boom" {
				t.Errorf("root: expected error \"boom\", got %q", msg)
			}
		})
	}
}

// TestFailInstanceAndAncestors_SiblingStillRunning verifies that a failure with
// a still-active sibling leaves the parent failing+waiting — it drains until
// the sibling settles.
func TestFailInstanceAndAncestors_SiblingStillRunning(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "root", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			insertChild(t, b.db, "c-running", model.StatusRunning, "root", "step1", []string{"root"}, "")
			insertChild(t, b.db, "c-bad", model.StatusRunning, "root", "step1", []string{"root"}, "")

			child, err := b.db.GetInstance("c-bad")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusFailed
			child.Error = "boom"
			if err := b.db.FailInstanceAndAncestors(child); err != nil {
				t.Fatalf("FailInstanceAndAncestors: %v", err)
			}

			if got := mustStatus(t, b.db, "root"); got != model.StatusFailing {
				t.Errorf("root: expected failing, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: expected wait_state=waiting (sibling active), got %q", got)
			}
			if got := mustStatus(t, b.db, "c-running"); got != model.StatusRunning {
				t.Errorf("c-running: expected running (untouched), got %q", got)
			}
		})
	}
}

// TestRetryProcess_MixedFailUnderCancel verifies a tree where a child failure
// overrode the root's cancellation: both the failed and the cancelled child of
// the pending spawn task are revived.
func TestRetryProcess_MixedFailUnderCancel(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "child failed")
			insertChild(t, b.db, "c-failed", model.StatusFailed, "root", "step1", []string{"root"}, "boom")
			insertChild(t, b.db, "c-cancelled", model.StatusCancelled, "root", "step1", []string{"root"}, "")

			if err := b.db.RetryProcess(context.Background(), "root", false); err != nil {
				t.Fatalf("RetryProcess: %v", err)
			}

			for _, id := range []string{"c-failed", "c-cancelled"} {
				if got := mustStatus(t, b.db, id); got != model.StatusRunning {
					t.Errorf("%q: expected running, got %q", id, got)
				}
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: expected wait_state=waiting, got %q", got)
			}
		})
	}
}

// TestRetryProcess_OnlyOnce_RejectedUnlessForced verifies that retrying a
// process whose pending task is marked only_once is rejected, and that force
// overrides the protection.
func TestRetryProcess_OnlyOnce_RejectedUnlessForced(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			trueVal := true
			// only_once now lives in the definition, not on the instance.
			saveDef(t, b.db, "oo", 1, []*model.Task{{ID: "step1", OnlyOnce: &trueVal}})
			inst := &model.ProcessInstance{
				ID:             "locked",
				ProcessName:    "oo",
				ProcessVersion: 1,
				Task:           "step1",
				ContextData:    map[string]any{},
				Status:         model.StatusFailed,
				Error:          "failed on only_once task",
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			err := b.db.RetryProcess(context.Background(), "locked", false)
			if err == nil {
				t.Fatal("expected error for only_once task, got nil")
			}
			if mustStatus(t, b.db, "locked") != model.StatusFailed {
				t.Error("status should remain failed after rejected retry")
			}

			// force overrides the protection
			if err := b.db.RetryProcess(context.Background(), "locked", true); err != nil {
				t.Fatalf("RetryProcess force: %v", err)
			}
			if got := mustStatus(t, b.db, "locked"); got != model.StatusRunning {
				t.Errorf("expected running after force retry, got %q", got)
			}
		})
	}
}

// TestRetryProcess_OnlyOnceDeep_RollsBack verifies that an only_once rejection
// deep in the tree aborts the whole transaction — no node is changed.
func TestRetryProcess_OnlyOnceDeep_RollsBack(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			trueVal := true
			// only_once now lives in the definition, not on the instance.
			saveDef(t, b.db, "oo", 1, []*model.Task{{ID: "step1", OnlyOnce: &trueVal}})
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "child failed")
			leaf := &model.ProcessInstance{
				ID:             "leaf",
				ProcessName:    "oo",
				ProcessVersion: 1,
				Task:           "step1",
				ContextData:    map[string]any{},
				Status:         model.StatusFailed,
				ParentID:       "root",
				SpawnTaskID:    "step1",
				CallStack:      []string{"root"},
				Error:          "boom",
			}
			if err := b.db.SaveInstance(leaf); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			err := b.db.RetryProcess(context.Background(), "root", false)
			if err == nil {
				t.Fatal("expected error for only_once leaf, got nil")
			}
			if got := mustStatus(t, b.db, "root"); got != model.StatusFailed {
				t.Errorf("root: expected failed (rolled back), got %q", got)
			}
			if got := mustStatus(t, b.db, "leaf"); got != model.StatusFailed {
				t.Errorf("leaf: expected failed (rolled back), got %q", got)
			}
			if got := mustError(t, b.db, "leaf"); got != "boom" {
				t.Errorf("leaf: error should be unchanged, got %q", got)
			}

			// force revives the whole path
			if err := b.db.RetryProcess(context.Background(), "root", true); err != nil {
				t.Fatalf("RetryProcess force: %v", err)
			}
			if got := mustStatus(t, b.db, "leaf"); got != model.StatusRunning {
				t.Errorf("leaf: expected running after force, got %q", got)
			}
			if got := mustWaitState(t, b.db, "root"); got != model.WaitStateWaiting {
				t.Errorf("root: expected wait_state=waiting after force, got %q", got)
			}
		})
	}
}

// TestFinishChild_StepScoped verifies that sibling counting is scoped to the
// spawn task: a straggler from another batch must not keep the parent waiting.
func TestFinishChild_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			// Leftover running child from an earlier spawn task.
			insertChild(t, b.db, "old-straggler", model.StatusRunning, "parent", "taskA", []string{"parent"}, "")
			// Current batch: a single child of taskB.
			insertChild(t, b.db, "current", model.StatusRunning, "parent", "taskB", []string{"parent"}, "")

			child, err := b.db.GetInstance("current")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusCompleted
			if err := b.db.FinishChild(child); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}

			// The taskB batch is done — parent must wake even though a taskA
			// child is still running.
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateCollecting {
				t.Errorf("parent: expected wait_state=collecting, got %q", got)
			}
		})
	}
}

// TestChildrenForStep_StepScoped verifies that reading a task's children returns
// only that task's batch, not earlier batches spawned by the same parent.
func TestChildrenForStep_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusRunning, "", nil, "")
			oldChild := &model.ProcessInstance{
				ID: "old", ProcessName: "test", Task: "",
				ContextData: map[string]any{"output": "stale"},
				Status:      model.StatusCompleted,
				ParentID:    "parent", SpawnTaskID: "taskA", CallStack: []string{"parent"},
			}
			newChild := &model.ProcessInstance{
				ID: "new", ProcessName: "test", Task: "",
				ContextData: map[string]any{"output": "fresh"},
				Status:      model.StatusCompleted,
				ParentID:    "parent", SpawnTaskID: "taskB", CallStack: []string{"parent"},
			}
			for _, c := range []*model.ProcessInstance{oldChild, newChild} {
				if err := b.db.SaveInstance(c); err != nil {
					t.Fatalf("SaveInstance %q: %v", c.ID, err)
				}
			}

			kids, err := b.db.ChildrenForTask(context.Background(), "parent", "taskB")
			if err != nil {
				t.Fatalf("ChildrenForTask: %v", err)
			}
			if len(kids) != 1 {
				t.Fatalf("expected 1 child for taskB, got %d", len(kids))
			}
			if kids[0].ID != "new" {
				t.Errorf("expected child %q, got %q", "new", kids[0].ID)
			}
			if got := kids[0].ContextData["output"]; got != "fresh" {
				t.Errorf("child output: expected %q, got %v", "fresh", got)
			}
		})
	}
}
