package schematest

import "testing"

func TestNormalize_anchorOnDefEntry(t *testing.T) {
	// $anchor on a $defs entry — ref via "#name" resolves the same def as
	// "#/$defs/Name" would, and $anchor is stripped from the output.
	out := normalize(t, `{
		"properties": {"a": {"$ref": "#my-type"}},
		"$defs": {
			"MyType": {"$anchor": "my-type", "type": "string"}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {"MyType": {"type": "string"}},
		"properties": {"a": {"$ref": "#/$defs/MyType"}}
	}`)
}

func TestNormalize_anchorOnInlineSchema(t *testing.T) {
	// $anchor on an inline schema (not in $defs) is extracted to root $defs.
	// The original inline node stays in place with $anchor stripped.
	out := normalize(t, `{
		"properties": {
			"a": {"$anchor": "inline-type", "type": "integer"},
			"b": {"$ref": "#inline-type"}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {"inline-type": {"type": "integer"}},
		"properties": {
			"a": {"type": "integer"},
			"b": {"$ref": "#/$defs/inline-type"}
		}
	}`)
}

func TestNormalize_anchorUnusedPruned(t *testing.T) {
	// A def with $anchor that is never referenced is pruned like any other unused def.
	out := normalize(t, `{
		"type": "object",
		"properties": {"a": {"$ref": "#/$defs/Used"}},
		"$defs": {
			"Used":   {"type": "string"},
			"Unused": {"$anchor": "unused-anchor", "type": "integer"}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {"Used": {"type": "string"}},
		"properties": {"a": {"$ref": "#/$defs/Used"}},
		"type": "object"
	}`)
}

func TestNormalize_anchorScopedByID(t *testing.T) {
	// Per the JSON Schema spec, $anchor is scoped to its $id resource.
	// A def that carries $id introduces a sub-resource; its $anchor is NOT
	// visible to refs in the parent resource.

	t.Run("path ref still resolves across $id boundary", func(t *testing.T) {
		// "#/$defs/Name" is a path-based ref and ignores $id scoping.
		out := normalize(t, `{
			"properties": {"a": {"$ref": "#/$defs/ScopedDef"}},
			"$defs": {
				"ScopedDef": {
					"$id": "https://example.com/scoped",
					"$anchor": "scoped-type",
					"type": "string"
				}
			}
		}`)
		assertJSON(t, out, `{
			"$defs": {"ScopedDef": {"type": "string"}},
			"properties": {"a": {"$ref": "#/$defs/ScopedDef"}}
		}`)
	})

	t.Run("anchor ref across $id boundary is rejected", func(t *testing.T) {
		// "#scoped-type" targets an anchor inside a sub-resource — not allowed.
		assertErr(t, `{
			"properties": {"a": {"$ref": "#scoped-type"}},
			"$defs": {
				"ScopedDef": {
					"$id": "https://example.com/scoped",
					"$anchor": "scoped-type",
					"type": "string"
				}
			}
		}`,
			`unresolved $ref "#scoped-type": anchor "scoped-type" is not defined in the root resource`,
		)
	})
}
