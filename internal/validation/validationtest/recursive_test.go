package validationtest

import (
	"encoding/json"
	"testing"

	"gent/internal/schema"
	"gent/internal/validation"
)

func mustMarshal(n *schema.SchemaNode) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func sprim(types ...string) *schema.SchemaNode {
	return &schema.SchemaNode{Type: schema.SchemaType(types)}
}

func sobj(req []string, props map[string]*schema.SchemaNode) *schema.SchemaNode {
	return &schema.SchemaNode{Type: schema.SchemaType{"object"}, Properties: props, Required: req}
}

// recCtx builds the schema context exactly as the validation pipeline would have
// it when inferring a self-referential step's output: both outputs.<selfID> and
// self.previous are $refs to $defs[<selfID>_output] (the recursive placeholder),
// and any sibling step outputs are always-available (required). This represents
// the process "in that state" without standing up the whole pipeline.
func recCtx(selfID string, siblings map[string]*schema.SchemaNode) (*schema.SchemaNode, string) {
	selfDef := selfID + "_output"
	ref := &schema.SchemaNode{Ref: "#/$defs/" + selfDef}

	outProps := map[string]*schema.SchemaNode{selfID: ref} // self output is NOT required (nullable previous)
	var outReq []string
	for k, v := range siblings {
		outProps[k] = v
		outReq = append(outReq, k)
	}

	ctx := &schema.SchemaNode{
		Type:     schema.SchemaType{"object"},
		Required: []string{"outputs", "self"},
		Properties: map[string]*schema.SchemaNode{
			"outputs": {Type: schema.SchemaType{"object"}, Properties: outProps, Required: outReq},
			"self": {Type: schema.SchemaType{"object"}, Properties: map[string]*schema.SchemaNode{
				"previous": ref,
			}},
		},
		Defs: map[string]*schema.SchemaNode{
			selfDef: {Type: schema.SchemaType{"null"}}, // placeholder; the fixpoint rebinds it
		},
	}
	return ctx, selfDef
}

func TestInferRecursiveOutput(t *testing.T) {
	tests := []struct {
		name     string
		exprs    map[string]string
		siblings map[string]*schema.SchemaNode
		selfID   string
		want     *schema.SchemaNode
		wantErr  bool
	}{
		{
			name:   "counter via outputs.<self>",
			exprs:  map[string]string{"n": "{{ (outputs.count.n ?? 0) + 1 }}"},
			selfID: "count",
			want:   sobj([]string{"n"}, map[string]*schema.SchemaNode{"n": sprim("integer")}),
		},
		{
			name:   "counter via self.previous",
			exprs:  map[string]string{"n": "{{ (self.previous.n ?? 0) + 1 }}"},
			selfID: "count",
			want:   sobj([]string{"n"}, map[string]*schema.SchemaNode{"n": sprim("integer")}),
		},
		{
			name:   "string accumulator",
			exprs:  map[string]string{"s": `{{ (outputs.cat.s ?? "") + "x" }}`},
			selfID: "cat",
			want:   sobj([]string{"s"}, map[string]*schema.SchemaNode{"s": sprim("string")}),
		},
		{
			name:   "boolean toggle via self.previous",
			exprs:  map[string]string{"f": "{{ !(self.previous.f ?? false) }}"},
			selfID: "tog",
			want:   sobj([]string{"f"}, map[string]*schema.SchemaNode{"f": sprim("boolean")}),
		},
		{
			name:     "sum folding a sibling output",
			exprs:    map[string]string{"total": "{{ (outputs.acc.total ?? 0) + outputs.item.value }}"},
			siblings: map[string]*schema.SchemaNode{"item": sobj([]string{"value"}, map[string]*schema.SchemaNode{"value": sprim("number")})},
			selfID:   "acc",
			want:     sobj([]string{"total"}, map[string]*schema.SchemaNode{"total": sprim("number")}),
		},
		{
			name: "multiple fields mixing both self references",
			exprs: map[string]string{
				"n": "{{ (outputs.s.n ?? 0) + 1 }}",
				"f": "{{ !(self.previous.f ?? false) }}",
			},
			selfID: "s",
			want: sobj([]string{"f", "n"}, map[string]*schema.SchemaNode{
				"n": sprim("integer"),
				"f": sprim("boolean"),
			}),
		},
		{
			name:    "no base case (no ?? default) is rejected",
			exprs:   map[string]string{"n": "{{ outputs.c.n + 1 }}"},
			selfID:  "c",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, selfDef := recCtx(tt.selfID, tt.siblings)
			got, err := validation.InferRecursiveOutput(tt.exprs, ctx, selfDef)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !schema.Equal(got, tt.want) {
				t.Errorf("type mismatch:\n  got:  %s\n  want: %s",
					mustMarshal(schema.Canonicalize(got)), mustMarshal(schema.Canonicalize(tt.want)))
			}
		})
	}
}
