package schematest

import (
	"testing"

	"gent/internal/schema"
)

func TestRedact(t *testing.T) {
	sc, err := schema.Parse([]byte(`{
		"type": "object",
		"properties": {
			"token": { "type": "string", "secret": true },
			"name":  { "type": "string" },
			"nested": {
				"type": "object",
				"properties": {
					"key": { "type": "string", "secret": true },
					"ok":  { "type": "string" }
				}
			},
			"list": { "type": "array", "items": { "type": "string", "secret": true } }
		}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	val := map[string]any{
		"token":  "s3cr3t",
		"name":   "public",
		"nested": map[string]any{"key": "deep-secret", "ok": "fine"},
		"list":   []any{"a", "b"},
	}
	got, ok := schema.Redact(val, sc.Node(), nil).(map[string]any)
	if !ok {
		t.Fatalf("Redact did not return a map: %T", schema.Redact(val, sc.Node(), nil))
	}
	if got["token"] != "***" {
		t.Errorf("token = %v, want ***", got["token"])
	}
	if got["name"] != "public" {
		t.Errorf("name = %v, want public", got["name"])
	}
	nested := got["nested"].(map[string]any)
	if nested["key"] != "***" {
		t.Errorf("nested.key = %v, want ***", nested["key"])
	}
	if nested["ok"] != "fine" {
		t.Errorf("nested.ok = %v, want fine", nested["ok"])
	}
	list := got["list"].([]any)
	if list[0] != "***" || list[1] != "***" {
		t.Errorf("list = %v, want [*** ***]", list)
	}
}
