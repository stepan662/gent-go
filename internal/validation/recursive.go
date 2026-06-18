package validation

import (
	"gent/internal/schema"
)

// maxRecursivePasses bounds the fixpoint iteration. Fixed-shape accumulators
// (counters, sums, toggles) converge in 1–2 passes; the cap turns a genuinely
// diverging type into an error instead of an infinite loop.
const maxRecursivePasses = 16

// maxOutputTypeBytes is a widening bound on the canonical size of an inferred
// output type. A non-converging recursion (e.g. `result: self.previous ?? input`)
// grows the type exponentially per pass, so the pass cap alone would still build
// a multi-megabyte schema before giving up. This bound — far larger than any
// realistic output type — catches the divergence within a few passes instead.
const maxOutputTypeBytes = 64 * 1024

// InferRecursiveOutput infers the type of a single self-referential output map.
// In ctx, both outputs.<id> and self.previous resolve via $ref to selfDef (the
// recursive placeholder in ctx.$defs). It is the one-member case of the joint
// fixpoint used for mutually-recursive output maps; ctx.$defs is mutated in place
// so the running estimate is observed through those $refs, and selfDef ends up
// holding the inferred (non-null) type, which is returned.
func InferRecursiveOutput(exprs map[string]string, ctx *schema.SchemaNode, selfDef string) (*schema.SchemaNode, error) {
	defs := ctx.Defs
	if defs == nil {
		defs = map[string]*schema.SchemaNode{}
		ctx = withDefs(ctx, defs)
	}
	node := make(map[string]any, len(exprs))
	for k, v := range exprs {
		node[k] = v
	}
	if err := inferOutputFixpoint([]sccMember{{defName: selfDef, node: node, ctx: ctx}}, defs); err != nil {
		return nil, err
	}
	return defs[selfDef], nil
}
