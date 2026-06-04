// Package expression provides static type inference and evaluation for a subset
// of expr-lang expressions against a JSON Schema context.
//
// Supported subset:
//   - Literals: integer, float, string, bool, nil
//   - Field access via dot notation: input.x, outputs.task.y
//   - Arithmetic: +, -, *, / (numbers; + also concatenates strings)
//   - Comparison: ==, !=, <, >, <=, >= → boolean
//   - Logical: &&, || → boolean (short-circuit); ! → boolean
//   - Conditional: cond ? a : b
//   - Null coalescing: a ?? b (returns a if non-nil, else b)
//
// All other expr-lang constructs return ErrUnsupported, so the accepted subset
// is identical for both InferType and Eval.
package expression

import (
	"fmt"

	"gent/internal/schema"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// inferCtx is the immutable type-inference context threaded through all infer
// calls. s and defs are shared across branches; guards is a shallow-copied
// overlay that maps dot-paths to schema overrides for type-narrowed branches.
type inferCtx struct {
	s      map[string]any
	defs   map[string]any
	guards map[string]map[string]any // dot-path → schema override
}

// withGuard returns a new inferCtx with path mapped to narrowed. s and
// defs are shared; only the guards map is copied.
func (c inferCtx) withGuard(path string, narrowed map[string]any) inferCtx {
	guards := make(map[string]map[string]any, len(c.guards)+1)
	for k, v := range c.guards {
		guards[k] = v
	}
	guards[path] = narrowed
	return inferCtx{s: c.s, defs: c.defs, guards: guards}
}

// InferType statically determines the JSON Schema type of expression when
// evaluated against s. $refs are resolved against s's $defs.
func InferType(expression string, s schema.Schema) (schema.Schema, error) {
	raw := s.Raw()
	defs, _ := raw["$defs"].(map[string]any)
	tree, err := parser.Parse(expression)
	if err != nil {
		return schema.Schema{}, fmt.Errorf("parse %q: %w", expression, err)
	}
	result, err := inferNode(tree.Node, inferCtx{s: raw, defs: defs})
	if err != nil {
		return schema.Schema{}, err
	}
	return schema.Load(result), nil
}

func inferNode(node ast.Node, ictx inferCtx) (map[string]any, error) {
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
		if s, ok := ictx.guards[n.Value]; ok {
			return s, nil
		}
		return schema.LookupProperty(ictx.s, n.Value, ictx.defs)
	case *ast.MemberNode:
		return inferMember(n, ictx)
	case *ast.BinaryNode:
		return inferBinary(n, ictx)
	case *ast.UnaryNode:
		return inferUnary(n, ictx)
	case *ast.ConditionalNode:
		return inferConditional(n, ictx)
	default:
		return nil, ErrUnsupported{Detail: fmt.Sprintf("node type %T", node)}
	}
}

func inferMember(n *ast.MemberNode, ictx inferCtx) (map[string]any, error) {
	// Check the guard overlay before traversing the schema — a guarded path
	// short-circuits the full lookup and propagates correctly into sub-accesses.
	if path := nodePath(n); path != "" {
		if s, ok := ictx.guards[path]; ok {
			return s, nil
		}
	}
	base, err := inferNode(n.Node, ictx)
	if err != nil {
		return nil, err
	}
	switch prop := n.Property.(type) {
	case *ast.StringNode:
		return schema.LookupProperty(base, prop.Value, ictx.defs)
	case *ast.IntegerNode:
		return schema.InferIndex(base, ictx.defs)
	default:
		return nil, ErrUnsupported{Detail: "computed member access [expr]"}
	}
}

func inferBinary(n *ast.BinaryNode, ictx inferCtx) (map[string]any, error) {
	op, ok := binaryOps[n.Operator]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("operator %q", n.Operator)}
	}
	left, err := inferNode(n.Left, ictx)
	if err != nil {
		return nil, err
	}
	right, err := inferNode(n.Right, ictx)
	if err != nil {
		return nil, err
	}
	return op.infer(unwrapSingleVariant(left), unwrapSingleVariant(right))
}

func inferUnary(n *ast.UnaryNode, ictx inferCtx) (map[string]any, error) {
	op, ok := unaryOps[n.Operator]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("unary operator %q", n.Operator)}
	}
	operand, err := inferNode(n.Node, ictx)
	if err != nil {
		return nil, err
	}
	return op.infer(unwrapSingleVariant(operand))
}

func inferConditional(n *ast.ConditionalNode, ictx inferCtx) (map[string]any, error) {
	thenCtx, elseCtx := narrowCondition(n.Cond, ictx)
	t, err := inferNode(n.Exp1, thenCtx)
	if err != nil {
		return nil, err
	}
	f, err := inferNode(n.Exp2, elseCtx)
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

// narrowCondition returns then/else contexts narrowed by an equality condition.
// For "x == nil": then→null, else→non-null. For "x != nil": then→non-null, else→null.
// For "x == <literal>": then→literal's type. For "x != <literal>": else→literal's type.
// Returns the original ctx unchanged when the condition doesn't match these patterns.
func narrowCondition(cond ast.Node, ictx inferCtx) (thenCtx, elseCtx inferCtx) {
	thenCtx, elseCtx = ictx, ictx
	bin, ok := cond.(*ast.BinaryNode)
	if !ok || (bin.Operator != "==" && bin.Operator != "!=") {
		return
	}

	var subject, litNode ast.Node
	switch {
	case isLiteralNode(bin.Right):
		subject, litNode = bin.Left, bin.Right
	case isLiteralNode(bin.Left):
		subject, litNode = bin.Right, bin.Left
	default:
		return
	}

	path := nodePath(subject)
	if path == "" {
		return
	}

	litSchema, err := inferNode(litNode, ictx)
	if err != nil {
		return
	}

	_, litIsNull := litNode.(*ast.NilNode)

	if bin.Operator == "==" {
		thenCtx = ictx.withGuard(path, litSchema)
		if litIsNull {
			if subjectSchema, err := inferNode(subject, ictx); err == nil {
				elseCtx = ictx.withGuard(path, stripNull(subjectSchema))
			}
		}
	} else {
		elseCtx = ictx.withGuard(path, litSchema)
		if litIsNull {
			if subjectSchema, err := inferNode(subject, ictx); err == nil {
				thenCtx = ictx.withGuard(path, stripNull(subjectSchema))
			}
		}
	}
	return
}

func isLiteralNode(n ast.Node) bool {
	switch n.(type) {
	case *ast.NilNode, *ast.BoolNode, *ast.StringNode, *ast.IntegerNode, *ast.FloatNode:
		return true
	}
	return false
}

// nodePath extracts a path string from a simple member/identifier chain,
// e.g. "input.amount" or "outputs.items[0].value".
// Returns "" for any other node form (dynamic index, function call, etc.).
func nodePath(node ast.Node) string {
	if node == nil {
		return ""
	}
	switch n := node.(type) {
	case *ast.IdentifierNode:
		return n.Value
	case *ast.MemberNode:
		if base := nodePath(n.Node); base != "" {
			switch prop := n.Property.(type) {
			case *ast.StringNode:
				return base + "." + prop.Value
			case *ast.IntegerNode:
				return fmt.Sprintf("%s[%d]", base, prop.Value)
			}
		}
	}
	return ""
}

// stripNull removes null from a schema's possible types, returning its non-nullable version.
// {"type":["X","null"]} → {"type":"X"}
// {"oneOf":[{"type":"X"},{"type":"null"}]} → {"type":"X"}
// Already non-nullable schemas are returned unchanged.
func stripNull(s map[string]any) map[string]any {
	if types, ok := s["type"].([]any); ok {
		var nonNull []any
		for _, t := range types {
			if t != "null" {
				nonNull = append(nonNull, t)
			}
		}
		if len(nonNull) == len(types) {
			return s
		}
		result := make(map[string]any, len(s))
		for k, v := range s {
			result[k] = v
		}
		if len(nonNull) == 1 {
			result["type"] = nonNull[0]
		} else {
			result["type"] = nonNull
		}
		return result
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		variants, ok := s[kw].([]any)
		if !ok {
			continue
		}
		var nonNull []any
		for _, v := range variants {
			vs, ok := v.(map[string]any)
			if !ok {
				nonNull = append(nonNull, v)
				continue
			}
			if !isNullType(vs) {
				nonNull = append(nonNull, vs)
			}
		}
		if len(nonNull) == len(variants) {
			return s
		}
		if len(nonNull) == 1 {
			return nonNull[0].(map[string]any)
		}
		result := make(map[string]any, len(s))
		for k, v := range s {
			result[k] = v
		}
		result[kw] = nonNull
		return result
	}
	return s
}
