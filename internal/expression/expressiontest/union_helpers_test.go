package expressiontest

import (
	"encoding/json"
	"fmt"
	"testing"

	exprlib "github.com/expr-lang/expr"

	"genroc/internal/expression"
	"genroc/internal/schema"
)

// namedSchema pairs a notation label with a schema so tests can loop over
// multiple representations of the same logical schema.
type namedSchema struct {
	name   string
	schema schema.Schema
}

// mustSchema parses a JSON schema string and panics if it is invalid.
// Intended for use in package-level var declarations.
func mustSchema(s string) schema.Schema {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic("invalid schema JSON: " + err.Error())
	}
	return schema.Load(m)
}

// Nullable schema sets — "x" declared as T|null in all three JSON Schema notations.
var (
	nullableInteger = []namedSchema{
		{"type_array", mustSchema(`{"properties": {"x": {"type": ["integer", "null"]}}, "required": ["x"]}`)},
		{"anyOf", mustSchema(`{"properties": {"x": {"anyOf": [{"type": "integer"}, {"type": "null"}]}}, "required": ["x"]}`)},
		{"oneOf", mustSchema(`{"properties": {"x": {"oneOf": [{"type": "integer"}, {"type": "null"}]}}, "required": ["x"]}`)},
	}
	nullableString = []namedSchema{
		{"type_array", mustSchema(`{"properties": {"x": {"type": ["string", "null"]}}, "required": ["x"]}`)},
		{"anyOf", mustSchema(`{"properties": {"x": {"anyOf": [{"type": "string"}, {"type": "null"}]}}, "required": ["x"]}`)},
		{"oneOf", mustSchema(`{"properties": {"x": {"oneOf": [{"type": "string"}, {"type": "null"}]}}, "required": ["x"]}`)},
	}
	nullableBoolean = []namedSchema{
		{"type_array", mustSchema(`{"properties": {"x": {"type": ["boolean", "null"]}}, "required": ["x"]}`)},
		{"anyOf", mustSchema(`{"properties": {"x": {"anyOf": [{"type": "boolean"}, {"type": "null"}]}}, "required": ["x"]}`)},
		{"oneOf", mustSchema(`{"properties": {"x": {"oneOf": [{"type": "boolean"}, {"type": "null"}]}}, "required": ["x"]}`)},
	}
)

// Commonly shared non-nullable and union schemas.
var (
	integerXSchema = mustSchema(`{"properties": {"x": {"type": "integer"}}, "required": ["x"]}`)

	numericUnionSchemas = []namedSchema{
		{"anyOf", mustSchema(`{"properties": {"x": {"anyOf": [{"type": "integer"}, {"type": "number"}]}}, "required": ["x"]}`)},
		{"oneOf", mustSchema(`{"properties": {"x": {"oneOf": [{"type": "integer"}, {"type": "number"}]}}, "required": ["x"]}`)},
	}
)

// testNullableOperand checks all three layers (expr-lang, Eval, InferType) for
// expr with "x" set to nil, running once per schema in schemas as a subtest.
func testNullableOperand(t *testing.T, expr string, schemas []namedSchema) {
	t.Helper()
	evalCtx := map[string]any{"x": nil}
	for _, ns := range schemas {
		t.Run(ns.name, func(t *testing.T) {
			_, libErr := exprlib.Eval(expr, evalCtx)
			_, ourErr := expression.Eval(expr, evalCtx)
			_, inferErr := expression.InferType(expr, ns.schema)
			if (libErr != nil) != (ourErr != nil) {
				t.Errorf("eval mismatch:\n  expr-lang: %v\n  our eval:  %v", libErr, ourErr)
			}
			if (libErr != nil) != (inferErr != nil) {
				t.Errorf("infer/runtime mismatch:\n  expr-lang: %v\n  infer:     %v", libErr, inferErr)
			}
		})
	}
}

// testAmbiguousTypeCase verifies that both Eval and InferType reject the schema
// when evaluated with wrongVal — the incompatible runtime value.
func testAmbiguousTypeCase(t *testing.T, expr string, wrongVal any, s schema.Schema) {
	t.Helper()
	evalCtx := map[string]any{"x": wrongVal}
	_, libErr := exprlib.Eval(expr, evalCtx)
	_, ourErr := expression.Eval(expr, evalCtx)
	_, inferErr := expression.InferType(expr, s)
	if libErr == nil {
		t.Fatalf("expr-lang: expected error with wrong-type value %T, got nil", wrongVal)
	}
	if ourErr == nil {
		t.Errorf("eval mismatch: expected error with wrong-type value, got nil (expr-lang: %v)", libErr)
	}
	if inferErr == nil {
		t.Errorf("infer/runtime mismatch: InferType accepted an ambiguous schema (expr-lang: %v)", libErr)
	}
}

// testNullableNarrowValid verifies InferType accepts expr for each schema in schemas.
func testNullableNarrowValid(t *testing.T, expr string, schemas []namedSchema) {
	t.Helper()
	for _, ns := range schemas {
		t.Run(ns.name, func(t *testing.T) {
			infer(t, expr, ns.schema)
		})
	}
}

// testNullableNarrowInvalid verifies InferType rejects expr for each schema in schemas.
func testNullableNarrowInvalid(t *testing.T, expr string, schemas []namedSchema) {
	t.Helper()
	for _, ns := range schemas {
		t.Run(ns.name, func(t *testing.T) {
			inferErr(t, expr, ns.schema, "")
		})
	}
}

// testNullableNarrowResultType verifies InferType accepts expr and its result
// matches wantJSON, for each schema in schemas.
func testNullableNarrowResultType(t *testing.T, expr string, schemas []namedSchema, wantJSON string) {
	t.Helper()
	for _, ns := range schemas {
		t.Run(ns.name, func(t *testing.T) {
			assertSchema(t, infer(t, expr, ns.schema), wantJSON)
		})
	}
}

// testNumericUnionCase verifies all three layers for expr with "x" declared as
// an all-numeric union, using both integer and float runtime values.
func testNumericUnionCase(t *testing.T, expr string, schemas []namedSchema) {
	t.Helper()
	for _, ns := range schemas {
		t.Run(ns.name+"/infer", func(t *testing.T) {
			_, err := expression.InferType(expr, ns.schema)
			if err != nil {
				t.Errorf("InferType rejected a valid all-numeric union: %v", err)
			}
		})
		for _, xVal := range []any{5, 5.5} {
			xVal := xVal
			t.Run(fmt.Sprintf("%s/x=%v", ns.name, xVal), func(t *testing.T) {
				evalCtx := map[string]any{"x": xVal}
				_, libErr := exprlib.Eval(expr, evalCtx)
				_, ourErr := expression.Eval(expr, evalCtx)
				if libErr != nil {
					t.Fatalf("expr-lang: unexpected error: %v", libErr)
				}
				if ourErr != nil {
					t.Errorf("eval mismatch: unexpected error: %v", ourErr)
				}
			})
		}
	}
}
