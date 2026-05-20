package exprtype_test

import (
	"testing"
)

// complexCtxJSON is a context schema whose fields use anyOf, oneOf, allOf and not.
const complexCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"flexible":      { "anyOf": [{"type":"integer"}, {"type":"string"}] },
				"exactly_one":   { "oneOf": [{"type":"integer"}, {"type":"number"}] },
				"combined":      { "allOf": [{"type":"integer"}, {"minimum": 0}] },
				"negated":       { "not": {"type":"string"} },
				"numeric_union": { "anyOf": [{"type":"integer"}, {"type":"number"}] },
				"obj_union": {
					"anyOf": [
						{"type":"object","properties":{"x":{"type":"integer"}}},
						{"type":"object","properties":{"x":{"type":"string"}}}
					]
				},
				"obj_union_oneof": {
					"oneOf": [
						{"type":"object","properties":{"x":{"type":"boolean"}}},
						{"type":"object","properties":{"x":{"type":"number"}}}
					]
				},
				"obj_union_same": {
					"anyOf": [
						{"type":"object","properties":{"x":{"type":"integer"}}},
						{"type":"object","properties":{"x":{"type":"integer"}}}
					]
				},
				"obj_union_nested": {
					"anyOf": [
						{"type":"object","properties":{"inner":{"type":"object","properties":{"z":{"type":"boolean"}}}}},
						{"type":"object","properties":{"inner":{"type":"object","properties":{"z":{"type":"integer"}}}}}
					]
				}
			}
		}
	}
}`

// --- field access: complex schemas are returned as-is ---

func TestInfer_AnyOf_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.flexible", c, nil), `{
		"anyOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

func TestInfer_OneOf_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.exactly_one", c, nil), `{
		"oneOf": [{"type":"integer"}, {"type":"number"}]
	}`)
}

func TestInfer_AllOf_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.combined", c, nil), `{
		"allOf": [{"type":"integer"}, {"minimum": 0}]
	}`)
}

func TestInfer_Not_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.negated", c, nil), `{
		"not": {"type":"string"}
	}`)
}

// --- comparison operators always produce boolean regardless of operand schema ---

func TestInfer_AnyOf_EqualityIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, `input.flexible == "x"`, c, nil), `{"type":"boolean"}`)
}

func TestInfer_AnyOf_InequalityIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.flexible != 0", c, nil), `{"type":"boolean"}`)
}

func TestInfer_AnyOf_OrderComparisonIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.flexible < 10", c, nil), `{"type":"boolean"}`)
}

func TestInfer_OneOf_EqualityIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.exactly_one == 0", c, nil), `{"type":"boolean"}`)
}

func TestInfer_Not_EqualityIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, `input.negated == "hello"`, c, nil), `{"type":"boolean"}`)
}

// --- logical operators always produce boolean ---

func TestInfer_AnyOf_LogicalAndIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// both operands are comparisons → boolean && boolean → boolean
	assertSchema(t, infer(t, "input.flexible == 1 && input.flexible == 2", c, nil), `{"type":"boolean"}`)
}

func TestInfer_AnyOf_UnaryNotIsBoolean(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// ! wraps a comparison; the inner comparison is boolean so ! is valid
	assertSchema(t, infer(t, "!(input.flexible == 1)", c, nil), `{"type":"boolean"}`)
}

// --- arithmetic on complex schemas fails (cannot determine numeric type) ---

func TestInfer_AnyOf_ArithmeticFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	inferErr(t, "input.flexible + 1", c, nil)
}

func TestInfer_OneOf_ArithmeticFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	inferErr(t, "input.exactly_one * 2", c, nil)
}

func TestInfer_AllOf_ArithmeticFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// allOf carries no plain "type" key at the top level
	inferErr(t, "input.combined + 1", c, nil)
}

func TestInfer_Not_ArithmeticFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	inferErr(t, "input.negated + 1", c, nil)
}

// --- nullable types ---

// nullableCtxJSON has fields declared with the {type: [X, "null"]} notation.
const nullableCtxJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"id":      { "type": "integer" },
				"comment": { "type": ["string", "null"] },
				"amount":  { "type": ["number", "null"] }
			}
		}
	}
}`

// Accessing a nullable field returns its schema as-is.
func TestInfer_Nullable_FieldReturnsSchema(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, "input.comment", c, nil), `{"type":["string","null"]}`)
}

// A conditional where one branch is nil produces a nullable type.
func TestInfer_Nullable_ConditionalWithNil_String(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, `input.id > 0 ? input.comment : nil`, c, nil), `{"type":["string","null"]}`)
}

func TestInfer_Nullable_ConditionalWithNil_Integer(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, `true ? input.id : nil`, c, nil), `{"type":["integer","null"]}`)
}

// nil on the left branch works the same way.
func TestInfer_Nullable_ConditionalNilFirst(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, `false ? nil : input.id`, c, nil), `{"type":["integer","null"]}`)
}

// Both branches nil → plain null type (not a type array).
func TestInfer_Nullable_BothBranchesNil(t *testing.T) {
	assertSchema(t, infer(t, `true ? nil : nil`, nil, nil), `{"type":"null"}`)
}

// Arithmetic on a nullable type fails — the null case is unhandled.
func TestInfer_Nullable_ArithmeticFails(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	inferErr(t, "input.amount + 1.0", c, nil)
}

// Comparisons on nullable types still return boolean.
func TestInfer_Nullable_ComparisonIsBoolean(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	assertSchema(t, infer(t, "input.comment == nil", c, nil), `{"type":"boolean"}`)
	assertSchema(t, infer(t, "input.amount != nil", c, nil), `{"type":"boolean"}`)
}

// A non-null + complex-type branch still falls back to oneOf.
func TestInfer_Nullable_ComplexBranchFallsBackToOneOf(t *testing.T) {
	c := ctx(t, nullableCtxJSON)
	// anyOf field vs nil — the non-null branch has no simple type string
	assertSchema(t, infer(t, `true ? input.comment : input.id`, c, nil), `{
		"oneOf": [{"type":["string","null"]}, {"type":"integer"}]
	}`)
}

// --- member access through anyOf / oneOf object variants ---

// TestInfer_AnyOf_MemberAccess_DifferentTypes accesses .x on a field that is
// anyOf two object schemas each with a differently typed .x — result is anyOf
// of those two types.
func TestInfer_AnyOf_MemberAccess_DifferentTypes(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union.x", c, nil), `{
		"anyOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

// TestInfer_OneOf_MemberAccess_DifferentTypes same as above but oneOf.
func TestInfer_OneOf_MemberAccess_DifferentTypes(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union_oneof.x", c, nil), `{
		"oneOf": [{"type":"boolean"}, {"type":"number"}]
	}`)
}

// TestInfer_AnyOf_MemberAccess_SameType deduplicates identical variant results
// and returns the type directly instead of wrapping in anyOf.
func TestInfer_AnyOf_MemberAccess_SameType(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union_same.x", c, nil), `{"type":"integer"}`)
}

// TestInfer_AnyOf_MemberAccess_DeepPath walks through two levels of anyOf.
func TestInfer_AnyOf_MemberAccess_DeepPath(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	assertSchema(t, infer(t, "input.obj_union_nested.inner.z", c, nil), `{
		"anyOf": [{"type":"boolean"}, {"type":"integer"}]
	}`)
}

// TestInfer_AnyOf_MemberAccess_MissingInVariant errors when one variant lacks
// the requested property.
func TestInfer_AnyOf_MemberAccess_MissingInVariant(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": {
				"type": "object",
				"properties": {
					"partial": {
						"anyOf": [
							{"type":"object","properties":{"x":{"type":"integer"}}},
							{"type":"object","properties":{"y":{"type":"string"}}}
						]
					}
				}
			}
		}
	}`)
	inferErr(t, "input.partial.x", c, nil)
}

// TestInfer_AnyOf_MemberAccess_NonObjectVariant errors when a variant is not
// an object schema.
func TestInfer_AnyOf_MemberAccess_NonObjectVariant(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// input.flexible is anyOf [integer, string] — neither has properties
	inferErr(t, "input.flexible.x", c, nil)
}

// --- member access on opaque schemas (not, allOf, unknown) always fails ---

func TestInfer_Not_MemberAccessFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// "not" schema carries no structural information — cannot resolve .x
	inferErr(t, "input.negated.x", c, nil)
}

func TestInfer_AllOf_MemberAccessFails(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// allOf without a top-level properties map — cannot resolve .x
	inferErr(t, "input.combined.x", c, nil)
}

// --- conditional with complex schemas ---

func TestInfer_Conditional_AnyOfBothBranches(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// both branches have the same anyOf schema → result is that schema
	assertSchema(t, infer(t, "true ? input.flexible : input.flexible", c, nil), `{
		"anyOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

func TestInfer_Conditional_AnyOfVsLiteral(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// branches differ → oneOf wrapping both
	assertSchema(t, infer(t, "true ? input.flexible : 42", c, nil), `{
		"oneOf": [
			{"anyOf": [{"type":"integer"}, {"type":"string"}]},
			{"type":"integer"}
		]
	}`)
}

func TestInfer_Conditional_OneOfVsOneOf(t *testing.T) {
	c := ctx(t, complexCtxJSON)
	// two different oneOf schemas → wrapped in oneOf
	assertSchema(t, infer(t, "true ? input.flexible : input.exactly_one", c, nil), `{
		"oneOf": [
			{"anyOf": [{"type":"integer"}, {"type":"string"}]},
			{"oneOf": [{"type":"integer"}, {"type":"number"}]}
		]
	}`)
}
