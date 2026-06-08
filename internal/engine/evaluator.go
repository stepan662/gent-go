package engine

import (
	"fmt"

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
	}
	if errCtx, ok := contextData["$error"]; ok {
		env["error"] = errCtx
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

func evalBool(expression string, contextData map[string]any, self any) (bool, error) {
	result, err := tmpl.EvalAny(expression, evalEnv(contextData, self))
	if err != nil {
		return false, fmt.Errorf("switch %q: %w", expression, err)
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("switch %q: expected bool, got %T", expression, result)
	}
	return b, nil
}
