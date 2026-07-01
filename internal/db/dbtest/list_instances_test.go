package dbtest

import (
	"testing"
	"time"

	dbpkg "genroc/internal/db"
	"genroc/internal/idgen"
	"genroc/internal/model"
)

// saveInstance persists a running instance with a non-trivial context (which the
// list projection must NOT carry) and returns it.
func saveInstance(t *testing.T, db *dbpkg.DB, process string) *model.ProcessInstance {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             idgen.New(),
		ProcessName:    process,
		ProcessVersion: 1,
		Task:           "",
		ContextData: map[string]any{
			"input":   map[string]any{"secret": "do-not-leak-in-list"},
			"outputs": map[string]any{},
		},
		Status: model.StatusRunning,
	}
	if err := db.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	return inst
}

func summaryIDs(items []*model.InstanceSummary) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = s.ID
	}
	return out
}

// TestListInstances_SortAndSummary covers the listing's two index-backed sorts and
// confirms the summary projection carries the scalar fields. The default sort is
// created (newest first, a stable/immutable key); updated is opt-in.
func TestListInstances_SortAndSummary(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			a := saveInstance(t, b.db, "alpha")
			dbpkg.AdvanceClock(time.Second)
			bb := saveInstance(t, b.db, "beta")
			dbpkg.AdvanceClock(time.Second)
			c := saveInstance(t, b.db, "gamma")

			// Touch 'a' last so it becomes the most recently *updated* (but still the
			// oldest *created*).
			dbpkg.AdvanceClock(time.Second)
			a.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(a); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}

			// Default: created desc -> newest created first: c, b, a.
			got, info, err := b.db.ListInstances("", dbpkg.PageReq{})
			if err != nil {
				t.Fatalf("ListInstances: %v", err)
			}
			if info.Sort != "created" || info.Order != "desc" {
				t.Errorf("default sort/order = %q/%q, want created/desc", info.Sort, info.Order)
			}
			if want := []string{c.ID, bb.ID, a.ID}; !equalStrs(summaryIDs(got), want) {
				t.Errorf("created-desc order = %v, want %v", summaryIDs(got), want)
			}

			// The head row carries its summary scalar fields.
			head := got[0]
			if head.ProcessName != "gamma" || head.Status != model.StatusRunning || head.ProcessVersion != 1 {
				t.Errorf("summary head = %+v", head)
			}
			if head.UpdatedAt.Before(head.CreatedAt) {
				t.Errorf("head updated_at %v before created_at %v", head.UpdatedAt, head.CreatedAt)
			}

			// updated desc -> most recently active first: a (just updated), c, b.
			byUpdated, info, err := b.db.ListInstances("", dbpkg.PageReq{Sort: "updated"})
			if err != nil {
				t.Fatalf("ListInstances updated: %v", err)
			}
			if info.Sort != "updated" {
				t.Errorf("sort echo = %q, want updated", info.Sort)
			}
			if want := []string{a.ID, c.ID, bb.ID}; !equalStrs(summaryIDs(byUpdated), want) {
				t.Errorf("updated-desc order = %v, want %v", summaryIDs(byUpdated), want)
			}

			// Status filter narrows the page.
			completed, _, err := b.db.ListInstances("completed", dbpkg.PageReq{})
			if err != nil {
				t.Fatalf("ListInstances completed: %v", err)
			}
			if len(completed) != 1 || completed[0].ID != a.ID {
				t.Errorf("status filter = %v, want [%s]", summaryIDs(completed), a.ID)
			}
		})
	}
}

// TestUpdatedAt_Advances documents the guarantee the updated sort relies on: every
// state-changing write bumps updated_at while created_at stays fixed.
func TestUpdatedAt_Advances(t *testing.T) {
	for _, b := range testBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			inst := saveInstance(t, b.db, "p")
			saved, err := b.db.GetInstance(inst.ID)
			if err != nil {
				t.Fatalf("GetInstance: %v", err)
			}
			if !saved.UpdatedAt.Equal(saved.CreatedAt) {
				t.Errorf("on insert updated_at %v != created_at %v", saved.UpdatedAt, saved.CreatedAt)
			}

			dbpkg.AdvanceClock(time.Second)
			saved.Status = model.StatusCompleted
			if err := b.db.UpdateInstance(saved); err != nil {
				t.Fatalf("UpdateInstance: %v", err)
			}

			after, err := b.db.GetInstance(inst.ID)
			if err != nil {
				t.Fatalf("GetInstance after: %v", err)
			}
			if !after.UpdatedAt.After(saved.CreatedAt) {
				t.Errorf("updated_at %v did not advance past created_at %v", after.UpdatedAt, after.CreatedAt)
			}
			if !after.CreatedAt.Equal(saved.CreatedAt) {
				t.Errorf("created_at changed: %v -> %v", saved.CreatedAt, after.CreatedAt)
			}
		})
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
