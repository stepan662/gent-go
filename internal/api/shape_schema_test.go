package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProcessSchemaShape guards the served process-schema.json wiring for the
// recursive Shape type: the def must exist as oneOf(string, object), recurse via
// a self $ref, and be referenced by params/output. Breaking this silently breaks
// editor autocomplete in the playground.
func TestProcessSchemaShape(t *testing.T) {
	b := buildProcessDefinitionSchema()
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	defs, _ := root["$defs"].(map[string]any)
	shape, ok := defs["ModelShape"].(map[string]any)
	if !ok {
		t.Fatalf("no ModelShape def; defs=%v", keysOf(defs))
	}
	variants, ok := shape["oneOf"].([]any)
	if !ok || len(variants) != 2 {
		t.Fatalf("ModelShape should be oneOf with 2 variants, got %v", shape["oneOf"])
	}
	// The object variant must recurse back into the Shape def.
	raw, _ := json.Marshal(shape)
	if !strings.Contains(string(raw), `"$ref":"#/$defs/ModelShape"`) {
		t.Errorf("ModelShape must recurse via #/$defs/ModelShape; got %s", raw)
	}
	// params and output must reference the Shape def.
	task, _ := defs["ModelTask"].(map[string]any)
	props, _ := task["properties"].(map[string]any)
	for _, f := range []string{"params", "output"} {
		fb, _ := json.Marshal(props[f])
		if !strings.Contains(string(fb), "ModelShape") {
			t.Errorf("Task.%s should reference ModelShape, got %s", f, fb)
		}
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
