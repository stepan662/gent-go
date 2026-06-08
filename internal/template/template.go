// Package template parses and evaluates {{ }} template strings.
//
// Three modes:
//   - Plain string (no {{ }}): returned as a string literal.
//   - Single expression "{{expr}}": evaluated and returned as-is (preserves type).
//   - Mixed "text{{expr}}text": each {{expr}} is evaluated, must be string/number/bool,
//     results are stringified and concatenated with the surrounding literal text.
package template

import (
	"fmt"
	"strings"

	"gent/internal/expression"
	"gent/internal/schema"
)

// EvalAny evaluates s as a template string against ctx.
func EvalAny(s string, ctx map[string]any) (any, error) {
	if expr, ok := singleExpr(s); ok {
		val, err := expression.Eval(expr, ctx)
		if err != nil {
			return nil, fmt.Errorf("template %q: %w", s, err)
		}
		return val, nil
	}
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	return evalMixed(s, ctx)
}

// InferType infers the JSON Schema type of template s against sc.
func InferType(s string, sc schema.Schema) (schema.Schema, error) {
	if expr, ok := singleExpr(s); ok {
		return expression.InferType(expr, sc)
	}
	if strings.Contains(s, "{{") {
		if err := checkMixedNullability(s, sc); err != nil {
			return schema.Schema{}, err
		}
	}
	return schema.FromNode(&schema.SchemaNode{Type: schema.SchemaType{"string"}}), nil
}

// checkMixedNullability rejects mixed templates where any expression may be null.
// Null values would silently become the string "null" at runtime, which is almost
// never intentional. The user must add a ?? default to make the intent explicit.
func checkMixedNullability(s string, sc schema.Schema) error {
	rest := s
	for {
		start := strings.Index(rest, "{{")
		if start == -1 {
			break
		}
		rest = rest[start+2:]
		end := strings.Index(rest, "}}")
		if end == -1 {
			break
		}
		expr := rest[:end]
		rest = rest[end+2:]
		inferred, err := expression.InferType(expr, sc)
		if err != nil {
			return fmt.Errorf("template expression %q: %w", expr, err)
		}
		if schema.HasNullType(inferred.Node()) {
			return fmt.Errorf("template expression %q may be null; use ?? to provide a default value", expr)
		}
	}
	return nil
}

// singleExpr reports whether s is exactly "{{expr}}" with nothing outside.
func singleExpr(s string) (string, bool) {
	if !strings.HasPrefix(s, "{{") || !strings.HasSuffix(s, "}}") {
		return "", false
	}
	inner := s[2 : len(s)-2]
	if strings.Contains(inner, "{{") || strings.Contains(inner, "}}") {
		return "", false
	}
	return inner, true
}

// evalMixed evaluates a mixed template: literal text interleaved with {{expr}} blocks.
// Each expression result must be a string, number, or bool.
func evalMixed(s string, ctx map[string]any) (string, error) {
	var result strings.Builder
	rest := s
	for {
		start := strings.Index(rest, "{{")
		if start == -1 {
			result.WriteString(rest)
			break
		}
		result.WriteString(rest[:start])
		rest = rest[start+2:]
		end := strings.Index(rest, "}}")
		if end == -1 {
			return "", fmt.Errorf("template %q: unclosed {{", s)
		}
		expr := rest[:end]
		rest = rest[end+2:]
		val, err := expression.Eval(expr, ctx)
		if err != nil {
			return "", fmt.Errorf("template expression %q: %w", expr, err)
		}
		str, err := stringify(val)
		if err != nil {
			return "", fmt.Errorf("template expression %q: %w", expr, err)
		}
		result.WriteString(str)
	}
	return result.String(), nil
}

func stringify(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case int:
		return fmt.Sprintf("%d", val), nil
	case int64:
		return fmt.Sprintf("%d", val), nil
	case float32:
		return fmt.Sprintf("%g", float64(val)), nil
	case float64:
		return fmt.Sprintf("%g", val), nil
	case bool:
		if val {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("cannot stringify %T in mixed template (only string, number, bool allowed)", v)
	}
}
