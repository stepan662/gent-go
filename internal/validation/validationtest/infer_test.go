package validationtest

import (
	"strings"
	"testing"
)

func TestGenerate_Input_FirstTaskNoInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"steps": [{
			"id": "charge",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }}
		}]
	}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_WithProcessInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": { "type": "object", "properties": { "order_id": { "type": "integer" } } },
		"steps": [{
			"id": "charge",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }}
		}]
	}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_PrecedingTaskOutput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }},
				"switch": "next"
			},
			{
				"id": "notify",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }},
				"switch": "end"
			}
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_TaskWithNoOutputSkippedInContext(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"steps": [
			{ "id": "log", "call": {"type": "rest", "endpoint": "http://x"}, "switch": "next" },
			{
				"id": "notify",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }},
				"switch": "end"
			}
		]
	}`)
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_SwitchOnlyStepSkippedInContext(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }},
				"switch": "next"
			},
			{
				"id": "route",
				"switch": [{"case": "outputs.charge.charged == true", "goto": "$ship"}, {"goto": "end"}]
			},
			{
				"id": "ship",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "tracking": { "type": "string" } } }}
			}
		]
	}`)
	if _, ok := out.Tasks["route"]; ok {
		t.Error("switch-only step should not appear in tasks")
	}
	if out.Tasks["ship"].Output == nil {
		t.Error("ship should have an output schema")
	}
}

func TestGenerate_Input_Params(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"properties": {
				"order_id": { "type": "integer" },
				"amount":   { "type": "number" }
			},
			"required": ["order_id", "amount"]
		},
		"steps": [{
			"id": "charge",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }},
			"params": {
				"id":  "{{input.order_id}}",
				"sum": "{{input.amount}}"
			}
		}]
	}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"$ref": "#/$defs/charge_input"}`)
	input := out.Defs["charge_input"]
	if input == nil {
		t.Fatal("charge_input not found in defs")
	}
	if !input.Type.Contains("object") {
		t.Errorf("input type: got %v, want object", input.Type)
	}
	assertJSON(t, input.Properties["id"], `{"type": "integer"}`)
	assertJSON(t, input.Properties["sum"], `{"type": "number"}`)
}

func TestGenerate_Input_ParamsOnlyTask(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": { "type": "object", "properties": { "user_id": { "type": "string" } }, "required": ["user_id"] },
		"steps": [{
			"id": "log",
			"call": {"type": "rest", "endpoint": "http://x"},
			"params": { "uid": "{{input.user_id}}" }
		}]
	}`)
	if _, ok := out.Tasks["log"]; !ok {
		t.Fatal("task with params but no output_schema should appear in tasks")
	}
	assertJSON(t, out.Tasks["log"].Input, `{"$ref": "#/$defs/log_input"}`)
	assertJSON(t, out.Defs["log_input"], `{
		"type": "object",
		"properties": { "uid": { "type": "string" } },
		"required": ["uid"]
	}`)
}

func TestGenerate_Input_Params_OneOfOutputPropertyAccess(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"steps": [
			{
				"id": "save_order",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"oneOf": [
						{"type":"object","properties":{"valid":{"type":"boolean"}}},
						{"type":"string"}
					]
				}},
				"switch": "next"
			},
			{
				"id": "check_fraud",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": { "result": "{{outputs.save_order.valid}}" },
				"switch": "end"
			}
		]
	}`)
	assertJSON(t, out.Tasks["check_fraud"].Input, `{"$ref": "#/$defs/check_fraud_input"}`)
	cfInput := out.Defs["check_fraud_input"]
	if cfInput == nil {
		t.Fatal("check_fraud_input not found in defs")
	}
	assertJSON(t, cfInput.Properties["result"], `{"type":["boolean","null"]}`)
}

func TestGenerate_MixedTemplate_NullableExpressionRejected(t *testing.T) {
	// error is nullable on finale (reachable via both normal and on_error paths).
	// Using it in a mixed template would silently produce "null_null" at runtime.
	err := runGenerateErr(t, `{
		"name": "p",
		"steps": [
			{
				"id": "start",
				"call": {"type": "rest", "endpoint": "http://x"},
				"on_error": [{"goto": "$finale"}],
				"switch": "next"
			},
			{
				"id": "finale",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": {"msg": "{{error.code}}_{{error.message}}"},
				"switch": "end"
			}
		]
	}`)
	if err == nil {
		t.Fatal("expected error for nullable expression in mixed template, got nil")
	}
	if !strings.Contains(err.Error(), "??") {
		t.Errorf("error should mention ?? operator, got: %v", err)
	}
}

func TestGenerate_MixedTemplate_NonNullableExpressionAccepted(t *testing.T) {
	// error is required (exclusive error path), so using it in a mixed template is fine.
	runGenerate(t, `{
		"name": "p",
		"steps": [
			{
				"id": "worker",
				"call": {"type": "rest", "endpoint": "http://x"},
				"switch": "end",
				"on_error": [{"goto": "$handler"}]
			},
			{
				"id": "handler",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": {"msg": "{{error.code}}_{{error.message}}"},
				"switch": "end"
			}
		]
	}`)
}

func TestGenerate_InvalidRef(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"steps": [{"id":"s1","call":{"type":"rest","endpoint":"http://x"}}],
		"input_schema": {
			"properties": { "x": { "$ref": "#/$defs/Missing" } }
		}
	}`)
	if err == nil {
		t.Fatal("expected error for unresolved $ref, got nil")
	}
	if !strings.Contains(err.Error(), "Missing") {
		t.Errorf("error should mention the missing ref, got: %v", err)
	}
}

func TestGenerate_Switch_SelfExpressionTypeChecked(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				}},
				"switch": [
					{"case": "self.charged == true", "goto": "$ship"},
					{"case": "self.charged == false", "goto": "$refund"}
				]
			},
			{ "id": "ship",   "call": {"type": "rest", "endpoint": "http://x"} },
			{ "id": "refund", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
}

func TestGenerate_Switch_OutputsExpressionTypeChecked(t *testing.T) {
	runGenerate(t, `{
		"name": "p",
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				}},
				"switch": [{"case": "outputs.charge.charged == true", "goto": "$notify"}]
			},
			{ "id": "notify", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
}

func TestGenerate_RecursiveStep_OwnOutputOptionalInParams(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"properties": { "tasks": { "type": "array", "items": { "type": "string" } } },
			"required": ["tasks"]
		},
		"steps": [{
			"id": "loop",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
				"type": "object",
				"properties": {
					"finished_index": { "type": "number" },
					"done": { "type": "boolean" }
				},
				"required": ["finished_index", "done"]
			}},
			"params": {
				"tasks": "{{input.tasks}}",
				"task_index": "{{outputs.loop.finished_index ? outputs.loop.finished_index : 0}}"
			},
			"switch": [{"case": "!self.done", "goto": "$loop"}, {"goto": "end"}]
		}]
	}`)
	assertJSON(t, out.Tasks["loop"].Input, `{"$ref": "#/$defs/loop_input"}`)
	loopInput := out.Defs["loop_input"]
	if loopInput == nil || loopInput.Properties == nil {
		t.Fatal("loop input should have properties")
	}
	if loopInput.Properties["task_index"] == nil {
		t.Error("task_index param should be inferred")
	}
	if loopInput.Properties["tasks"] == nil {
		t.Error("tasks param should be inferred")
	}
}

func TestGenerate_SwitchStep_NextStepNotReachableViaFallthrough(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"steps": [
			{
				"id": "decide",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } }, "required": ["ok"] }},
				"switch": [{"case": "self.ok", "goto": "$work"}, {"goto": "end"}]
			},
			{
				"id": "work",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "done": { "type": "boolean" } } }},
				"params": { "flag": "{{outputs.decide.ok}}" }
			}
		]
	}`)
	assertJSON(t, out.Tasks["work"].Input, `{"$ref": "#/$defs/work_input"}`)
	workInput := out.Defs["work_input"]
	if workInput == nil || workInput.Properties == nil {
		t.Fatal("work input should have properties")
	}
	assertJSON(t, workInput.Properties["flag"], `{"type": "boolean"}`)
}

func TestGenerate_Switch_OneOfAllBooleanAccepted(t *testing.T) {
	runGenerate(t, `{
		"name": "p",
		"steps": [{
			"id": "check",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
				"type": "object",
				"properties": {
					"ok": { "oneOf": [{"type": "boolean"}, {"type": "boolean"}] }
				},
				"required": ["ok"]
			}},
			"switch": [{"case": "self.ok", "goto": "$next"}, {"goto": "end"}]
		},
		{ "id": "next", "call": {"type": "rest", "endpoint": "http://x"} }]
	}`)
}

func TestGenerate_Switch_OneOfBooleanOptionalFieldRejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "oneOf": [{"type": "boolean"}] } }
		},
		"steps": [
			{
				"id": "route",
				"switch": [{"case": "input.go_then", "goto": "$next"}, {"goto": "end"}]
			},
			{ "id": "next", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
	if err == nil {
		t.Fatal("expected error for nullable oneOf boolean switch expression, got nil")
	}
}

func TestGenerate_Switch_NullableBooleanRejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "type": "boolean" } }
		},
		"steps": [
			{
				"id": "route",
				"switch": [{"case": "input.go_then", "goto": "$work"}, {"goto": "end"}]
			},
			{ "id": "work", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
	if err == nil {
		t.Fatal("expected error for nullable boolean switch expression, got nil")
	}
	if !strings.Contains(err.Error(), "boolean") {
		t.Errorf("error should mention expected type, got: %v", err)
	}
}

func TestGenerate_Switch_StringExpressionRejectsNonBoolean(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"steps": [
			{
				"id": "check",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "label": { "type": "string" } },
					"required": ["label"]
				}},
				"switch": [{"case": "self.label", "goto": "$next"}, {"goto": "end"}]
			},
			{ "id": "next", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
	if err == nil {
		t.Fatal("expected error for non-boolean switch expression, got nil")
	}
	if !strings.Contains(err.Error(), "boolean") {
		t.Errorf("error should mention expected type, got: %v", err)
	}
}
