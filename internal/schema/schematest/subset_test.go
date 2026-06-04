package schematest

import (
	"encoding/json"
	"testing"

	"gent/internal/schema"
)

func assertSubset(t *testing.T, subJSON, superJSON string, want bool) {
	t.Helper()
	var sub, super map[string]any
	if err := json.Unmarshal([]byte(subJSON), &sub); err != nil {
		t.Fatalf("invalid sub schema: %v", err)
	}
	if err := json.Unmarshal([]byte(superJSON), &super); err != nil {
		t.Fatalf("invalid super schema: %v", err)
	}
	got := schema.IsSubset(sub, super)
	if got != want {
		t.Errorf("IsSubset(%s, %s) = %v, want %v", subJSON, superJSON, got, want)
	}
}
