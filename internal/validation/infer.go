package validation

import (
	"fmt"
	"slices"
	"strings"

	"genroc/internal/expression"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/template"
)

func buildInputs(tasks []*model.Task, taskSchemas map[string]TaskSchemas, processInput, configSchema *schema.SchemaNode, defs map[string]*schema.SchemaNode) error {
	if err := checkReachability(tasks); err != nil {
		return err
	}
	required, optional, mustErr, mayErr := computeContextSets(tasks)

	// Phase 1: infer every output-map task's exported type, in dependency order
	// (mutually-recursive tasks resolved jointly), writing each to defs so the
	// switches and later tasks below see the final types.
	if err := inferOutputs(tasks, taskSchemas, processInput, configSchema, defs, required, optional, mustErr, mayErr); err != nil {
		return err
	}

	// Phase 2: action inputs and switch type-checks.
	for _, s := range tasks {
		if s.Action != nil {
			ts, inMap := taskSchemas[s.ID]
			isREST := s.Action.Type == model.ActionTypeREST
			hasEndpoint := isREST && s.Action.Endpoint != ""
			hasHeaders := isREST && len(s.Action.Headers) > 0
			if inMap || s.Action.Input.Present() || hasEndpoint || hasHeaders {
				ctx := contextSchema(required[s.ID], optional[s.ID], taskSchemas, processInput, configSchema, mustErr[s.ID], mayErr[s.ID])
				if len(defs) > 0 {
					ctx = withDefs(ctx, defs)
				}
				// The rest endpoint and header values are templates evaluated against the
				// context; type-check them and reject a possibly-null result (a null URL or
				// header value would silently stringify to "null").
				if hasEndpoint {
					if err := checkNonNullTemplate(s.Action.Endpoint, ctx, fmt.Sprintf("task %q endpoint", s.ID)); err != nil {
						return err
					}
				}
				if hasHeaders {
					names := make([]string, 0, len(s.Action.Headers))
					for h := range s.Action.Headers {
						names = append(names, h)
					}
					slices.Sort(names)
					for _, h := range names {
						if err := checkNonNullTemplate(s.Action.Headers[h], ctx, fmt.Sprintf("task %q header %q", s.ID, h)); err != nil {
							return err
						}
					}
				}
				if inMap || s.Action.Input.Present() {
					input, err := inferInput(s, ctx, defs)
					if err != nil {
						return err
					}
					if !inMap {
						ts.ActionType = s.Action.Type
					}
					ts.Input = input
					taskSchemas[s.ID] = ts
				}
			}
		}

		if len(s.Switch) > 0 {
			switchCtx := contextSchema(required[s.ID], optional[s.ID], taskSchemas, processInput, configSchema, mustErr[s.ID], mayErr[s.ID])
			if s.Action != nil || s.Output.Present() {
				loops := slices.Contains(optional[s.ID], s.ID) || slices.Contains(required[s.ID], s.ID)
				switchCtx = addSelfSchema(switchCtx, s, loops)
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
					return fmt.Errorf("task %q switch case %q: %w", s.ID, c.Case, err)
				}
				if !isType(inferred.Node(), "boolean") {
					return fmt.Errorf("task %q switch case %q: expression must evaluate to boolean, got %q", s.ID, c.Case, schemaTypeName(inferred.Node()))
				}
			}
		}
	}
	return nil
}

// checkNonNullTemplate infers a template string (a rest endpoint or header value)
// against ctx and returns an error if it fails to type-check or may be null — a
// null URL or header value would silently stringify to "null".
func checkNonNullTemplate(expr string, ctx *schema.SchemaNode, label string) error {
	inferred, err := inferShape(expr, ctx, label)
	if err != nil {
		return err
	}
	if schema.HasNullType(inferred) {
		return fmt.Errorf("%s may be null; use ?? to provide a default value", label)
	}
	return nil
}

func inferInput(s *model.Task, ctx *schema.SchemaNode, defs map[string]*schema.SchemaNode) (*schema.SchemaNode, error) {
	if !s.Action.Input.Present() {
		return &schema.SchemaNode{Type: schema.SchemaType{"object"}}, nil
	}
	if len(defs) > 0 {
		ctx = withDefs(ctx, defs)
	}
	return inferShape(s.Action.Input.Raw, ctx, fmt.Sprintf("task %q input", s.ID))
}

// inferShape infers the JSON Schema of a model.Shape value: a string leaf yields
// its template's inferred type (which may be any shape), and an object yields an
// object schema whose values are inferred recursively (all keys required). label
// prefixes errors. The string|object grammar is enforced at unmarshal.
func inferShape(node any, ctx *schema.SchemaNode, label string) (*schema.SchemaNode, error) {
	switch n := node.(type) {
	case string:
		inferred, err := template.InferType(n, schema.FromNode(ctx))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		out := inferred.Node()
		// Taint the leaf if its expression reads a secret. Structural secrets (a
		// passed-through secret node) are already carried on `out`; this adds the
		// reference-taint that survives any transformation the expression applies.
		if sec, serr := template.ReferencesSecret(n, schema.FromNode(ctx)); serr == nil && sec {
			out = schema.Taint(out)
		}
		return out, nil
	case map[string]any:
		props := make(map[string]*schema.SchemaNode, len(n))
		required := make([]string, 0, len(n))
		for name, child := range n {
			p, err := inferShape(child, ctx, fmt.Sprintf("%s.%s", label, name))
			if err != nil {
				return nil, err
			}
			props[name] = p
			required = append(required, name)
		}
		return &schema.SchemaNode{
			Type:       schema.SchemaType{"object"},
			Properties: props,
			Required:   required,
		}, nil
	default:
		return nil, fmt.Errorf("%s: invalid shape node %T", label, node)
	}
}

func contextSchema(preceding []string, optional []string, tasks map[string]TaskSchemas, processInput, configSchema *schema.SchemaNode, errRequired, errOptional bool) *schema.SchemaNode {
	props := make(map[string]*schema.SchemaNode)
	required := []string{"outputs"}
	if processInput != nil {
		props["input"] = processInput
		required = append(required, "input")
	}
	if configSchema != nil {
		props["config"] = configSchema
		required = append(required, "config")
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
				"task":    {Type: schema.SchemaType{"string"}},
				"message": {Type: schema.SchemaType{"string"}},
				"code":    {Type: schema.SchemaType{"string"}},
			},
			Required: []string{"task", "message", "code"},
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

// addSelfSchema gives a switch context this task's transient self scope:
//   - self.result: the raw action result (typed by result_schema; null for delay
//     or a no-action task). Always present.
//   - self.output: the exported output projection — present only when the task
//     defines an `output`. Routing on the raw result of a task with no projection
//     uses self.result; referencing self.output there is an error.
//   - self.previous: this task's prior output — present only when it loops (and so
//     has a prior iteration). Both output and previous resolve through
//     $defs[<id>_output].
func addSelfSchema(ctx *schema.SchemaNode, s *model.Task, loops bool) *schema.SchemaNode {
	selfProps := map[string]*schema.SchemaNode{"result": actionResultType(s)}
	required := []string{"result"}
	if s.Output.Present() {
		selfProps["output"] = schemaRef(s.ID + "_output")
		required = append(required, "output")
		if loops {
			selfProps["previous"] = schemaRef(s.ID + "_output")
		}
	}
	self := &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: selfProps,
		Required:   required,
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

// actionResultType is the type of a task's raw action result — self.result inside
// an output map (typed by result_schema when present, else permissive; null for
// delay or a no-action task).
func actionResultType(s *model.Task) *schema.SchemaNode {
	if s.Action == nil {
		return &schema.SchemaNode{Type: schema.SchemaType{"null"}}
	}
	switch s.Action.Type {
	case model.ActionTypeChildParallel:
		return childParallelOutputSchema(s)
	case model.ActionTypeDelay:
		return &schema.SchemaNode{Type: schema.SchemaType{"null"}}
	default:
		if s.Action.ResultSchema != nil {
			return s.Action.ResultSchema
		}
		return &schema.SchemaNode{Type: schema.SchemaType{"object"}}
	}
}

// outputMapContext builds the context for inferring a task's output map: the base
// context plus self.result (the raw action result), and — only when the task
// actually loops back to itself — self.previous (its own prior output).
//
// The self-reference is meaningful only for a looping task, which alone has a
// prior iteration. When loops is true, both self.previous and outputs.<id> (the
// latter supplied by the base context via reachability) resolve through
// $defs[<id>_output], the recursive placeholder the fixpoint drives. When the
// task does not loop, neither is available — referencing one's own output without
// looping is an error, since the task is not its own predecessor.
func outputMapContext(base *schema.SchemaNode, resultType *schema.SchemaNode, taskID string, loops bool) *schema.SchemaNode {
	selfProps := map[string]*schema.SchemaNode{"result": resultType}
	if loops {
		selfProps["previous"] = schemaRef(taskID + "_output")
	}

	newProps := make(map[string]*schema.SchemaNode, len(base.Properties)+1)
	for k, v := range base.Properties {
		newProps[k] = v
	}
	newProps["self"] = &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: selfProps,
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
