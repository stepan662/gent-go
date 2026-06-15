// Package idgen mints instance ids as time-ordered UUIDv7 values, with helpers to
// keep a process tree sortable: sibling ids are a contiguous increasing run, and a
// child id is always strictly greater than its parent's. That lets the DB order
// (and lock) a tree by id alone — ancestors before descendants, creation order
// within a level.
package idgen

import (
	"bytes"
	"encoding/binary"

	"github.com/google/uuid"
)

// New returns a fresh time-ordered UUIDv7 as a string.
func New() string { return NewV7().String() }

// NewV7 returns a fresh time-ordered UUIDv7. uuid.NewV7 only errors when
// crypto/rand fails; the v4 fallback would fail the same way, so this never
// silently returns a non-unique id.
func NewV7() uuid.UUID {
	if v7, err := uuid.NewV7(); err == nil {
		return v7
	}
	return uuid.New()
}

// Add returns base + n treated as a 128-bit big-endian integer, carrying from the
// low word into the high word. Deriving sibling ids as base, base+1, base+2 … makes
// them a strictly increasing run that sorts in spawn order.
func Add(base uuid.UUID, n uint64) uuid.UUID {
	hi := binary.BigEndian.Uint64(base[:8])
	lo := binary.BigEndian.Uint64(base[8:])
	sum := lo + n
	if sum < lo { // overflow of the low word carries into the high word
		hi++
	}
	binary.BigEndian.PutUint64(base[:8], hi)
	binary.BigEndian.PutUint64(base[8:], sum)
	return base
}

// After returns a UUIDv7 guaranteed to sort strictly after prev. A fresh v7
// normally already does (later timestamp), but if it doesn't — e.g. prev was minted
// in the same millisecond by another process — it derives prev+1 instead. This is
// what guarantees a child id is always greater than its parent's.
func After(prev uuid.UUID) uuid.UUID {
	v := NewV7()
	if bytes.Compare(v[:], prev[:]) > 0 {
		return v
	}
	return Add(prev, 1)
}

// ChildBase returns a v7 base id guaranteed to sort after parentID, for the
// spawning instance's children. Falls back to a plain v7 if parentID is not a valid
// UUID (it always is for real instances).
func ChildBase(parentID string) uuid.UUID {
	if p, err := uuid.Parse(parentID); err == nil {
		return After(p)
	}
	return NewV7()
}
