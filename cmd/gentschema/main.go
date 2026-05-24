// gentschema reads a process definition JSON file and writes a single JSON file
// containing normalised JSON Schemas for the process input and every task output.
//
// Usage:
//
//	gentschema -i definition.json [-o out.json]
//
// If -i is omitted, the definition is read from stdin.
// If -o is omitted, the result is written to stdout.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"gent/internal/exprtype"
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

func main() {
	in := flag.String("i", "", `input definition file (omit or "-" to read from stdin)`)
	out := flag.String("o", "-", `output file path (omit or "-" for stdout)`)
	flag.Parse()

	var src io.Reader = os.Stdin
	if *in != "" && *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			fatal("open %s: %v", *in, err)
		}
		defer f.Close()
		src = f
	}

	var def model.ProcessDefinition
	if err := json.NewDecoder(src).Decode(&def); err != nil {
		fatal("decode definition: %v", err)
	}

	result, err := Generate(&def)
	if err != nil {
		fatal("generate schemas: %v", err)
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal("marshal output: %v", err)
	}
	data = append(data, '\n')

	if *out == "-" {
		os.Stdout.Write(data)
		return
	}

	if err := os.WriteFile(*out, data, 0644); err != nil {
		fatal("write %s: %v", *out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *out, len(data))
}

// Generate normalises all schemas in def and builds the SchemaFile output.
func Generate(def *model.ProcessDefinition) (SchemaFile, error) {
	if err := def.Normalize(); err != nil {
		return SchemaFile{}, err
	}
	result := SchemaFile{Process: def.Name, Version: def.Version}

	// Collect named schemas: "input" for process input, "<id>_output" per action step.
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

// collectNamedOutputs walks steps and adds each action step's OutputSchema
// to named under the key "<id>_output".
func collectNamedOutputs(steps []*model.Step, named map[string]map[string]any) {
	for _, s := range steps {
		if len(s.OutputSchema) > 0 {
			named[s.ID+"_output"] = s.OutputSchema
		}
	}
}

// collectTaskRefs walks steps and populates out with a $ref output
// for every step that has an OutputSchema.
func collectTaskRefs(steps []*model.Step, out map[string]TaskSchemas) {
	for _, s := range steps {
		if len(s.OutputSchema) > 0 {
			out[s.ID] = TaskSchemas{Output: schemaRef(s.ID + "_output")}
		}
	}
}

// flattenNamedSchemas builds a container schema with all named schemas as $defs,
// each tagged with $id so that schema.Normalize scopes their internal $refs
// correctly. After normalisation the flat root $defs map is returned.
//
// Using $id means Normalize handles naming conflicts between inner $defs of
// different schemas automatically, exactly as it does for nested sub-resources.
func flattenNamedSchemas(named map[string]map[string]any) (map[string]any, error) {
	defs := make(map[string]any, len(named))
	refs := make([]any, 0, len(named))
	for name, s := range named {
		entry := deepCopy(s)
		entry["$id"] = name
		defs[name] = entry
		refs = append(refs, schemaRef(name))
	}
	// allOf refs ensure every def is reachable from the root so Normalize
	// does not prune them as unused.
	container := map[string]any{"$defs": defs, "allOf": refs}
	normalised, err := schema.Normalize(container)
	if err != nil {
		return nil, err
	}
	rootDefs, _ := normalised["$defs"].(map[string]any)
	return rootDefs, nil
}

// buildInputs infers input schemas for action steps and type-checks switch
// expressions. It uses CFG-based data-flow analysis (computeContextSets) to
// determine which preceding step outputs are guaranteed (required) or possible
// (optional) before each step runs.
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
			// At switch evaluation time the action for s just ran, so s's own output
			// is now guaranteed. Promote it from optional to required.
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
					continue // always matches; no expression to type-check
				}
				if _, err := exprtype.InferType(c.When, switchCtx); err != nil {
					return nil, fmt.Errorf("step %q switch when %q: %w", s.ID, c.When, err)
				}
			}
		}
	}
	return accumulated, nil
}

// computeContextSets builds a control-flow graph from steps — switch targets
// provide explicit jump edges; non-final steps also have a fall-through edge to
// the next step — then runs two forward data-flow passes:
//
//   - must-analysis (intersection): outputs guaranteed before each step runs
//   - may-analysis  (union):        outputs possibly present before each step
//
// The difference (may − must) gives the optional set. The virtual "start" node
// (index -1) has no outputs, so the first step's required set is always empty.
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

	// Predecessor lists; -1 is the virtual start node (produces no outputs).
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
		// Steps with no switch fall through to the next step in the list.
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

	// must-analysis: intersection lattice, top = all-true.
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
					break // ∩ {} = {} regardless of remaining preds
				}
				for j := range in {
					in[j] = in[j] && mustOut[p][j]
				}
			}
			if len(preds[i]) == 0 {
				in = allFalse() // unreachable step
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

	// may-analysis: union lattice, bottom = all-false.
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

	// Derive mustIn / mayIn from converged mustOut / mayOut and build result maps.
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

// addSelfSchema injects a "self" property into ctx representing this step's own
// action output. Used when type-checking switch expressions.
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

// inferInput returns the input schema for an action step. Each param name maps to
// the inferred type of its expression against ctx. Steps with no params receive an
// empty payload from the engine, so their input schema is an empty object.
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

// contextSchema builds a JSON Schema for the context available to a step:
// the process input (if any, as a $ref) and $ref outputs of all preceding action steps.
// Steps in preceding are guaranteed present at runtime (marked required).
// Steps in optional may or may not have run (added as properties but not required,
// making them nullable via the type inference layer).
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

func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/$defs/" + name}
}

func deepCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gentschema: "+format+"\n", args...)
	os.Exit(1)
}
