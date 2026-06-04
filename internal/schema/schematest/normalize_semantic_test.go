package schematest

import "testing"

func TestNormalizeSemantic_simpleObject(t *testing.T) {
	assertSemanticEquivalence(t,
		`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
		[]any{
			map[string]any{"name": "Alice"},
		},
		[]any{
			map[string]any{},           // missing required field
			map[string]any{"name": 42}, // wrong type
		},
	)
}

func TestNormalizeSemantic_flatRef(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "object",
			"properties": {"age": {"$ref": "#/$defs/Age"}},
			"$defs": {"Age": {"type": "integer", "minimum": 0}}
		}`,
		[]any{
			map[string]any{"age": 25},
			map[string]any{},
		},
		[]any{
			map[string]any{"age": -1},
			map[string]any{"age": "old"},
		},
	)
}

func TestNormalizeSemantic_nestedDefs(t *testing.T) {
	// Uses the full JSON Pointer path #/$defs/Order/$defs/Item so that
	// gojsonschema can validate both the original and the normalized form.
	assertSemanticEquivalence(t,
		`{
			"type": "object",
			"properties": {"order": {"$ref": "#/$defs/Order"}},
			"$defs": {
				"Order": {
					"type": "object",
					"properties": {"item": {"$ref": "#/$defs/Order/$defs/Item"}},
					"$defs": {
						"Item": {
							"type": "object",
							"properties": {"qty": {"type": "integer"}},
							"required": ["qty"]
						}
					}
				}
			}
		}`,
		[]any{
			map[string]any{"order": map[string]any{"item": map[string]any{"qty": 3}}},
			map[string]any{},
		},
		[]any{
			map[string]any{"order": map[string]any{"item": map[string]any{}}}, // missing required qty
		},
	)
}

func TestNormalizeSemantic_oneOf(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"oneOf": [
				{"$ref": "#/$defs/Str"},
				{"$ref": "#/$defs/Int"}
			],
			"$defs": {
				"Str": {"type": "string"},
				"Int": {"type": "integer"}
			}
		}`,
		[]any{"hello", 42},
		[]any{true, 3.14, map[string]any{}},
	)
}

func TestNormalizeSemantic_anyOf(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"anyOf": [{"$ref": "#/$defs/Str"}, {"type": "null"}],
			"$defs": {"Str": {"type": "string"}}
		}`,
		[]any{"hi"},
		[]any{123, map[string]any{}},
	)
}

func TestNormalizeSemantic_allOf(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"allOf": [
				{"$ref": "#/$defs/HasName"},
				{"$ref": "#/$defs/HasAge"}
			],
			"$defs": {
				"HasName": {"type":"object","required":["name"],"properties":{"name":{"type":"string"}}},
				"HasAge":  {"type":"object","required":["age"], "properties":{"age": {"type":"integer"}}}
			}
		}`,
		[]any{map[string]any{"name": "Bob", "age": 30}},
		[]any{
			map[string]any{"name": "Bob"},
			map[string]any{"age": 30},
		},
	)
}

func TestNormalizeSemantic_recursiveRef(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "object",
			"properties": {"tree": {"$ref": "#/$defs/Node"}},
			"$defs": {
				"Node": {
					"type": "object",
					"properties": {
						"value": {"type": "string"},
						"child": {"$ref": "#/$defs/Node"}
					}
				}
			}
		}`,
		[]any{
			map[string]any{"tree": map[string]any{
				"value": "root",
				"child": map[string]any{"value": "leaf"},
			}},
			map[string]any{},
		},
		[]any{
			map[string]any{"tree": map[string]any{"value": 99}}, // value must be string
		},
	)
}

func TestNormalizeSemantic_items(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "array",
			"items": {"$ref": "#/$defs/Num"},
			"$defs": {"Num": {"type": "number"}}
		}`,
		[]any{[]any{1, 2.5, 3}},
		[]any{[]any{1, "two", 3}},
	)
}

func TestNormalizeSemantic_additionalProperties(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "object",
			"additionalProperties": {"$ref": "#/$defs/Val"},
			"$defs": {"Val": {"type": "string"}}
		}`,
		[]any{map[string]any{"a": "x", "b": "y"}},
		[]any{map[string]any{"a": 1}},
	)
}

func TestNormalizeSemantic_ifThenElse(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"if":   {"$ref": "#/$defs/IsStr"},
			"then": {"$ref": "#/$defs/LongStr"},
			"else": {"$ref": "#/$defs/PosInt"},
			"$defs": {
				"IsStr":   {"type": "string"},
				"LongStr": {"type": "string", "minLength": 3},
				"PosInt":  {"type": "integer", "minimum": 1}
			}
		}`,
		[]any{"hello", 5},
		[]any{"hi", 0},
	)
}

func TestNormalizeSemantic_nameCollision(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "object",
			"properties": {"order": {"$ref": "#/$defs/Order"}},
			"$defs": {
				"Order": {
					"type": "object",
					"properties": {"inner": {"$ref": "#/$defs/Order/$defs/Order"}},
					"$defs": {
						"Order": {
							"type": "object",
							"properties": {"n": {"type": "integer"}},
							"required": ["n"]
						}
					}
				}
			}
		}`,
		[]any{
			map[string]any{"order": map[string]any{"inner": map[string]any{"n": 1}}},
			map[string]any{},
		},
		[]any{
			map[string]any{"order": map[string]any{"inner": map[string]any{}}}, // missing n
		},
	)
}

func TestNormalizeSemantic_transitiveRefsUnusedPruned(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"$ref": "#/$defs/A",
			"$defs": {
				"A":           {"type": "string"},
				"Unreachable": {"type": "boolean"}
			}
		}`,
		[]any{"ok"},
		[]any{42, true},
	)
}
