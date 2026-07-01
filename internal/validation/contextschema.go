package validation

import (
	"genroc/internal/model"
	"genroc/internal/schema"
)

// ContextSchema composes a single navigable schema for a process's entire runtime
// context. Its root is an object shaped like context_data:
//
//	{
//	  "input":   <process input schema>,
//	  "outputs": { "<taskID>": <task output schema>, ... },
//	  "output":  <process output schema>
//	}
//
// with every $ref resolving against a shared root $defs. Because it is a plain
// schema.Schema, the whole process shape can then be queried uniformly:
//
//	ctx.ValidateAt("outputs.charge", value)  // validate + normalize a subpath
//	ctx.SecretAt("input.password")           // is a path secret?
//	ctx.Infer("outputs.charge.amount")       // subschema / type at a path
//	ctx.Redact(contextData)                  // scrub the whole context for logs
//
// This is the composition half of the design: Generate does the dataflow analysis
// (which prior outputs are available where, recursion, etc.); ContextSchema folds
// its SchemaFile into one object so callers no longer juggle ProcessInput/Tasks/
// ProcessOutput/Defs separately.
func ContextSchema(def *model.ProcessDefinition) (schema.Schema, error) {
	sf, err := Generate(def)
	if err != nil {
		return schema.Schema{}, err
	}
	return SchemaFileContext(sf), nil
}

// SchemaFileContext assembles the context schema from an already-computed
// SchemaFile, avoiding a re-run of Generate on hot paths (a caller that already
// holds the SchemaFile — e.g. cached per process version — reuses it here).
func SchemaFileContext(sf SchemaFile) schema.Schema {
	props := make(map[string]*schema.SchemaNode, 3)
	var required []string

	if sf.ProcessInput != nil {
		props["input"] = sf.ProcessInput
		required = append(required, "input")
	}
	if len(sf.Tasks) > 0 {
		outProps := make(map[string]*schema.SchemaNode, len(sf.Tasks))
		for tid, ts := range sf.Tasks {
			if ts.Output != nil {
				outProps[tid] = ts.Output
			}
		}
		if len(outProps) > 0 {
			props["outputs"] = &schema.SchemaNode{Type: schema.SchemaType{"object"}, Properties: outProps}
		}
	}
	if sf.ProcessOutput != nil {
		props["output"] = sf.ProcessOutput
	}

	root := &schema.SchemaNode{
		Type:       schema.SchemaType{"object"},
		Properties: props,
		Required:   required,
		Defs:       sf.Defs,
	}
	return schema.FromNode(root)
}
