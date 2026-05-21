package exprtype_test

// TestNullableContract is the authoritative test for how null/nullable operands
// behave across all three layers:
//
//  1. expr-lang (the underlying library) — runtime oracle.
//  2. exprtype.Eval — our thin eval wrapper; must agree with expr-lang.
//  3. exprtype.InferType — static analysis; must predict failures that
//     expr-lang would surface at runtime.
//
// Each case exercises a single expression with the variable "x" set to nil at
// runtime. The infer schema declares "x" as nullable using three equivalent
// notations — {"type":["T","null"]}, {"anyOf":[…]}, {"oneOf":[…]} — so the
// contract is verified regardless of how the process definition was authored.

import (
	"fmt"
	"testing"

	exprlib "github.com/expr-lang/expr"

	"gent/internal/exprtype"
)

// nullableSchema builds a schema with a single nullable field "x" of the given
// base type, expressed in the requested notation.
func nullableSchema(base, notation string) map[string]any {
	var xSchema map[string]any
	switch notation {
	case "type_array":
		xSchema = map[string]any{"type": []any{base, "null"}}
	case "anyOf":
		xSchema = map[string]any{"anyOf": []any{
			map[string]any{"type": base},
			map[string]any{"type": "null"},
		}}
	case "oneOf":
		xSchema = map[string]any{"oneOf": []any{
			map[string]any{"type": base},
			map[string]any{"type": "null"},
		}}
	default:
		panic(fmt.Sprintf("unknown notation %q", notation))
	}
	return map[string]any{
		"properties": map[string]any{"x": xSchema},
	}
}

func TestNullableContract(t *testing.T) {
	cases := []struct {
		name string
		expr string
		base string // non-null base type of "x"
	}{
		// arithmetic
		{"add_int",    "x + 1",   "integer"},
		{"sub_int",    "x - 1",   "integer"},
		{"mul_int",    "x * 2",   "integer"},
		{"div_int",    "x / 2",   "integer"},
		{"unary_neg",  "-x",      "integer"},
		{"str_concat", `x + "s"`, "string"},

		// ordering comparisons
		{"lt", "x < 1",  "integer"},
		{"gt", "x > 1",  "integer"},
		{"le", "x <= 1", "integer"},
		{"ge", "x >= 1", "integer"},

		// equality — nil is a valid operand
		{"eq",  "x == nil", "integer"},
		{"neq", "x != nil", "integer"},

		// logical
		{"and", "x && true",  "boolean"},
		{"or",  "x || false", "boolean"},
		{"not", "!x",         "boolean"},
	}

	notations := []string{"type_array", "anyOf", "oneOf"}
	evalCtx := map[string]any{"x": nil}

	for _, tc := range cases {
		for _, notation := range notations {
			t.Run(tc.name+"/"+notation, func(t *testing.T) {
				schema := nullableSchema(tc.base, notation)

				_, libErr := exprlib.Eval(tc.expr, evalCtx)
				_, ourErr := exprtype.Eval(tc.expr, evalCtx)
				_, inferErr := exprtype.InferType(tc.expr, schema)

				libFailed := libErr != nil
				ourFailed := ourErr != nil
				inferFailed := inferErr != nil

				// Our Eval must agree with expr-lang.
				if libFailed != ourFailed {
					t.Errorf("eval mismatch:\n  expr-lang: %v\n  our eval:  %v", libErr, ourErr)
				}

				// InferType must predict what expr-lang does at runtime.
				if libFailed != inferFailed {
					t.Errorf("infer/runtime mismatch:\n  expr-lang: %v\n  infer:     %v", libErr, inferErr)
				}
			})
		}
	}
}

// TestAmbiguousTypeContract verifies that InferType rejects anyOf/oneOf schemas
// whose variants include an incompatible type for the operator. The runtime
// oracle uses the incompatible variant as the actual value so that expr-lang
// also fails, keeping eval and infer in sync.
func TestAmbiguousTypeContract(t *testing.T) {
	obj := map[string]any{"value": 1.0}

	cases := []struct {
		name      string
		expr      string
		wrongVal  any    // the incompatible variant value — makes expr-lang fail
		rightType string // the type the operator expects
		wrongType string // the incompatible type in the anyOf
	}{
		// arithmetic
		{"add",        "x + 1",    obj, "integer", "object"},
		{"sub",        "x - 1",    obj, "integer", "object"},
		{"mul",        "x * 2",    obj, "integer", "object"},
		{"div",        "x / 2",    obj, "integer", "object"},
		{"unary_neg",  "-x",       obj, "integer", "object"},
		{"str_concat", `x + "s"`,  42,  "string",  "integer"},

		// ordering comparisons
		{"lt", "x < 1",  obj, "integer", "object"},
		{"gt", "x > 1",  obj, "integer", "object"},
		{"le", "x <= 1", obj, "integer", "object"},
		{"ge", "x >= 1", obj, "integer", "object"},

		// logical
		{"and", "x && true",  obj, "boolean", "object"},
		{"or",  "x || false", obj, "boolean", "object"},
		{"not", "!x",         obj, "boolean", "object"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evalCtx := map[string]any{"x": tc.wrongVal}
			inferSchema := map[string]any{
				"properties": map[string]any{
					"x": map[string]any{
						"anyOf": []any{
							map[string]any{"type": tc.rightType},
							map[string]any{"type": tc.wrongType},
						},
					},
				},
			}

			_, libErr := exprlib.Eval(tc.expr, evalCtx)
			_, ourErr := exprtype.Eval(tc.expr, evalCtx)
			_, inferErr := exprtype.InferType(tc.expr, inferSchema)

			// expr-lang must fail with the incompatible value (sanity check).
			if libErr == nil {
				t.Fatalf("expr-lang: expected error with wrong-type value %T, got nil", tc.wrongVal)
			}

			// Our Eval must agree with expr-lang.
			if ourErr == nil {
				t.Errorf("eval mismatch: expected error with wrong-type value, got nil (expr-lang: %v)", libErr)
			}

			// InferType must reject an anyOf that includes an incompatible type.
			if inferErr == nil {
				t.Errorf("infer/runtime mismatch: InferType accepted an ambiguous schema (expr-lang: %v)", libErr)
			}
		})
	}
}

// TestConditionalNullNarrowing verifies that InferType narrows the type of a
// nullable variable inside the branches of a null-check ternary:
//
//   - "x == nil ? a : b" → in b, x is treated as non-null
//   - "x != nil ? a : b" → in a, x is treated as non-null
//   - the branch where x is known to be null must still reject unsafe operators
//
// All three nullable notations are exercised for each case.
func TestConditionalNullNarrowing(t *testing.T) {
	valid := []struct {
		name string
		expr string
		base string
	}{
		// == nil: else branch has non-null x
		{"eq_nil_else_gt",     "x == nil ? false : x > 0",  "integer"},
		{"eq_nil_else_add",    "x == nil ? 0 : x + 1",      "integer"},
		{"eq_nil_else_concat", `x == nil ? "" : x + "!"`,   "string"},
		{"eq_nil_else_not",    "x == nil ? true : !x",      "boolean"},
		// != nil: then branch has non-null x
		{"ne_nil_then_gt",     "x != nil ? x > 0 : false",  "integer"},
		{"ne_nil_then_add",    "x != nil ? x + 1 : 0",      "integer"},
		// commuted nil position
		{"nil_eq_else_gt",     "nil == x ? false : x > 0",  "integer"},
		{"nil_ne_then_gt",     "nil != x ? x > 0 : false",  "integer"},
	}

	invalid := []struct {
		name string
		expr string
		base string
	}{
		// == nil: then branch has null x → unsafe operators must fail
		{"eq_nil_then_add", "x == nil ? x + 1 : 0",   "integer"},
		// != nil: else branch has null x → unsafe operators must fail
		{"ne_nil_else_gt",  "x != nil ? false : x > 0", "integer"},
	}

	notations := []string{"type_array", "anyOf", "oneOf"}

	for _, tc := range valid {
		for _, notation := range notations {
			t.Run(tc.name+"/"+notation, func(t *testing.T) {
				infer(t, tc.expr, nullableSchema(tc.base, notation))
			})
		}
	}

	for _, tc := range invalid {
		for _, notation := range notations {
			t.Run(tc.name+"/"+notation, func(t *testing.T) {
				inferErr(t, tc.expr, nullableSchema(tc.base, notation), "")
			})
		}
	}
}

// TestConditionalNullNarrowingResultType checks that the inferred schema of a
// null-guarded ternary is the concrete non-null type, not a nullable union.
func TestConditionalNullNarrowingResultType(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		base     string
		wantJSON string
	}{
		{"eq_nil_bool_else_cmp", "x == nil ? false : x > 0", "integer", `{"type":"boolean"}`},
		{"eq_nil_zero_else_x",   "x == nil ? 0 : x",         "integer", `{"type":"integer"}`},
		{"ne_nil_x_else_zero",   "x != nil ? x : 0",         "integer", `{"type":"integer"}`},
	}

	notations := []string{"type_array", "anyOf", "oneOf"}

	for _, tc := range cases {
		for _, notation := range notations {
			t.Run(tc.name+"/"+notation, func(t *testing.T) {
				assertSchema(t, infer(t, tc.expr, nullableSchema(tc.base, notation)), tc.wantJSON)
			})
		}
	}
}

// TestConditionalLiteralNarrowing verifies that equality against a non-nil
// literal narrows the subject to the literal's type in the matching branch:
//
//   - "x == <lit> ? a : b" → in a, x has the literal's type
//   - "x != <lit> ? a : b" → in b, x has the literal's type
func TestConditionalLiteralNarrowing(t *testing.T) {
	cases := []struct {
		name     string
		expr     string
		base     string
		notation string
	}{
		// string literal — then branch
		{"eq_str_then_concat/type_array", `x == "hi" ? x + "!" : ""`,   "string", "type_array"},
		{"eq_str_then_concat/anyOf",      `x == "hi" ? x + "!" : ""`,   "string", "anyOf"},
		{"eq_str_then_concat/oneOf",      `x == "hi" ? x + "!" : ""`,   "string", "oneOf"},
		// string literal — else branch
		{"ne_str_else_concat/type_array", `x != "hi" ? "" : x + "!"`,   "string", "type_array"},
		{"ne_str_else_concat/anyOf",      `x != "hi" ? "" : x + "!"`,   "string", "anyOf"},
		{"ne_str_else_concat/oneOf",      `x != "hi" ? "" : x + "!"`,   "string", "oneOf"},
		// integer literal — then branch
		{"eq_int_then_add/type_array",    "x == 5 ? x + 1 : 0",         "integer", "type_array"},
		{"eq_int_then_add/anyOf",         "x == 5 ? x + 1 : 0",         "integer", "anyOf"},
		{"eq_int_then_add/oneOf",         "x == 5 ? x + 1 : 0",         "integer", "oneOf"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			infer(t, tc.expr, nullableSchema(tc.base, tc.notation))
		})
	}
}

// TestNumericUnionContract verifies that expressions work correctly when the
// operand schema is an all-numeric union (anyOf/oneOf with integer and number
// variants). Both layers must agree with expr-lang and must NOT error.
func TestNumericUnionContract(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		// arithmetic — result type is number (widened from integer|number)
		{"add",       "x + 1"},
		{"sub",       "x - 1"},
		{"mul",       "x * 2"},
		{"div",       "x / 2"},
		{"unary_neg", "-x"},

		// ordering comparisons
		{"lt", "x < 100"},
		{"gt", "x > 100"},
		{"le", "x <= 100"},
		{"ge", "x >= 100"},

		// equality
		{"eq",  "x == 5"},
		{"neq", "x != 5"},
	}

	notations := map[string]map[string]any{
		"anyOf": {"properties": map[string]any{"x": map[string]any{
			"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "number"}},
		}}},
		"oneOf": {"properties": map[string]any{"x": map[string]any{
			"oneOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "number"}},
		}}},
	}

	// Exercise both the integer and the float variant at runtime.
	runtimeValues := []any{5, 5.5}

	for _, tc := range cases {
		for notation, schema := range notations {
			// InferType is independent of runtime value — check once per (expr, schema).
			t.Run(tc.name+"/"+notation+"/infer", func(t *testing.T) {
				_, inferErr := exprtype.InferType(tc.expr, schema)
				if inferErr != nil {
					t.Errorf("InferType rejected a valid all-numeric union: %v", inferErr)
				}
			})

			for _, xVal := range runtimeValues {
				xVal := xVal
				t.Run(fmt.Sprintf("%s/%s/x=%v", tc.name, notation, xVal), func(t *testing.T) {
					evalCtx := map[string]any{"x": xVal}

					_, libErr := exprlib.Eval(tc.expr, evalCtx)
					_, ourErr := exprtype.Eval(tc.expr, evalCtx)

					// expr-lang must succeed — sanity-check the test case itself.
					if libErr != nil {
						t.Fatalf("expr-lang: unexpected error: %v", libErr)
					}

					// Our Eval must agree with expr-lang.
					if ourErr != nil {
						t.Errorf("eval mismatch: unexpected error: %v", ourErr)
					}
				})
			}
		}
	}
}
