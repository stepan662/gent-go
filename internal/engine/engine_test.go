package engine

import (
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
		{"outputs.step.ok == true", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"ok": true}}}, true, false},
		{"outputs.step.ok == true", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"ok": false}}}, false, false},
		{"outputs.step.amount > 100", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"amount": 200}}}, true, false},
		{"outputs.step.amount > 100", map[string]interface{}{"outputs": map[string]any{"step": map[string]any{"amount": 50}}}, false, false},
		{"input.a == true && input.b == true", map[string]interface{}{"input": map[string]any{"a": true, "b": true}}, true, false},
		{"input.a == true && input.b == true", map[string]interface{}{"input": map[string]any{"a": true, "b": false}}, false, false},
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

func TestStepQueueConditional(t *testing.T) {
	shipStep := &model.Step{Type: model.StepTypeTask, ID: "ship", Transport: model.TransportHTTP, Endpoint: "http://x"}
	refundStep := &model.Step{Type: model.StepTypeTask, ID: "refund", Transport: model.TransportHTTP, Endpoint: "http://x"}
	followUp := &model.Step{Type: model.StepTypeTask, ID: "followup", Transport: model.TransportHTTP, Endpoint: "http://x"}

	conditional := &model.Step{
		Type:      model.StepTypeConditional,
		ID:        "check",
		Condition: "outputs.pay.paid == true",
		Then:      []*model.Step{shipStep},
		Else:      []*model.Step{refundStep},
	}

	inst := &model.ProcessInstance{
		StepQueue:   []*model.Step{conditional, followUp},
		ContextData: map[string]interface{}{"outputs": map[string]any{"pay": map[string]any{"paid": true}}},
	}

	eval := Evaluator{}
	result, _ := eval.Eval(conditional.Condition, inst.ContextData)

	rest := inst.StepQueue[1:]
	var branch []*model.Step
	if result {
		branch = conditional.Then
	} else {
		branch = conditional.Else
	}
	inst.StepQueue = append(branch, rest...)

	if len(inst.StepQueue) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(inst.StepQueue))
	}
	if inst.StepQueue[0].ID != "ship" {
		t.Errorf("expected ship step first, got %q", inst.StepQueue[0].ID)
	}
	if inst.StepQueue[1].ID != "followup" {
		t.Errorf("expected followup second, got %q", inst.StepQueue[1].ID)
	}
}
