package engine

import (
	"fmt"

	"gent/internal/exprtype"
)

// Evaluator compiles and evaluates boolean expressions against a context map.
// Expressions use dot-notation to access context fields, e.g.:
//
//	"outputs.payment.success == true"
//	"input.amount > 1000 && outputs.charge.charged"
//
// Only the subset supported by exprtype is accepted; any other construct
// returns an error so that expressions are always statically type-checkable.
type Evaluator struct{}

func evalEnv(contextData map[string]any) map[string]any {
	outputs, _ := contextData["outputs"].(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	return map[string]any{
		"input":   contextData["input"],
		"outputs": outputs,
	}
}

// EvalAny evaluates an expression and returns the result as any value.
func (Evaluator) EvalAny(expression string, contextData map[string]any) (any, error) {
	result, err := exprtype.Eval(expression, evalEnv(contextData))
	if err != nil {
		return nil, fmt.Errorf("eval %q: %w", expression, err)
	}
	return result, nil
}

// Eval evaluates the expression against contextData and returns the boolean result.
func (Evaluator) Eval(expression string, contextData map[string]any) (bool, error) {
	result, err := exprtype.Eval(expression, evalEnv(contextData))
	if err != nil {
		return false, fmt.Errorf("eval %q: %w", expression, err)
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("expression %q returned %T, expected bool", expression, result)
	}
	return b, nil
}
