// Package gentschema infers and type-checks JSON Schemas for process definitions.
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
type TaskSchemas struct {
	CallType model.CallType     `json:"call_type"`
	Input    *schema.SchemaNode `json:"input,omitempty"`
	Output   *schema.SchemaNode `json:"output,omitempty"`
}

// SchemaFile is the top-level output.
type SchemaFile struct {
	Process       string                        `json:"process"`
	Version       int                           `json:"version"`
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
func Generate(def *model.ProcessDefinition, version int) (SchemaFile, error) {
	if err := def.Normalize(); err != nil {
		return SchemaFile{}, err
	}
	result := SchemaFile{Process: def.Name, Version: version}

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

func outputContextSets(def *model.ProcessDefinition) (required, optional []string, errRequired, errOptional bool) {
	steps := def.Steps
	n := len(steps)
	if n == 0 {
		return
	}

	reqMap, optMap, mustErrMap, mayErrMap := computeContextSets(steps)

	type endSet struct {
		must   map[string]bool
		may    map[string]bool
		errMin bool // error is always present on this terminal path
		errMax bool // error is sometimes present on this terminal path
	}

	var terminals []endSet

	addTerminal := func(s *model.Step, includeOwnOutput bool, errMin, errMax bool) {
		must := make(map[string]bool)
		for _, id := range reqMap[s.ID] {
			must[id] = true
		}
		if includeOwnOutput && stepHasOutput(s) {
			must[s.ID] = true
		}
		may := make(map[string]bool)
		for id := range must {
			may[id] = true
		}
		for _, id := range optMap[s.ID] {
			may[id] = true
		}
		terminals = append(terminals, endSet{must: must, may: may, errMin: errMin, errMax: errMax})
	}
	for i, s := range steps {
		isNormal := (len(s.Switch) == 0 && i == n-1) ||
			func() bool {
				for _, c := range s.Switch {
					if c.Goto == model.GotoEnd {
						return true
					}
				}
				return false
			}()
		isErrEnd := func() bool {
			for _, ec := range s.OnError {
				if ec.Goto == model.GotoEnd {
					return true
				}
			}
			return false
		}()

		if isNormal {
			addTerminal(s, true, mustErrMap[s.ID], mayErrMap[s.ID])
		}
		if isErrEnd {
			// failing step never produced output; error is always present on this path
			addTerminal(s, false, true, true)
		}
	}

	if len(terminals) == 0 {
		return
	}

	mustAtEnd := make(map[string]bool)
	for id := range terminals[0].must {
		mustAtEnd[id] = true
	}
	for _, t := range terminals[1:] {
		for id := range mustAtEnd {
			if !t.must[id] {
				delete(mustAtEnd, id)
			}
		}
	}

	mayAtEnd := make(map[string]bool)
	for _, t := range terminals {
		for id := range t.may {
			mayAtEnd[id] = true
		}
	}

	for id := range mustAtEnd {
		required = append(required, id)
	}
	for id := range mayAtEnd {
		if !mustAtEnd[id] {
			optional = append(optional, id)
		}
	}

	allErrMin := true
	for _, t := range terminals {
		if !t.errMin {
			allErrMin = false
			break
		}
	}
	anyErrMax := false
	for _, t := range terminals {
		if t.errMax {
			anyErrMax = true
			break
		}
	}
	errRequired = allErrMin
	errOptional = anyErrMax && !allErrMin
	return
}

func stepHasOutput(s *model.Step) bool {
	if s.Call == nil {
		return false
	}
	if s.Call.Type == model.CallTypeChildProcess {
		return true
	}
	return s.Call.OutputSchema != nil
}

func childProcessOutputSchema(s *model.Step) *schema.SchemaNode {
	itemProps := map[string]*schema.SchemaNode{
		"id": {Type: schema.SchemaType{"string"}},
	}
	itemRequired := []string{"id"}
	if s.Call.ChildOutputSchema != nil {
		itemProps["output"] = s.Call.ChildOutputSchema
		itemRequired = append(itemRequired, "output")
	}
	return &schema.SchemaNode{
		Type: schema.SchemaType{"array"},
		Items: &schema.SchemaNode{
			Type:       schema.SchemaType{"object"},
			Properties: itemProps,
			Required:   itemRequired,
		},
	}
}

func collectNamedOutputs(steps []*model.Step, named map[string]*schema.SchemaNode) {
	for _, s := range steps {
		if !stepHasOutput(s) {
			continue
		}
		if s.Call.Type == model.CallTypeChildProcess {
			named[s.ID+"_output"] = childProcessOutputSchema(s)
		} else {
			named[s.ID+"_output"] = s.Call.OutputSchema
		}
	}
}

func collectTaskRefs(steps []*model.Step, out map[string]TaskSchemas) {
	for _, s := range steps {
		if stepHasOutput(s) {
			out[s.ID] = TaskSchemas{CallType: s.Call.Type, Output: schemaRef(s.ID + "_output")}
		}
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

func buildInputs(steps []*model.Step, tasks map[string]TaskSchemas, processInput *schema.SchemaNode, defs map[string]*schema.SchemaNode) error {
	required, optional, mustErr, mayErr := computeContextSets(steps)
	for _, s := range steps {
		if s.Call != nil {
			ctx := contextSchema(required[s.ID], optional[s.ID], tasks, processInput, mustErr[s.ID], mayErr[s.ID])
			if len(defs) > 0 {
				ctx = withDefs(ctx, defs)
			}
			ts, inMap := tasks[s.ID]
			if inMap || len(s.Params) > 0 {
				input, err := inferInput(s, ctx, defs)
				if err != nil {
					return err
				}
				if !inMap {
					ts.CallType = s.Call.Type
				}
				ts.Input = input
				tasks[s.ID] = ts
			}
		}

		if len(s.Switch) > 0 {
			req := required[s.ID]
			opt := optional[s.ID]
			if stepHasOutput(s) {
				req = append(req, s.ID)
				var filtered []string
				for _, id := range opt {
					if id != s.ID {
						filtered = append(filtered, id)
					}
				}
				opt = filtered
			}
			switchCtx := contextSchema(req, opt, tasks, processInput, mustErr[s.ID], mayErr[s.ID])
			if s.Call != nil {
				switchCtx = addSelfSchema(switchCtx, s)
			}
			if len(defs) > 0 {
				switchCtx = withDefs(switchCtx, defs)
			}
			for _, c := range s.Switch {
				if c.When == "default" {
					continue
				}
				inferred, err := template.InferType(c.When, schema.FromNode(switchCtx))
				if err != nil {
					return fmt.Errorf("step %q switch when %q: %w", s.ID, c.When, err)
				}
				if !isType(inferred.Node(), "boolean") {
					return fmt.Errorf("step %q switch when %q: expression must evaluate to boolean, got %q", s.ID, c.When, schemaTypeName(inferred.Node()))
				}
			}
		}
	}
	return nil
}

// predEdge is a predecessor edge in the step graph.
// isErr is true for on_error routes: the failing step has no output on this path.
type predEdge struct {
	idx   int  // predecessor step index; -1 = process start
	isErr bool // true = on_error route
}

// computeContextSets computes, for each step, which prior step outputs are
// always available (required) and which are only sometimes available (optional).
// It also returns mustErr and mayErr maps indicating whether the $error context
// key is always / sometimes present at each step.
func computeContextSets(steps []*model.Step) (required, optional map[string][]string, mustErr, mayErr map[string]bool) {
	n := len(steps)
	required = make(map[string][]string, n)
	optional = make(map[string][]string, n)
	mustErr = make(map[string]bool, n)
	mayErr = make(map[string]bool, n)
	if n == 0 {
		return
	}

	idx := make(map[string]int, n)
	for i, s := range steps {
		idx[s.ID] = i
	}

	preds := make([][]predEdge, n)
	preds[0] = append(preds[0], predEdge{idx: -1})
	for i, s := range steps {
		for _, c := range s.Switch {
			if c.Goto != model.GotoEnd {
				if j, ok := idx[c.Goto]; ok {
					preds[j] = append(preds[j], predEdge{idx: i})
				}
			}
		}
		if len(s.Switch) == 0 && i+1 < n {
			preds[i+1] = append(preds[i+1], predEdge{idx: i})
		}
		for _, ec := range s.OnError {
			if ec.Goto != "" && ec.Goto != model.GotoEnd {
				if j, ok := idx[ec.Goto]; ok {
					preds[j] = append(preds[j], predEdge{idx: i, isErr: true})
				}
			}
		}
	}

	hasOutput := make([]bool, n)
	for i, s := range steps {
		hasOutput[i] = stepHasOutput(s)
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

	// mustOut[i][j] = step j's output is ALWAYS available when entering step i.
	// Error edges clear the failing step's own output bit.
	mustOut := make([][]bool, n)
	for i := range mustOut {
		mustOut[i] = allTrue()
	}
	for {
		changed := false
		for i := range steps {
			in := allTrue()
			for _, p := range preds[i] {
				if p.idx == -1 {
					in = allFalse()
					break
				}
				src := mustOut[p.idx]
				if p.isErr && hasOutput[p.idx] {
					src = append([]bool{}, mustOut[p.idx]...)
					src[p.idx] = false // failing step produced no output
				}
				for j := range in {
					in[j] = in[j] && src[j]
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

	// mayOut[i][j] = step j's output is POSSIBLY available when entering step i.
	mayOut := make([][]bool, n)
	for i := range mayOut {
		mayOut[i] = allFalse()
	}
	for {
		changed := false
		for i := range steps {
			in := allFalse()
			for _, p := range preds[i] {
				if p.idx == -1 {
					continue
				}
				src := mayOut[p.idx]
				if p.isErr && hasOutput[p.idx] {
					src = append([]bool{}, mayOut[p.idx]...)
					src[p.idx] = false
				}
				for j := range in {
					in[j] = in[j] || src[j]
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

	// mustErrArr[i] = $error is ALWAYS present when entering step i (all paths are error paths).
	mustErrArr := make([]bool, n)
	for {
		changed := false
		for i := range steps {
			if len(preds[i]) == 0 {
				continue
			}
			val := true
			for _, p := range preds[i] {
				if p.idx == -1 {
					val = false
					break
				}
				if p.isErr {
					// error edge always contributes error
				} else {
					val = val && mustErrArr[p.idx]
				}
			}
			if mustErrArr[i] != val {
				mustErrArr[i] = val
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// mayErrArr[i] = $error is POSSIBLY present when entering step i.
	mayErrArr := make([]bool, n)
	for {
		changed := false
		for i := range steps {
			val := false
			for _, p := range preds[i] {
				if p.idx != -1 && (p.isErr || mayErrArr[p.idx]) {
					val = true
					break
				}
			}
			if mayErrArr[i] != val {
				mayErrArr[i] = val
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
			if p.idx == -1 {
				mustIn = allFalse()
				break
			}
			src := mustOut[p.idx]
			if p.isErr && hasOutput[p.idx] {
				src = append([]bool{}, mustOut[p.idx]...)
				src[p.idx] = false
			}
			for j := range mustIn {
				mustIn[j] = mustIn[j] && src[j]
			}
		}
		if len(preds[i]) == 0 {
			mustIn = allFalse()
		}

		mayIn := allFalse()
		for _, p := range preds[i] {
			if p.idx == -1 {
				continue
			}
			src := mayOut[p.idx]
			if p.isErr && hasOutput[p.idx] {
				src = append([]bool{}, mayOut[p.idx]...)
				src[p.idx] = false
			}
			for j := range mayIn {
				mayIn[j] = mayIn[j] || src[j]
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

		mustErr[s.ID] = mustErrArr[i]
		mayErr[s.ID] = mayErrArr[i]
	}
	return
}

func addSelfSchema(ctx *schema.SchemaNode, s *model.Step) *schema.SchemaNode {
	var selfSchema *schema.SchemaNode
	if s.Call != nil {
		selfSchema = s.Call.OutputSchema
	}
	if selfSchema == nil {
		selfSchema = &schema.SchemaNode{Type: schema.SchemaType{"object"}}
	}
	// Shallow-copy ctx and its Properties map to avoid mutating shared nodes.
	n := *ctx
	newProps := make(map[string]*schema.SchemaNode, len(ctx.Properties)+1)
	for k, v := range ctx.Properties {
		newProps[k] = v
	}
	newProps["self"] = selfSchema
	n.Properties = newProps
	n.Required = append(append([]string{}, ctx.Required...), "self")
	return &n
}

func inferObjectSchema(exprs map[string]string, ctx *schema.SchemaNode, errFmt func(string) string) (*schema.SchemaNode, error) {
	props := make(map[string]*schema.SchemaNode, len(exprs))
	required := make([]string, 0, len(exprs))
	for name, expr := range exprs {
		inferred, err := template.InferType(expr, schema.FromNode(ctx))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", errFmt(name), err)
		}
		props[name] = inferred.Node()
		required = append(required, name)
	}
	return &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: props,
		Required:   required,
	}, nil
}

func inferInput(s *model.Step, ctx *schema.SchemaNode, defs map[string]*schema.SchemaNode) (*schema.SchemaNode, error) {
	if len(s.Params) == 0 {
		return &schema.SchemaNode{Type: schema.SchemaType{"object"}}, nil
	}
	if len(defs) > 0 {
		ctx = withDefs(ctx, defs)
	}
	return inferObjectSchema(s.Params, ctx, func(name string) string {
		return fmt.Sprintf("task %q param %q", s.ID, name)
	})
}

func contextSchema(preceding []string, optional []string, tasks map[string]TaskSchemas, processInput *schema.SchemaNode, errRequired, errOptional bool) *schema.SchemaNode {
	props := make(map[string]*schema.SchemaNode)
	required := []string{"outputs"}
	if processInput != nil {
		props["input"] = processInput
		required = append(required, "input")
	}
	outputProps := make(map[string]*schema.SchemaNode)
	outputRequired := make([]string, 0)
	for _, id := range preceding {
		if ts, ok := tasks[id]; ok && ts.Output != nil {
			outputProps[id] = ts.Output
			outputRequired = append(outputRequired, id)
		}
	}
	for _, id := range optional {
		if _, already := outputProps[id]; already {
			continue
		}
		if ts, ok := tasks[id]; ok && ts.Output != nil {
			outputProps[id] = ts.Output
		}
	}
	outputs := &schema.SchemaNode{Type: schema.SchemaType{"object"}}
	if len(outputProps) > 0 {
		outputs.Properties = outputProps
		outputs.Required = outputRequired
	}
	props["outputs"] = outputs
	if errRequired || errOptional {
		errSchema := &schema.SchemaNode{
			Type: schema.SchemaType{"object"},
			Properties: map[string]*schema.SchemaNode{
				"step":    {Type: schema.SchemaType{"string"}},
				"message": {Type: schema.SchemaType{"string"}},
				"code":    {Type: schema.SchemaType{"string"}},
			},
			Required: []string{"step", "message", "code"},
		}
		if errOptional {
			props["error"] = schema.WithNull(errSchema)
		} else {
			props["error"] = errSchema
			required = append(required, "error")
		}
	}
	return &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: props,
		Required:   required,
	}
}

// withDefs returns a shallow copy of ctx with Defs set.
func withDefs(ctx *schema.SchemaNode, defs map[string]*schema.SchemaNode) *schema.SchemaNode {
	if len(defs) == 0 || ctx == nil {
		return ctx
	}
	n := *ctx
	n.Defs = defs
	return &n
}

func isType(s *schema.SchemaNode, typ string) bool {
	if s == nil {
		return false
	}
	if len(s.Type) > 0 {
		for _, t := range s.Type {
			if t != typ {
				return false
			}
		}
		return len(s.Type) > 0
	}
	for _, variants := range [][]*schema.SchemaNode{s.OneOf, s.AnyOf} {
		if variants == nil {
			continue
		}
		for _, v := range variants {
			if v == nil || !isType(v, typ) {
				return false
			}
		}
		return len(variants) > 0
	}
	return false
}

func schemaTypeName(s *schema.SchemaNode) string {
	if s == nil {
		return "unknown"
	}
	if len(s.Type) > 0 {
		return strings.Join([]string(s.Type), "|")
	}
	for _, variants := range [][]*schema.SchemaNode{s.OneOf, s.AnyOf} {
		if variants == nil {
			continue
		}
		seen := make(map[string]bool, len(variants))
		var parts []string
		for _, v := range variants {
			if v == nil {
				continue
			}
			name := schemaTypeName(v)
			if !seen[name] {
				seen[name] = true
				parts = append(parts, name)
			}
		}
		return strings.Join(parts, "|")
	}
	return "unknown"
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
