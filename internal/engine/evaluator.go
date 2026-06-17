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

// evalWithSelf evaluates a template expression with a self object exposed (used
// by output maps, where self = {result, previous}).
func evalWithSelf(expression string, contextData map[string]any, self any) (any, error) {
	result, err := tmpl.EvalAny(expression, evalEnv(contextData, self))
	if err != nil {
		return nil, fmt.Errorf("%q: %w", expression, err)
	}
	return result, nil
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
