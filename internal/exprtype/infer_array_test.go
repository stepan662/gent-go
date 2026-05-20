package exprtype_test

import (
	"errors"
	"testing"

	"gent/internal/exprtype"
)

const arrayCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"tags":    { "type": "array", "items": { "type": "string" } },
				"counts":  { "type": "array", "items": { "type": "integer" } },
				"matrix":  { "type": "array", "items": { "type": "array", "items": { "type": "number" } } },
				"bare":    { "type": "array" }
			}
		}
	}
}`

// Accessing an array field returns its full schema (including items).
func TestInfer_Array_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.tags", c, nil), `{
		"type": "array",
		"items": { "type": "string" }
	}`)
}

func TestInfer_Array_FieldWithoutItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.bare", c, nil), `{"type":"array"}`)
}

func TestInfer_Array_NestedItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.matrix", c, nil), `{
		"type": "array",
		"items": { "type": "array", "items": { "type": "number" } }
	}`)
}

// Member access on an array type fails — arrays have no named properties.
func TestInfer_Array_MemberAccessFails(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	inferErr(t, "input.tags.length", c, nil)
}

// Comparison on an array field always returns boolean.
func TestInfer_Array_ComparisonIsBoolean(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.tags == nil", c, nil), `{"type":"boolean"}`)
}

// Arithmetic on an array type fails.
func TestInfer_Array_ArithmeticFails(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	inferErr(t, "input.counts + 1", c, nil)
}

// Array literals are outside the supported subset.
func TestInfer_Array_LiteralUnsupported(t *testing.T) {
	err := inferErr(t, "[1, 2, 3]", nil, nil)
	var e exprtype.ErrUnsupported
	if !errors.As(err, &e) {
		t.Errorf("expected ErrUnsupported, got %T: %v", err, err)
	}
}

// Nullable array: conditional with nil preserves the items schema.
func TestInfer_Array_NullablePreservesItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "true ? input.tags : nil", c, nil), `{
		"type": ["array", "null"],
		"items": { "type": "string" }
	}`)
}

// --- static index access ---

// Index into a typed array returns the items schema wrapped as nullable.
func TestInfer_Array_Index_KnownItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.tags[0]", c, nil), `{"type":["string","null"]}`)
}

func TestInfer_Array_Index_IntegerItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.counts[0]", c, nil), `{"type":["integer","null"]}`)
}

// Index into an array without items returns {} (any value, including null).
func TestInfer_Array_Index_NoItems(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertSchema(t, infer(t, "input.bare[0]", c, nil), `{}`)
}

// Index into a non-array schema fails.
func TestInfer_Array_Index_NonArrayFails(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	inferErr(t, "input.tags[0][1]", c, nil) // tags[0] is string, not array
}

// Dynamic index is unsupported.
func TestInfer_Array_DynamicIndexUnsupported(t *testing.T) {
	c := ctx(t, arrayCtxJSON)
	assertUnsupported(t, inferErr(t, "input.tags[input.counts[0]]", c, nil))
}

// --- Already-nullable array combined with nil stays the same.
func TestInfer_Array_AlreadyNullableStable(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"tags": { "type": ["array", "null"], "items": { "type": "string" } }
				}
			}
		}
	}`)
	assertSchema(t, infer(t, "true ? input.tags : nil", c, nil), `{
		"type": ["array", "null"],
		"items": { "type": "string" }
	}`)
}
