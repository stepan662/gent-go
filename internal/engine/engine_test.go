package engine

import (
	"encoding/json"
	"testing"

	"gent/internal/model"
)

func TestEvaluator(t *testing.T) {
	eval := Evaluator{}

	tests := []struct {
		expr    string
		ctx     map[string]interface{}
		want    bool
		wantErr bool
	}{
		{"{{outputs.step.ok == true}}", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"ok": true}}}, true, false},
		{"{{outputs.step.ok == true}}", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"ok": false}}}, false, false},
		{"{{outputs.step.amount > 100}}", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"amount": 200}}}, true, false},
		{"{{outputs.step.amount > 100}}", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"amount": 50}}}, false, false},
		{"{{input.a == true && input.b == true}}", map[string]interface{}{"input": map[string]any{"a": true, "b": true}}, true, false},
		{"{{input.a == true && input.b == true}}", map[string]interface{}{"input": map[string]any{"a": true, "b": false}}, false, false},
		{"invalid %%% expr", nil, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := eval.Eval(tt.expr, tt.ctx)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("Eval(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestEvaluator_EvalBool_WithSelf(t *testing.T) {
	eval := Evaluator{}
	ctx := map[string]any{"outputs": map[string]any{}, "input": map[string]any{}}

	tests := []struct {
		name string
		expr string
		self any
		want bool
	}{
		{"self field true", "{{self.paid == true}}", map[string]any{"paid": true}, true},
		{"self field false", "{{self.paid == true}}", map[string]any{"paid": false}, false},
		{"self nested field", "{{self.result.ok == true}}", map[string]any{"result": map[string]any{"ok": true}}, true},
		{"self nil when no action", "{{self == nil}}", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := eval.EvalBool(tt.expr, ctx, tt.self)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("EvalBool(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestSwitchMap_JSON_RoundTrip(t *testing.T) {
	original := model.SwitchMap{
		{When: "self.paid == true", Goto: "ship"},
		{When: "self.paid == false", Goto: "refund"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `[{"when":"self.paid == true","goto":"#ship"},{"when":"self.paid == false","goto":"#refund"}]`
	if string(data) != want {
		t.Errorf("marshal: got %s, want %s", data, want)
	}

	var decoded model.SwitchMap
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded) != len(original) {
		t.Fatalf("length mismatch: got %d, want %d", len(decoded), len(original))
	}
	for i := range original {
		if decoded[i] != original[i] {
			t.Errorf("case %d: got %+v, want %+v", i, decoded[i], original[i])
		}
	}
}
