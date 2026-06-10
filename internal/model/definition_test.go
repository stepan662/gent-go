package model

import (
	"testing"

	"gent/internal/schema"
)

func TestProcessDefinition_Normalize(t *testing.T) {
	validStep := func(id string) *Step {
		return &Step{ID: id, Call: &Call{Type: CallTypeREST, Endpoint: "http://localhost/x"}}
	}

	t.Run("no schemas is a no-op", func(t *testing.T) {
		d := ProcessDefinition{Name: "p", Steps: []*Step{validStep("s1")}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("simple InputSchema without refs is unchanged", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Steps: []*Step{validStep("s1")},
			InputSchema: &schema.SchemaNode{
				Type:       schema.SchemaType{"object"},
				Properties: map[string]*schema.SchemaNode{"id": {Type: schema.SchemaType{"integer"}}},
			},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.InputSchema.Properties == nil {
			t.Fatal("properties missing after normalize")
		}
	})

	t.Run("InputSchema $defs are flattened to root", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Steps: []*Step{validStep("s1")},
			InputSchema: &schema.SchemaNode{
				Type: schema.SchemaType{"object"},
				Properties: map[string]*schema.SchemaNode{
					"addr": {Ref: "#/$defs/Address"},
				},
				Defs: map[string]*schema.SchemaNode{
					"Address": {
						Type: schema.SchemaType{"object"},
						Properties: map[string]*schema.SchemaNode{
							"street": {Type: schema.SchemaType{"string"}},
						},
					},
				},
			},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.InputSchema.Defs == nil || d.InputSchema.Defs["Address"] == nil {
			t.Fatal("$defs/Address missing after normalize")
		}
		if d.InputSchema.Properties == nil {
			t.Fatal("properties missing after normalize")
		}
	})

	t.Run("step call.output_schema $defs are flattened to root", func(t *testing.T) {
		step := validStep("charge")
		step.Call.OutputSchema = &schema.SchemaNode{
			Type: schema.SchemaType{"object"},
			Defs: map[string]*schema.SchemaNode{
				"Result": {
					Type:       schema.SchemaType{"object"},
					Properties: map[string]*schema.SchemaNode{"ok": {Type: schema.SchemaType{"boolean"}}},
				},
			},
			Properties: map[string]*schema.SchemaNode{
				"result": {Ref: "#/$defs/Result"},
			},
		}
		d := ProcessDefinition{Name: "p", Steps: []*Step{step}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step.Call.OutputSchema.Defs == nil || step.Call.OutputSchema.Defs["Result"] == nil {
			t.Fatal("$defs/Result missing in call.output_schema after normalize")
		}
	})

	t.Run("all step call.output_schemas are normalized", func(t *testing.T) {
		step1 := validStep("charge")
		step1.Call.OutputSchema = &schema.SchemaNode{
			Type: schema.SchemaType{"object"},
			Defs: map[string]*schema.SchemaNode{"Tracking": {Type: schema.SchemaType{"object"}}},
			Properties: map[string]*schema.SchemaNode{
				"tracking": {Ref: "#/$defs/Tracking"},
			},
		}
		step2 := validStep("notify")
		step2.Call.OutputSchema = &schema.SchemaNode{
			Type: schema.SchemaType{"object"},
			Defs: map[string]*schema.SchemaNode{"Result": {Type: schema.SchemaType{"object"}}},
			Properties: map[string]*schema.SchemaNode{
				"result": {Ref: "#/$defs/Result"},
			},
		}
		d := ProcessDefinition{Name: "p", Steps: []*Step{step1, step2}}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step1.Call.OutputSchema.Defs == nil || step1.Call.OutputSchema.Defs["Tracking"] == nil {
			t.Fatal("step1 $defs/Tracking missing after normalize")
		}
		if step2.Call.OutputSchema.Defs == nil || step2.Call.OutputSchema.Defs["Result"] == nil {
			t.Fatal("step2 $defs/Result missing after normalize")
		}
	})

	t.Run("invalid $ref in InputSchema returns error", func(t *testing.T) {
		d := ProcessDefinition{
			Name: "p", Steps: []*Step{validStep("s1")},
			InputSchema: &schema.SchemaNode{
				Type:       schema.SchemaType{"object"},
				Properties: map[string]*schema.SchemaNode{"x": {Ref: "#/$defs/Missing"}},
			},
		}
		if err := d.Normalize(); err == nil {
			t.Fatal("expected error for unresolved $ref, got nil")
		}
	})

	t.Run("invalid $ref in step call.output_schema returns error with step ID", func(t *testing.T) {
		step := validStep("charge")
		step.Call.OutputSchema = &schema.SchemaNode{
			Type:       schema.SchemaType{"object"},
			Properties: map[string]*schema.SchemaNode{"x": {Ref: "#/$defs/Missing"}},
		}
		d := ProcessDefinition{Name: "p", Steps: []*Step{step}}
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
			Name: "p", Steps: []*Step{validStep("s1")},
			InputSchema: &schema.SchemaNode{
				Type: schema.SchemaType{"object"},
				Defs: map[string]*schema.SchemaNode{
					"Used":   {Type: schema.SchemaType{"string"}},
					"Unused": {Type: schema.SchemaType{"integer"}},
				},
				Properties: map[string]*schema.SchemaNode{
					"name": {Ref: "#/$defs/Used"},
				},
			},
		}
		if err := d.Normalize(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d.InputSchema.Defs == nil || d.InputSchema.Defs["Used"] == nil {
			t.Fatal("$defs/Used should be present")
		}
		if d.InputSchema.Defs["Unused"] != nil {
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
			def:     ProcessDefinition{Name: "p", Steps: []*Step{restStep("step1", "http://localhost/action")}},
			wantErr: "",
		},
		{
			name: "valid script call step",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{ID: "run", Call: &Call{Type: CallTypeScript, Exec: "python3 foo.py"}},
			}},
			wantErr: "",
		},
		{
			name: "valid switch-only step",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{ID: "router", Switch: SwitchMap{
					{Case: "input.ok == true", Next: "act"},
					{Next: GotoEnd},
				}},
				restStep("act", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "valid step with both call and switch",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{Case: "self.ok == true", Next: "ship"},
						{Next: GotoEnd},
					},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: "",
		},
		{
			name:    "missing name",
			def:     ProcessDefinition{Steps: []*Step{restStep("step1", "http://x")}},
			wantErr: "name is required",
		},
		{
			name:    "no steps",
			def:     ProcessDefinition{Name: "p"},
			wantErr: "steps",
		},
		{
			name: "step with neither call nor switch",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{ID: "empty"},
			}},
			wantErr: "must have a call or a switch",
		},
		{
			name: "rest call missing endpoint",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{ID: "s1", Call: &Call{Type: CallTypeREST}},
			}},
			wantErr: "call.endpoint is required",
		},
		{
			name: "script call missing exec",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{ID: "s1", Call: &Call{Type: CallTypeScript}},
			}},
			wantErr: "call.exec is required",
		},
		{
			name: "unknown call type",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{ID: "s1", Call: &Call{Type: "ftp", Endpoint: "ftp://x"}},
			}},
			wantErr: "call.type must be one of: rest, script",
		},
		{
			name: "switch missing catch-all is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{Case: "self.ok == true", Next: "ship"},
					},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: `last case must be a catch-all`,
		},
		{
			name: "switch catch-all not last is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{Next: GotoEnd},
						{Case: "self.ok == true", Next: "ship"},
					},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: `catch-all at index 0 must be the last case`,
		},
		{
			name: "switch end in non-catch-all case is valid",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{Case: "self.error == true", Next: GotoEnd},
						{Next: "ship"},
					},
				},
				restStep("ship", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "switch next references unknown step",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					Switch: SwitchMap{
						{Case: "self.ok == true", Next: "nonexistent"},
						{Next: GotoEnd},
					},
				},
			}},
			wantErr: `next "nonexistent" is not a known step`,
		},

		// ── only_once: true static validation ───────────────────────────────

		{
			name: "only_once:true — retries on pre.% is valid",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"pre.%"}, Retries: 3},
					},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — retries on exact pre.* codes is valid",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"pre.error", "pre.timeout"}, Retries: 3},
					},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — retries:0 with http.% next is valid",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"http.%"}, Next: "handler"},
					},
				},
				restStep("handler", "http://x"),
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — not_reached:true overrides http.422 retry",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"http.422"}, NotReached: boolPtr(true), Retries: 2},
					},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — not_reached:true on catch-all retry is valid",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{NotReached: boolPtr(true), Retries: 2},
					},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once:true — retries on http.% is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"http.%"}, Retries: 3},
					},
				},
			}},
			wantErr: `pattern "http.%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — retries on exact http.500 is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"http.500"}, Retries: 1},
					},
				},
			}},
			wantErr: `pattern "http.500" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — retries on script.% is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "run", Call: &Call{Type: CallTypeScript, Exec: "echo ok"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"script.%"}, Retries: 1},
					},
				},
			}},
			wantErr: `pattern "script.%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — catch-all with retries is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Retries: 2},
					},
				},
			}},
			wantErr: "catch-all rule cannot have retries on an only_once step",
		},
		{
			name: "only_once:true — wildcard crossing namespaces is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"s%"}, Retries: 3},
					},
				},
			}},
			wantErr: `pattern "s%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:true — mixed pre and non-pre patterns in one rule is rejected",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(true),
					OnError: []ErrorCase{
						{Code: []string{"pre.%", "http.%"}, Retries: 1},
					},
				},
			}},
			wantErr: `pattern "http.%" can match errors where the call may have executed`,
		},
		{
			name: "only_once:false (explicit) — retries on http.% is valid",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnlyOnce: boolPtr(false),
					OnError: []ErrorCase{
						{Code: []string{"http.%"}, Retries: 3},
					},
				},
			}},
			wantErr: "",
		},
		{
			name: "only_once nil (default) — retries on http.% is valid",
			def: ProcessDefinition{Name: "p", Steps: []*Step{
				{
					ID: "charge", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"},
					OnError: []ErrorCase{
						{Code: []string{"http.%"}, Retries: 3},
					},
				},
			}},
			wantErr: "",
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
	def := ProcessDefinition{
		Name: "p",
		InputSchema: &schema.SchemaNode{
			Type:     schema.SchemaType{"object"},
			Required: []string{"id"},
			Properties: map[string]*schema.SchemaNode{
				"id":      {Type: schema.SchemaType{"integer"}},
				"comment": {Type: schema.SchemaType{"string", "null"}},
			},
		},
		Steps: []*Step{{ID: "s", Call: &Call{Type: CallTypeREST, Endpoint: "http://x"}}},
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
			OutputSchema: &schema.SchemaNode{
				Type:     schema.SchemaType{"object"},
				Required: []string{"charged"},
				Properties: map[string]*schema.SchemaNode{
					"charged": {Type: schema.SchemaType{"boolean"}},
					"receipt": {Type: schema.SchemaType{"string", "null"}},
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
		{Case: "self.paid == true", Next: "ship"},
		{Case: "self.paid == false", Next: "refund"},
	}

	data, err := original.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `[{"case":"self.paid == true","next":"$ship"},{"case":"self.paid == false","next":"$refund"}]`
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

func TestPatternOnlyMatchesPre(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		// exact pre.* codes
		{"pre.error", true},
		{"pre.timeout", true},
		{"pre.exec", true},
		{"pre.", true},
		// wildcards rooted at pre.
		{"pre.%", true},
		{"pre._%", true},
		{"pre._rror", true},
		// does not start with "pre." — no wildcard
		{"http.500", false},
		{"script.1", false},
		{"output.parse", false},
		{"child.failed", false},
		// wildcards not rooted at pre.
		{"%", false},
		{"http.%", false},
		{"script.%", false},
		// "pre" without dot: prefix is "pre", not "pre."
		{"pre%", false},
		// "p%" could match pre.* but also other codes
		{"p%", false},
		// empty string
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got := patternOnlyMatchesPre(tt.pattern)
			if got != tt.want {
				t.Errorf("patternOnlyMatchesPre(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

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
