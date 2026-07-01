package schematest

import (
	"encoding/json"
	"testing"

	"genroc/internal/schema"
)

func assertSubset(t *testing.T, subJSON, superJSON string, want bool) {
	t.Helper()
	var sub, super schema.SchemaNode
	if err := json.Unmarshal([]byte(subJSON), &sub); err != nil {
		t.Fatalf("invalid sub schema: %v", err)
	}
	if err := json.Unmarshal([]byte(superJSON), &super); err != nil {
		t.Fatalf("invalid super schema: %v", err)
	}
	got := schema.IsSubset(&sub, &super)
	if got != want {
		t.Errorf("IsSubset(%s, %s) = %v, want %v", subJSON, superJSON, got, want)
	}
}

// assertEquivalent checks that a ⊆ b and b ⊆ a, proving semantic equivalence.
func assertEquivalent(t *testing.T, aJSON, bJSON string, want bool) {
	t.Helper()
	var a, b schema.SchemaNode
	if err := json.Unmarshal([]byte(aJSON), &a); err != nil {
		t.Fatalf("invalid schema a: %v", err)
	}
	if err := json.Unmarshal([]byte(bJSON), &b); err != nil {
		t.Fatalf("invalid schema b: %v", err)
	}
	aSubB := schema.IsSubset(&a, &b)
	bSubA := schema.IsSubset(&b, &a)
	got := aSubB && bSubA
	if got != want {
		t.Errorf("equivalent(%s, %s): a⊆b=%v b⊆a=%v, want equivalent=%v", aJSON, bJSON, aSubB, bSubA, want)
	}
}
