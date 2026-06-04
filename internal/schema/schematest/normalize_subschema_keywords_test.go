package schematest

import "testing"

// ---------------------------------------------------------------------------
// patternProperties — map of sub-schemas, draft-04
// ---------------------------------------------------------------------------

func TestNormalize_patternProperties(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"type": "object",
		"patternProperties": {"^num_": {"$ref": "#/$defs/Num"}},
		"$defs": {
			"Num":    {"type": "number"},
			"Unused": {"type": "string"}
		}
	}`), `{
		"$defs": {"Num": {"type": "number"}},
		"patternProperties": {"^num_": {"$ref": "#/$defs/Num"}},
		"type": "object"
	}`)
}

func TestNormalizeSemantic_patternProperties(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "object",
			"patternProperties": {"^num_": {"$ref": "#/$defs/Num"}},
			"$defs": {"Num": {"type": "number"}}
		}`,
		[]any{
			map[string]any{"num_a": 1.5, "num_b": 2},
			map[string]any{"other": "hello"},
			map[string]any{},
		},
		[]any{
			map[string]any{"num_a": "not-a-number"},
		},
	)
}

// ---------------------------------------------------------------------------
// propertyNames — single sub-schema applied to each key, draft-06
// ---------------------------------------------------------------------------

func TestNormalize_propertyNames(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"type": "object",
		"propertyNames": {"$ref": "#/$defs/KeyPat"},
		"$defs": {
			"KeyPat": {"pattern": "^[a-z]+$"},
			"Unused": {"type": "string"}
		}
	}`), `{
		"$defs": {"KeyPat": {"pattern": "^[a-z]+$"}},
		"propertyNames": {"$ref": "#/$defs/KeyPat"},
		"type": "object"
	}`)
}

func TestNormalizeSemantic_propertyNames(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "object",
			"propertyNames": {"$ref": "#/$defs/KeyPat"},
			"$defs": {"KeyPat": {"pattern": "^[a-z]+$"}}
		}`,
		[]any{
			map[string]any{"abc": 1, "xyz": 2},
			map[string]any{},
		},
		[]any{
			map[string]any{"ABC": 1},
			map[string]any{"abc_def": 1},
		},
	)
}

// ---------------------------------------------------------------------------
// contains — single sub-schema, at least one item must match, draft-06
// ---------------------------------------------------------------------------

func TestNormalize_contains(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"type": "array",
		"contains": {"$ref": "#/$defs/Str"},
		"$defs": {
			"Str":    {"type": "string"},
			"Unused": {"type": "integer"}
		}
	}`), `{
		"$defs": {"Str": {"type": "string"}},
		"contains": {"$ref": "#/$defs/Str"},
		"type": "array"
	}`)
}

func TestNormalizeSemantic_contains(t *testing.T) {
	assertSemanticEquivalence(t,
		`{
			"type": "array",
			"contains": {"$ref": "#/$defs/Str"},
			"$defs": {"Str": {"type": "string"}}
		}`,
		[]any{
			[]any{"hello", 1, 2},
			[]any{"a", "b"},
		},
		[]any{
			[]any{1, 2, 3},
			[]any{},
		},
	)
}
