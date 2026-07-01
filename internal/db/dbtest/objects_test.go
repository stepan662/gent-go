package dbtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/model"
)

// bigString returns a value larger than the externalization threshold (8 KiB) so it
// is stored in process_objects rather than inline.
func bigString(tag string) string {
	return tag + ":" + strings.Repeat("x", 10*1024)
}

// TestObjects_BigValueRoundTrip verifies that a large value-slot is externalized,
// resolves back to the same value via HydrateContext, and that a small value stays
// inline.
func TestObjects_BigValueRoundTrip(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			big := bigString("input")
			inst := &model.ProcessInstance{
				ID:          "inst-big",
				ProcessName: "test",
				Task:        "",
				ContextData: map[string]any{
					"input":   big,
					"outputs": map[string]any{"small": "tiny", "huge": bigString("out")},
				},
				Status: model.StatusRunning,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			got, err := b.db.GetInstance("inst-big")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			// Big slots come back as lazy markers, small ones inline.
			if _, isRef := got.ContextData["input"].(*model.ObjectRef); !isRef {
				t.Fatalf("expected input to be an *ObjectRef marker, got %T", got.ContextData["input"])
			}
			outs := got.ContextData["outputs"].(map[string]any)
			if outs["small"] != "tiny" {
				t.Errorf("small output: got %v, want tiny", outs["small"])
			}
			if _, isRef := outs["huge"].(*model.ObjectRef); !isRef {
				t.Fatalf("expected huge output to be a marker, got %T", outs["huge"])
			}

			if err := b.db.HydrateContext(got); err != nil {
				t.Fatalf("HydrateContext: %v", err)
			}
			if got.ContextData["input"] != big {
				t.Errorf("hydrated input mismatch")
			}
			if got.ContextData["outputs"].(map[string]any)["huge"] != bigString("out") {
				t.Errorf("hydrated huge output mismatch")
			}
		})
	}
}

// TestObjects_DerefDeletesImmediately verifies that recomputing a task output with a
// different big value (a looping task) deletes the old object right away — a replaced
// value does not linger — while the new one resolves.
func TestObjects_DerefDeletesImmediately(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectRetention(time.Hour)

			inst := &model.ProcessInstance{
				ID:          "inst-deref",
				ProcessName: "test",
				Task:        "",
				ContextData: map[string]any{"outputs": map[string]any{"loop": bigString("v1")}},
				Status:      model.StatusRunning,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}

			// Reload (so the output is a marker carrying its hash), capture the old ref,
			// then recompute it with a different big value and persist progress.
			r1, err := b.db.GetInstance("inst-deref")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			oldRef, ok := r1.ContextData["outputs"].(map[string]any)["loop"].(*model.ObjectRef)
			if !ok {
				t.Fatalf("expected loop output to be a marker")
			}
			r1.ContextData["outputs"].(map[string]any)["loop"] = bigString("v2")
			if err := b.db.UpdateInstanceProgress(r1); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}

			// The old object is gone immediately — not waiting for any retention horizon.
			if _, err := b.db.ResolveObject(context.Background(), "inst-deref", oldRef); err == nil {
				t.Fatalf("expected old object to be deleted immediately on dereference")
			}
			// The new value resolves.
			r2, err := b.db.GetInstance("inst-deref")
			if err != nil {
				t.Fatalf("GetInstance after overwrite: %v", err)
			}
			if err := b.db.HydrateContext(r2); err != nil {
				t.Fatalf("HydrateContext: %v", err)
			}
			if r2.ContextData["outputs"].(map[string]any)["loop"] != bigString("v2") {
				t.Errorf("v2 output not preserved")
			}
		})
	}
}

// TestObjects_LogReferencedSurvivesDeref verifies that an object a log references is
// NOT deleted when the context slot sharing it is dereferenced — it stays fetchable
// via the log endpoint until the retention horizon, then the GC sweep reclaims it.
func TestObjects_LogReferencedSurvivesDeref(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			b.db.SetObjectRetention(time.Hour)

			// A secret-free value: its context object and its (pre-redacted) log object
			// are byte-identical, so they share one row.
			val := bigString("shared")
			content, _ := json.Marshal(val) // exactly what the context encoder stores

			// Context-reference it via an instance output...
			inst := &model.ProcessInstance{
				ID:          "inst-shared",
				ProcessName: "test",
				Task:        "",
				ContextData: map[string]any{"outputs": map[string]any{"out": val}},
				Status:      model.StatusRunning,
			}
			if err := b.db.SaveInstance(inst); err != nil {
				t.Fatalf("SaveInstance: %v", err)
			}
			// ...and log-reference the identical content (shares the same row).
			ref, err := b.db.WriteLogObject("inst-shared", string(content))
			if err != nil {
				t.Fatalf("WriteLogObject: %v", err)
			}

			// Dereference the context slot (replace the output with a small value).
			r, err := b.db.GetInstance("inst-shared")
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			r.ContextData["outputs"].(map[string]any)["out"] = "small"
			if err := b.db.UpdateInstanceProgress(r); err != nil {
				t.Fatalf("UpdateInstanceProgress: %v", err)
			}

			// The shared object survives — the log still needs it — and serves its payload.
			got, err := b.db.GetLogObject("inst-shared", ref.Ref)
			if err != nil {
				t.Fatalf("log object missing after context dereference: %v", err)
			}
			if got != string(content) {
				t.Errorf("log object content mismatch")
			}

			// Before the horizon the sweep leaves it; past the horizon it is reclaimed.
			if n, err := b.db.DeleteExpiredObjects(nowPlusHours(0)); err != nil || n != 0 {
				t.Fatalf("premature sweep: n=%d err=%v", n, err)
			}
			if n, err := b.db.DeleteExpiredObjects(nowPlusHours(2)); err != nil || n != 1 {
				t.Fatalf("expected 1 object swept after horizon, got n=%d err=%v", n, err)
			}
			if _, err := b.db.GetLogObject("inst-shared", ref.Ref); err == nil {
				t.Fatalf("expected log object to be swept after horizon")
			}
		})
	}
}

// nowPlusHours returns a unix-ms cutoff h hours from the DB clock, for the GC sweep.
func nowPlusHours(h int) int64 {
	return dbpkg.Now().Add(time.Duration(h) * time.Hour).UnixMilli()
}
