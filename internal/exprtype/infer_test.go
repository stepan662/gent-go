package exprtype_test

import (
	"testing"
)

// --- Literals ---

func TestInfer_IntegerLiteral(t *testing.T) {
	assertSchema(t, infer(t, "42", nil, nil), `{"type":"integer"}`)
}

func TestInfer_FloatLiteral(t *testing.T) {
	assertSchema(t, infer(t, "3.14", nil, nil), `{"type":"number"}`)
}

func TestInfer_StringLiteral(t *testing.T) {
	assertSchema(t, infer(t, `"hello"`, nil, nil), `{"type":"string"}`)
}

func TestInfer_BoolLiteral(t *testing.T) {
	assertSchema(t, infer(t, "true", nil, nil), `{"type":"boolean"}`)
	assertSchema(t, infer(t, "false", nil, nil), `{"type":"boolean"}`)
}

func TestInfer_NilLiteral(t *testing.T) {
	assertSchema(t, infer(t, "nil", nil, nil), `{"type":"null"}`)
}

// --- Field access ---

func TestInfer_TopLevelField(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input", c, nil), `{
		"type": "object",
		"properties": {
			"order_id": {"type":"integer"},
			"amount":   {"type":"number"},
			"label":    {"type":"string"},
			"active":   {"type":"boolean"}
		}
	}`)
}

func TestInfer_NestedField_Integer(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id", c, nil), `{"type":"integer"}`)
}

func TestInfer_NestedField_Number(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.amount", c, nil), `{"type":"number"}`)
}

func TestInfer_NestedField_Boolean(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "outputs.charge.charged", c, nil), `{"type":"boolean"}`)
}

func TestInfer_NestedField_DeepPath(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "outputs.charge.fee", c, nil), `{"type":"number"}`)
}

func TestInfer_FieldNotFound(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "input.missing", c, nil)
}

func TestInfer_FieldOnNonObject(t *testing.T) {
	c := ctx(t, richContextJSON)
	// order_id is integer, not an object — accessing .x should fail
	inferErr(t, "input.order_id.x", c, nil)
}

// --- $ref resolution ---

func TestInfer_RefResolution(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": { "$ref": "#/$defs/Input" }
		}
	}`)
	d := defs(t, `{
		"Input": {
			"type": "object",
			"properties": {
				"order_id": { "type": "integer" }
			}
		}
	}`)
	assertSchema(t, infer(t, "input.order_id", c, d), `{"type":"integer"}`)
}

func TestInfer_RefMissing(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": { "$ref": "#/$defs/Missing" }
		}
	}`)
	inferErr(t, "input.order_id", c, defs(t, `{}`))
}

func TestInfer_RefWithoutDefs(t *testing.T) {
	c := ctx(t, `{
		"type": "object",
		"properties": {
			"input": { "$ref": "#/$defs/Input" }
		}
	}`)
	inferErr(t, "input.order_id", c, nil)
}

// --- Arithmetic ---

func TestInfer_IntPlusInt(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id + 1", c, nil), `{"type":"integer"}`)
}

func TestInfer_IntPlusFloat(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id + input.amount", c, nil), `{"type":"number"}`)
}

func TestInfer_FloatArithmetic(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.amount * 2.0", c, nil), `{"type":"number"}`)
}

func TestInfer_IntegerSubtraction(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id - 1", c, nil), `{"type":"integer"}`)
}

func TestInfer_StringConcatenation(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, `input.label + "_suffix"`, c, nil), `{"type":"string"}`)
}

func TestInfer_ArithmeticOnStrings(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "input.label - 1", c, nil)
}

func TestInfer_ArithmeticOnBool(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "input.active + 1", c, nil)
}

// --- Unary ---

func TestInfer_UnaryNot(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "!input.active", c, nil), `{"type":"boolean"}`)
}

func TestInfer_UnaryMinus(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "-input.order_id", c, nil), `{"type":"integer"}`)
}

func TestInfer_UnaryMinusOnString(t *testing.T) {
	c := ctx(t, richContextJSON)
	inferErr(t, "-input.label", c, nil)
}

// --- Comparison ---

func TestInfer_ComparisonEq(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.order_id == 0", c, nil), `{"type":"boolean"}`)
}

func TestInfer_ComparisonLt(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.amount < 100.0", c, nil), `{"type":"boolean"}`)
}

func TestInfer_LogicalAnd(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.active && outputs.charge.charged", c, nil), `{"type":"boolean"}`)
}

func TestInfer_LogicalOr(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.active || false", c, nil), `{"type":"boolean"}`)
}

// --- Conditional ---

func TestInfer_Conditional_SameType(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, "input.active ? 1 : 2", c, nil), `{"type":"integer"}`)
}

func TestInfer_Conditional_DifferentTypes(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t, infer(t, `input.active ? input.order_id : "unknown"`, c, nil), `{
		"oneOf": [{"type":"integer"}, {"type":"string"}]
	}`)
}

func TestInfer_Conditional_FieldFromContext(t *testing.T) {
	c := ctx(t, richContextJSON)
	assertSchema(t,
		infer(t, "outputs.charge.charged ? input.amount : 0.0", c, nil),
		`{"type":"number"}`)
}

// --- Unsupported constructs ---

func TestInfer_FunctionCall_Unsupported(t *testing.T) {
	err := inferErr(t, `len("hello")`, nil, nil)
	assertUnsupported(t, err)
}

func TestInfer_InOperator_Unsupported(t *testing.T) {
	c := ctx(t, richContextJSON)
	err := inferErr(t, "1 in [1, 2, 3]", c, nil)
	assertUnsupported(t, err)
}
