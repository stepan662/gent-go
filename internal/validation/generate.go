// Package validation infers and type-checks JSON Schemas for process definitions.
package validation

import (
	"encoding/json"
	"fmt"
	"slices"

	"gent/internal/model"
	"gent/internal/schema"
)

// TaskSchemas holds the schemas associated with a single task task.
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
func buildSchemaContext(def *model.ProcessDefinition) (defs map[string]*schema.SchemaNode, tasks map[string]TaskSchemas, processInput *schema.SchemaNode, configSchema *schema.SchemaNode, err error) {
	named := make(map[string]*schema.SchemaNode)
	if def.InputSchema != nil {
		named["input"] = def.InputSchema
	}
	collectNamedOutputs(def.Tasks, named)
	if len(named) > 0 {
		defs, err = flattenNamedSchemas(named)
		if err != nil {
			return
		}
	}
	tasks = make(map[string]TaskSchemas)
	collectTaskRefs(def.Tasks, tasks)
	if named["input"] != nil {
		processInput = schemaRef("input")
	}
	configSchema = buildConfigSchema(def.ConfigSchema)
	return
}

// buildConfigSchema types the "config" namespace from the definition's
// config_schema so expressions referencing config.<NAME> are type-checked and an
// undeclared config.<NAME> is rejected at registration. A property is marked
// required (non-null) only when it is actually guaranteed present at runtime — it
// is in config_schema.required or has a default; everything else stays optional,
// so accessing it yields a nullable type and the inferrer flags unsafe uses (e.g.
// a possibly-null value interpolated into a URL). Returns nil when no config is
// declared.
func buildConfigSchema(cs *schema.SchemaNode) *schema.SchemaNode {
	if cs == nil || len(cs.Properties) == 0 {
		return nil
	}
	present := make(map[string]bool, len(cs.Properties))
	for _, r := range cs.Required {
		present[r] = true
	}
	for name, prop := range cs.Properties {
		if prop != nil && prop.Default != nil {
			present[name] = true
		}
	}
	required := make([]string, 0, len(present))
	for name := range present {
		required = append(required, name)
	}
	slices.Sort(required)
	return &schema.SchemaNode{Type: schema.SchemaType{"object"}, Properties: cs.Properties, Required: required}
}

// Generate normalises all schemas in def and builds the SchemaFile output.
func Generate(def *model.ProcessDefinition) (SchemaFile, error) {
	if err := def.Normalize(); err != nil {
		return SchemaFile{}, err
	}
	result := SchemaFile{Process: def.Name}

	defs, tasks, processInput, configSchema, err := buildSchemaContext(def)
	if err != nil {
		return SchemaFile{}, err
	}
	result.ProcessInput = processInput

	if err := buildInputs(def.Tasks, tasks, processInput, configSchema, defs); err != nil {
		return SchemaFile{}, err
	}

	if defs == nil {
		defs = make(map[string]*schema.SchemaNode)
	}

	for _, s := range def.Tasks {
		if ts, ok := tasks[s.ID]; ok {
			if ts.Input != nil && ts.Input.Properties != nil {
				name := uniqueDefName(s.ID+"_input", defs)
				defs[name] = ts.Input
				ts.Input = schemaRef(name)
				tasks[s.ID] = ts
			}
		}
	}

	if def.Output.Present() {
		outputSchema, err := inferProcessOutput(def, tasks, result.ProcessInput, configSchema, defs)
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

func inferProcessOutput(def *model.ProcessDefinition, tasks map[string]TaskSchemas, processInput, configSchema *schema.SchemaNode, defs map[string]*schema.SchemaNode) (*schema.SchemaNode, error) {
	req, opt, errReq, errOpt := outputContextSets(def)
	ctx := contextSchema(req, opt, tasks, processInput, configSchema, errReq, errOpt)
	if len(defs) > 0 {
		ctx = withDefs(ctx, defs)
	}
	return inferShape(def.Output.Raw, ctx, "output")
}

func collectNamedOutputs(tasks []*model.Task, named map[string]*schema.SchemaNode) {
	for _, s := range tasks {
		if !s.Output.Present() {
			continue
		}
		// Inferred during the per-task walk (it may be recursive); a permissive
		// placeholder holds the $defs slot until then.
		named[s.ID+"_output"] = &schema.SchemaNode{Type: schema.SchemaType{"object"}}
	}
}

func collectTaskRefs(tasks []*model.Task, out map[string]TaskSchemas) {
	for _, s := range tasks {
		if !s.Output.Present() {
			continue
		}
		var at model.ActionType // empty for a no-action (routing) task
		if s.Action != nil {
			at = s.Action.Type
		}
		out[s.ID] = TaskSchemas{ActionType: at, Output: schemaRef(s.ID + "_output")}
	}
}

func childParallelOutputSchema(s *model.Task) *schema.SchemaNode {
	props := make(map[string]*schema.SchemaNode, len(s.Action.Children))
	var required []string
	for key, entry := range s.Action.Children {
		if entry.ResultSchema != nil {
			props[key] = entry.ResultSchema
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
