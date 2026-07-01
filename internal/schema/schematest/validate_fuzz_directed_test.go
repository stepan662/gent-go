package schematest

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"genroc/internal/schema"

	"github.com/xeipuuv/gojsonschema"
)

// TestValidateDecisionMatchesGojsonschemaDirected complements the blind fuzz test
// with a schema-*directed* generator. Blind random documents are almost always
// invalid for a structured schema (a random map rarely carries the right required
// keys), so they exercise the rejection path but barely touch acceptance or the
// near-miss boundaries. This generator instead builds instances shaped by the
// schema whose scalars/lengths/counts straddle each declared bound and that
// occasionally drop a required field — yielding a balanced mix of accepted docs
// and boundary rejects, which is where a decision divergence would actually hide.
func TestValidateDecisionMatchesGojsonschemaDirected(t *testing.T) {
	const (
		seed           = 0x2545f491
		itersPerSchema = 4000
	)

	schemas := []string{
		`{"type":"object","properties":{"id":{"type":"integer","minimum":1,"maximum":10},"name":{"type":["string","null"]},"tags":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":2}},"required":["id","name"]}`,
		`{"type":"object","properties":{"user":{"type":"object","properties":{"name":{"type":"string","minLength":1,"maxLength":4}},"required":["name"]},"item":{"$ref":"#/$defs/Item"}},"required":["user"],"$defs":{"Item":{"type":"object","properties":{"v":{"type":"integer","minimum":0}},"required":["v"]}}}`,
		`{"oneOf":[{"type":"string","minLength":2},{"type":"integer","minimum":0,"maximum":100}]}`,
		`{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"null"}]}`,
		`{"type":"array","items":{"type":"object","properties":{"v":{"type":"integer"},"label":{"type":"string","minLength":2}},"required":["v"]},"minItems":1,"maxItems":3}`,
		`{"type":"string","enum":["red","green","blue"]}`,
	}

	for si, schemaJSON := range schemas {
		compiled, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(schemaJSON))
		if err != nil {
			t.Fatalf("schema[%d] gojsonschema compile: %v", si, err)
		}
		sc, err := schema.Parse([]byte(schemaJSON))
		if err != nil {
			t.Fatalf("schema[%d] schema.Parse: %v", si, err)
		}
		node := sc.Node()

		r := rand.New(rand.NewSource(int64(seed) + int64(si)))
		var nValid, nInvalid int
		for i := 0; i < itersPerSchema; i++ {
			// Round-trip so both validators see identical JSON typing.
			raw, err := json.Marshal(genFromSchema(r, node, node.Defs, 4))
			if err != nil {
				continue
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				continue
			}

			result, err := compiled.Validate(gojsonschema.NewGoLoader(doc))
			if err != nil {
				t.Fatalf("schema[%d] gojsonschema.Validate(%s): %v", si, raw, err)
			}
			theirsValid := result.Valid()

			_, ourErr := sc.Validate(doc)
			oursValid := ourErr == nil

			if oursValid != theirsValid {
				t.Fatalf("DISAGREEMENT\n  schema: %s\n  doc:    %s\n  ours-valid=%v (err=%v)\n  gojsonschema-valid=%v (errs=%v)",
					schemaJSON, raw, oursValid, ourErr, theirsValid, result.Errors())
			}
			if theirsValid {
				nValid++
			} else {
				nInvalid++
			}
		}
		if nValid == 0 || nInvalid == 0 {
			t.Errorf("schema[%d] degenerate directed split: valid=%d invalid=%d", si, nValid, nInvalid)
		}
		t.Logf("schema[%d]: %d accepted / %d rejected (agree)", si, nValid, nInvalid)
	}
}

// genFromSchema builds a value shaped by node. It is deliberately imperfect: it
// straddles numeric/length/item bounds and occasionally drops a required property
// or emits a wrong-typed value, so the corpus spans valid, boundary, and invalid.
func genFromSchema(r *rand.Rand, node *schema.SchemaNode, defs map[string]*schema.SchemaNode, depth int) any {
	node, err := schema.Deref(node, defs)
	if err != nil || node == nil {
		return randScalar(r)
	}
	// ~1 in 12: emit a wrong-shaped value to force a type error at this node.
	if r.Intn(12) == 0 {
		return randScalar(r)
	}
	if len(node.OneOf) > 0 {
		return genFromSchema(r, node.OneOf[r.Intn(len(node.OneOf))], defs, depth)
	}
	if len(node.AnyOf) > 0 {
		return genFromSchema(r, node.AnyOf[r.Intn(len(node.AnyOf))], defs, depth)
	}
	if len(node.Enum) > 0 {
		if r.Intn(4) == 0 {
			return "definitely-not-in-enum"
		}
		return node.Enum[r.Intn(len(node.Enum))]
	}

	typ := "object"
	switch {
	case len(node.Type) > 0:
		typ = node.Type[r.Intn(len(node.Type))]
	case node.Properties != nil:
		typ = "object"
	case node.Items != nil:
		typ = "array"
	default:
		return randScalar(r)
	}

	switch typ {
	case "null":
		return nil
	case "boolean":
		return r.Intn(2) == 0
	case "integer":
		return genBoundedInt(r, node)
	case "number":
		return float64(genBoundedInt(r, node)) + 0.5*float64(r.Intn(2))
	case "string":
		return genBoundedString(r, node)
	case "array":
		return genArray(r, node, defs, depth)
	default: // object
		return genObject(r, node, defs, depth)
	}
}

func genObject(r *rand.Rand, node *schema.SchemaNode, defs map[string]*schema.SchemaNode, depth int) any {
	obj := map[string]any{}
	for name, prop := range node.Properties {
		required := contains(node.Required, name)
		include := required || r.Intn(2) == 0
		if required && r.Intn(10) == 0 {
			include = false // occasionally drop a required field → invalid
		}
		if !include {
			continue
		}
		if depth <= 0 {
			obj[name] = randScalar(r)
		} else {
			obj[name] = genFromSchema(r, prop, defs, depth-1)
		}
	}
	if r.Intn(3) == 0 {
		obj["undeclared"] = randScalar(r) // both validators accept extras
	}
	return obj
}

func genArray(r *rand.Rand, node *schema.SchemaNode, defs map[string]*schema.SchemaNode, depth int) any {
	lo, hi := 0, 3
	if node.MinItems != nil {
		lo = *node.MinItems - 1
	}
	if node.MaxItems != nil {
		hi = *node.MaxItems + 1
	}
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	n := lo + r.Intn(hi-lo+1)
	arr := make([]any, n)
	for i := range arr {
		if depth <= 0 {
			arr[i] = randScalar(r)
		} else {
			arr[i] = genFromSchema(r, node.Items, defs, depth-1)
		}
	}
	return arr
}

// genBoundedInt returns an int that straddles [minimum, maximum] so off-by-one
// boundary cases (which must decide identically on both sides) show up often.
func genBoundedInt(r *rand.Rand, node *schema.SchemaNode) int {
	lo, hi := -4, 14
	if node.Minimum != nil {
		lo = int(*node.Minimum) - 2
	}
	if node.Maximum != nil {
		hi = int(*node.Maximum) + 2
	}
	if hi <= lo {
		hi = lo + 1
	}
	return lo + r.Intn(hi-lo+1)
}

// genBoundedString returns a string whose length straddles [minLength, maxLength].
func genBoundedString(r *rand.Rand, node *schema.SchemaNode) string {
	lo, hi := 0, 6
	if node.MinLength != nil {
		lo = *node.MinLength - 1
	}
	if node.MaxLength != nil {
		hi = *node.MaxLength + 1
	}
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	return strings.Repeat("a", lo+r.Intn(hi-lo+1))
}

// randScalar returns an arbitrary scalar or small container, used both as the
// leaf filler and as the intentional wrong-type corruption.
func randScalar(r *rand.Rand) any {
	switch r.Intn(6) {
	case 0:
		return nil
	case 1:
		return r.Intn(2) == 0
	case 2:
		return r.Intn(20) - 5
	case 3:
		return float64(r.Intn(10)) + 0.5
	case 4:
		return stringPool[r.Intn(len(stringPool))]
	default:
		return []any{r.Intn(3)}
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
