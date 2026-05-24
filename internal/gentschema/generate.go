// Package gentschema infers and type-checks JSON Schemas for process definitions.
// It is used by the gentschema CLI tool and by the API endpoint when registering
// a new definition, so that expression errors are caught at definition-save time.
package gentschema

import (
	"encoding/json"
	"fmt"
	"strings"

	"gent/internal/model"
	"gent/internal/schema"
	"gent/internal/template"
)

// TaskSchemas holds the schemas associated with a single task step.
// Output is a $ref into $defs; Input is inferred from params expressions or
// equals the full context schema when the task has no params.
type TaskSchemas struct {
	Input  map[string]any `json:"input,omitempty"`
	Output map[string]any `json:"output,omitempty"`
}

// SchemaFile is the top-level output. $defs collects every named schema so
// that code generators (e.g. json-schema-to-typescript) can emit one type per
// entry. All other schema fields are $ref pointers into $defs.
type SchemaFile struct {
	Process      string                 `json:"process"`
	Version      int                    `json:"version"`
	ProcessInput map[string]any         `json:"process_input,omitempty"`
	Tasks        map[string]TaskSchemas `json:"tasks,omitempty"`
	Defs         map[string]any         `json:"$defs,omitempty"`
}

// Generate normalises all schemas in def and builds the SchemaFile output.
// It also type-checks all param and switch expressions, so calling Generate
// is sufficient to fully validate a process definition.
func Generate(def *model.ProcessDefinition) (SchemaFile, error) {
	if err := def.Normalize(); err != nil {
		return SchemaFile{}, err
	}
	result := SchemaFile{Process: def.Name, Version: def.Version}

	named := make(map[string]map[string]any)
	if len(def.InputSchema) > 0 {
		named["input"] = def.InputSchema
	}
	collectNamedOutputs(def.Steps, named)

	if len(named) > 0 {
		defs, err := flattenNamedSchemas(named)
		if err != nil {
			return SchemaFile{}, err
		}
		result.Defs = defs
	}

	if named["input"] != nil {
		result.ProcessInput = schemaRef("input")
	}

	tasks := make(map[string]TaskSchemas)
	collectTaskRefs(def.Steps, tasks)
	if _, err := buildInputs(def.Steps, nil, tasks, result.ProcessInput, result.Defs); err != nil {
		return SchemaFile{}, err
	}
	if len(tasks) > 0 {
		result.Tasks = tasks
	}
	return result, nil
}

func collectNamedOutputs(steps []*model.Step, named map[string]map[string]any) {
	for _, s := range steps {
		if len(s.OutputSchema) > 0 {
			named[s.ID+"_output"] = s.OutputSchema
		}
	}
}

func collectTaskRefs(steps []*model.Step, out map[string]TaskSchemas) {
	for _, s := range steps {
		if len(s.OutputSchema) > 0 {
			out[s.ID] = TaskSchemas{Output: schemaRef(s.ID + "_output")}
		}
	}
}

func flattenNamedSchemas(named map[string]map[string]any) (map[string]any, error) {
	defs := make(map[string]any, len(named))
	refs := make([]any, 0, len(named))
	for name, s := range named {
		entry := deepCopy(s)
		entry["$id"] = name
		defs[name] = entry
		refs = append(refs, schemaRef(name))
	}
	container := map[string]any{"$defs": defs, "allOf": refs}
	normalised, err := schema.Normalize(container)
	if err != nil {
		return nil, err
	}
	rootDefs, _ := normalised["$defs"].(map[string]any)
	return rootDefs, nil
}

func buildInputs(steps []*model.Step, _ []string, tasks map[string]TaskSchemas, processInput map[string]any, defs map[string]any) ([]string, error) {
	required, optional := computeContextSets(steps)
	var accumulated []string
	for _, s := range steps {
		if s.Transport != "" {
			ctx := contextSchema(required[s.ID], optional[s.ID], tasks, processInput)
			if len(defs) > 0 {
				ctx["$defs"] = defs
			}
			ts, inMap := tasks[s.ID]
			if inMap || len(s.Params) > 0 {
				input, err := inferInput(s, ctx, defs)
				if err != nil {
					return nil, err
				}
				ts.Input = input
				tasks[s.ID] = ts
			}
			accumulated = append(accumulated, s.ID)
		}

		if len(s.Switch) > 0 {
			req := required[s.ID]
			opt := optional[s.ID]
			if s.Transport != "" && len(s.OutputSchema) > 0 {
				req = append(req, s.ID)
				var filtered []string
				for _, id := range opt {
					if id != s.ID {
						filtered = append(filtered, id)
					}
				}
				opt = filtered
			}
			switchCtx := contextSchema(req, opt, tasks, processInput)
			if s.Transport != "" {
				addSelfSchema(switchCtx, s)
			}
			if len(defs) > 0 {
				switchCtx["$defs"] = defs
			}
			for _, c := range s.Switch {
				if c.When == "default" {
					continue
				}
				inferred, err := template.InferType(c.When, switchCtx)
				if err != nil {
					return nil, fmt.Errorf("step %q switch when %q: %w", s.ID, c.When, err)
				}
				if !isType(inferred, "boolean") {
					return nil, fmt.Errorf("step %q switch when %q: expression must evaluate to boolean, got %q", s.ID, c.When, schemaTypeName(inferred))
				}
			}
		}
	}
	return accumulated, nil
}

func computeContextSets(steps []*model.Step) (required, optional map[string][]string) {
	n := len(steps)
	required = make(map[string][]string, n)
	optional = make(map[string][]string, n)
	if n == 0 {
		return
	}

	idx := make(map[string]int, n)
	for i, s := range steps {
		idx[s.ID] = i
	}

	preds := make([][]int, n)
	preds[0] = append(preds[0], -1)
	for i, s := range steps {
		for _, c := range s.Switch {
			if c.Goto != model.GotoEnd {
				if j, ok := idx[c.Goto]; ok {
					preds[j] = append(preds[j], i)
				}
			}
		}
		if len(s.Switch) == 0 && i+1 < n {
			preds[i+1] = append(preds[i+1], i)
		}
	}

	hasOutput := make([]bool, n)
	for i, s := range steps {
		hasOutput[i] = len(s.OutputSchema) > 0
	}

	allTrue := func() []bool { s := make([]bool, n); for i := range s { s[i] = true }; return s }
	allFalse := func() []bool { return make([]bool, n) }
	eq := func(a, b []bool) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	mustOut := make([][]bool, n)
	for i := range mustOut {
		mustOut[i] = allTrue()
	}
	for {
		changed := false
		for i := range steps {
			in := allTrue()
			for _, p := range preds[i] {
				if p == -1 {
					in = allFalse()
					break
				}
				for j := range in {
					in[j] = in[j] && mustOut[p][j]
				}
			}
			if len(preds[i]) == 0 {
				in = allFalse()
			}
			out := append([]bool{}, in...)
			if hasOutput[i] {
				out[i] = true
			}
			if !eq(mustOut[i], out) {
				mustOut[i] = out
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	mayOut := make([][]bool, n)
	for i := range mayOut {
		mayOut[i] = allFalse()
	}
	for {
		changed := false
		for i := range steps {
			in := allFalse()
			for _, p := range preds[i] {
				if p != -1 {
					for j := range in {
						in[j] = in[j] || mayOut[p][j]
					}
				}
			}
			out := append([]bool{}, in...)
			if hasOutput[i] {
				out[i] = true
			}
			if !eq(mayOut[i], out) {
				mayOut[i] = out
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	for i, s := range steps {
		mustIn := allTrue()
		for _, p := range preds[i] {
			if p == -1 {
				mustIn = allFalse()
				break
			}
			for j := range mustIn {
				mustIn[j] = mustIn[j] && mustOut[p][j]
			}
		}
		if len(preds[i]) == 0 {
			mustIn = allFalse()
		}

		mayIn := allFalse()
		for _, p := range preds[i] {
			if p != -1 {
				for j := range mayIn {
					mayIn[j] = mayIn[j] || mayOut[p][j]
				}
			}
		}

		for j, ss := range steps {
			switch {
			case mustIn[j]:
				required[s.ID] = append(required[s.ID], ss.ID)
			case mayIn[j]:
				optional[s.ID] = append(optional[s.ID], ss.ID)
			}
		}
	}
	return
}

func addSelfSchema(ctx map[string]any, s *model.Step) {
	props, _ := ctx["properties"].(map[string]any)
	selfSchema := map[string]any(s.OutputSchema)
	if len(selfSchema) == 0 {
		selfSchema = map[string]any{"type": "object"}
	}
	props["self"] = selfSchema
	req, _ := ctx["required"].([]any)
	ctx["required"] = append(req, "self")
}

func inferInput(s *model.Step, ctx map[string]any, defs map[string]any) (map[string]any, error) {
	if len(s.Params) == 0 {
		return map[string]any{"type": "object"}, nil
	}
	if len(defs) > 0 {
		ctx["$defs"] = defs
	}
	props := make(map[string]any, len(s.Params))
	required := make([]string, 0, len(s.Params))
	for name, expr := range s.Params {
		inferred, err := template.InferType(expr, ctx)
		if err != nil {
			return nil, fmt.Errorf("task %q param %q: %w", s.ID, name, err)
		}
		props[name] = inferred
		required = append(required, name)
	}
	return map[string]any{"type": "object", "properties": props, "required": required}, nil
}

func contextSchema(preceding []string, optional []string, tasks map[string]TaskSchemas, processInput map[string]any) map[string]any {
	props := make(map[string]any)
	required := []any{"outputs"}
	if len(processInput) > 0 {
		props["input"] = processInput
		required = append(required, "input")
	}
	outputProps := make(map[string]any)
	outputRequired := make([]any, 0)
	for _, id := range preceding {
		if ts, ok := tasks[id]; ok && len(ts.Output) > 0 {
			outputProps[id] = ts.Output
			outputRequired = append(outputRequired, id)
		}
	}
	for _, id := range optional {
		if _, already := outputProps[id]; already {
			continue
		}
		if ts, ok := tasks[id]; ok && len(ts.Output) > 0 {
			outputProps[id] = ts.Output
		}
	}
	outputs := map[string]any{"type": "object"}
	if len(outputProps) > 0 {
		outputs["properties"] = outputProps
		outputs["required"] = outputRequired
	}
	props["outputs"] = outputs
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// isType reports whether schema s guarantees values of the given simple JSON
// type. It recurses into oneOf/anyOf and requires every variant to match.
func isType(s map[string]any, typ string) bool {
	switch t := s["type"].(type) {
	case string:
		return t == typ
	case []any:
		for _, v := range t {
			if v != typ {
				return false
			}
		}
		return len(t) > 0
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		variants, ok := s[kw].([]any)
		if !ok {
			continue
		}
		for _, v := range variants {
			vs, ok := v.(map[string]any)
			if !ok || !isType(vs, typ) {
				return false
			}
		}
		return len(variants) > 0
	}
	return false
}

func schemaTypeName(s map[string]any) string {
	switch t := s["type"].(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, v := range t {
			if s, ok := v.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "|")
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		variants, ok := s[kw].([]any)
		if !ok {
			continue
		}
		seen := make(map[string]bool, len(variants))
		parts := make([]string, 0, len(variants))
		for _, v := range variants {
			vs, ok := v.(map[string]any)
			if !ok {
				continue
			}
			name := schemaTypeName(vs)
			if !seen[name] {
				seen[name] = true
				parts = append(parts, name)
			}
		}
		return strings.Join(parts, "|")
	}
	return "unknown"
}

func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/$defs/" + name}
}

func deepCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}
