package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
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
		eng := New(database, pollEvery, 10, true /* immediateRetries */, log)
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
