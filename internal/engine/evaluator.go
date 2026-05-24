package engine

import (
	"fmt"

	tmpl "gent/internal/template"
)

// Evaluator compiles and evaluates expressions against a context map.
// Expressions use dot-notation to access context fields, e.g.:
//
//	"outputs.payment.success == true"
//	"input.amount > 1000 && outputs.charge.charged"
//	"self.status == 'approved'"   (in switch expressions, self = this step's output)
//
// Only the subset supported by exprtype is accepted; any other construct
// returns an error so that expressions are always statically type-checkable.
type Evaluator struct{}

func evalEnv(contextData map[string]any, self any) map[string]any {
	outputs, _ := contextData["outputs"].(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	return map[string]any{
		"input":   contextData["input"],
		"outputs": outputs,
		"self":    self,
	}
}

// EvalAny evaluates a template string and returns the result.
// Plain strings are returned as-is; "{{expr}}" returns the expression result
// preserving type; mixed templates are stringified. self is nil (params are
// evaluated before the action runs).
func (Evaluator) EvalAny(expression string, contextData map[string]any) (any, error) {
	result, err := tmpl.EvalAny(expression, evalEnv(contextData, nil))
	if err != nil {
		return nil, fmt.Errorf("param %q: %w", expression, err)
	}
	return result, nil
}

// Eval evaluates the expression against contextData and returns the boolean result.
// self is nil; use EvalBool when the step's own output should be accessible.
func (e Evaluator) Eval(expression string, contextData map[string]any) (bool, error) {
	return e.EvalBool(expression, contextData, nil)
}

// EvalBool evaluates a template switch expression with self available as the
// step's own action output. The template must evaluate to a boolean.
func (Evaluator) EvalBool(expression string, contextData map[string]any, self any) (bool, error) {
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
