package engine

import (
	"context"
	"fmt"

	"genroc/internal/expression"
	"genroc/internal/model"
	tmpl "genroc/internal/template"
)

// resolveValue returns v as-is unless it is an *model.ObjectRef marker (an
// externalized, not-yet-loaded value), in which case it loads the object from the
// store and memoises it on the instance for the rest of the advance. inst must be the
// instance that OWNS the value (e.g. a child instance for its own output).
func (e *Engine) resolveValue(inst *model.ProcessInstance, v any) (any, error) {
	ref, ok := v.(*model.ObjectRef)
	if !ok {
		return v, nil
	}
	if cached, ok := inst.ResolvedObjects[ref.Ref]; ok {
		return cached, nil
	}
	val, err := e.db.ResolveObject(context.Background(), inst.ID, ref)
	if err != nil {
		return nil, err
	}
	if inst.ResolvedObjects == nil {
		inst.ResolvedObjects = map[string]any{}
	}
	inst.ResolvedObjects[ref.Ref] = val
	return val, nil
}

// buildEnv assembles the expression environment for inst, resolving only the
// externalized value-slots the expression actually reads (per roots). A small inline
// value is always included (cheap, and robust to any ref under-detection); a big
// externalized value (an *model.ObjectRef marker) is loaded only when referenced —
// this is the slot-level lazy load.
func (e *Engine) buildEnv(inst *model.ProcessInstance, self any, roots expression.Roots) (map[string]any, error) {
	config := inst.Config
	if config == nil {
		config = map[string]any{}
	}
	env := map[string]any{"self": self, "config": config}

	// self.previous is this task's own prior output — the same value as outputs[<this
	// task>], so when that output was externalized it reloads as an *ObjectRef marker.
	// Resolve it just like an outputs.<id> ref (lazily — only when the expression reads
	// it), otherwise self.previous.<field> would read through the marker and yield null.
	if roots.SelfPrevious {
		if sm, ok := self.(map[string]any); ok {
			prev, err := e.resolveValue(inst, sm["previous"])
			if err != nil {
				return nil, err
			}
			selfCopy := make(map[string]any, len(sm))
			for k, v := range sm {
				selfCopy[k] = v
			}
			selfCopy["previous"] = prev
			env["self"] = selfCopy
		}
	}

	include := func(key string, referenced bool) error {
		v := inst.ContextData[key]
		if _, isRef := v.(*model.ObjectRef); isRef && !referenced {
			env[key] = nil
			return nil
		}
		rv, err := e.resolveValue(inst, v)
		if err != nil {
			return err
		}
		env[key] = rv
		return nil
	}
	if err := include("input", roots.Input); err != nil {
		return nil, err
	}
	if err := include("error", roots.Error); err != nil {
		return nil, err
	}

	outs, _ := inst.ContextData["outputs"].(map[string]any)
	refSet := make(map[string]struct{}, len(roots.Outputs))
	for _, id := range roots.Outputs {
		refSet[id] = struct{}{}
	}
	envOuts := make(map[string]any, len(outs))
	for k, v := range outs {
		if _, isRef := v.(*model.ObjectRef); isRef && !roots.AllOutputs {
			if _, referenced := refSet[k]; !referenced {
				continue // unreferenced big output: don't load it
			}
		}
		rv, err := e.resolveValue(inst, v)
		if err != nil {
			return nil, err
		}
		envOuts[k] = rv
	}
	env["outputs"] = envOuts
	return env, nil
}

// shapeRoots unions the root references of every template-string leaf in a shape.
func shapeRoots(node any) (expression.Roots, error) {
	var r expression.Roots
	var walk func(n any) error
	walk = func(n any) error {
		switch v := n.(type) {
		case string:
			tr, err := tmpl.RootRefs(v)
			if err != nil {
				return err
			}
			r.Input = r.Input || tr.Input
			r.Error = r.Error || tr.Error
			r.AllOutputs = r.AllOutputs || tr.AllOutputs
			r.Outputs = append(r.Outputs, tr.Outputs...)
			r.SelfPrevious = r.SelfPrevious || tr.SelfPrevious
		case map[string]any:
			for _, vv := range v {
				if err := walk(vv); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return r, walk(node)
}

// evalShapeCtx evaluates a shape against inst's context, resolving only the slots the
// shape references.
func (e *Engine) evalShapeCtx(inst *model.ProcessInstance, node any, self any) (any, error) {
	roots, err := shapeRoots(node)
	if err != nil {
		return nil, err
	}
	env, err := e.buildEnv(inst, self, roots)
	if err != nil {
		return nil, err
	}
	return evalShape(node, env)
}

// evalAnyCtx evaluates a single template expression against inst's context.
func (e *Engine) evalAnyCtx(inst *model.ProcessInstance, expr string) (any, error) {
	roots, err := tmpl.RootRefs(expr)
	if err != nil {
		return nil, fmt.Errorf("param %q: %w", expr, err)
	}
	env, err := e.buildEnv(inst, nil, roots)
	if err != nil {
		return nil, err
	}
	result, err := tmpl.EvalAny(expr, env)
	if err != nil {
		return nil, fmt.Errorf("param %q: %w", expr, err)
	}
	return result, nil
}

// evalBoolCtx evaluates a boolean switch expression against inst's context.
func (e *Engine) evalBoolCtx(inst *model.ProcessInstance, expr string, self any) (bool, error) {
	roots, err := expression.RootRefs(expr)
	if err != nil {
		return false, fmt.Errorf("switch %q: %w", expr, err)
	}
	env, err := e.buildEnv(inst, self, roots)
	if err != nil {
		return false, err
	}
	result, err := expression.Eval(expr, env)
	if err != nil {
		return false, fmt.Errorf("switch %q: %w", expr, err)
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("switch %q: expected bool, got %T", expr, result)
	}
	return b, nil
}

func evalEnv(contextData, config map[string]any, self any) map[string]any {
	outputs, _ := contextData["outputs"].(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	if config == nil {
		config = map[string]any{}
	}
	env := map[string]any{
		"input":   contextData["input"],
		"outputs": outputs,
		"self":    self,
		"error":   contextData["error"],
		"config":  config,
	}
	return env
}

func evalAny(expression string, contextData, config map[string]any) (any, error) {
	result, err := tmpl.EvalAny(expression, evalEnv(contextData, config, nil))
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

func evalBool(expr string, contextData, config map[string]any, self any) (bool, error) {
	result, err := expression.Eval(expr, evalEnv(contextData, config, self))
	if err != nil {
		return false, fmt.Errorf("switch %q: %w", expr, err)
	}
	b, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("switch %q: expected bool, got %T", expr, result)
	}
	return b, nil
}
