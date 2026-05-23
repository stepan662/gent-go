package union_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	exprlib "github.com/expr-lang/expr"

	"gent/internal/exprtype"
)

// namedSchema pairs a notation label with a parsed schema map so tests can
// loop over multiple representations of the same logical schema.
type namedSchema struct {
	name   string
	schema map[string]any
}

// mustSchema parses a JSON schema string and panics if it is invalid.
// Intended for use in package-level var declarations.
func mustSchema(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic("invalid schema JSON: " + err.Error())
	}
	return m
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
			_, ourErr := exprtype.Eval(expr, evalCtx)
			_, inferErr := exprtype.InferType(expr, ns.schema)
			if (libErr != nil) != (ourErr != nil) {
				t.Errorf("eval mismatch:\n  expr-lang: %v\n  our eval:  %v", libErr, ourErr)
			}
			if (libErr != nil) != (inferErr != nil) {
				t.Errorf("infer/runtime mismatch:\n  expr-lang: %v\n  infer:     %v", libErr, inferErr)
			}
		})
	}
}

// testAmbiguousTypeCase verifies that both Eval and InferType reject schema
// when evaluated with wrongVal — the incompatible runtime value.
func testAmbiguousTypeCase(t *testing.T, expr string, wrongVal any, schema map[string]any) {
	t.Helper()
	evalCtx := map[string]any{"x": wrongVal}
	_, libErr := exprlib.Eval(expr, evalCtx)
	_, ourErr := exprtype.Eval(expr, evalCtx)
	_, inferErr := exprtype.InferType(expr, schema)
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
			_, err := exprtype.InferType(expr, ns.schema)
			if err != nil {
				t.Errorf("InferType rejected a valid all-numeric union: %v", err)
			}
		})
		for _, xVal := range []any{5, 5.5} {
			xVal := xVal
			t.Run(fmt.Sprintf("%s/x=%v", ns.name, xVal), func(t *testing.T) {
				evalCtx := map[string]any{"x": xVal}
				_, libErr := exprlib.Eval(expr, evalCtx)
				_, ourErr := exprtype.Eval(expr, evalCtx)
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

// infer calls InferType and fails the test on error.
func infer(t *testing.T, expr string, schema map[string]any) map[string]any {
	t.Helper()
	got, err := exprtype.InferType(expr, schema)
	if err != nil {
		t.Fatalf("InferType(%q): %v", expr, err)
	}
	return got
}

// inferErr calls InferType, expects an error, and optionally checks that the
// error message contains wantContains (pass "" to skip the message check).
func inferErr(t *testing.T, expr string, schema map[string]any, wantContains string) error {
	t.Helper()
	_, err := exprtype.InferType(expr, schema)
	if err == nil {
		t.Fatalf("InferType(%q): expected error, got nil", expr)
	}
	if wantContains != "" && !strings.Contains(err.Error(), wantContains) {
		t.Errorf("InferType(%q): error %q does not contain %q", expr, err.Error(), wantContains)
	}
	return err
}

// assertSchema checks that got matches the JSON Schema described by wantJSON.
func assertSchema(t *testing.T, got map[string]any, wantJSON string) {
	t.Helper()
	ga, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatalf("invalid wantJSON: %v\n%s", err, wantJSON)
	}
	wb, _ := json.MarshalIndent(want, "", "  ")
	if string(ga) != string(wb) {
		t.Errorf("schema mismatch:\n got:  %s\n want: %s", ga, wb)
	}
}

// evalOK calls Eval and fails the test on error.
func evalOK(t *testing.T, expr string, context map[string]any) any {
	t.Helper()
	v, err := exprtype.Eval(expr, context)
	if err != nil {
		t.Fatalf("Eval(%q): %v", expr, err)
	}
	return v
}

// evalErr calls Eval and expects an error.
func evalErr(t *testing.T, expr string, context map[string]any) error {
	t.Helper()
	_, err := exprtype.Eval(expr, context)
	if err == nil {
		t.Fatalf("Eval(%q): expected error, got nil", expr)
	}
	return err
}
