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
// the same SQLite database, each polling at 1 ms, and verifies that each step of
// each process instance is executed exactly once — no double-execution despite
// workers all racing to claim the same pool of instances.
func TestStress_MultiWorker_ExactlyOnce(t *testing.T) {
	const (
		instanceCount = 20
		engineCount   = 4
		pollEvery     = 1 * time.Millisecond
	)

	database := openStressDB(t)

	// Counting server: records how many times each (instanceID, stepID) pair is called.
	var mu sync.Mutex
	callCounts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			InstanceID string `json:"instance_id"`
			StepID     string `json:"step_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		callCounts[req.InstanceID+"/"+req.StepID]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// Two-step definition: step-a → step-b → end.
	processName := fmt.Sprintf("stress-once-%d", time.Now().UnixNano())
	steps := []*model.Step{
		{
			ID:     "step-a",
			Call:   &model.Call{Type: model.CallTypeREST, Endpoint: srv.URL},
			Switch: model.SwitchMap{{Goto: model.GotoNext}},
		},
		{
			ID:     "step-b",
			Call:   &model.Call{Type: model.CallTypeREST, Endpoint: srv.URL},
			Switch: model.SwitchMap{{Goto: model.GotoEnd}},
		},
	}
	def := &model.ProcessDefinition{Name: processName, Steps: steps}
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
			StepQueue:      steps,
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

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var wg sync.WaitGroup
	for e := 0; e < engineCount; e++ {
		wg.Add(1)
		eng := New(database, pollEvery, 10, true /* immediateRetries */, 0, 0 /* default lease */, LogConfig{}, log)
		go func() {
			defer wg.Done()
			eng.Run(ctx)
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

	// Each step must have been called exactly once per instance.
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
		for _, stepID := range []string{"step-a", "step-b"} {
			if n := callCounts[id+"/"+stepID]; n != 1 {
				t.Errorf("instance %s step %s: called %d times, want 1", id, stepID, n)
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
	// Per (instance, step) it tracks whether a 200 was already served: repeat
	// calls are legitimate only after failures (automatic on_error retries or a
	// manual retry of a failed step), so any call arriving after a success is a
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
			StepID     string `json:"step_id"`
		}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		key := req.InstanceID + "/" + req.StepID

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
	// Every REST step gets one automatic retry on http.% so the on_error path
	// is exercised too (immediateRetries → no backoff).
	uid := time.Now().UnixNano()
	leafName := fmt.Sprintf("chaos-leaf-%d", uid)
	midName := fmt.Sprintf("chaos-mid-%d", uid)
	rootName := fmt.Sprintf("chaos-root-%d", uid)

	restStep := func(id string, last bool) *model.Step {
		gt := model.GotoNext
		if last {
			gt = model.GotoEnd
		}
		return &model.Step{
			ID:      id,
			Call:    &model.Call{Type: model.CallTypeREST, Endpoint: srv.URL},
			OnError: []model.ErrorCase{{Code: []string{"http.%"}, Retries: 1}},
			Switch:  model.SwitchMap{{Goto: gt}},
		}
	}
	leafDef := &model.ProcessDefinition{Name: leafName, Steps: []*model.Step{
		restStep("work-1", false),
		restStep("work-2", true),
	}}
	midDef := &model.ProcessDefinition{Name: midName, Steps: []*model.Step{
		{
			ID: "fanout",
			Call: &model.Call{Type: model.CallTypeChildParallel, Children: map[string]model.ChildEntry{
				"a": {Name: leafName},
				"b": {Name: leafName},
			}},
			Switch: model.SwitchMap{{Goto: model.GotoNext}},
		},
		restStep("mid-work", true),
	}}
	rootDef := &model.ProcessDefinition{Name: rootName, Steps: []*model.Step{
		{
			ID:     "spawn-mid",
			Call:   &model.Call{Type: model.CallTypeChild, Name: midName},
			Switch: model.SwitchMap{{Goto: model.GotoNext}},
		},
		restStep("root-work", true),
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
			StepQueue:      rootDef.Steps,
			ContextData:    map[string]any{},
			Status:         model.StatusRunning,
		}
		if err := database.SaveInstance(inst); err != nil {
			t.Fatalf("SaveInstance %s: %v", id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var engines sync.WaitGroup
	for e := 0; e < engineCount; e++ {
		engines.Add(1)
		eng := New(database, pollEvery, 20, true /* immediateRetries */, 0, 0 /* default lease */, LogConfig{}, log)
		go func() {
			defer engines.Done()
			eng.Run(ctx)
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

	// Exactly-once on success: a step that returned 200 must never have been
	// called again — cancels, retries, and on_error retries may only re-run
	// steps whose previous attempts failed.
	callMu.Lock()
	for _, key := range doubleExecs {
		t.Errorf("step executed again after a successful call: %s", key)
	}
	stepsRun := len(succeeded)
	callMu.Unlock()

	// And since every tree completed, every REST step succeeded exactly once.
	// REST steps per tree: root-work(1) + mid-work(1) + 2 leaves × work-1/2(2) = 6.
	if want := rootCount * 6; stepsRun != want {
		t.Errorf("distinct successful (instance, step) pairs = %d, want %d", stepsRun, want)
	}
	t.Logf("instances: %d (roots %d), HTTP calls: %d, distinct successful steps: %d",
		len(insts), rootCount, totalCalls.Load(), stepsRun)
}

// TestGracefulShutdown_ReleasesLeases verifies that a clean shutdown (ctx cancel)
// drains in-flight work and releases the worker's leases. That invariant is what
// keeps a healthy restart quiet: a released lease (worker_id NULL) is reclaimed as
// a clean claim, so the engine never logs a "reclaimed instance with expired lease"
// takeover warning. Such warnings should only appear after a hard crash, where the
// lease is left held.
func TestGracefulShutdown_ReleasesLeases(t *testing.T) {
	database := openStressDB(t)

	// Signals when the step is hit, then blocks until the test releases it, so the
	// step stays in-flight (lease held) right up to shutdown. On shutdown the engine
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
	steps := []*model.Step{{
		ID:     "work",
		Call:   &model.Call{Type: model.CallTypeREST, Endpoint: srv.URL},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: processName, Steps: steps}, 1, nil, "graceful-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}

	instID := fmt.Sprintf("gi-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID:             instID,
		ProcessName:    processName,
		ProcessVersion: 1,
		StepQueue:      steps,
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

	// Wait until the engine has claimed the instance and the step is in-flight.
	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("step never went in-flight")
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
