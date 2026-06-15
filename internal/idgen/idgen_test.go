package idgen

import (
	"encoding/binary"
	"sort"
	"testing"

	"github.com/google/uuid"
)

// mk builds a UUID from explicit high/low 64-bit words for precise carry tests.
func mk(hi, lo uint64) uuid.UUID {
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[:8], hi)
	binary.BigEndian.PutUint64(u[8:], lo)
	return u
}

func TestAdd_Basic(t *testing.T) {
	base := mk(5, 100)
	if got := Add(base, 0); got != base {
		t.Errorf("Add(base, 0) = %v, want %v", got, base)
	}
	if got := Add(base, 7); got != mk(5, 107) {
		t.Errorf("Add(base, 7) = %v, want %v", got, mk(5, 107))
	}
}

func TestAdd_CarryIntoHighWord(t *testing.T) {
	// Low word at max: adding 1 must wrap to 0 and increment the high word.
	base := mk(1, ^uint64(0))
	if got := Add(base, 1); got != mk(2, 0) {
		t.Errorf("Add(maxlo, 1) = %v, want %v", got, mk(2, 0))
	}
	// +3 across the boundary: 0xFFFF…FFFD + 3 = high+1, low 2.
	if got := Add(mk(1, ^uint64(0)-2), 3); got != mk(2, 0) {
		t.Errorf("carry +3 = %v, want %v", got, mk(2, 0))
	}
}

func TestAdd_StrictlyIncreasing(t *testing.T) {
	base := NewV7()
	const n = 1000
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = Add(base, uint64(i)).String()
	}
	// The run must already be in ascending string order (= the DB sort order).
	if !sort.StringsAreSorted(ids) {
		t.Fatal("Add run is not in ascending string order")
	}
	for i := 1; i < n; i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("not strictly increasing at %d: %s <= %s", i, ids[i], ids[i-1])
		}
	}
}

func TestNew_IsV7(t *testing.T) {
	u := uuid.MustParse(New())
	if u.Version() != 7 {
		t.Errorf("New() version = %d, want 7", u.Version())
	}
	if New() == New() {
		t.Error("New() returned duplicate ids")
	}
}

func TestAfter_FreshIsGreater(t *testing.T) {
	// prev is an old v7 (tiny timestamp), so a fresh v7 is naturally greater and
	// After must return that fresh value, not the prev+1 fallback.
	prev := mk(0, 0)
	got := After(prev)
	if got.Version() != 7 {
		t.Errorf("After version = %d, want 7", got.Version())
	}
	if got == Add(prev, 1) {
		t.Error("After took the fallback path when a fresh v7 was already greater")
	}
	if !idLess(prev, got) {
		t.Errorf("After(prev)=%v not greater than prev=%v", got, prev)
	}
}

func TestAfter_FallbackWhenNotGreater(t *testing.T) {
	// prev is a far-future id (first byte 0xFF), so a current v7 is below it and
	// After must fall back to prev+1.
	var prev uuid.UUID
	prev[0] = 0xFF
	got := After(prev)
	if got != Add(prev, 1) {
		t.Errorf("After(future prev) = %v, want prev+1 = %v", got, Add(prev, 1))
	}
	if !idLess(prev, got) {
		t.Errorf("After(prev)=%v not greater than prev=%v", got, prev)
	}
}

func TestChildBase_GreaterThanParent(t *testing.T) {
	// A tree: each level's base must sort strictly after its parent.
	root := uuid.MustParse(New())
	child := ChildBase(root.String())
	grandchild := ChildBase(child.String())
	if !idLess(root, child) {
		t.Errorf("child %v not > root %v", child, root)
	}
	if !idLess(child, grandchild) {
		t.Errorf("grandchild %v not > child %v", grandchild, child)
	}

	// Parallel siblings off a base are all > parent and ordered among themselves.
	base := ChildBase(root.String())
	prev := root
	for i := 0; i < 5; i++ {
		sib := Add(base, uint64(i))
		if !idLess(root, sib) {
			t.Errorf("sibling %d %v not > parent %v", i, sib, root)
		}
		if i > 0 && !idLess(prev, sib) {
			t.Errorf("sibling %d %v not > previous %v", i, sib, prev)
		}
		prev = sib
	}
}

func TestChildBase_InvalidParentFallsBack(t *testing.T) {
	got := ChildBase("not-a-uuid")
	if got.Version() != 7 {
		t.Errorf("ChildBase(invalid) version = %d, want 7", got.Version())
	}
}

func idLess(a, b uuid.UUID) bool { return a.String() < b.String() }
