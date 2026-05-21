package exprtype_test

import (
	"encoding/json"
	"errors"
	"testing"

	"gent/internal/exprtype"
)

// ctx parses a JSON string into a context schema map.
func ctx(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid context schema JSON: %v", err)
	}
	return m
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

// inferErr calls InferType and expects an error.
func inferErr(t *testing.T, expr string, schema map[string]any) error {
	t.Helper()
	_, err := exprtype.InferType(expr, schema)
	if err == nil {
		t.Fatalf("InferType(%q): expected error, got nil", expr)
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

// assertEq compares two values with numeric leniency (int and float64 with the
// same value are considered equal).
func assertEq(t *testing.T, got, want any) {
	t.Helper()
	gf, gok := toF64(got)
	wf, wok := toF64(want)
	if gok && wok {
		if gf != wf {
			t.Errorf("got %v (%T), want %v (%T)", got, got, want, want)
		}
		return
	}
	if got != want {
		t.Errorf("got %v (%T), want %v (%T)", got, got, want, want)
	}
}

func toF64(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// richCtx is a reusable runtime context for eval tests.
var richCtx = map[string]any{
	"input": map[string]any{
		"order_id": 42,
		"amount":   9.99,
		"label":    "order",
		"active":   true,
	},
	"outputs": map[string]any{
		"charge": map[string]any{
			"charged": true,
			"fee":     1.5,
		},
	},
}

// assertUnsupported checks that the error is ErrUnsupported.
func assertUnsupported(t *testing.T, err error) {
	t.Helper()
	var e exprtype.ErrUnsupported
	if !errors.As(err, &e) {
		t.Errorf("expected ErrUnsupported, got %T: %v", err, err)
	}
}

// richContext is a reusable context schema for most tests.
const richContextJSON = `{
	"type": "object",
	"properties": {
		"input": {
			"type": "object",
			"properties": {
				"order_id": { "type": "integer" },
				"amount":   { "type": "number"  },
				"label":    { "type": "string"  },
				"active":   { "type": "boolean" }
			}
		},
		"outputs": {
			"type": "object",
			"properties": {
				"charge": {
					"type": "object",
					"properties": {
						"charged": { "type": "boolean" },
						"fee":     { "type": "number"  }
					}
				}
			}
		}
	}
}`
