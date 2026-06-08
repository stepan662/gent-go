package validationtest

import (
	"strings"
	"testing"
)

func TestGenerate_Switch_SelfExpressionTypeChecked(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				}},
				"switch": [
					{"when": "{{self.charged == true}}", "goto": "#ship"},
					{"when": "{{self.charged == false}}", "goto": "#refund"}
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
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				}},
				"switch": [{"when": "{{outputs.charge.charged == true}}", "goto": "#notify"}]
			},
			{ "id": "notify", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
}

func TestGenerate_RecursiveStep_OwnOutputOptionalInParams(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
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
			"switch": [{"when": "{{!self.done}}", "goto": "#loop"}, {"when": "default", "goto": "$end"}]
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
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "decide",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } }, "required": ["ok"] }},
				"switch": [{"when": "{{self.ok}}", "goto": "#work"}, {"when": "default", "goto": "$end"}]
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
		"name": "p", "version": 1,
		"steps": [{
			"id": "check",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
				"type": "object",
				"properties": {
					"ok": { "oneOf": [{"type": "boolean"}, {"type": "boolean"}] }
				},
				"required": ["ok"]
			}},
			"switch": [{"when": "{{self.ok}}", "goto": "#next"}, {"when": "default", "goto": "$end"}]
		},
		{ "id": "next", "call": {"type": "rest", "endpoint": "http://x"} }]
	}`)
}

func TestGenerate_Switch_OneOfBooleanOptionalFieldRejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "oneOf": [{"type": "boolean"}] } }
		},
		"steps": [
			{
				"id": "route",
				"switch": [{"when": "{{input.go_then}}", "goto": "#next"}, {"when": "default", "goto": "$end"}]
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
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "type": "boolean" } }
		},
		"steps": [
			{
				"id": "route",
				"switch": [{"when": "{{input.go_then}}", "goto": "#work"}, {"when": "default", "goto": "$end"}]
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

func TestGenerate_Switch_MixedTemplateRejectsStringResult(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "check",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "ok": { "type": "boolean" } },
					"required": ["ok"]
				}},
				"switch": [{"when": "{{self.ok}}_", "goto": "#next"}, {"when": "default", "goto": "$end"}]
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
