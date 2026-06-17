package validation

import (
	"fmt"
	"strings"

	"gent/internal/model"
)

// predEdge is a predecessor edge in the step graph.
// isErr is true for on_error routes: the failing step has no output on this path.
type predEdge struct {
	idx   int  // predecessor step index; -1 = process start
	isErr bool // true = on_error route
}

func stepHasOutput(s *model.Step) bool {
	if s.Action == nil {
		return false
	}
	if s.Action.Type == model.ActionTypeChildParallel {
		return len(s.Action.Children) > 0
	}
	return s.Action.OutputSchema != nil
}

// outputContextSets returns which step outputs are required/optional at the
// process output boundary, and whether $error is required or optional there.
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
		errMin bool
		errMax bool
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

// buildPreds constructs the predecessor graph for the step slice.
// preds[i] lists all edges that route into step i; the process start is
// represented as predEdge{idx: -1} on step 0.
func buildPreds(steps []*model.Step) [][]predEdge {
	n := len(steps)
	idx := make(map[string]int, n)
	for i, s := range steps {
		idx[s.ID] = i
	}
	preds := make([][]predEdge, n)
	preds[0] = append(preds[0], predEdge{idx: -1})
	for i, s := range steps {
		addedNext := false
		for _, c := range s.Switch {
			if strings.HasPrefix(c.Goto, "$") {
				if j, ok := idx[c.Goto[1:]]; ok {
					preds[j] = append(preds[j], predEdge{idx: i})
				}
			} else if c.Goto == model.GotoNext && !addedNext && i+1 < n {
				preds[i+1] = append(preds[i+1], predEdge{idx: i})
				addedNext = true
			}
		}
		// Backward-compat: steps with no switch fall through to the next step.
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
	return preds
}

// checkReachability returns an error if any step cannot be reached from the
// first step via switch gotos or on_error routes.
func checkReachability(steps []*model.Step) error {
	if len(steps) == 0 {
		return nil
	}
	preds := buildPreds(steps)
	reachable := make([]bool, len(steps))
	reachable[0] = true
	for {
		changed := false
		for i, ps := range preds {
			if reachable[i] {
				continue
			}
			for _, p := range ps {
				if p.idx >= 0 && reachable[p.idx] {
					reachable[i] = true
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	for i, s := range steps {
		if !reachable[i] {
			return fmt.Errorf("step %q is unreachable: no switch or error handler routes to it", s.ID)
		}
	}
	return nil
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

	preds := buildPreds(steps)

	hasOutput := make([]bool, n)
	for i, s := range steps {
		hasOutput[i] = stepHasOutput(s)
	}

	allTrue := func() []bool {
		s := make([]bool, n)
		for i := range s {
			s[i] = true
		}
		return s
	}
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
