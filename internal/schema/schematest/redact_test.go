package schematest

import (
	"testing"

	"genroc/internal/schema"
)

func TestCollectSecrets(t *testing.T) {
	sc, err := schema.Parse([]byte(`{
		"type": "object",
		"properties": {
			"token":  { "type": "string", "secret": true },
			"name":   { "type": "string" },
			"nested": { "type": "object", "properties": {
				"key": { "type": "string", "secret": true },
				"ok":  { "type": "string" }
			} }
		}
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	val := map[string]any{
		"token":  "s3cr3t",
		"name":   "public",
		"nested": map[string]any{"key": "deep-secret", "ok": "fine"},
	}
	var got []string
	schema.CollectSecrets(val, sc.Node(), nil, &got)
	has := func(s string) bool {
		for _, x := range got {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has("s3cr3t") || !has("deep-secret") {
		t.Errorf("collected = %v, want both s3cr3t and deep-secret", got)
	}
	if has("public") || has("fine") {
		t.Errorf("collected a non-secret value: %v", got)
	}
}

// A secret nested inside an array of objects (the common "list of records, one
// field secret" shape) must be collected for every element.
func TestCollectSecretsArrayOfObjects(t *testing.T) {
	sc, err := schema.Parse([]byte(`{
		"type": "array",
		"items": { "type": "object", "properties": {
			"sleep": { "type": "number", "secret": true }
		} }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	val := []any{
		map[string]any{"sleep": float64(5)},
		map[string]any{"sleep": float64(50)},
	}
	var got []string
	schema.CollectSecrets(val, sc.Node(), nil, &got)
	if len(got) != 2 || got[0] != "5" || got[1] != "50" {
		t.Errorf("collected = %v, want [5 50]", got)
	}
}

// A secret declared inside a oneOf / anyOf branch must still be found: CollectSecrets
// descends via LookupProperty, so it gets the same combinator coverage as type
// inference (which resolves a field across union variants). allOf is intentionally
// not navigated — consistent with LookupProperty / type inference.
func TestCollectSecretsCombinators(t *testing.T) {
	cases := map[string]string{
		"oneOf union": `{"type":"object","properties":{"data":{"oneOf":[
			{"type":"object","properties":{"a":{"type":"string"}}},
			{"type":"object","properties":{"token":{"type":"string","secret":true}}}
		]}}}`,
		"anyOf union": `{"type":"object","properties":{"data":{"anyOf":[
			{"type":"object","properties":{"a":{"type":"string"}}},
			{"type":"object","properties":{"token":{"type":"string","secret":true}}}
		]}}}`,
	}
	for name, doc := range cases {
		sc, err := schema.Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		val := map[string]any{"data": map[string]any{"token": "SEKRET"}}
		var got []string
		schema.CollectSecrets(val, sc.Node(), nil, &got)
		found := false
		for _, g := range got {
			if g == "SEKRET" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: collected = %v, want it to include SEKRET", name, got)
		}
	}
}

// A numeric secret must be collected in the same textual form it takes in a JSON
// log line. fmt's "%v" renders a large float64 in scientific notation ("1e+06"),
// which never matches the "1000000" that json.Marshal writes into the log, so the
// scrub would miss it.
func TestCollectSecretsNumericFormatting(t *testing.T) {
	sc, err := schema.Parse([]byte(`{"type":"array","items":{"type":"number","secret":true}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	schema.CollectSecrets([]any{float64(1000000), float64(0.0001)}, sc.Node(), nil, &got)
	if len(got) != 2 || got[0] != "1000000" || got[1] != "0.0001" {
		t.Errorf("collected = %v, want [1000000 0.0001]", got)
	}
}

// Redact must also descend into combinator branches so a secret in a oneOf/allOf
// variant is replaced, not just collected.
func TestRedactCombinators(t *testing.T) {
	sc, err := schema.Parse([]byte(`{"type":"object","properties":{"data":{"oneOf":[
		{"type":"object","properties":{"a":{"type":"string"}}},
		{"type":"object","properties":{"token":{"type":"string","secret":true},"ok":{"type":"string"}}}
	]}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	val := map[string]any{"data": map[string]any{"token": "SEKRET", "ok": "fine"}}
	got := schema.Redact(val, sc.Node(), nil).(map[string]any)
	data := got["data"].(map[string]any)
	if data["token"] != "***" {
		t.Errorf("data.token = %v, want ***", data["token"])
	}
	if data["ok"] != "fine" {
		t.Errorf("data.ok = %v, want fine (non-secret preserved)", data["ok"])
	}
}

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
