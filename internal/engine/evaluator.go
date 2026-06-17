package engine

import (
	"fmt"

	"gent/internal/expression"
	tmpl "gent/internal/template"
)

func evalEnv(contextData map[string]any, self any) map[string]any {
	outputs, _ := contextData["outputs"].(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	env := map[string]any{
		"input":   contextData["input"],
		"outputs": outputs,
		"self":    self,
		"error":   contextData["error"],
	}
	return env
}

func evalAny(expression string, contextData map[string]any) (any, error) {
	result, err := tmpl.EvalAny(expression, evalEnv(contextData, nil))
	if err != nil {
		return nil, fmt.Errorf("param %q: %w", expression, err)
	}
	return result, nil
}

// evalShape recursively evaluates a model.Shape value against env: a string leaf
// is evaluated as a template (preserving type), an object recurses into each
// value. The string|object grammar is enforced at unmarshal (model.checkShape),
// so any other node type is an internal error.
func evalShape(node any, env map[string]any) (any, error) {
	switch n := node.(type) {
	case string:
		return tmpl.EvalAny(n, env)
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			ev, err := evalShape(v, env)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", k, err)
			}
			out[k] = ev
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid shape node %T", node)
	}
}

func evalBool(expr string, contextData map[string]any, self any) (bool, error) {
	result, err := expression.Eval(expr, evalEnv(contextData, self))
	if err != nil {
		return false, fmt.Errorf("switch %q: %w", expr, err)
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("switch %q: expected bool, got %T", expr, result)
	}
	return b, nil
}
