package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/model"
)

// TestGracefulShutdown_ReleasesLeases verifies that a clean shutdown (ctx cancel)
// drains in-flight work and releases the worker's leases. That invariant is what
// keeps a healthy restart quiet: a released lease (worker_id NULL) is reclaimed as
// a clean claim, so the engine never logs a "reclaimed instance with expired lease"
// takeover warning. Such warnings should only appear after a hard crash, where the
// lease is left held.
func TestGracefulShutdown_ReleasesLeases(t *testing.T) {
	database := openTestDB(t)

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
		Action: &model.Action{Type: model.ActionTypeREST, Endpoint: srv.URL},
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
		Task:           tasks[0].ID,
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
	database := openTestDB(t)

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
		Action: &model.Action{Type: model.ActionTypeREST, Endpoint: srv.URL},
		Switch: model.SwitchMap{{Goto: model.GotoEnd}},
	}}
	if err := database.SaveDefinition(&model.ProcessDefinition{Name: name, Tasks: tasks}, 1, nil, "overwhelm-hash", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	id := fmt.Sprintf("oi-%d", time.Now().UnixNano())
	if err := database.SaveInstance(&model.ProcessInstance{
		ID: id, ProcessName: name, ProcessVersion: 1,
		Task: tasks[0].ID, ContextData: map[string]any{}, Status: model.StatusRunning,
	}); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	// 100ms lease, renew interval a minute out (so the renewer never re-stamps it),
	// maxConcurrent=2 (a free slot to re-claim on), fast poll. The test asserts the
	// behaviour and t.Logf's the captured error, so the engine's own logs are
	// discarded to keep passing runs quiet.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := New(database, 10*time.Millisecond, 2, true /* immediateRetries */, 100*time.Millisecond, time.Minute, LogConfig{}, log)

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

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp("", "genroc-test-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	path := f.Name()
	t.Cleanup(func() { os.Remove(path) })
	database, err := db.OpenSQLite(path, "")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}
