// Package schematest exercises the Schema OO API from internal/schema.
package schematest

import (
	"encoding/json"
	"testing"

	"gent/internal/schema"
)

// mustParse parses JSON into a Schema, failing the test on error.
func mustParse(t *testing.T, s string) schema.Schema {
	t.Helper()
	sc, err := schema.Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return sc
}

// assertRaw marshals sc and checks it equals the expected JSON string.
func assertRaw(t *testing.T, sc schema.Schema, want string) {
	t.Helper()
	got, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wantParsed, gotParsed any
	if err := json.Unmarshal([]byte(want), &wantParsed); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	if err := json.Unmarshal(got, &gotParsed); err != nil {
		t.Fatalf("marshal output is not valid JSON: %v", err)
	}
	wantBytes, _ := json.MarshalIndent(wantParsed, "", "  ")
	gotBytes, _ := json.MarshalIndent(gotParsed, "", "  ")
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("schema mismatch\ngot:\n%s\n\nwant:\n%s", gotBytes, wantBytes)
	}
}

// ---- Load / Parse ----

func TestLoad(t *testing.T) {
	raw := map[string]any{"type": "string"}
	sc := schema.Load(raw)
	if sc.Raw() == nil {
		t.Fatal("Raw() returned nil")
	}
	if sc.Raw()["type"] != "string" {
		t.Errorf("type = %v, want \"string\"", sc.Raw()["type"])
	}
}

func TestParse(t *testing.T) {
	sc, err := schema.Parse([]byte(`{"type":"integer"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if sc.Raw()["type"] != "integer" {
		t.Errorf("type = %v, want \"integer\"", sc.Raw()["type"])
	}
}

func TestParseInvalidJSON(t *testing.T) {
	_, err := schema.Parse([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// ---- Normalize ----

func TestNormalizeFlattens(t *testing.T) {
	src := `{
		"type": "object",
		"properties": {
			"user": {"$ref": "#/$defs/User"}
		},
		"$defs": {
			"User": {
				"type": "object",
				"properties": {
					"id": {"type": "integer"}
				},
				"required": ["id"]
			}
		}
	}`
	sc := mustParse(t, src)
	norm, err := sc.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// $defs must appear at root in normalized output
	defs, ok := norm.Raw()["$defs"].(map[string]any)
	if !ok {
		t.Fatal("normalized schema has no $defs")
	}
	if _, ok := defs["User"]; !ok {
		t.Error("expected $defs.User in normalized schema")
	}
}

func TestNormalizeDoesNotMutateOriginal(t *testing.T) {
	src := `{
		"type": "object",
		"properties": {"x": {"$ref": "#/$defs/X"}},
		"$defs": {"X": {"$id": "nested", "type": "string"}}
	}`
	sc := mustParse(t, src)
	_, err := sc.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// Original schema's $defs entry should not have been stripped of $id by the clone.
	defs, _ := sc.Raw()["$defs"].(map[string]any)
	x, _ := defs["X"].(map[string]any)
	if x["$id"] == nil {
		t.Error("Normalize mutated the original schema (stripped $id from source)")
	}
}

func TestNormalizeError(t *testing.T) {
	bad := `{"$ref": "#/$defs/Missing"}`
	sc := mustParse(t, bad)
	_, err := sc.Normalize()
	if err == nil {
		t.Fatal("expected error for unresolved $ref, got nil")
	}
}

// ---- Infer ----

func TestInferTopLevelProperty(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"required": ["name"]
	}`)
	norm, _ := sc.Normalize()
	sub, err := norm.Infer("name")
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	assertRaw(t, sub, `{"type":"string"}`)
}

func TestInferOptionalPropertyIsNullable(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"age": {"type": "integer"}
		}
	}`)
	norm, _ := sc.Normalize()
	sub, err := norm.Infer("age")
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	assertRaw(t, sub, `{"type":["integer","null"]}`)
}

func TestInferNestedProperty(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"user": {
				"type": "object",
				"properties": {
					"id": {"type": "integer"}
				},
				"required": ["id"]
			}
		},
		"required": ["user"]
	}`)
	norm, _ := sc.Normalize()
	sub, err := norm.Infer("user.id")
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	assertRaw(t, sub, `{"type":"integer"}`)
}

func TestInferArrayIndex(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"items": {
				"type": "array",
				"items": {"type": "string"}
			}
		},
		"required": ["items"]
	}`)
	norm, _ := sc.Normalize()
	sub, err := norm.Infer("items[0]")
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	// Array index is always nullable (may be out of bounds)
	assertRaw(t, sub, `{"type":["string","null"]}`)
}

func TestInferDeepPath(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"user": {
				"type": "object",
				"properties": {
					"issues": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"value": {"type": "number"}
							},
							"required": ["value"]
						}
					}
				},
				"required": ["issues"]
			}
		},
		"required": ["user"]
	}`)
	norm, _ := sc.Normalize()
	sub, err := norm.Infer("user.issues[0].value")
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	// value is required in its object, but [0] makes the whole thing nullable
	assertRaw(t, sub, `{"type":["number","null"]}`)
}

func TestInferWithRef(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"user": {"$ref": "#/$defs/User"}
		},
		"required": ["user"],
		"$defs": {
			"User": {
				"type": "object",
				"properties": {
					"id": {"type": "integer"}
				},
				"required": ["id"]
			}
		}
	}`)
	norm, err := sc.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	sub, err := norm.Infer("user.id")
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	assertRaw(t, sub, `{"type":"integer"}`)
}

func TestInferUnknownProperty(t *testing.T) {
	sc := mustParse(t, `{"type":"object","properties":{"x":{"type":"string"}}}`)
	norm, _ := sc.Normalize()
	_, err := norm.Infer("missing")
	if err == nil {
		t.Fatal("expected error for unknown property, got nil")
	}
}

func TestInferEmptyPath(t *testing.T) {
	sc := mustParse(t, `{"type":"object"}`)
	norm, _ := sc.Normalize()
	_, err := norm.Infer("")
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
}

func TestInferIndexOnNonArray(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"required": ["name"]
	}`)
	norm, _ := sc.Normalize()
	_, err := norm.Infer("name[0]")
	if err == nil {
		t.Fatal("expected error for index on non-array, got nil")
	}
}

// ---- IsSubset ----

func TestIsSubsetTrue(t *testing.T) {
	narrow := mustParse(t, `{"type":"integer"}`)
	wide := mustParse(t, `{"type":"number"}`)
	n, _ := narrow.Normalize()
	w, _ := wide.Normalize()
	if !n.IsSubset(w) {
		t.Error("integer should be subset of number")
	}
}

func TestIsSubsetFalse(t *testing.T) {
	a := mustParse(t, `{"type":"string"}`)
	b := mustParse(t, `{"type":"integer"}`)
	an, _ := a.Normalize()
	bn, _ := b.Normalize()
	if an.IsSubset(bn) {
		t.Error("string should not be subset of integer")
	}
}

func TestIsSubsetBetweenInferred(t *testing.T) {
	sc := mustParse(t, `{
		"type": "object",
		"properties": {
			"count": {"type": "integer"},
			"score": {"type": "number"}
		},
		"required": ["count", "score"]
	}`)
	norm, _ := sc.Normalize()
	count, err := norm.Infer("count")
	if err != nil {
		t.Fatalf("Infer count: %v", err)
	}
	score, err := norm.Infer("score")
	if err != nil {
		t.Fatalf("Infer score: %v", err)
	}
	// integer ⊆ number
	if !count.IsSubset(score) {
		t.Error("inferred integer should be subset of inferred number")
	}
}

// ---- WithDef ----

func TestWithDefAddsDefinition(t *testing.T) {
	base := mustParse(t, `{
		"type": "object",
		"properties": {
			"user": {"$ref": "#/$defs/User"}
		}
	}`)
	userDef := mustParse(t, `{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`)
	composed := base.WithDef("User", userDef)

	// The new schema should have $defs.User
	defs, ok := composed.Raw()["$defs"].(map[string]any)
	if !ok {
		t.Fatal("WithDef did not add $defs")
	}
	if _, ok := defs["User"]; !ok {
		t.Error("WithDef did not add $defs.User")
	}
}

func TestWithDefDoesNotMutateOriginal(t *testing.T) {
	base := mustParse(t, `{"type":"object"}`)
	def := mustParse(t, `{"type":"string"}`)
	_ = base.WithDef("Foo", def)

	// Original should still have no $defs.
	if base.Raw()["$defs"] != nil {
		t.Error("WithDef mutated the original schema")
	}
}

func TestWithDefThenNormalize(t *testing.T) {
	base := mustParse(t, `{
		"type": "object",
		"properties": {
			"user": {"$ref": "#/$defs/User"}
		}
	}`)
	userDef := mustParse(t, `{
		"type": "object",
		"properties": {"name": {"type": "string"}},
		"required": ["name"]
	}`)
	norm, err := base.WithDef("User", userDef).Normalize()
	if err != nil {
		t.Fatalf("Normalize after WithDef: %v", err)
	}
	sub, err := norm.Infer("user.name")
	if err != nil {
		t.Fatalf("Infer after WithDef+Normalize: %v", err)
	}
	// user is optional (not in required), so user.name is nullable
	assertRaw(t, sub, `{"type":["string","null"]}`)
}

// ---- MarshalJSON ----

func TestMarshalJSON(t *testing.T) {
	sc := mustParse(t, `{"type":"boolean"}`)
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if m["type"] != "boolean" {
		t.Errorf("type = %v, want \"boolean\"", m["type"])
	}
}
