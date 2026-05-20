// Package exprtype provides static type inference and evaluation for a subset
// of expr-lang expressions against a JSON Schema context.
//
// Supported subset:
//   - Literals: integer, float, string, bool, nil
//   - Field access via dot notation: input.x, outputs.task.y
//   - Arithmetic: +, -, *, / (numbers; + also concatenates strings)
//   - Comparison: ==, !=, <, >, <=, >= → boolean
//   - Logical: &&, || → boolean (short-circuit); ! → boolean
//   - Conditional: cond ? a : b
//
// All other expr-lang constructs return ErrUnsupported, so the accepted subset
// is identical for both InferType and Eval.
package exprtype

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// InferType statically determines the JSON Schema type of expression when
// evaluated against a context matching contextSchema.
// defs provides $ref resolution; pass nil if contextSchema contains no $refs.
func InferType(expression string, contextSchema map[string]any, defs map[string]any) (map[string]any, error) {
	tree, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", expression, err)
	}
	return inferNode(tree.Node, contextSchema, defs)
}

func inferNode(node ast.Node, ctx map[string]any, defs map[string]any) (map[string]any, error) {
	switch n := node.(type) {
	case *ast.IntegerNode:
		return typeSchema("integer"), nil
	case *ast.FloatNode:
		return typeSchema("number"), nil
	case *ast.StringNode:
		return typeSchema("string"), nil
	case *ast.BoolNode:
		return typeSchema("boolean"), nil
	case *ast.NilNode:
		return typeSchema("null"), nil
	case *ast.IdentifierNode:
		return lookupProperty(ctx, n.Value, defs)
	case *ast.MemberNode:
		return inferMember(n, ctx, defs)
	case *ast.BinaryNode:
		return inferBinary(n, ctx, defs)
	case *ast.UnaryNode:
		return inferUnary(n, ctx, defs)
	case *ast.ConditionalNode:
		return inferConditional(n, ctx, defs)
	default:
		return nil, ErrUnsupported{Detail: fmt.Sprintf("node type %T", node)}
	}
}

func inferMember(n *ast.MemberNode, ctx map[string]any, defs map[string]any) (map[string]any, error) {
	base, err := inferNode(n.Node, ctx, defs)
	if err != nil {
		return nil, err
	}
	switch prop := n.Property.(type) {
	case *ast.StringNode:
		return lookupProperty(base, prop.Value, defs)
	case *ast.IntegerNode:
		return inferIndex(base)
	default:
		return nil, ErrUnsupported{Detail: "computed member access [expr]"}
	}
}

// inferIndex returns the nullable element type for a static index into an array schema.
// The result is always nullable because the index may be out of bounds at runtime.
func inferIndex(arraySchema map[string]any) (map[string]any, error) {
	t, _ := arraySchema["type"].(string)
	if t != "array" {
		return nil, fmt.Errorf("index access [n] requires an array schema, got type %q", t)
	}
	items, _ := arraySchema["items"].(map[string]any)
	if items == nil {
		return map[string]any{}, nil // no items constraint → any element, including null
	}
	return withNull(items), nil
}

func inferBinary(n *ast.BinaryNode, ctx map[string]any, defs map[string]any) (map[string]any, error) {
	op, ok := binaryOps[n.Operator]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("operator %q", n.Operator)}
	}
	left, err := inferNode(n.Left, ctx, defs)
	if err != nil {
		return nil, err
	}
	right, err := inferNode(n.Right, ctx, defs)
	if err != nil {
		return nil, err
	}
	return op.infer(left, right)
}

func inferUnary(n *ast.UnaryNode, ctx map[string]any, defs map[string]any) (map[string]any, error) {
	op, ok := unaryOps[n.Operator]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("unary operator %q", n.Operator)}
	}
	operand, err := inferNode(n.Node, ctx, defs)
	if err != nil {
		return nil, err
	}
	return op.infer(operand)
}

func inferConditional(n *ast.ConditionalNode, ctx map[string]any, defs map[string]any) (map[string]any, error) {
	t, err := inferNode(n.Exp1, ctx, defs)
	if err != nil {
		return nil, err
	}
	f, err := inferNode(n.Exp2, ctx, defs)
	if err != nil {
		return nil, err
	}
	if schemasEqual(t, f) {
		return t, nil
	}
	if s, ok := nullableSchema(t, f); ok {
		return s, nil
	}
	return map[string]any{"oneOf": []any{t, f}}, nil
}

// lookupProperty returns the schema of a named property within schemaObj,
// resolving any $ref using defs first.
// For anyOf/oneOf schemas it tries each variant and collects the results.
func lookupProperty(schemaObj map[string]any, name string, defs map[string]any) (map[string]any, error) {
	resolved, err := deref(schemaObj, defs)
	if err != nil {
		return nil, err
	}

	// For anyOf / oneOf: resolve the property in every variant and collect results.
	for _, kw := range []string{"anyOf", "oneOf"} {
		variants, ok := resolved[kw].([]any)
		if !ok {
			continue
		}
		results := make([]any, 0, len(variants))
		for i, v := range variants {
			varSchema, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("cannot access .%s: %s[%d] is not a schema object", name, kw, i)
			}
			r, err := lookupProperty(varSchema, name, defs)
			if err != nil {
				return nil, fmt.Errorf("cannot access .%s in %s[%d]: %w", name, kw, i, err)
			}
			results = append(results, r)
		}
		if len(results) == 0 {
			return nil, fmt.Errorf("cannot access .%s: %s has no variants", name, kw)
		}
		if allSameSchema(results) {
			return results[0].(map[string]any), nil
		}
		return map[string]any{kw: results}, nil
	}

	// Standard case: flat object schema with properties.
	props, _ := resolved["properties"].(map[string]any)
	if props == nil {
		return nil, fmt.Errorf("cannot access .%s: schema has no properties", name)
	}
	prop, ok := props[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("field %q not found in schema", name)
	}
	return deref(prop, defs)
}

func allSameSchema(schemas []any) bool {
	if len(schemas) == 0 {
		return true
	}
	first, _ := json.Marshal(schemas[0])
	for _, s := range schemas[1:] {
		other, _ := json.Marshal(s)
		if string(first) != string(other) {
			return false
		}
	}
	return true
}

// deref follows a $ref pointer if present, looking it up in defs.
func deref(s map[string]any, defs map[string]any) (map[string]any, error) {
	ref, ok := s["$ref"].(string)
	if !ok {
		return s, nil
	}
	if defs == nil {
		return nil, fmt.Errorf("cannot resolve $ref %q: no defs provided", ref)
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref format %q: only #/$defs/<name> is supported", ref)
	}
	target, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q not found in defs", ref)
	}
	return target, nil
}
