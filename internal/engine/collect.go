package engine

import (
	"context"
	"fmt"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// collectChildOutputs is called when a parent instance is in WaitStateCollecting.
// It reads all child instances of the task and returns their merged output as the
// task's action result (self.result) — a single value for a child call, or a
// keyed map for child_parallel. It is exported to outputs.<id> only if the task
// projects it via `output`.
//
// Collecting is only valid when every child of the batch completed — a failed
// or cancelled child makes the parent failing/cancelling, which exits advance()
// before the collect phase. The guard below enforces this rather than silently
// merging nil outputs if that invariant is ever broken.
func (e *Engine) collectChildOutputs(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, error) {
	siblings, err := e.db.ChildrenForTask(ctx, inst.ID, task.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range siblings {
		if c.Status != model.StatusCompleted {
			return nil, fmt.Errorf("child %q is %s; outputs can only be collected when all children completed", c.ID, c.Status)
		}
	}
	if task.Action.Type == model.ActionTypeChild {
		return e.buildSingleChildOutput(siblings)
	}
	return e.buildParallelChildOutput(siblings)
}

// buildSingleChildOutput returns the single child's output (validated against the
// declared result_schema, if any), resolving it from the object store if externalized.
func (e *Engine) buildSingleChildOutput(siblings []*model.ProcessInstance) (any, error) {
	if len(siblings) == 0 {
		return nil, nil
	}
	child := siblings[0]
	output, err := e.resolveValue(child, child.ContextData["output"])
	if err != nil {
		return nil, err
	}
	if schemaRaw, _ := child.ContextData["_spawn_result_schema"].(string); schemaRaw != "" {
		normalized, err := validateChildOutput(schemaRaw, output)
		if err != nil {
			return nil, fmt.Errorf("child process %q (%s) output validation: %v", child.ID, child.ProcessName, err)
		}
		output = normalized
	}
	return output, nil
}

// buildParallelChildOutput returns a map of each sibling's output keyed by its
// child key (validated against the declared result_schema, if any), resolving each
// from the object store if externalized.
func (e *Engine) buildParallelChildOutput(siblings []*model.ProcessInstance) (any, error) {
	result := make(map[string]any, len(siblings))
	for _, child := range siblings {
		key, _ := child.ContextData["_spawn_child_key"].(string)
		output, err := e.resolveValue(child, child.ContextData["output"])
		if err != nil {
			return nil, err
		}
		if schemaRaw, _ := child.ContextData["_spawn_result_schema"].(string); schemaRaw != "" {
			normalized, err := validateChildOutput(schemaRaw, output)
			if err != nil {
				return nil, fmt.Errorf("child process %q (%s) output validation: %v", child.ID, child.ProcessName, err)
			}
			output = normalized
		}
		result[key] = output
	}
	return result, nil
}

// validateChildOutput parses the child's stored (already-normalized) result_schema
// and validates the child output against it, returning the normalized output
// (undeclared keys dropped, defaults filled).
func validateChildOutput(schemaRaw string, output any) (any, error) {
	sc, err := schema.Parse([]byte(schemaRaw))
	if err != nil {
		return nil, fmt.Errorf("schema validation error: %w", err)
	}
	return sc.Validate(output)
}
