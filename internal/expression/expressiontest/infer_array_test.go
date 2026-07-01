package expressiontest

import (
	"errors"
	"testing"

	"genroc/internal/expression"
	"genroc/internal/schema"
)

const arrayCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"tags":       { "type": "array", "items": { "type": "string" } },
				"counts":     { "type": "array", "items": { "type": "integer" } },
				"matrix":     { "type": "array", "items": { "type": "array", "items": { "type": "number" } } },
				"bare":       { "type": "array" },
				"referenced": { "type": "array", "items": { "$ref": "#/$defs/object" } }
			},
			"required": ["tags", "counts", "matrix", "bare", "referenced"]
		}
	},
	"required": ["input"],
	"$defs": {
		"object": {
			"type": "object",
			"properties": {
				"name":  { "type": "string" },
				"value": { "type": "number" }
			},
			"required": ["name", "value"]
		}
	}
}`

// Accessing an array field returns its full schema (including items).
func TestInfer_Array_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.tags", c), `{
		"type": "array",
		"items": { "type": "string" }
	}`)
}

func TestInfer_Array_FieldWithoutItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.bare", c), `{"type":"array"}`)
}

func TestInfer_Array_NestedItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.matrix", c), `{
		"type": "array",
		"items": { "type": "array", "items": { "type": "number" } }
	}`)
}

// Member access on an array type fails — arrays have no named properties.
func TestInfer_Array_MemberAccessFails(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	inferErr(t, "input.tags.length", c, "cannot access .length: schema has no properties")
}

// Comparison on an array field always returns boolean.
func TestInfer_Array_ComparisonIsBoolean(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.tags == null", c), `{"type":"boolean"}`)
}

// Arithmetic on an array type fails.
func TestInfer_Array_ArithmeticFails(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	inferErr(t, "input.counts + 1", c, "operator requires numeric operands")
}

// Array literals are outside the supported subset.
func TestInfer_Array_LiteralUnsupported(t *testing.T) {
	err := inferErr(t, "[1, 2, 3]", schema.Schema{}, "")
	var e expression.ErrUnsupported
	if !errors.As(err, &e) {
		t.Errorf("expected ErrUnsupported, got %T: %v", err, err)
	}
}

// Nullable array: conditional with nil preserves the items schema.
func TestInfer_Array_NullablePreservesItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "true ? input.tags : null", c), `{
		"type": ["array", "null"],
		"items": { "type": "string" }
	}`)
}

// --- static index access ---

// Index into a typed array returns the items schema wrapped as nullable.
func TestInfer_Array_Index_KnownItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.tags[0]", c), `{"type":["string","null"]}`)
}

func TestInfer_Array_Index_IntegerItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.counts[0]", c), `{"type":["integer","null"]}`)
}

// Index into an array without items returns {} (any value, including null).
func TestInfer_Array_Index_NoItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.bare[0]", c), `{}`)
}

func TestInfer_Array_ReferencedType(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.referenced[0].value", c), `{"type":["number","null"]}`)
}

// Index into a non-array schema fails.
func TestInfer_Array_Index_NonArrayFails(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	inferErr(t, "input.tags[0][1]", c, "index access [n] requires an array schema") // tags[0] is nullable string, not array
}

// Dynamic index is unsupported.
func TestInfer_Array_DynamicIndexUnsupported(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertUnsupported(t, inferErr(t, "input.tags[input.counts[0]]", c, ""))
}

// Indexing a nullable array (type-array form: {"type":["array","null"]}) returns
// the nullable item type — the main regression target for the optional-output fix.
func TestInfer_Array_Index_NullableTypeArrayForm(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"tags": { "type": ["array", "null"], "items": { "type": "string" } }
		},
		"required": ["tags"]
	}`)
	assertSchema(t, infer(t, "tags[0]", c), `{"type":["string","null"]}`)
}

// Indexing an optional array field (not in "required") works: lookupProperty
// wraps it in withNull, producing the type-array form, and inferIndex handles it.
func TestInfer_Array_Index_OptionalField(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": {
					"results": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": { "score": { "type": "number" } },
							"required": ["score"]
						}
					}
				}
			}
		},
		"required": ["outputs"]
	}`)
	assertSchema(t, infer(t, "outputs.results[0].score", c), `{"type":["number","null"]}`)
}

// optionalArrayCtxJSON is shared by the null-narrowing tests below.
// "results" is optional (not in required) so index access returns nullable items.
const optionalArrayCtxJSON = `{
	"type": "object",
	"properties": {
		"outputs": {
			"type": "object",
			"properties": {
				"results": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": { "score": { "type": "number" } },
						"required": ["score"]
					}
				}
			}
		}
	},
	"required": ["outputs"]
}`

// x[0].score is nullable; "!= null" narrows the then-branch to non-nullable number.
// The else literal 0 is integer, so the ternary is oneOf[number, integer].
// The key assertion is that the result is NOT nullable — narrowing worked.
func TestInfer_Array_Index_NullNarrowing_TernaryReturnsNumber(t *testing.T) {
	c := ctx(t, optionalArrayCtxJSON)
	assertSchema(t, infer(t, "outputs.results[0].score != null ? outputs.results[0].score : 0", c), `{
		"oneOf": [{"type":"number"}, {"type":"integer"}]
	}`)
}

// Adding two null-coalesced index expressions should produce a number.
// This is the direct regression target: nodePath must include [n] so that
// narrowCondition can guard paths like "outputs.results[0].score".
func TestInfer_Array_Index_NullNarrowing_ArithmeticOnTwoElements(t *testing.T) {
	c := ctx(t, optionalArrayCtxJSON)
	expr := "(outputs.results[0].score != null ? outputs.results[0].score : 0) + (outputs.results[1].score != null ? outputs.results[1].score : 0)"
	assertSchema(t, infer(t, expr, c), `{"type":"number"}`)
}

// --- Already-nullable array combined with null stays the same.
func TestInfer_Array_AlreadyNullableStable(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"tags": { "type": ["array", "null"], "items": { "type": "string" } }
				},
				"required": ["tags"]
			}
		},
		"required": ["input"]
	}`)
	assertSchema(t, infer(t, "true ? input.tags : null", c), `{
		"type": ["array", "null"],
		"items": { "type": "string" }
	}`)
}
