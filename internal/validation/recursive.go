package validation

import (
	"fmt"

	"gent/internal/schema"
)

// maxRecursivePasses bounds the fixpoint iteration. Fixed-shape accumulators
// (counters, sums, toggles) converge in 1–2 passes; the cap turns a genuinely
// diverging type into an error instead of an infinite loop.
const maxRecursivePasses = 16

// InferRecursiveOutput infers the type of a step's output map (field → template
// expression) when the step references its own previous output. In ctx, both
// outputs.<id> and self.previous resolve via $ref to the same $defs entry,
// selfDef; that entry is the recursive placeholder.
//
// It runs a bounded fixpoint over the type lattice: selfDef is seeded as null —
// the previous output's genuine value before the first iteration, which a
// `?? default` base case resolves — then re-bound to the running estimate
// (nullable) each pass, joined and canonicalized, until the type stabilizes. A
// recursive expression with no base case surfaces as an inference error from the
// null seed; a type that never stabilizes hits the pass cap and errors.
func InferRecursiveOutput(exprs map[string]string, ctx *schema.SchemaNode, selfDef string) (*schema.SchemaNode, error) {
	var prev *schema.SchemaNode
	for pass := 0; pass < maxRecursivePasses; pass++ {
		seed := &schema.SchemaNode{Type: schema.SchemaType{"null"}}
		if prev != nil {
			seed = schema.WithNull(prev)
		}
		cur, err := inferObjectSchema(exprs, bindDef(ctx, selfDef, seed), func(name string) string {
			return fmt.Sprintf("output %q", name)
		})
		if err != nil {
			return nil, err
		}
		cur = schema.Canonicalize(cur)
		if prev == nil {
			prev = cur
			continue
		}
		joined := schema.Join(prev, cur)
		if schema.Equal(joined, prev) {
			return prev, nil
		}
		prev = joined
	}
	return nil, fmt.Errorf("recursive output type did not stabilize after %d passes", maxRecursivePasses)
}

// bindDef returns a shallow copy of ctx whose $defs[name] is val, leaving the
// original untouched. Both the outputs.<id> and self.previous $refs read through
// this entry, so a single rebind updates the whole recursive view.
func bindDef(ctx *schema.SchemaNode, name string, val *schema.SchemaNode) *schema.SchemaNode {
	defs := make(map[string]*schema.SchemaNode, len(ctx.Defs)+1)
	for k, v := range ctx.Defs {
		defs[k] = v
	}
	defs[name] = val

	n := *ctx
	n.Defs = defs
	return &n
}
