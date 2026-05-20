package model

import (
	"testing"
)

func TestProcessDefinition_Normalize(t *testing.T) {
	validTask := func(id string) *Step {
		return &Step{Type: StepTypeTask, ID: id, Transport: TransportHTTP, Endpoint: "http://localhost/x"}
	}

	t.Run("no schemas is a no-op", func(t *testing.T) {
		d := ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{validTask("s1")}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("simple InputSchema without refs is unchanged", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Version: 1, Steps: []*Step{validTask("s1")},
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "integer"}},
			},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		if props == nil {
			t.Fatal("properties missing after normalize")
		}
	})

	t.Run("InputSchema $defs are flattened to root", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Version: 1, Steps: []*Step{validTask("s1")},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"addr": map[string]any{"$ref": "#/$defs/Address"},
				},
				"$defs": map[string]any{
					"Address": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"street": map[string]any{"type": "string"},
						},
					},
				},
			},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defs, _ := d.InputSchema["$defs"].(map[string]any)
		if defs == nil || defs["Address"] == nil {
			t.Fatal("$defs/Address missing after normalize")
		}
		if d.InputSchema["properties"] == nil {
			t.Fatal("properties missing after normalize")
		}
	})

	t.Run("step OutputSchema $defs are flattened to root", func(t *testing.T) {
		step := validTask("charge")
		step.OutputSchema = map[string]any{
			"type": "object",
			"$defs": map[string]any{
				"Result": map[string]any{"type": "object", "properties": map[string]any{
					"ok": map[string]any{"type": "boolean"},
				}},
			},
			"properties": map[string]any{
				"result": map[string]any{"$ref": "#/$defs/Result"},
			},
		}
		d := ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{step}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defs, _ := step.OutputSchema["$defs"].(map[string]any)
		if defs == nil || defs["Result"] == nil {
			t.Fatal("$defs/Result missing in OutputSchema after normalize")
		}
	})

	t.Run("nested step OutputSchema is normalized", func(t *testing.T) {
		inner := validTask("ship")
		inner.OutputSchema = map[string]any{
			"type": "object",
			"$defs": map[string]any{
				"Tracking": map[string]any{"type": "object"},
			},
			"properties": map[string]any{
				"tracking": map[string]any{"$ref": "#/$defs/Tracking"},
			},
		}
		cond := &Step{
			Type: StepTypeConditional, ID: "check", Condition: "true",
			Then: []*Step{inner},
		}
		d := ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{cond}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defs, _ := inner.OutputSchema["$defs"].(map[string]any)
		if defs == nil || defs["Tracking"] == nil {
			t.Fatal("nested step $defs/Tracking missing after normalize")
		}
	})

	t.Run("invalid $ref in InputSchema returns error", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Version: 1, Steps: []*Step{validTask("s1")},
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/Missing"}},
			},
		}
		if err := d.Normalize(); err == nil {
			t.Fatal("expected error for unresolved $ref, got nil")
		}
	})

	t.Run("invalid $ref in step OutputSchema returns error with step ID", func(t *testing.T) {
		step := validTask("charge")
		step.OutputSchema = map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/Missing"}},
		}
		d := ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{step}}
		err := d.Normalize()
		if err == nil {
			t.Fatal("expected error for unresolved $ref, got nil")
		}
		if !containsStr(err.Error(), "charge") {
			t.Errorf("error %q should mention step ID %q", err.Error(), "charge")
		}
	})

	t.Run("unused $defs are removed from InputSchema", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Version: 1, Steps: []*Step{validTask("s1")},
			InputSchema: map[string]any{
				"type": "object",
				"$defs": map[string]any{
					"Used":   map[string]any{"type": "string"},
					"Unused": map[string]any{"type": "integer"},
				},
				"properties": map[string]any{
					"name": map[string]any{"$ref": "#/$defs/Used"},
				},
			},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defs, _ := d.InputSchema["$defs"].(map[string]any)
		if defs == nil || defs["Used"] == nil {
			t.Fatal("$defs/Used should be present")
		}
		if defs["Unused"] != nil {
			t.Fatal("$defs/Unused should have been removed as unused")
		}
	})
}

func TestProcessDefinition_Validate(t *testing.T) {
	validTask := &Step{
		Type:      StepTypeTask,
		ID:        "step1",
		Transport: TransportHTTP,
		Endpoint:  "http://localhost/action",
	}

	tests := []struct {
		name    string
		def     ProcessDefinition
		wantErr string
	}{
		{
			name:    "valid definition",
			def:     ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{validTask}},
			wantErr: "",
		},
		{
			name:    "missing name",
			def:     ProcessDefinition{Version: 1, Steps: []*Step{validTask}},
			wantErr: "name is required",
		},
		{
			name:    "version zero",
			def:     ProcessDefinition{Name: "p", Version: 0, Steps: []*Step{validTask}},
			wantErr: "version must have at least 1 item(s)",
		},
		{
			name:    "no steps",
			def:     ProcessDefinition{Name: "p", Version: 1},
			wantErr: "steps",
		},
		{
			name: "task missing endpoint",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{Type: StepTypeTask, ID: "s1", Transport: TransportHTTP},
			}},
			wantErr: "endpoint is required",
		},
		{
			name: "task unknown transport",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{Type: StepTypeTask, ID: "s1", Transport: "ftp", Endpoint: "ftp://x"},
			}},
			wantErr: "transport must be one of: http, tcp, uds",
		},
		{
			name: "conditional missing condition",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{Type: StepTypeConditional, ID: "c1"},
			}},
			wantErr: "condition is required",
		},
		{
			name: "unknown step type",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{Type: "parallel", ID: "p1"},
			}},
			wantErr: "type must be one of: task, conditional",
		},
		{
			name: "nested step invalid",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{
					Type: StepTypeConditional, ID: "c1", Condition: "context.ok == true",
					Then: []*Step{{Type: StepTypeTask, ID: "t1"}}, // missing transport+endpoint
				},
			}},
			wantErr: "endpoint is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.def.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !containsStr(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstr(s, sub))
}

func containsSubstr(s, sub string) bool {
	for i := range s {
		if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
