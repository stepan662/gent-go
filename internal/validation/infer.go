package validation

import (
	"fmt"
	"strings"

	"gent/internal/expression"
	"gent/internal/model"
	"gent/internal/schema"
	"gent/internal/template"
)

func buildInputs(steps []*model.Step, tasks map[string]TaskSchemas, processInput *schema.SchemaNode, defs map[string]*schema.SchemaNode) error {
	if err := checkReachability(steps); err != nil {
		return err
	}
	required, optional, mustErr, mayErr := computeContextSets(steps)

	// Phase 1: infer every output-map step's exported type, in dependency order
	// (mutually-recursive steps resolved jointly), writing each to defs so the
	// switches and later steps below see the final types.
	if err := inferOutputs(steps, tasks, processInput, defs, required, optional, mustErr, mayErr); err != nil {
		return err
	}

	// Phase 2: action inputs (params) and switch type-checks.
	for _, s := range steps {
		if s.Action != nil {
			ts, inMap := tasks[s.ID]
			if inMap || len(s.Params) > 0 {
				ctx := contextSchema(required[s.ID], optional[s.ID], tasks, processInput, mustErr[s.ID], mayErr[s.ID])
				if len(defs) > 0 {
					ctx = withDefs(ctx, defs)
				}
				input, err := inferInput(s, ctx, defs)
				if err != nil {
					return err
				}
				if !inMap {
					ts.ActionType = s.Action.Type
				}
				ts.Input = input
				tasks[s.ID] = ts
			}
		}

		if len(s.Switch) > 0 {
			switchCtx := contextSchema(required[s.ID], optional[s.ID], tasks, processInput, mustErr[s.ID], mayErr[s.ID])
			if s.Action != nil || len(s.Output) > 0 {
				switchCtx = addSelfSchema(switchCtx, s)
			}
			if len(defs) > 0 {
				switchCtx = withDefs(switchCtx, defs)
			}
			for _, c := range s.Switch {
				if c.Case == "" {
					continue
				}
				inferred, err := expression.InferType(c.Case, schema.FromNode(switchCtx))
				if err != nil {
					return fmt.Errorf("step %q switch case %q: %w", s.ID, c.Case, err)
				}
				if !isType(inferred.Node(), "boolean") {
					return fmt.Errorf("step %q switch case %q: expression must evaluate to boolean, got %q", s.ID, c.Case, schemaTypeName(inferred.Node()))
				}
			}
		}
	}
	return nil
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
		if errRequired {
			props["error"] = errSchema
			required = append(required, "error")
		} else {
			props["error"] = schema.WithNull(errSchema)
		}
	}
	return &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: props,
		Required:   required,
	}
}

// addSelfSchema gives the switch context self.output — the step's final output
// (the remapped output map result, the action's output_schema, or a permissive
// object for an untyped raw result). The switch never sees the intermediate
// self.result. self.output resolves through $defs[<id>_output], which holds the
// inferred output-map type (filled earlier in the walk) or the declared schema.
func addSelfSchema(ctx *schema.SchemaNode, s *model.Step) *schema.SchemaNode {
	var outputType *schema.SchemaNode
	if stepHasOutput(s) {
		outputType = schemaRef(s.ID + "_output")
	} else {
		outputType = &schema.SchemaNode{Type: schema.SchemaType{"object"}}
	}
	self := &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: map[string]*schema.SchemaNode{"output": outputType},
		Required:   []string{"output"},
	}
	n := *ctx
	newProps := make(map[string]*schema.SchemaNode, len(ctx.Properties)+1)
	for k, v := range ctx.Properties {
		newProps[k] = v
	}
	newProps["self"] = self
	n.Properties = newProps
	n.Required = append(append([]string{}, ctx.Required...), "self")
	return &n
}

// actionResultType is the type of a step's raw action result — self.result inside
// an output map (typed by output_schema when present, else permissive; null for
// delay or a no-action step).
func actionResultType(s *model.Step) *schema.SchemaNode {
	if s.Action == nil {
		return &schema.SchemaNode{Type: schema.SchemaType{"null"}}
	}
	switch s.Action.Type {
	case model.ActionTypeChildParallel:
		return childParallelOutputSchema(s)
	case model.ActionTypeDelay:
		return &schema.SchemaNode{Type: schema.SchemaType{"null"}}
	default:
		if s.Action.OutputSchema != nil {
			return s.Action.OutputSchema
		}
		return &schema.SchemaNode{Type: schema.SchemaType{"object"}}
	}
}

// outputMapContext builds the context for inferring a step's output map: the base
// context plus self.result (the raw action result), self.previous, and
// outputs.<id> — the latter two both $refs to $defs[<id>_output], the recursive
// placeholder the fixpoint drives.
func outputMapContext(base *schema.SchemaNode, resultType *schema.SchemaNode, stepID string) *schema.SchemaNode {
	selfRef := schemaRef(stepID + "_output")

	outProps := map[string]*schema.SchemaNode{}
	var outReq []string
	if outs := base.Properties["outputs"]; outs != nil {
		for k, v := range outs.Properties {
			outProps[k] = v
		}
		outReq = outs.Required
	}
	outProps[stepID] = selfRef // the prior iteration's value (nullable), so not required

	newProps := make(map[string]*schema.SchemaNode, len(base.Properties)+1)
	for k, v := range base.Properties {
		newProps[k] = v
	}
	newProps["outputs"] = &schema.SchemaNode{Type: schema.SchemaType{"object"}, Properties: outProps, Required: outReq}
	newProps["self"] = &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: map[string]*schema.SchemaNode{"result": resultType, "previous": selfRef},
		Required:   []string{"result"},
	}

	n := *base
	n.Properties = newProps
	n.Required = append(append([]string{}, base.Required...), "self")
	return &n
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
