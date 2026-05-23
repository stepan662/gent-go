package exprtype_test

import (
	"errors"
	"testing"

	"gent/internal/exprtype"
)

// --- Literals ---

func TestEval_IntegerLiteral(t *testing.T) {
	assertEq(t, evalOK(t, "7", nil), 7)
}
func TestEval_FloatLiteral(t *testing.T) {
	assertEq(t, evalOK(t, "3.14", nil), 3.14)
}
func TestEval_StringLiteral(t *testing.T) {
	assertEq(t, evalOK(t, `"hi"`, nil), "hi")
}
func TestEval_BoolLiteralTrue(t *testing.T) {
	assertEq(t, evalOK(t, "true", nil), true)
}
func TestEval_BoolLiteralFalse(t *testing.T) {
	assertEq(t, evalOK(t, "false", nil), false)
}
func TestEval_NilLiteral(t *testing.T) {
	assertEq(t, evalOK(t, "nil", nil), nil)
}

// --- Field access ---

func TestEval_TopLevelField(t *testing.T) {
	v := evalOK(t, "input", richCtx)
	m, ok := v.(map[string]any)
	if !ok || m["order_id"] == nil {
		t.Errorf("expected input map, got %v", v)
	}
}

func TestEval_NestedField_Int(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id", richCtx), 42)
}

func TestEval_NestedField_Float(t *testing.T) {
	assertEq(t, evalOK(t, "input.amount", richCtx), 9.99)
}

func TestEval_NestedField_Bool(t *testing.T) {
	assertEq(t, evalOK(t, "outputs.charge.charged", richCtx), true)
}

func TestEval_NestedField_DeepPath(t *testing.T) {
	assertEq(t, evalOK(t, "outputs.charge.fee", richCtx), 1.5)
}

func TestEval_FieldNotFound(t *testing.T) {
	assertEq(t, evalOK(t, "input.missing", richCtx), nil)
}

func TestEval_FieldOnNonObject(t *testing.T) {
	evalErr(t, "input.order_id.x", richCtx)
}

// --- Arithmetic ---

func TestEval_IntPlusInt(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id + 1", richCtx), 43)
}

func TestEval_IntPlusFloat(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id + 0.5", richCtx), 42.5)
}

func TestEval_Subtraction(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id - 2", richCtx), 40)
}

func TestEval_Multiplication(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id * 2", richCtx), 84)
}

func TestEval_Division(t *testing.T) {
	assertEq(t, evalOK(t, "10.0 / 4.0", nil), 2.5)
}

func TestEval_DivisionByZero(t *testing.T) {
	evalErr(t, "1 / 0", nil)
}

func TestEval_StringConcat(t *testing.T) {
	assertEq(t, evalOK(t, `input.label + "_v2"`, richCtx), "order_v2")
}

func TestEval_ArithmeticOnStrings_Error(t *testing.T) {
	evalErr(t, "input.label - 1", richCtx)
}

// --- Unary ---

func TestEval_UnaryNot(t *testing.T) {
	assertEq(t, evalOK(t, "!input.active", richCtx), false)
	assertEq(t, evalOK(t, "!false", nil), true)
}

func TestEval_UnaryMinus(t *testing.T) {
	assertEq(t, evalOK(t, "-input.order_id", richCtx), -42)
}

func TestEval_UnaryMinusOnString_Error(t *testing.T) {
	evalErr(t, "-input.label", richCtx)
}

// --- Comparison ---

func TestEval_EqualInts(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id == 42", richCtx), true)
	assertEq(t, evalOK(t, "input.order_id == 0", richCtx), false)
}

func TestEval_NotEqual(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id != 0", richCtx), true)
}

func TestEval_LessThan(t *testing.T) {
	assertEq(t, evalOK(t, "input.order_id < 100", richCtx), true)
	assertEq(t, evalOK(t, "input.order_id < 10", richCtx), false)
}

func TestEval_GreaterThan(t *testing.T) {
	assertEq(t, evalOK(t, "input.amount > 5.0", richCtx), true)
}

func TestEval_EqualStrings(t *testing.T) {
	assertEq(t, evalOK(t, `input.label == "order"`, richCtx), true)
}

func TestEval_EqualBools(t *testing.T) {
	assertEq(t, evalOK(t, "input.active == true", richCtx), true)
}

// --- Logical ---

func TestEval_LogicalAnd_BothTrue(t *testing.T) {
	assertEq(t, evalOK(t, "input.active && outputs.charge.charged", richCtx), true)
}

func TestEval_LogicalAnd_ShortCircuit(t *testing.T) {
	// false && <anything> — second side should not be evaluated
	assertEq(t, evalOK(t, "false && input.missing_field", richCtx), false)
}

func TestEval_LogicalOr_ShortCircuit(t *testing.T) {
	// true || <anything> — second side should not be evaluated
	assertEq(t, evalOK(t, "true || input.missing_field", richCtx), true)
}

func TestEval_LogicalOr_BothFalse(t *testing.T) {
	assertEq(t, evalOK(t, "false || false", nil), false)
}

// --- Conditional ---

func TestEval_Conditional_TrueBranch(t *testing.T) {
	assertEq(t, evalOK(t, "input.active ? 1 : 2", richCtx), 1)
}

func TestEval_Conditional_FalseBranch(t *testing.T) {
	assertEq(t, evalOK(t, "false ? 1 : 2", nil), 2)
}

func TestEval_Conditional_FieldFromContext(t *testing.T) {
	assertEq(t, evalOK(t, `outputs.charge.charged ? "yes" : "no"`, richCtx), "yes")
}

// --- Static index access ---

func TestEval_Index_InBounds(t *testing.T) {
	c := map[string]any{"tags": []any{"a", "b", "c"}}
	assertEq(t, evalOK(t, "tags[0]", c), "a")
	assertEq(t, evalOK(t, "tags[2]", c), "c")
}

func TestEval_Index_OutOfBounds_ReturnsNil(t *testing.T) {
	c := map[string]any{"tags": []any{"a", "b"}}
	assertEq(t, evalOK(t, "tags[5]", c), nil)
}

func TestEval_Index_NonSliceFails(t *testing.T) {
	evalErr(t, "input.order_id[0]", richCtx)
}

// --- Unsupported ---

func TestEval_FunctionCall_Unsupported(t *testing.T) {
	err := evalErr(t, `len("hello")`, nil)
	var e exprtype.ErrUnsupported
	if !errors.As(err, &e) {
		t.Errorf("expected ErrUnsupported, got %T: %v", err, err)
	}
}
