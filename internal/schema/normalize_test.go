package schema

import (
	"encoding/json"
	"fmt"
	"testing"
)

func schema(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid test schema: %v", err)
	}
	return m
}

func mustNormalize(t *testing.T, s map[string]any) map[string]any {
	t.Helper()
	out, err := Normalize(s)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return out
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func TestNormalize_noRefs(t *testing.T) {
	in := schema(t, `{"type":"object","properties":{"name":{"type":"string"}}}`)
	out := mustNormalize(t, in)
	if _, ok := out["$defs"]; ok {
		t.Error("expected no $defs in output")
	}
	if out["type"] != "object" {
		t.Error("expected type object")
	}
}

func TestNormalize_flattenNestedDefs(t *testing.T) {
	// $defs nested inside a property definition should be moved to root.
	in := schema(t, `{
		"type": "object",
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"$defs": {
			"Order": {
				"$id": "Order",
				"type": "object",
				"properties": {"item": {"$ref": "#/$defs/Item"}},
				"$defs": {
					"Item": {"type": "object", "properties": {"name": {"type": "string"}}}
				}
			}
		}
	}`)
	out := mustNormalize(t, in)

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}
	if _, ok := defs["Order"]; !ok {
		t.Error("expected Order in root $defs")
	}
	if _, ok := defs["Item"]; !ok {
		t.Error("expected Item in root $defs")
	}

	// Nested $defs should be gone from Order.
	order := defs["Order"].(map[string]any)
	if _, ok := order["$defs"]; ok {
		t.Error("nested $defs should be removed from Order")
	}
}

func TestNormalize_pruneUnused(t *testing.T) {
	in := schema(t, `{
		"type": "object",
		"properties": {"a": {"$ref": "#/$defs/Used"}},
		"$defs": {
			"Used": {"type": "string"},
			"Unused": {"type": "integer"}
		}
	}`)
	out := mustNormalize(t, in)

	fmt.Println(toJSON(t, out))

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["Used"]; !ok {
		t.Error("Used should be kept")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned")
	}
}

func TestNormalize_transitiveRefs(t *testing.T) {
	in := schema(t, `{
		"$ref": "#/$defs/A",
		"$defs": {
			"A": {"$ref": "#/$defs/B"},
			"B": {"type": "string"},
			"Unreachable": {"type": "boolean"}
		}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["A"]; !ok {
		t.Error("A should be kept")
	}
	if _, ok := defs["B"]; !ok {
		t.Error("B should be kept (transitively reachable)")
	}
	if _, ok := defs["Unreachable"]; ok {
		t.Error("Unreachable should be pruned")
	}
}

func TestNormalize_unsupportedRef(t *testing.T) {
	in := schema(t, `{"$ref": "https://example.com/schema"}`)
	_, err := Normalize(in)
	if err == nil {
		t.Fatal("expected error for external ref")
	}
}

func TestNormalize_relativeRefRejected(t *testing.T) {
	in := schema(t, `{"$ref": "#/properties/foo"}`)
	_, err := Normalize(in)
	if err == nil {
		t.Fatal("expected error for relative ref")
	}
}

func TestNormalize_nestedDefs(t *testing.T) {
	in := schema(t, `{
		"type": "object",
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"$defs": {
			"Order": {
				"type": "object",
				"properties": {"item": {"$ref": "#/$defs/Order/$defs/Item"}},
				"$defs": {
					"Item": {"type": "object", "properties": {"name": {"type": "string"}}}
				}
			}
		}
	}`)
	out := mustNormalize(t, in)

	fmt.Println(toJSON(t, out))

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}
	if _, ok := defs["Order"]; !ok {
		t.Error("expected Order in root $defs")
	}
	if _, ok := defs["Item"]; !ok {
		t.Error("expected Item in root $defs")
	}
}

func TestNormalize_namesConflict(t *testing.T) {
	in := schema(t, `{
		"type": "object",
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"$defs": {
			"Order": {
				"type": "object",
				"properties": {"item": {"$ref": "#/$defs/Order/$defs/Order"}},
				"$defs": {
					"Order": {"type": "object", "properties": {"name": {"type": "string"}}}
				}
			}
		}
	}`)
	out := mustNormalize(t, in)

	fmt.Println(toJSON(t, out))

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}
	if _, ok := defs["Order"]; !ok {
		t.Error("expected Order in root $defs")
	}
	if _, ok := defs["Order_1"]; !ok {
		t.Error("expected Order_1 in root $defs")
	}
}

func TestNormalize_idRemovedAndDefsFlattened(t *testing.T) {
	// A $defs entry with $id and nested $defs — $id should be stripped and
	// the nested defs should land at root.
	in := schema(t, `{
		"type": "object",
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"$defs": {
			"Order": {
				"$id": "Order",
				"type": "object",
				"properties": {"item": {"$ref": "#/$defs/Item"}},
				"$defs": {
					"Item": {"type": "string"}
				}
			}
		}
	}`)
	out := mustNormalize(t, in)

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}
	if _, ok := defs["Order"]; !ok {
		t.Error("expected Order in root $defs")
	}
	if _, ok := defs["Item"]; !ok {
		t.Error("expected Item in root $defs")
	}
	order := defs["Order"].(map[string]any)
	if order["$id"] != nil {
		t.Error("$id should be removed from Order")
	}
	if order["$defs"] != nil {
		t.Error("nested $defs should be removed from Order")
	}
}

func TestNormalize_recursiveSchema(t *testing.T) {
	// Klasická rekurzivní struktura: Uzel (Node), který má vlastnost "children",
	// která odkazuje na pole Uzlů, nebo přímo na další Uzel.
	in := schema(t, `{
		"type": "object",
		"properties": {
			"tree": {"$ref": "#/$defs/Node"}
		},
		"$defs": {
			"Node": {
				"type": "object",
				"properties": {
					"value": {"type": "string"},
					"parent": {"$ref": "#/$defs/Node"}
				}
			}
		}
	}`)

	// Spuštění normalizace
	out, err := Normalize(in)

	fmt.Println(toJSON(t, out))

	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}

	// Ověření, že Node zůstal zachován a jeho vnitřní ref se správně přepsal
	if _, ok := defs["Node"]; !ok {
		t.Error("expected Node in root $defs")
	}
}

func TestNormalize_nestedDefsWithInternalRef(t *testing.T) {
	in := schema(t, `{
		"type": "object",
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"$defs": {
			"Order": {
				"type": "object",
				"properties": {
					"item": {"$ref": "#/$defs/Order/$defs/Item"}
				},
				"$defs": {
					"Item": {
						"type": "object",
						"properties": {
							"self": {"$ref": "#/$defs/Order/$defs/Item"}
						}
					}
				}
			}
		}
	}`)

	fmt.Println(toJSON(t, in))
	_, err := Normalize(in)
	if err != nil {
		t.Fatalf("Tento test selže na: %v", err)
	}
}

func TestNormalize_itemsRef(t *testing.T) {
	in := schema(t, `{
		"type": "array",
		"items": {"$ref": "#/$defs/Item"},
		"$defs": {
			"Item": {"type": "string"},
			"Unused": {"type": "integer"}
		}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["Item"]; !ok {
		t.Error("Item should be kept (referenced via items)")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned")
	}
}

func TestNormalize_prefixItemsRef(t *testing.T) {
	in := schema(t, `{
		"type": "array",
		"prefixItems": [
			{"$ref": "#/$defs/First"},
			{"type": "string"}
		],
		"$defs": {
			"First": {"type": "integer"},
			"Unused": {"type": "boolean"}
		}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["First"]; !ok {
		t.Error("First should be kept (referenced via prefixItems)")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned")
	}
}

func TestNormalize_oneOfRef(t *testing.T) {
	in := schema(t, `{
		"oneOf": [
			{"$ref": "#/$defs/A"},
			{"$ref": "#/$defs/B"}
		],
		"$defs": {
			"A": {"type": "string"},
			"B": {"type": "integer"},
			"Unused": {"type": "boolean"}
		}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["A"]; !ok {
		t.Error("A should be kept (referenced via oneOf)")
	}
	if _, ok := defs["B"]; !ok {
		t.Error("B should be kept (referenced via oneOf)")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned")
	}
}

func TestNormalize_allOfAnyOfRef(t *testing.T) {
	in := schema(t, `{
		"allOf": [{"$ref": "#/$defs/Base"}],
		"anyOf": [{"$ref": "#/$defs/Extra"}, {"type": "null"}],
		"$defs": {
			"Base":   {"type": "object"},
			"Extra":  {"type": "string"},
			"Unused": {"type": "boolean"}
		}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["Base"]; !ok {
		t.Error("Base should be kept (referenced via allOf)")
	}
	if _, ok := defs["Extra"]; !ok {
		t.Error("Extra should be kept (referenced via anyOf)")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned")
	}
}

func TestNormalize_notRef(t *testing.T) {
	in := schema(t, `{
		"not": {"$ref": "#/$defs/Forbidden"},
		"$defs": {
			"Forbidden": {"type": "string"},
			"Unused":    {"type": "integer"}
		}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["Forbidden"]; !ok {
		t.Error("Forbidden should be kept (referenced via not)")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned")
	}
}

func TestNormalize_additionalPropertiesRef(t *testing.T) {
	in := schema(t, `{
		"type": "object",
		"additionalProperties": {"$ref": "#/$defs/Value"},
		"$defs": {
			"Value":  {"type": "string"},
			"Unused": {"type": "integer"}
		}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["Value"]; !ok {
		t.Error("Value should be kept (referenced via additionalProperties)")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned")
	}
}

// --- $anchor tests ---

func TestNormalize_anchorOnDefEntry(t *testing.T) {
	// $anchor on a $defs entry: ref via "#anchor" should work same as "#/$defs/Name".
	in := schema(t, `{
		"properties": {
			"a": {"$ref": "#my-type"}
		},
		"$defs": {
			"MyType": {"$anchor": "my-type", "type": "string"}
		}
	}`)
	out := mustNormalize(t, in)

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}
	if _, ok := defs["MyType"]; !ok {
		t.Error("MyType should be in root $defs")
	}
	myType := defs["MyType"].(map[string]any)
	if myType["$anchor"] != nil {
		t.Error("$anchor should be removed from output")
	}
	props := out["properties"].(map[string]any)
	aRef := props["a"].(map[string]any)["$ref"].(string)
	if aRef != "#/$defs/MyType" {
		t.Errorf("expected $ref rewritten to #/$defs/MyType, got %q", aRef)
	}
}

func TestNormalize_anchorOnInlineSchema(t *testing.T) {
	// $anchor on an inline schema (not in $defs) — should be extracted to root $defs.
	in := schema(t, `{
		"properties": {
			"a": {"$anchor": "inline-type", "type": "integer"},
			"b": {"$ref": "#inline-type"}
		}
	}`)
	out := mustNormalize(t, in)

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}
	if _, ok := defs["inline-type"]; !ok {
		t.Error("inline-type should be extracted to root $defs")
	}
	props := out["properties"].(map[string]any)
	bRef := props["b"].(map[string]any)["$ref"].(string)
	if bRef != "#/$defs/inline-type" {
		t.Errorf("expected ref rewritten, got %q", bRef)
	}
}

func TestNormalize_anchorUnused(t *testing.T) {
	// An anchored def that is never referenced should be pruned.
	in := schema(t, `{
		"type": "object",
		"$defs": {
			"Used":   {"type": "string"},
			"Unused": {"$anchor": "unused-anchor", "type": "integer"}
		},
		"properties": {"a": {"$ref": "#/$defs/Used"}}
	}`)
	out := mustNormalize(t, in)

	defs := out["$defs"].(map[string]any)
	if _, ok := defs["Used"]; !ok {
		t.Error("Used should be kept")
	}
	if _, ok := defs["Unused"]; ok {
		t.Error("Unused should be pruned even though it has an anchor")
	}
}

func TestNormalize_anchorInsideIDScopedDef_pathRefWorks(t *testing.T) {
	// A $defs entry with $id introduces a sub-resource. Its $anchor is scoped to
	// that resource and is NOT reachable via "#anchorName" from the root, but a
	// path-based ref "#/$defs/Name" still resolves it normally.
	in := schema(t, `{
		"properties": {
			"a": {"$ref": "#/$defs/ScopedDef"}
		},
		"$defs": {
			"ScopedDef": {
				"$id": "https://example.com/scoped",
				"$anchor": "scoped-type",
				"type": "string"
			}
		}
	}`)
	out := mustNormalize(t, in)

	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatal("expected $defs at root")
	}
	if _, ok := defs["ScopedDef"]; !ok {
		t.Error("ScopedDef should be in root $defs")
	}
	scopedDef := defs["ScopedDef"].(map[string]any)
	if scopedDef["$id"] != nil {
		t.Error("$id should be removed from output")
	}
	if scopedDef["$anchor"] != nil {
		t.Error("$anchor should be removed from output")
	}
	props := out["properties"].(map[string]any)
	if got := props["a"].(map[string]any)["$ref"].(string); got != "#/$defs/ScopedDef" {
		t.Errorf("expected $ref rewritten to #/$defs/ScopedDef, got %q", got)
	}
}

func TestNormalize_anchorCrossIDBoundaryRejected(t *testing.T) {
	// An anchor inside a $id sub-resource is not visible to the root resource.
	// A ref "#anchorName" that crosses the $id boundary must be rejected.
	in := schema(t, `{
		"properties": {
			"a": {"$ref": "#scoped-type"}
		},
		"$defs": {
			"ScopedDef": {
				"$id": "https://example.com/scoped",
				"$anchor": "scoped-type",
				"type": "string"
			}
		}
	}`)
	_, err := Normalize(in)
	if err == nil {
		t.Fatal("expected error for anchor ref crossing $id boundary, got nil")
	}
}
