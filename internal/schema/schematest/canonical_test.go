package schematest

import (
	"encoding/json"
	"testing"

	"gent/internal/schema"
)

func canonJSON(t *testing.T, n *schema.SchemaNode) string {
	t.Helper()
	got := schema.Canonicalize(n)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	// Idempotence: canonicalizing again must not change the JSON.
	b2, _ := json.Marshal(schema.Canonicalize(got))
	if string(b) != string(b2) {
		t.Fatalf("Canonicalize not idempotent:\n  once: %s\n twice: %s", b, b2)
	}
	return string(b)
}

func prim(types ...string) *schema.SchemaNode {
	return &schema.SchemaNode{Type: schema.SchemaType(types)}
}

func oneOf(vs ...*schema.SchemaNode) *schema.SchemaNode {
	return &schema.SchemaNode{OneOf: vs}
}

func TestCanonicalize_EqualTypesProduceEqualJSON(t *testing.T) {
	tests := []struct {
		name string
		a, b *schema.SchemaNode
	}{
		{"oneOf order-insensitive",
			oneOf(prim("integer"), prim("string")),
			oneOf(prim("string"), prim("integer"))},
		{"oneOf dedup",
			oneOf(prim("integer"), prim("integer"), prim("string")),
			oneOf(prim("string"), prim("integer"))},
		{"nullable simple: oneOf spelling == type-array spelling",
			oneOf(prim("string"), prim("null")),
			prim("string", "null")},
		{"type-array order-insensitive",
			prim("string", "null"),
			prim("null", "string")},
		{"singleton union collapses to the bare type",
			oneOf(prim("integer")),
			prim("integer")},
		{"nested unions flatten",
			oneOf(oneOf(prim("integer"), prim("string")), prim("boolean")),
			prim("boolean", "integer", "string")},
		{"property values are canonicalized",
			&schema.SchemaNode{Type: schema.SchemaType{"object"},
				Properties: map[string]*schema.SchemaNode{"x": oneOf(prim("integer"), prim("string"))}},
			&schema.SchemaNode{Type: schema.SchemaType{"object"},
				Properties: map[string]*schema.SchemaNode{"x": prim("string", "integer")}}},
		{"required is sorted and deduped",
			&schema.SchemaNode{Type: schema.SchemaType{"object"}, Required: []string{"b", "a", "b"}},
			&schema.SchemaNode{Type: schema.SchemaType{"object"}, Required: []string{"a", "b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ja := canonJSON(t, tt.a)
			jb := canonJSON(t, tt.b)
			if ja != jb {
				t.Errorf("canonical forms differ:\n  a: %s\n  b: %s", ja, jb)
			}
		})
	}
}

func TestCanonicalize_DistinctTypesStayDistinct(t *testing.T) {
	a := canonJSON(t, prim("integer"))
	b := canonJSON(t, prim("string"))
	if a == b {
		t.Errorf("integer and string canonicalized identically: %s", a)
	}
	// A nullable object stays a union (object is not a simple type), with the
	// null variant preserved and the object canonicalized.
	nullableObj := oneOf(
		&schema.SchemaNode{Type: schema.SchemaType{"object"},
			Properties: map[string]*schema.SchemaNode{"x": oneOf(prim("integer"), prim("integer"))}},
		prim("null"),
	)
	got := canonJSON(t, nullableObj)
	want := mustJSON(t, &schema.SchemaNode{OneOf: []*schema.SchemaNode{
		prim("null"),
		{Type: schema.SchemaType{"object"}, Properties: map[string]*schema.SchemaNode{"x": prim("integer")}},
	}})
	// Order of oneOf variants is canonical (sorted by JSON); compare as canonical.
	wantCanon := canonJSON(t, &schema.SchemaNode{OneOf: []*schema.SchemaNode{
		prim("null"),
		{Type: schema.SchemaType{"object"}, Properties: map[string]*schema.SchemaNode{"x": prim("integer")}},
	}})
	if got != wantCanon {
		t.Errorf("nullable object canonical mismatch:\n  got:  %s\n  want: %s (raw %s)", got, wantCanon, want)
	}
}

func mustJSON(t *testing.T, n *schema.SchemaNode) string {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
