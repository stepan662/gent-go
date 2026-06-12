package db

import (
	"context"
	"strings"
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

func mustWaitState(t *testing.T, db *DB, id string) model.WaitState {
	t.Helper()
	inst, err := db.GetInstance(id)
	if err != nil {
		t.Fatalf("GetInstance %q: %v", id, err)
	}
	return inst.WaitState
}

// insertInstW inserts an instance with an explicit wait_state (for testing waiting parents).
func insertInstW(t *testing.T, db *DB, id string, status model.Status, waitState model.WaitState, parentID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		StepQueue:      []*model.Step{{ID: "step1"}},
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

// TestCancelProcess_NonRoot verifies that cancelling a descendant directly is
// rejected with an error naming the tree root, leaving the tree untouched.
func TestCancelProcess_NonRoot(t *testing.T) {
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
				StepQueue:   []*model.Step{{ID: "step1"}},
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

// insertChild inserts a child instance spawned by the given parent step.
func insertChild(t *testing.T, db *DB, id string, status model.Status, parentID, spawnStepID string, callStack []string, errMsg string) {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             id,
		ProcessName:    "test",
		ProcessVersion: 1,
		StepQueue:      []*model.Step{{ID: "step1"}},
		ContextData:    map[string]any{},
		Status:         status,
		ParentID:       parentID,
		SpawnStepID:    spawnStepID,
		CallStack:      callStack,
		Error:          errMsg,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("insertChild %q: %v", id, err)
	}
}

// TestRetryProcess_NonRoot verifies that retrying a descendant directly is
// rejected with an error naming the tree root, leaving the tree untouched.
func TestRetryProcess_NonRoot(t *testing.T) {
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

// TestRetryProcess_FailedLeaf verifies that retrying the root of a failed tree
// revives only the failed leaf and reconstructs the root as running+waiting,
// leaving completed siblings untouched.
func TestRetryProcess_FailedLeaf(t *testing.T) {
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

// TestRetryProcess_MultipleFailedChildren verifies that a root retry revives
// every failed child of the pending spawn step in one pass.
func TestRetryProcess_MultipleFailedChildren(t *testing.T) {
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

// TestRetryProcess_DeepFailedTree verifies revival of a multi-level failed tree:
// the origin leaf re-runs, every intermediate ancestor returns to running+waiting.
func TestRetryProcess_DeepFailedTree(t *testing.T) {
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

// TestRetryProcess_CancelledTree_Waiting verifies that retrying a cancelled root
// whose spawn step has an unfinished child revives the child and reconstructs
// the root as waiting.
func TestRetryProcess_CancelledTree_Waiting(t *testing.T) {
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

// TestRetryProcess_CancelledTree_Collecting verifies that retrying a cancelled
// root whose spawn-step children all completed revives it straight to
// collecting, so the engine re-runs the lost collect.
func TestRetryProcess_CancelledTree_Collecting(t *testing.T) {
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

// TestRetryProcess_CancelledActionStep verifies that a cancelled instance whose
// pending step spawned nothing simply re-runs it (wait_state none).
func TestRetryProcess_CancelledActionStep(t *testing.T) {
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
// last step and the completed write revives cleanly; advance() finishes it.
func TestRetryProcess_EmptyQueue(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := &model.ProcessInstance{
				ID:          "root",
				ProcessName: "test",
				StepQueue:   []*model.Step{},
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

// TestRetryProcess_MixedFailUnderCancel verifies a tree where a child failure
// overrode the root's cancellation: both the failed and the cancelled child of
// the pending spawn step are revived.
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

// TestRetryProcess_OnlyOnce verifies that retrying a process whose pending step
// is marked only_once is rejected, and that force overrides the protection.
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

			err := b.db.RetryProcess(context.Background(), "locked", false)
			if err == nil {
				t.Fatal("expected error for only_once step, got nil")
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
			insertInst(t, b.db, "root", model.StatusFailed, "", nil, "child failed")
			leaf := &model.ProcessInstance{
				ID:          "leaf",
				ProcessName: "test",
				StepQueue:   []*model.Step{{ID: "step1", OnlyOnce: &trueVal}},
				ContextData: map[string]any{},
				Status:      model.StatusFailed,
				ParentID:    "root",
				SpawnStepID: "step1",
				CallStack:   []string{"root"},
				Error:       "boom",
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
// spawn step: a straggler from another batch must not keep the parent waiting.
func TestFinishChild_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInstW(t, b.db, "parent", model.StatusRunning, model.WaitStateWaiting, "", nil, "")
			// Leftover running child from an earlier spawn step.
			insertChild(t, b.db, "old-straggler", model.StatusRunning, "parent", "stepA", []string{"parent"}, "")
			// Current batch: a single child of stepB.
			insertChild(t, b.db, "current", model.StatusRunning, "parent", "stepB", []string{"parent"}, "")

			child, err := b.db.GetInstance("current")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			child.Status = model.StatusCompleted
			if err := b.db.FinishChild(child); err != nil {
				t.Fatalf("FinishChild: %v", err)
			}

			// The stepB batch is done — parent must wake even though a stepA
			// child is still running.
			if got := mustWaitState(t, b.db, "parent"); got != model.WaitStateCollecting {
				t.Errorf("parent: expected wait_state=collecting, got %q", got)
			}
		})
	}
}

// TestCollectChildOutputs_StepScoped verifies that output collection only reads
// children of the collecting step, not earlier batches.
func TestCollectChildOutputs_StepScoped(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			insertInst(t, b.db, "parent", model.StatusRunning, "", nil, "")
			oldChild := &model.ProcessInstance{
				ID: "old", ProcessName: "test", StepQueue: []*model.Step{},
				ContextData: map[string]any{"output": "stale"},
				Status:      model.StatusCompleted,
				ParentID:    "parent", SpawnStepID: "stepA", CallStack: []string{"parent"},
			}
			newChild := &model.ProcessInstance{
				ID: "new", ProcessName: "test", StepQueue: []*model.Step{},
				ContextData: map[string]any{"output": "fresh"},
				Status:      model.StatusCompleted,
				ParentID:    "parent", SpawnStepID: "stepB", CallStack: []string{"parent"},
			}
			for _, c := range []*model.ProcessInstance{oldChild, newChild} {
				if err := b.db.SaveInstance(c); err != nil {
					t.Fatalf("SaveInstance %q: %v", c.ID, err)
				}
			}

			parent, err := b.db.GetInstance("parent")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			step := &model.Step{ID: "stepB", Call: &model.Call{Type: model.CallTypeChild}}
			if err := b.db.CollectChildOutputs(context.Background(), parent, step); err != nil {
				t.Fatalf("CollectChildOutputs: %v", err)
			}

			got := parent.ContextData["outputs"].(map[string]any)["stepB"]
			if got != "fresh" {
				t.Errorf("outputs[stepB]: expected %q, got %v", "fresh", got)
			}
		})
	}
}
