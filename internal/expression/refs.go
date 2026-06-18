package expression

import (
	"sort"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// OutputRefs returns the distinct task ids referenced via outputs.<id> in expr
// (e.g. "outputs.charge.ok + outputs.ship.n" → ["charge", "ship"]). Used to build
// the output-dependency graph for ordering and recursion detection.
func OutputRefs(expression string) ([]string, error) {
	tree, err := parser.Parse(expression)
	if err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	collectOutputRefs(tree.Node, set)
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func collectOutputRefs(node ast.Node, set map[string]struct{}) {
	switch n := node.(type) {
	case *ast.MemberNode:
		if id := outputRefID(n); id != "" {
			set[id] = struct{}{}
		}
		collectOutputRefs(n.Node, set)
	case *ast.BinaryNode:
		collectOutputRefs(n.Left, set)
		collectOutputRefs(n.Right, set)
	case *ast.UnaryNode:
		collectOutputRefs(n.Node, set)
	case *ast.ConditionalNode:
		collectOutputRefs(n.Cond, set)
		collectOutputRefs(n.Exp1, set)
		collectOutputRefs(n.Exp2, set)
	}
}

// outputRefID returns <id> when n is exactly outputs.<id> (base identifier
// "outputs", string property), else "".
func outputRefID(n *ast.MemberNode) string {
	base, ok := n.Node.(*ast.IdentifierNode)
	if !ok || base.Value != "outputs" {
		return ""
	}
	prop, ok := n.Property.(*ast.StringNode)
	if !ok {
		return ""
	}
	return prop.Value
}
