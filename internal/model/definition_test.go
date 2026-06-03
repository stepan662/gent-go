package model

import (
	"testing"
)

func TestProcessDefinition_Normalize(t *testing.T) {
	validStep := func(id string) *Step {
		return &Step{ID: id, Call: &Call{Type: CallTypeREST, Endpoint: "http://localhost/x"}}
	}

	t.Run("no schemas is a no-op", func(t *testing.T) {
		d := ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{validStep("s1")}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("simple InputSchema without refs is unchanged", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Version: 1, Steps: []*Step{validStep("s1")},
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
			Name: "p", Version: 1, Steps: []*Step{validStep("s1")},
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

	t.Run("step call.output_schema $defs are flattened to root", func(t *testing.T) {
		step := validStep("charge")
		step.Call.OutputSchema = map[string]any{
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
		defs, _ := step.Call.OutputSchema["$defs"].(map[string]any)
		if defs == nil || defs["Result"] == nil {
			t.Fatal("$defs/Result missing in call.output_schema after normalize")
		}
	})

	t.Run("all step call.output_schemas are normalized", func(t *testing.T) {
		step1 := validStep("charge")
		step1.Call.OutputSchema = map[string]any{
			"type": "object",
			"$defs": map[string]any{
				"Tracking": map[string]any{"type": "object"},
			},
			"properties": map[string]any{
				"tracking": map[string]any{"$ref": "#/$defs/Tracking"},
			},
		}
		step2 := validStep("notify")
		step2.Call.OutputSchema = map[string]any{
			"type": "object",
			"$defs": map[string]any{
				"Result": map[string]any{"type": "object"},
			},
			"properties": map[string]any{
				"result": map[string]any{"$ref": "#/$defs/Result"},
			},
		}
		d := ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{step1, step2}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defs1, _ := step1.Call.OutputSchema["$defs"].(map[string]any)
		if defs1 == nil || defs1["Tracking"] == nil {
			t.Fatal("step1 $defs/Tracking missing after normalize")
		}
		defs2, _ := step2.Call.OutputSchema["$defs"].(map[string]any)
		if defs2 == nil || defs2["Result"] == nil {
			t.Fatal("step2 $defs/Result missing after normalize")
		}
	})

	t.Run("invalid $ref in InputSchema returns error", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Version: 1, Steps: []*Step{validStep("s1")},
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/Missing"}},
			},
		}
		if err := d.Normalize(); err == nil {
			t.Fatal("expected error for unresolved $ref, got nil")
		}
	})

	t.Run("invalid $ref in step call.output_schema returns error with step ID", func(t *testing.T) {
		step := validStep("charge")
		step.Call.OutputSchema = map[string]any{
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
			Name: "p", Version: 1, Steps: []*Step{validStep("s1")},
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
	restStep := func(id, endpoint string) *Step {
		return &Step{ID: id, Call: &Call{Type: CallTypeREST, Endpoint: endpoint}}
	}

	tests := []struct {
		name    string
		def     ProcessDefinition
		wantErr string
	}{
		{
			name:    "valid rest call step",
			def:     ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{restStep("step1", "http://localhost/action")}},
			wantErr: "",
		},
		{
			name: "valid script call step",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{ID: "run", Call: &Call{Type: CallTypeScript, Exec: "python3 foo.py"}},
			}},
			wantErr: "",
		},
		{
			name: "valid switch-only step",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{ID: "router", Switch: SwitchMap{
					{When: "input.ok == true", Goto: "act"},
					{When: "default", Goto: GotoEnd},
				}},
				restStep("act", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "valid step with both call and switch",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{When: "self.ok == true", Goto: "ship"},
						{When: "default", Goto: GotoEnd},
					},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: "",
		},
		{
			name:    "missing name",
			def:     ProcessDefinition{Version: 1, Steps: []*Step{restStep("step1", "http://x")}},
			wantErr: "name is required",
		},
		{
			name:    "version zero",
			def:     ProcessDefinition{Name: "p", Version: 0, Steps: []*Step{restStep("step1", "http://x")}},
			wantErr: "version must have at least 1 item(s)",
		},
		{
			name:    "no steps",
			def:     ProcessDefinition{Name: "p", Version: 1},
			wantErr: "steps",
		},
		{
			name: "step with neither call nor switch",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{ID: "empty"},
			}},
			wantErr: "must have a call or a switch",
		},
		{
			name: "rest call missing endpoint",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{ID: "s1", Call: &Call{Type: CallTypeREST}},
			}},
			wantErr: "call.endpoint is required",
		},
		{
			name: "script call missing exec",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{ID: "s1", Call: &Call{Type: CallTypeScript}},
			}},
			wantErr: "call.exec is required",
		},
		{
			name: "unknown call type",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{ID: "s1", Call: &Call{Type: "ftp", Endpoint: "ftp://x"}},
			}},
			wantErr: "call.type must be one of: rest, script",
		},
		{
			name: "switch missing default case",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{{When: "self.ok == true", Goto: "ship"}},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: `last case must be "default"`,
		},
		{
			name: "switch default is not the last case",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{When: "default", Goto: GotoEnd},
						{When: "self.ok == true", Goto: "ship"},
					},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: `last case must be "default"`,
		},
		{
			name: "switch $end in non-default case is valid",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{When: "self.error == true", Goto: GotoEnd},
						{When: "default", Goto: "ship"},
					},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "switch goto references unknown step",
			def: ProcessDefinition{Name: "p", Version: 1, Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{When: "self.ok == true", Goto: "nonexistent"},
						{When: "default", Goto: GotoEnd},
					},
				},
			}},
			wantErr: `goto "nonexistent" is not a known step`,
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

func TestProcessDefinition_ValidateInput_Nullable(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"id"},
		"properties": map[string]any{
			"id":      map[string]any{"type": "integer"},
			"comment": map[string]any{"type": []any{"string", "null"}},
		},
	}
	def := ProcessDefinition{
		Name: "p", Version: 1,
		InputSchema: schema,
		Steps:       []*Step{{ID: "s", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"}}},
	}

	tests := []struct {
		name    string
		input   any
		wantErr bool
	}{
		{
			name:  "required field present, nullable field absent",
			input: map[string]any{"id": 1},
		},
		{
			name:  "required field present, nullable field is null",
			input: map[string]any{"id": 1, "comment": nil},
		},
		{
			name:  "required field present, nullable field has value",
			input: map[string]any{"id": 1, "comment": "hello"},
		},
		{
			name:    "required field missing",
			input:   map[string]any{"comment": "hello"},
			wantErr: true,
		},
		{
			name:    "required field is null (non-nullable)",
			input:   map[string]any{"id": nil},
			wantErr: true,
		},
		{
			name:    "nullable field has wrong type",
			input:   map[string]any{"id": 1, "comment": 42},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := def.ValidateInput(tt.input)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestStep_ValidateOutput_Nullable(t *testing.T) {
	step := &Step{
		ID: "charge",
		Call: &Call{
			Type:     CallTypeREST,
			Endpoint: "http://x",
			OutputSchema: map[string]any{
				"type":     "object",
				"required": []any{"charged"},
				"properties": map[string]any{
					"charged": map[string]any{"type": "boolean"},
					"receipt": map[string]any{"type": []any{"string", "null"}},
				},
			},
		},
	}

	tests := []struct {
		name    string
		output  map[string]any
		wantErr bool
	}{
		{
			name:   "required field present, nullable field absent",
			output: map[string]any{"charged": true},
		},
		{
			name:   "required field present, nullable field is null",
			output: map[string]any{"charged": true, "receipt": nil},
		},
		{
			name:   "required field present, nullable field has value",
			output: map[string]any{"charged": true, "receipt": "REC-001"},
		},
		{
			name:    "required field missing",
			output:  map[string]any{"receipt": "REC-001"},
			wantErr: true,
		},
		{
			name:    "required field is null (non-nullable)",
			output:  map[string]any{"charged": nil},
			wantErr: true,
		},
		{
			name:    "nullable field has wrong type",
			output:  map[string]any{"charged": true, "receipt": 123},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := step.Call.ValidateOutput(tt.output)
			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestSwitchMap_MarshalUnmarshal(t *testing.T) {
	original := SwitchMap{
		{When: "self.paid == true", Goto: "ship"},
		{When: "self.paid == false", Goto: "refund"},
	}

	data, err := original.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `[{"when":"self.paid == true","goto":"#ship"},{"when":"self.paid == false","goto":"#refund"}]`
	if string(data) != want {
		t.Errorf("marshal: got %s, want %s", data, want)
	}

	var decoded SwitchMap
	if err := decoded.UnmarshalJSON(data); err != nil {
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
