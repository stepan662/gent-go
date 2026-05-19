package engine

import (
	"fmt"

	"github.com/expr-lang/expr"
)

// Evaluator compiles and evaluates boolean expressions against a context map.
// Expressions use dot-notation to access context fields, e.g.:
//
//	"context.payment_success == true"
//	"context.amount > 1000 && context.user_verified"
type Evaluator struct{}

func evalEnv(contextData map[string]interface{}) map[string]interface{} {
	env := map[string]interface{}{
		"input":   contextData["input"],
		"outputs": contextData["outputs"],
	}
	if env["outputs"] == nil {
		env["outputs"] = map[string]interface{}{}
	}
	return env
}

// EvalAny evaluates an expression and returns the result as any value.
func (Evaluator) EvalAny(expression string, contextData map[string]interface{}) (any, error) {
	env := evalEnv(contextData)
	program, err := expr.Compile(expression, expr.Env(env))
	if err != nil {
		return nil, fmt.Errorf("compile expression %q: %w", expression, err)
	}
	result, err := expr.Run(program, env)
	if err != nil {
		return nil, fmt.Errorf("eval expression %q: %w", expression, err)
	}
	return result, nil
}

// Eval evaluates the expression string against contextData and returns the boolean result.
func (Evaluator) Eval(expression string, contextData map[string]interface{}) (bool, error) {
	env := evalEnv(contextData)

	program, err := expr.Compile(expression, expr.Env(env), expr.AsBool())
	if err != nil {
		return false, fmt.Errorf("compile expression %q: %w", expression, err)
	}

	result, err := expr.Run(program, env)
	if err != nil {
		return false, fmt.Errorf("eval expression %q: %w", expression, err)
	}

	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("expression %q did not return bool", expression)
	}
	return b, nil
}
