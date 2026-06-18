package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gent/internal/db"
	"gent/internal/model"
)

// TestStress_MultiWorker_ExactlyOnce starts engineCount Engine instances sharing
// the same SQLite database, each polling at 1 ms, and verifies that each task of
// each process instance is executed exactly once — no double-execution despite
// workers all racing to claim the same pool of instances.
func TestStress_MultiWorker_ExactlyOnce(t *testing.T) {
	const (
		instanceCount = 20
		engineCount   = 4
		pollEvery     = 1 * time.Millisecond
	)

	database := openStressDB(t)

	// Counting server: records how many times each (instanceID, taskID) pair is called.
	var mu sync.Mutex
	callCounts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			InstanceID string `json:"instance_id"`
			TaskID     string `json:"task_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		callCounts[req.InstanceID+"/"+req.TaskID]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// Two-task definition: task-a → task-b → end.
	processName := fmt.Sprintf("stress-once-%d", time.Now().UnixNano())
	tasks := []*model.Task{
		{
			ID:     "task-a",
			Action:   &model.Action{Type: model.ActionTypeREST, Endpoint: srv.URL},
			Switch: model.SwitchMap{{Goto: model.GotoNext}},
		},
		{
			ID:     "task-b",
			Action:   &model.Action{Type: model.ActionTypeREST, Endpoint: srv.URL},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		},
	}
	def := &model.ProcessDefinition{Name: processName, Tasks: tasks}
	if err := database.SaveDefinition(def, 1, nil, "stress-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}

	instanceIDs := make([]string, instanceCount)
	for i := 0; i < instanceCount; i++ {
		id := fmt.Sprintf("si-%d-%d", time.Now().UnixNano(), i)
		instanceIDs[i] = id
		inst := &model.ProcessInstance{
			ID:             id,
			ProcessName:    processName,
			ProcessVersion: 1,
			TaskQueue:      tasks,
			ContextData:    map[string]any{},
			Status:         model.StatusRunning,
		}
		if err := database.SaveInstance(inst); err != nil {
			t.Fatalf("SaveInstance %s: %v", id, err)
		}
	}

	// Start engineCount engines with a 1 ms poll — they will be screaming over
	// each other's ClaimInstances queries immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for e := 0; e < engineCount; e++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runEngineWithRestart(ctx, t, database, pollEvery, 10)
		}()
	}

	// Wait until every instance is terminal.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		allDone := true
		for _, id := range instanceIDs {
			inst, _ := database.GetInstance(id)
			if inst == nil || (inst.Status != model.StatusCompleted && inst.Status != model.StatusFailed) {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	wg.Wait()

	// Each task must have been called exactly once per instance.
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, n := range callCounts {
		total += n
	}
	for _, id := range instanceIDs {
		inst, err := database.GetInstance(id)
		if err != nil {
			t.Errorf("GetInstance %s: %v", id, err)
			continue
		}
		if inst.Status != model.StatusCompleted {
			t.Errorf("instance %s: status = %q, want completed", id, inst.Status)
		}
		for _, taskID := range []string{"task-a", "task-b"} {
			if n := callCounts[id+"/"+taskID]; n != 1 {
				t.Errorf("instance %s task %s: called %d times, want 1", id, taskID, n)
			}
		}
	}
	t.Logf("engines: %d, instances: %d, poll: %v", engineCount, instanceCount, pollEvery)
	t.Logf("total HTTP calls: %d (want %d)", total, instanceCount*2)
}

// TestStress_Chaos_CancelRetryRandomErrors runs a fleet of engines against
// 3-level process trees (root → mid → parallel leaves) whose REST calls fail
// randomly, while two chaos goroutines concurrently cancel and retry random
// roots. Throughout the run a checker asserts the core lifecycle invariant on
// live snapshots:
//
//	a terminal (completed/failed/cancelled) parent never has a non-terminal child
//
// — i.e. trees always settle bottom-up, and retries never revive a subtree
// under a dead parent. After the chaos window the mock turns green and the
// test drives every tree to completion via retries, proving no tree is ever
// left stuck.
//
// SQLite only: the db package's stress tests share the PostgreSQL database and
// truncate process_instances between iterations, which would race this test.
// The PostgreSQL-specific locking paths are covered in internal/db/stress_test.go.
func TestStress_Chaos_CancelRetryRandomErrors(t *testing.T) {
	const (
		rootCount   = 32 // 32 trees × 4 instances = 128 instances
		engineCount = 6
		pollEvery   = 1 * time.Millisecond
		chaosFor    = 8 * time.Second
		settleFor   = 60 * time.Second
	)

	database := openStressDB(t)

	// Mock service: fails with probability failPct/100 (atomically switchable).
	// Per (instance, task) it tracks whether a 200 was already served: repeat
	// calls are legitimate only after failures (automatic on_error retries or a
	// manual retry of a failed task), so any call arriving after a success is a
	// double execution and recorded as a violation.
	var failPct atomic.Int64
	failPct.Store(30)
	var totalCalls atomic.Int64
	var callMu sync.Mutex
	succeeded := map[string]bool{}
	var doubleExecs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		var req struct {
			InstanceID string `json:"instance_id"`
			TaskID     string `json:"task_id"`
		}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		key := req.InstanceID + "/" + req.TaskID

		fail := rand.Int63n(100) < failPct.Load()
		callMu.Lock()
		if succeeded[key] {
			doubleExecs = append(doubleExecs, key)
		}
		if !fail {
			succeeded[key] = true
		}
		callMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"chaos"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// Definitions: root ─child→ mid ─child_parallel→ {a,b} leaves.
	// Every REST task gets one automatic retry on http.% so the on_error path
	// is exercised too (immediateRetries → no backoff).
	uid := time.Now().UnixNano()
	leafName := fmt.Sprintf("chaos-leaf-%d", uid)
	midName := fmt.Sprintf("chaos-mid-%d", uid)
	rootName := fmt.Sprintf("chaos-root-%d", uid)

	restTask := func(id string, last bool) *model.Task {
		gt := model.GotoNext
		if last {
			gt = model.GotoEnd
		}
		return &model.Task{
			ID:      id,
			Action:    &model.Action{Type: model.ActionTypeREST, Endpoint: srv.URL},
			OnError: []model.ErrorCase{{Code: []string{"http.%"}, Retries: 1}},
			Switch:  model.SwitchMap{{Goto: gt}},
		}
	}
	leafDef := &model.ProcessDefinition{Name: leafName, Tasks: []*model.Task{
		restTask("work-1", false),
		restTask("work-2", true),
	}}
	midDef := &model.ProcessDefinition{Name: midName, Tasks: []*model.Task{
		{
			ID: "fanout",
			Action: &model.Action{Type: model.ActionTypeChildParallel, Children: map[string]model.ChildEntry{
				"a": {Name: leafName},
				"b": {Name: leafName},
			}},
			Switch: model.SwitchMap{{Goto: model.GotoNext}},
		},
		restTask("mid-work", true),
	}}
	rootDef := &model.ProcessDefinition{Name: rootName, Tasks: []*model.Task{
		{
			ID:     "spawn-mid",
			Action:   &model.Action{Type: model.ActionTypeChild, Name: midName},
			Switch: model.SwitchMap{{Goto: model.GotoNext}},
		},
		restTask("root-work", true),
	}}
	for _, def := range []*model.ProcessDefinition{leafDef, midDef, rootDef} {
		if err := database.SaveDefinition(def, 1, nil, "chaos-hash-"+def.Name, ""); err != nil {
			t.Fatalf("SaveDefinition %s: %v", def.Name, err)
		}
	}

	rootIDs := make([]string, rootCount)
	for i := 0; i < rootCount; i++ {
		id := fmt.Sprintf("chaos-root-%d-%d", uid, i)
		rootIDs[i] = id
		inst := &model.ProcessInstance{
			ID:             id,
			ProcessName:    rootName,
			ProcessVersion: 1,
			TaskQueue:      rootDef.Tasks,
			ContextData:    map[string]any{},
			Status:         model.StatusRunning,
		}
		if err := database.SaveInstance(inst); err != nil {
			t.Fatalf("SaveInstance %s: %v", id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var engines sync.WaitGroup
	for e := 0; e < engineCount; e++ {
		engines.Add(1)
		go func() {
			defer engines.Done()
			runEngineWithRestart(ctx, t, database, pollEvery, 20)
		}()
	}

	isTerminal := func(s model.Status) bool {
		return s == model.StatusCompleted || s == model.StatusFailed || s == model.StatusCancelled
	}

	// Invariant check on a single ListInstances snapshot (one query = one
	// consistent view). Returns a description of the first violation found.
	checkInvariant := func() string {
		insts, err := database.ListInstances("")
		if err != nil {
			return "" // transient read error under contention; not a violation
		}
		byID := make(map[string]*model.ProcessInstance, len(insts))
		for _, in := range insts {
			byID[in.ID] = in
		}
		for _, in := range insts {
			if in.ParentID == "" {
				continue
			}
			parent, ok := byID[in.ParentID]
			if !ok {
				continue
			}
			if isTerminal(parent.Status) && !isTerminal(in.Status) {
				return fmt.Sprintf("terminal parent %s (%s) has live child %s (%s)",
					parent.ID, parent.Status, in.ID, in.Status)
			}
		}
		return ""
	}

	// Chaos window: random cancels, random retries, continuous invariant checks.
	var chaos sync.WaitGroup
	chaosDone := make(chan struct{})
	var violations []string
	var vmu sync.Mutex

	chaos.Add(3)
	go func() { // canceller
		defer chaos.Done()
		for {
			select {
			case <-chaosDone:
				return
			case <-time.After(time.Duration(5+rand.Intn(15)) * time.Millisecond):
				id := rootIDs[rand.Intn(len(rootIDs))]
				database.CancelProcess(context.Background(), id) //nolint:errcheck
			}
		}
	}()
	go func() { // retrier
		defer chaos.Done()
		for {
			select {
			case <-chaosDone:
				return
			case <-time.After(time.Duration(5+rand.Intn(15)) * time.Millisecond):
				id := rootIDs[rand.Intn(len(rootIDs))]
				// Most retries race a non-settled tree and are rejected — that
				// rejection path is part of what we are stressing.
				database.RetryProcess(context.Background(), id, rand.Intn(2) == 0) //nolint:errcheck
			}
		}
	}()
	go func() { // invariant checker
		defer chaos.Done()
		for {
			select {
			case <-chaosDone:
				return
			case <-time.After(50 * time.Millisecond):
				if v := checkInvariant(); v != "" {
					vmu.Lock()
					violations = append(violations, v)
					vmu.Unlock()
				}
			}
		}
	}()

	time.Sleep(chaosFor)
	close(chaosDone)
	chaos.Wait()

	// Settlement phase: service turns green; drive every tree to completion by
	// retrying settled failed/cancelled roots. Engines keep running.
	failPct.Store(0)
	deadline := time.Now().Add(settleFor)
	for time.Now().Before(deadline) {
		insts, err := database.ListInstances("")
		if err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		allRootsCompleted := true
		allTerminal := true
		byID := make(map[string]*model.ProcessInstance, len(insts))
		for _, in := range insts {
			byID[in.ID] = in
			if !isTerminal(in.Status) {
				allTerminal = false
			}
		}
		for _, id := range rootIDs {
			root := byID[id]
			if root == nil {
				t.Fatalf("root %s disappeared", id)
			}
			switch root.Status {
			case model.StatusCompleted:
				continue
			case model.StatusFailed, model.StatusCancelled:
				allRootsCompleted = false
				database.RetryProcess(context.Background(), id, true) //nolint:errcheck
			default:
				allRootsCompleted = false // still draining or running
			}
		}
		if allRootsCompleted && allTerminal {
			break
		}
		if v := checkInvariant(); v != "" {
			vmu.Lock()
			violations = append(violations, v)
			vmu.Unlock()
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	engines.Wait()

	// Final assertions.
	vmu.Lock()
	for _, v := range violations {
		t.Errorf("invariant violation: %s", v)
	}
	vmu.Unlock()

	insts, err := database.ListInstances("")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	byID := make(map[string]*model.ProcessInstance, len(insts))
	for _, in := range insts {
		byID[in.ID] = in
	}
	for _, in := range insts {
		if !isTerminal(in.Status) {
			t.Errorf("instance %s stuck in %q (wait_state %q)", in.ID, in.Status, in.WaitState)
		}
		if isTerminal(in.Status) && in.WaitState != model.WaitStateNone {
			t.Errorf("terminal instance %s has wait_state %q", in.ID, in.WaitState)
		}
		if in.ParentID != "" {
			parent := byID[in.ParentID]
			if parent != nil && isTerminal(parent.Status) && !isTerminal(in.Status) {
				t.Errorf("final state: terminal parent %s has live child %s (%s)", parent.ID, in.ID, in.Status)
			}
		}
	}
	for _, id := range rootIDs {
		if got := byID[id].Status; got != model.StatusCompleted {
			t.Errorf("root %s: status %q, want completed after green retries", id, got)
		}
	}

	// Exactly-once on success: a task that returned 200 must never have been
	// called again — cancels, retries, and on_error retries may only re-run
	// tasks whose previous attempts failed.
	callMu.Lock()
	for _, key := range doubleExecs {
		t.Errorf("task executed again after a successful call: %s", key)
	}
	tasksRun := len(succeeded)
	callMu.Unlock()

	// And since every tree completed, every REST task succeeded exactly once.
	// REST tasks per tree: root-work(1) + mid-work(1) + 2 leaves × work-1/2(2) = 6.
	if want := rootCount * 6; tasksRun != want {
		t.Errorf("distinct successful (instance, task) pairs = %d, want %d", tasksRun, want)
	}
	t.Logf("instances: %d (roots %d), HTTP calls: %d, distinct successful tasks: %d",
		len(insts), rootCount, totalCalls.Load(), tasksRun)
}

// TestGracefulShutdown_ReleasesLeases verifies that a clean shutdown (ctx cancel)
// drains in-flight work and releases the worker's leases. That invariant is what
// keeps a healthy restart quiet: a released lease (worker_id NULL) is reclaimed as
// a clean claim, so the engine never logs a "reclaimed instance with expired lease"
// takeover warning. Such warnings should only appear after a hard crash, where the
// lease is left held.
func TestGracefulShutdown_ReleasesLeases(t *testing.T) {
	database := openStressDB(t)

	// Signals when the task is hit, then blocks until the test releases it, so the
	// task stays in-flight (lease held) right up to shutdown. On shutdown the engine
	// aborts its request client-side (transport.Send returns), so the assertions run
	// without needing the handler to unblock; we close release at the end purely so
	// srv.Close() doesn't wait on the leaked handler goroutine.
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		<-release
	}))
	defer srv.Close()
	defer close(release) // LIFO: runs before srv.Close() so the handler can exit

	processName := fmt.Sprintf("graceful-%d", time.Now().UnixNano())
	tasks := []*model.Task{{
		ID:     "work",
		Action:   &model.Action{Type: model.ActionTypeREST, Endpoint: srv.URL},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: processName, Tasks: tasks}, 1, nil, "graceful-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}

	instID := fmt.Sprintf("gi-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID:             instID,
		ProcessName:    processName,
		ProcessVersion: 1,
		TaskQueue:      tasks,
		ContextData:    map[string]any{},
		Status:         model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(database, time.Millisecond, 10, true /* immediateRetries */, 0, 0 /* default lease */, LogConfig{}, log)

	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	// Wait until the engine has claimed the instance and the task is in-flight.
	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("task never went in-flight")
	}

	// Sanity: an in-flight instance holds a lease.
	if got, err := database.GetInstance(instID); err != nil {
		t.Fatalf("GetInstance (in-flight): %v", err)
	} else if got.WorkerID == nil {
		t.Fatal("expected the in-flight instance to hold a lease (worker_id set)")
	}

	// Graceful shutdown: cancel and wait for Run to drain + return.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("engine did not shut down within 10s")
	}

	// The drain must have released the lease — otherwise a restart would treat the
	// instance as a takeover and log a spurious warning.
	got, err := database.GetInstance(instID)
	if err != nil {
		t.Fatalf("GetInstance (after shutdown): %v", err)
	}
	if got.WorkerID != nil {
		t.Fatalf("graceful shutdown left a held lease (worker_id=%q); a restart would log a spurious takeover warning", *got.WorkerID)
	}
	if got.LeaseExpiresAt != nil {
		t.Fatal("graceful shutdown left lease_expires_at set; expected it cleared")
	}
}

// TestOverwhelm_GracefulExit deterministically forces the overwhelm condition and
// shows the new behaviour: the engine stops the pump, drains the in-flight advance,
// and Run returns an *OverwhelmError (no os.Exit). A task blocks server-side well past
// a deliberately tiny 1s lease (with the renewer far enough out that it can't save it),
// and maxConcurrent=2 leaves a free slot for the pump to re-claim the still-in-flight
// instance on. Engine logs go to stderr so the diagnostic is visible under `-v`.
func TestOverwhelm_GracefulExit(t *testing.T) {
	database := openStressDB(t)

	const blockFor = 3 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(blockFor) // keep the advance in-flight past the lease
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	name := fmt.Sprintf("overwhelm-%d", time.Now().UnixNano())
	tasks := []*model.Task{{
		ID:     "slow",
		Action:   &model.Action{Type: model.ActionTypeREST, Endpoint: srv.URL},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: name, Tasks: tasks}, 1, nil, "overwhelm-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("oi-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: name, ProcessVersion: 1,
		TaskQueue: tasks, ContextData: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	// 1s lease, renew interval a minute out (so the renewer never re-stamps it),
	// maxConcurrent=2 (a free slot to re-claim on), fast poll. The test asserts the
	// behaviour and t.Logf's the captured error, so the engine's own logs are
	// discarded to keep passing runs quiet.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(database, 10*time.Millisecond, 2, true /* immediateRetries */, 1*time.Second, time.Minute, LogConfig{}, log)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := eng.Run(ctx) // blocks until the pump stops and in-flight work drains
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an OverwhelmError, got nil (engine shut down cleanly)")
	}
	oe, ok := err.(*OverwhelmError)
	if !ok {
		t.Fatalf("expected *OverwhelmError, got %T: %v", err, err)
	}

	t.Logf("Run returned after %v (drained in-flight work before returning)", elapsed.Round(50*time.Millisecond))
	t.Logf("error: %v", oe)
	t.Logf("fields: instance=%s worker=%s lease=%s max_concurrent=%d", oe.InstanceID, oe.WorkerID, oe.Lease, oe.MaxConcurrent)

	// The drained advance ran to completion, so the claimed instance finished.
	if inst, gerr := database.GetInstance(id); gerr != nil {
		t.Errorf("GetInstance: %v", gerr)
	} else {
		t.Logf("instance %s final status after drain: %s", id, inst.Status)
		if inst.Status != model.StatusCompleted {
			t.Errorf("expected the in-flight instance to finish (completed), got %q", inst.Status)
		}
	}
	if elapsed < blockFor-time.Second {
		t.Errorf("Run returned in %v, too fast to have drained the %v in-flight task", elapsed, blockFor)
	}
}

// runEngineWithRestart runs one engine until ctx is cancelled, restarting it if it
// stops with an *OverwhelmError — exactly what a process supervisor does for a worker
// fleet in production (the scenario these tests emulate). The overwhelm is logged
// (visible under `go test -v`, which `make test-stress` uses) but isn't a hard failure
// by itself: it's an artifact of an oversubscribed/slow runner, not a correctness bug,
// and the test's own assertions still gate correctness — genuine breakage surfaces as
// work that never completes.
func runEngineWithRestart(ctx context.Context, t *testing.T, database *db.DB, pollEvery time.Duration, maxConcurrent int) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for {
		eng := New(database, pollEvery, maxConcurrent, true /* immediateRetries */, 0, 0 /* default lease */, LogConfig{}, log)
		err := eng.Run(ctx)
		if err == nil {
			return // clean shutdown (ctx cancelled)
		}
		if oe, ok := err.(*OverwhelmError); ok {
			t.Logf("engine overwhelmed, restarting (a supervisor would too): %v", oe)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		t.Errorf("engine Run returned unexpected error: %v", err)
		return
	}
}

func openStressDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp("", "gent-stress-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	path := f.Name()
	t.Cleanup(func() { os.Remove(path) })
	database, err := db.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}
