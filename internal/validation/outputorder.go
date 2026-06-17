package validation

import (
	"fmt"
	"sort"

	"gent/internal/model"
	"gent/internal/schema"
	"gent/internal/template"
)

// inferOutputs infers the type of every output-map step's output and writes it to
// defs (as <id>_output). Steps are processed in dependency order so a step that
// reads another's output is inferred after it; mutually-recursive steps (a cycle
// of outputs.<id> references, including a single step referencing itself) are
// resolved together by a joint fixpoint over the strongly-connected component.
func inferOutputs(steps []*model.Step, tasks map[string]TaskSchemas, processInput *schema.SchemaNode,
	defs map[string]*schema.SchemaNode, required, optional map[string][]string, mustErr, mayErr map[string]bool) error {

	stepByID := make(map[string]*model.Step, len(steps))
	isOutputMap := make(map[string]bool)
	var omIDs []string
	for _, s := range steps {
		stepByID[s.ID] = s
		if len(s.Output) > 0 {
			isOutputMap[s.ID] = true
			omIDs = append(omIDs, s.ID)
		}
	}
	if len(omIDs) == 0 {
		return nil
	}

	// Edges A -> B when A's output map reads outputs.B and B is itself an
	// output-map step (static output_schema outputs are already in defs, so they
	// impose no ordering). A self-edge (A reads outputs.A) marks self-recursion.
	graph := make(map[string][]string, len(omIDs))
	for _, id := range omIDs {
		refSet := map[string]bool{}
		for _, expr := range stepByID[id].Output {
			refs, err := template.OutputRefs(expr)
			if err != nil {
				return fmt.Errorf("step %q output: %w", id, err)
			}
			for _, r := range refs {
				if isOutputMap[r] {
					refSet[r] = true
				}
			}
		}
		deps := make([]string, 0, len(refSet))
		for r := range refSet {
			deps = append(deps, r)
		}
		sort.Strings(deps)
		graph[id] = deps
	}

	// Tarjan emits SCCs dependency-first, which is exactly the inference order.
	for _, scc := range tarjanSCC(graph, omIDs) {
		members := make([]sccMember, 0, len(scc))
		for _, id := range scc {
			base := contextSchema(required[id], optional[id], tasks, processInput, mustErr[id], mayErr[id])
			if len(defs) > 0 {
				base = withDefs(base, defs)
			}
			ctx := outputMapContext(base, actionResultType(stepByID[id]), id)
			members = append(members, sccMember{defName: id + "_output", exprs: stepByID[id].Output, ctx: ctx})
		}
		if err := inferOutputFixpoint(members, defs); err != nil {
			return err
		}
	}
	return nil
}

// sccMember is one output map in a strongly-connected component, with the context
// it is inferred against (which references defs through $refs, so estimate updates
// are observed as the fixpoint iterates).
type sccMember struct {
	defName string
	exprs   map[string]string
	ctx     *schema.SchemaNode
}

// inferOutputFixpoint drives a joint fixpoint over an SCC of output maps. Each
// member's def is seeded null (its value before the first iteration, which a
// `?? default` base case resolves), re-fed as the running estimate (nullable)
// each pass, joined and canonicalized, until every member stabilizes. defs is
// finalized in place to each member's (non-null) inferred type. A single
// non-recursive member converges in one pass.
func inferOutputFixpoint(members []sccMember, defs map[string]*schema.SchemaNode) error {
	est := make(map[string]*schema.SchemaNode, len(members))
	for _, m := range members {
		defs[m.defName] = &schema.SchemaNode{Type: schema.SchemaType{"null"}}
	}
	for pass := 0; pass < maxRecursivePasses; pass++ {
		stable := true
		for _, m := range members {
			cur, err := inferObjectSchema(m.exprs, m.ctx, func(name string) string {
				return fmt.Sprintf("output %q", name)
			})
			if err != nil {
				return err
			}
			cur = schema.Canonicalize(cur)
			next := cur
			if prev := est[m.defName]; prev != nil {
				next = schema.Join(prev, cur)
				if !schema.Equal(next, prev) {
					stable = false
				}
			} else {
				stable = false
			}
			est[m.defName] = next
			// Feed the estimate back (nullable) so other members — and this one on
			// the next pass — read it through their $refs.
			defs[m.defName] = schema.WithNull(next)
		}
		if stable {
			for _, m := range members {
				defs[m.defName] = est[m.defName] // exported output is non-null
			}
			return nil
		}
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.defName
	}
	return fmt.Errorf("recursive output types did not stabilize after %d passes (cycle: %v)", maxRecursivePasses, ids)
}

// tarjanSCC returns the strongly-connected components of graph in dependency-first
// (reverse-topological) order. nodes fixes the iteration order for determinism.
func tarjanSCC(graph map[string][]string, nodes []string) [][]string {
	index := make(map[string]int, len(nodes))
	low := make(map[string]int, len(nodes))
	onStack := make(map[string]bool, len(nodes))
	var stack []string
	next := 0
	var sccs [][]string

	var strongconnect func(v string)
	strongconnect = func(v string) {
		index[v] = next
		low[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range graph[v] {
			if _, seen := index[w]; !seen {
				strongconnect(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	for _, v := range nodes {
		if _, seen := index[v]; !seen {
			strongconnect(v)
		}
	}
	return sccs
}
