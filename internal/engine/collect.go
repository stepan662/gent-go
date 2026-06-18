package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xeipuuv/gojsonschema"

	"gent/internal/model"
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
		return buildSingleChildOutput(siblings)
	}
	return buildParallelChildOutput(siblings)
}

// buildSingleChildOutput returns the single child's output (validated against the
// declared result_schema, if any).
func buildSingleChildOutput(siblings []*model.ProcessInstance) (any, error) {
	if len(siblings) == 0 {
		return nil, nil
	}
	child := siblings[0]
	output := child.ContextData["output"]
	if schemaRaw, _ := child.ContextData["_spawn_result_schema"].(string); schemaRaw != "" {
		var schema map[string]any
		json.Unmarshal([]byte(schemaRaw), &schema) //nolint:errcheck
		if err := validateChildOutput(schema, output); err != nil {
			return nil, fmt.Errorf("child process %q (%s) output validation: %v", child.ID, child.ProcessName, err)
		}
	}
	return output, nil
}

// buildParallelChildOutput returns a map of each sibling's output keyed by its
// child key (validated against the declared result_schema, if any).
func buildParallelChildOutput(siblings []*model.ProcessInstance) (any, error) {
	result := make(map[string]any, len(siblings))
	for _, child := range siblings {
		key, _ := child.ContextData["_spawn_child_key"].(string)
		output := child.ContextData["output"]
		if schemaRaw, _ := child.ContextData["_spawn_result_schema"].(string); schemaRaw != "" {
			var schema map[string]any
			json.Unmarshal([]byte(schemaRaw), &schema) //nolint:errcheck
			if err := validateChildOutput(schema, output); err != nil {
				return nil, fmt.Errorf("child process %q (%s) output validation: %v", child.ID, child.ProcessName, err)
			}
		}
		result[key] = output
	}
	return result, nil
}

func validateChildOutput(schema map[string]any, output any) error {
	result, err := gojsonschema.Validate(
		gojsonschema.NewGoLoader(schema),
		gojsonschema.NewGoLoader(output),
	)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	if !result.Valid() {
		msgs := make([]string, len(result.Errors()))
		for i, e := range result.Errors() {
			msgs[i] = e.String()
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}
