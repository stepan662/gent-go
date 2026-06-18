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
// All other expr-lang constructs return ErrUnsupported.
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
	s      *schema.SchemaNode
	defs   map[string]*schema.SchemaNode
	guards map[string]*schema.SchemaNode
}

func (c inferCtx) withGuard(path string, narrowed *schema.SchemaNode) inferCtx {
	guards := make(map[string]*schema.SchemaNode, len(c.guards)+1)
	for k, v := range c.guards {
		guards[k] = v
	}
	guards[path] = narrowed
	return inferCtx{s: c.s, defs: c.defs, guards: guards}
}

// InferType statically determines the JSON Schema type of expression when
// evaluated against s. $refs are resolved against s's $defs.
func InferType(expression string, s schema.Schema) (schema.Schema, error) {
	node := s.Node()
	var defs map[string]*schema.SchemaNode
	if node != nil {
		defs = node.Defs
	}
	tree, err := parser.Parse(expression)
	if err != nil {
		return schema.Schema{}, fmt.Errorf("parse %q: %w", expression, err)
	}
	result, err := inferNode(tree.Node, inferCtx{s: node, defs: defs})
	if err != nil {
		return schema.Schema{}, err
	}
	return schema.FromNode(result), nil
}

func inferNode(node ast.Node, ictx inferCtx) (*schema.SchemaNode, error) {
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
		return nil, fmt.Errorf("nil is not supported; use null")
	case *ast.IdentifierNode:
		if n.Value == "null" {
			return typeSchema("null"), nil
		}
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

func inferMember(n *ast.MemberNode, ictx inferCtx) (*schema.SchemaNode, error) {
	if path := nodePath(n); path != "" {
		if s, ok := ictx.guards[path]; ok {
			return s, nil
		}
	}
	base, err := inferNode(n.Node, ictx)
	if err != nil {
		return nil, err
	}
	// Member access on a known-null base is null, matching runtime optional
	// chaining (eval returns nil for a nil base). This is also what lets the
	// recursive-inference seed work: the self-reference's previous value is null
	// on the first iteration, and `self.previous.x` must resolve to null so a
	// `?? default` base case can fire rather than erroring on a missing property.
	if schema.IsNullType(base) {
		return typeSchema("null"), nil
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

func inferBinary(n *ast.BinaryNode, ictx inferCtx) (*schema.SchemaNode, error) {
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

func inferUnary(n *ast.UnaryNode, ictx inferCtx) (*schema.SchemaNode, error) {
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

func inferConditional(n *ast.ConditionalNode, ictx inferCtx) (*schema.SchemaNode, error) {
	if _, err := inferNode(n.Cond, ictx); err != nil {
		return nil, err
	}
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
	return &schema.SchemaNode{OneOf: []*schema.SchemaNode{t, f}}, nil
}

// narrowCondition returns then/else contexts narrowed by an equality condition.
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

	litIsNull := isNullLiteral(litNode)

	if bin.Operator == "==" {
		thenCtx = ictx.withGuard(path, litSchema)
		if litIsNull {
			if subjectSchema, err := inferNode(subject, ictx); err == nil {
				elseCtx = ictx.withGuard(path, schema.StripNull(subjectSchema))
			}
		}
	} else {
		elseCtx = ictx.withGuard(path, litSchema)
		if litIsNull {
			if subjectSchema, err := inferNode(subject, ictx); err == nil {
				thenCtx = ictx.withGuard(path, schema.StripNull(subjectSchema))
			}
		}
	}
	return
}

func isLiteralNode(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.BoolNode, *ast.StringNode, *ast.IntegerNode, *ast.FloatNode:
		return true
	case *ast.IdentifierNode:
		return n.Value == "null"
	}
	return false
}

func isNullLiteral(n ast.Node) bool {
	id, ok := n.(*ast.IdentifierNode)
	return ok && id.Value == "null"
}

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
