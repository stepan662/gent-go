// Package validation infers and type-checks JSON Schemas for process definitions.
package validation

import (
	"encoding/json"
	"fmt"

	"gent/internal/model"
	"gent/internal/schema"
)

// TaskSchemas holds the schemas associated with a single task step.
type TaskSchemas struct {
	ActionType model.ActionType     `json:"action_type"`
	Input    *schema.SchemaNode `json:"input,omitempty"`
	Output   *schema.SchemaNode `json:"output,omitempty"`
}

// SchemaFile is the top-level output.
type SchemaFile struct {
	Process       string                        `json:"process"`
	ProcessInput  *schema.SchemaNode            `json:"process_input,omitempty"`
	ProcessOutput *schema.SchemaNode            `json:"process_output,omitempty"`
	Tasks         map[string]TaskSchemas        `json:"tasks,omitempty"`
	Defs          map[string]*schema.SchemaNode `json:"$defs,omitempty"`
}

// buildSchemaContext derives the shared defs, tasks, and processInput from a definition.
// Both Generate and ValidateChildProcessRefs use it to avoid duplicating setup.
func buildSchemaContext(def *model.ProcessDefinition) (defs map[string]*schema.SchemaNode, tasks map[string]TaskSchemas, processInput *schema.SchemaNode, err error) {
	named := make(map[string]*schema.SchemaNode)
	if def.InputSchema != nil {
		named["input"] = def.InputSchema
	}
	collectNamedOutputs(def.Steps, named)
	if len(named) > 0 {
		defs, err = flattenNamedSchemas(named)
		if err != nil {
			return
		}
	}
	tasks = make(map[string]TaskSchemas)
	collectTaskRefs(def.Steps, tasks)
	if named["input"] != nil {
		processInput = schemaRef("input")
	}
	return
}

// Generate normalises all schemas in def and builds the SchemaFile output.
func Generate(def *model.ProcessDefinition) (SchemaFile, error) {
	if err := def.Normalize(); err != nil {
		return SchemaFile{}, err
	}
	result := SchemaFile{Process: def.Name}

	defs, tasks, processInput, err := buildSchemaContext(def)
	if err != nil {
		return SchemaFile{}, err
	}
	result.ProcessInput = processInput

	if err := buildInputs(def.Steps, tasks, processInput, defs); err != nil {
		return SchemaFile{}, err
	}

	if defs == nil {
		defs = make(map[string]*schema.SchemaNode)
	}

	for _, s := range def.Steps {
		if ts, ok := tasks[s.ID]; ok {
			if ts.Input != nil && ts.Input.Properties != nil {
				name := uniqueDefName(s.ID+"_input", defs)
				defs[name] = ts.Input
				ts.Input = schemaRef(name)
				tasks[s.ID] = ts
			}
		}
	}

	if len(def.Output) > 0 {
		outputSchema, err := inferProcessOutput(def, tasks, result.ProcessInput, defs)
		if err != nil {
			return SchemaFile{}, err
		}
		name := uniqueDefName("output", defs)
		defs[name] = outputSchema
		result.ProcessOutput = schemaRef(name)
	}

	if len(tasks) > 0 {
		result.Tasks = tasks
	}
	if len(defs) > 0 {
		result.Defs = defs
	}
	return result, nil
}

func inferProcessOutput(def *model.ProcessDefinition, tasks map[string]TaskSchemas, processInput *schema.SchemaNode, defs map[string]*schema.SchemaNode) (*schema.SchemaNode, error) {
	req, opt, errReq, errOpt := outputContextSets(def)
	ctx := contextSchema(req, opt, tasks, processInput, errReq, errOpt)
	if len(defs) > 0 {
		ctx = withDefs(ctx, defs)
	}
	return inferObjectSchema(def.Output, ctx, func(name string) string {
		return fmt.Sprintf("output %q", name)
	})
}

func collectNamedOutputs(steps []*model.Step, named map[string]*schema.SchemaNode) {
	for _, s := range steps {
		if !stepHasOutput(s) {
			continue
		}
		if s.Action.Type == model.ActionTypeChildParallel {
			named[s.ID+"_output"] = childParallelOutputSchema(s)
		} else {
			named[s.ID+"_output"] = s.Action.OutputSchema
		}
	}
}

func collectTaskRefs(steps []*model.Step, out map[string]TaskSchemas) {
	for _, s := range steps {
		if stepHasOutput(s) {
			out[s.ID] = TaskSchemas{ActionType: s.Action.Type, Output: schemaRef(s.ID + "_output")}
		}
	}
}

func childParallelOutputSchema(s *model.Step) *schema.SchemaNode {
	props := make(map[string]*schema.SchemaNode, len(s.Action.Children))
	var required []string
	for key, entry := range s.Action.Children {
		if entry.OutputSchema != nil {
			props[key] = entry.OutputSchema
			required = append(required, key)
		} else {
			props[key] = &schema.SchemaNode{Type: schema.SchemaType{"object"}}
		}
	}
	return &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: props,
		Required:   required,
	}
}

func flattenNamedSchemas(named map[string]*schema.SchemaNode) (map[string]*schema.SchemaNode, error) {
	defs := make(map[string]*schema.SchemaNode, len(named))
	refs := make([]*schema.SchemaNode, 0, len(named))
	for name, s := range named {
		entry := deepCopyNode(s)
		entry.ID = name
		defs[name] = entry
		refs = append(refs, schemaRef(name))
	}
	container := &schema.SchemaNode{Defs: defs, AllOf: refs}
	normalised, err := schema.Normalize(container)
	if err != nil {
		return nil, err
	}
	return normalised.Defs, nil
}

func uniqueDefName(base string, defs map[string]*schema.SchemaNode) string {
	name := base
	for i := 1; defs[name] != nil; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	return name
}

func schemaRef(name string) *schema.SchemaNode {
	return &schema.SchemaNode{Ref: "#/$defs/" + name}
}

func deepCopyNode(n *schema.SchemaNode) *schema.SchemaNode {
	if n == nil {
		return nil
	}
	b, _ := json.Marshal(n)
	// Use alias to bypass strict UnmarshalJSON on a round-trip of already-valid data.
	type alias schema.SchemaNode
	var a alias
	json.Unmarshal(b, &a) //nolint:errcheck
	result := schema.SchemaNode(a)
	return &result
}
