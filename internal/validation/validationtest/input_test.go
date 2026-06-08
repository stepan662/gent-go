package validationtest

import (
	"strings"
	"testing"
)

func TestGenerate_Input_FirstTaskNoInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{
			"id": "charge",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }}
		}]
	}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_WithProcessInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
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
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }}
			},
			{
				"id": "notify",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }}
			}
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_TaskWithNoOutputSkippedInContext(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{ "id": "log", "call": {"type": "rest", "endpoint": "http://x"} },
			{
				"id": "notify",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }}
			}
		]
	}`)
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_SwitchOnlyStepSkippedInContext(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }}
			},
			{
				"id": "route",
				"switch": [{"when": "{{outputs.charge.charged == true}}", "goto": "#ship"}]
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
		"name": "p", "version": 1,
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
		"name": "p", "version": 1,
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
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "save_order",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"oneOf": [
						{"type":"object","properties":{"valid":{"type":"boolean"}}},
						{"type":"string"}
					]
				}}
			},
			{
				"id": "check_fraud",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": { "result": "{{outputs.save_order.valid}}" }
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

func TestGenerate_InvalidRef(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p", "version": 1,
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
