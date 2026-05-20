package schema

import "testing"

func TestNormalize_noRefs(t *testing.T) {
	out := normalize(t, `{
		"type": "object",
		"properties": {"name": {"type": "string"}}
	}`)
	assertJSON(t, out, `{
		"properties": {"name": {"type": "string"}},
		"type": "object"
	}`)
}

func TestNormalize_flattenNestedDefs(t *testing.T) {
	// Nested $defs are lifted to root; $id and nested $defs key are stripped.
	out := normalize(t, `{
		"type": "object",
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"$defs": {
			"Order": {
				"$id": "urn:order",
				"type": "object",
				"properties": {"item": {"$ref": "#/$defs/Item"}},
				"$defs": {
					"Item": {"type": "object", "properties": {"name": {"type": "string"}}}
				}
			}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {
			"Item": {
				"properties": {"name": {"type": "string"}},
				"type": "object"
			},
			"Order": {
				"properties": {"item": {"$ref": "#/$defs/Item"}},
				"type": "object"
			}
		},
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"type": "object"
	}`)
}

func TestNormalize_pruneUnused(t *testing.T) {
	out := normalize(t, `{
		"type": "object",
		"properties": {"a": {"$ref": "#/$defs/Used"}},
		"$defs": {
			"Used":   {"type": "string"},
			"Unused": {"type": "integer"}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {
			"Used": {"type": "string"}
		},
		"properties": {"a": {"$ref": "#/$defs/Used"}},
		"type": "object"
	}`)
}

func TestNormalize_transitiveRefs(t *testing.T) {
	// Refs inside def schemas are walked, so B is kept even though only A is
	// directly referenced from the root.
	out := normalize(t, `{
		"$ref": "#/$defs/A",
		"$defs": {
			"A":           {"$ref": "#/$defs/B"},
			"B":           {"type": "string"},
			"Unreachable": {"type": "boolean"}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {
			"A": {"$ref": "#/$defs/B"},
			"B": {"type": "string"}
		},
		"$ref": "#/$defs/A"
	}`)
}

func TestNormalize_nameCollision(t *testing.T) {
	// Two defs share the same local name. Shallower path wins the clean name;
	// deeper gets the _1 suffix. Order is deterministic (sorted by path).
	out := normalize(t, `{
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
	assertJSON(t, out, `{
		"$defs": {
			"Order": {
				"properties": {"item": {"$ref": "#/$defs/Order_1"}},
				"type": "object"
			},
			"Order_1": {
				"properties": {"name": {"type": "string"}},
				"type": "object"
			}
		},
		"properties": {
			"order": {"$ref": "#/$defs/Order"}
		},
		"type": "object"
	}`)
}

func TestNormalize_recursiveRef(t *testing.T) {
	// A def that references itself must not loop or be pruned.
	out := normalize(t, `{
		"type": "object",
		"properties": {
			"tree": {"$ref": "#/$defs/Node"}
		},
		"$defs": {
			"Node": {
				"type": "object",
				"properties": {
					"value":    {"type": "string"},
					"children": {"$ref": "#/$defs/Node"}
				}
			}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {
			"Node": {
				"properties": {
					"children": {"$ref": "#/$defs/Node"},
					"value":    {"type": "string"}
				},
				"type": "object"
			}
		},
		"properties": {
			"tree": {"$ref": "#/$defs/Node"}
		},
		"type": "object"
	}`)
}
