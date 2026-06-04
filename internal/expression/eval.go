package expression

import (
	"fmt"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// Eval evaluates expression against context and returns the result.
// Only the same subset of constructs accepted by InferType is supported;
// any other construct returns ErrUnsupported.
func Eval(expression string, context map[string]any) (any, error) {
	tree, err := parser.Parse(expression)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", expression, err)
	}
	return evalNode(tree.Node, context)
}

func evalNode(node ast.Node, ctx map[string]any) (any, error) {
	switch n := node.(type) {
	case *ast.IntegerNode:
		return n.Value, nil
	case *ast.FloatNode:
		return n.Value, nil
	case *ast.StringNode:
		return n.Value, nil
	case *ast.BoolNode:
		return n.Value, nil
	case *ast.NilNode:
		return nil, nil
	case *ast.IdentifierNode:
		return getField(ctx, n.Value)
	case *ast.MemberNode:
		return evalMember(n, ctx)
	case *ast.BinaryNode:
		return evalBinary(n, ctx)
	case *ast.UnaryNode:
		return evalUnary(n, ctx)
	case *ast.ConditionalNode:
		return evalConditional(n, ctx)
	default:
		return nil, ErrUnsupported{Detail: fmt.Sprintf("node type %T", node)}
	}
}

func evalMember(n *ast.MemberNode, ctx map[string]any) (any, error) {
	base, err := evalNode(n.Node, ctx)
	if err != nil {
		return nil, err
	}
	switch prop := n.Property.(type) {
	case *ast.StringNode:
		if base == nil {
			return nil, nil
		}
		m, ok := base.(map[string]any)
		if !ok {
			return nil, nil // non-object value → null, mirrors type-inference optional-chain semantics
		}
		v, ok := m[prop.Value]
		if !ok {
			return nil, nil // missing field → null
		}
		return v, nil
	case *ast.IntegerNode:
		if base == nil {
			return nil, nil
		}
		slice, ok := base.([]any)
		if !ok {
			return nil, nil // non-array value → null, mirrors property access on non-objects
		}
		if prop.Value < 0 || prop.Value >= len(slice) {
			return nil, nil // out of bounds → null
		}
		return slice[prop.Value], nil
	default:
		return nil, ErrUnsupported{Detail: "computed member access [expr]"}
	}
}

func evalBinary(n *ast.BinaryNode, ctx map[string]any) (any, error) {
	// Short-circuit operators are evaluated before the ops table lookup.
	switch n.Operator {
	case "??":
		left, err := evalNode(n.Left, ctx)
		if err != nil {
			return nil, err
		}
		if left != nil {
			return left, nil
		}
		return evalNode(n.Right, ctx)
	case "&&":
		left, err := evalNode(n.Left, ctx)
		if err != nil {
			return nil, err
		}
		lb, ok := left.(bool)
		if !ok {
			return nil, fmt.Errorf("&& requires boolean operands, got %T", left)
		}
		if !lb {
			return false, nil
		}
		right, err := evalNode(n.Right, ctx)
		if err != nil {
			return nil, err
		}
		rb, ok := right.(bool)
		if !ok {
			return nil, fmt.Errorf("&& requires boolean operands, got %T", right)
		}
		return rb, nil
	case "||":
		left, err := evalNode(n.Left, ctx)
		if err != nil {
			return nil, err
		}
		lb, ok := left.(bool)
		if !ok {
			return nil, fmt.Errorf("|| requires boolean operands, got %T", left)
		}
		if lb {
			return true, nil
		}
		right, err := evalNode(n.Right, ctx)
		if err != nil {
			return nil, err
		}
		rb, ok := right.(bool)
		if !ok {
			return nil, fmt.Errorf("|| requires boolean operands, got %T", right)
		}
		return rb, nil
	}

	op, ok := binaryOps[n.Operator]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("operator %q", n.Operator)}
	}
	left, err := evalNode(n.Left, ctx)
	if err != nil {
		return nil, err
	}
	right, err := evalNode(n.Right, ctx)
	if err != nil {
		return nil, err
	}
	return op.eval(left, right)
}

func evalUnary(n *ast.UnaryNode, ctx map[string]any) (any, error) {
	op, ok := unaryOps[n.Operator]
	if !ok {
		return nil, ErrUnsupported{Detail: fmt.Sprintf("unary operator %q", n.Operator)}
	}
	operand, err := evalNode(n.Node, ctx)
	if err != nil {
		return nil, err
	}
	return op.eval(operand)
}

func evalConditional(n *ast.ConditionalNode, ctx map[string]any) (any, error) {
	cond, err := evalNode(n.Cond, ctx)
	if err != nil {
		return nil, err
	}
	if mustBool(cond) {
		return evalNode(n.Exp1, ctx)
	}
	return evalNode(n.Exp2, ctx)
}

func getField(ctx map[string]any, name string) (any, error) {
	if ctx == nil {
		return nil, fmt.Errorf("field %q not found: context is nil", name)
	}
	v, ok := ctx[name]
	if !ok {
		return nil, fmt.Errorf("field %q not found in context", name)
	}
	return v, nil
}
