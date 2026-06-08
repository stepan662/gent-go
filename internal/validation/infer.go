package validation

import (
	"fmt"
	"strings"

	"gent/internal/model"
	"gent/internal/schema"
	"gent/internal/template"
)

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
