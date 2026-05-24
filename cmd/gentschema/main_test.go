package main_test

import (
	"strings"
	"testing"
)

func TestGenerate_NoSchemas(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{"id":"s1","transport":"http","endpoint":"http://x"}]
	}`)
	if out.Process != "p" || out.Version != 1 {
		t.Errorf("metadata: got process=%q version=%d", out.Process, out.Version)
	}
	if out.ProcessInput != nil {
		t.Error("process_input should be absent")
	}
	if len(out.Tasks) != 0 {
		t.Errorf("tasks should be empty, got %v", out.Tasks)
	}
	if len(out.Defs) != 0 {
		t.Errorf("$defs should be empty, got %v", out.Defs)
	}
}

func TestGenerate_ProcessInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "order", "version": 2,
		"steps": [{"id":"s1","transport":"http","endpoint":"http://x"}],
		"input_schema": {
			"type": "object",
			"properties": { "order_id": { "type": "integer" } },
			"required": ["order_id"]
		}
	}`)
	assertJSON(t, out.ProcessInput, `{"$ref": "#/$defs/input"}`)
	assertJSON(t, out.Defs["input"], `{
		"type": "object",
		"properties": { "order_id": { "type": "integer" } },
		"required": ["order_id"]
	}`)
}

func TestGenerate_TaskOutput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"transport": "http", "endpoint": "http://x",
				"output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } }
				}
			},
			{ "id": "notify", "transport": "http", "endpoint": "http://x" }
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
	assertJSON(t, out.Defs["charge_output"], `{
		"type": "object",
		"properties": { "charged": { "type": "boolean" } }
	}`)
	if _, ok := out.Tasks["notify"]; ok {
		t.Error("notify has no schemas and should not appear in tasks")
	}
}

func TestGenerate_FlatStepsWithOutputs(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"transport": "http", "endpoint": "http://x",
				"switch": {"self.charged == true": "ship"},
				"output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }
			},
			{
				"id": "ship",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "tracking": { "type": "string" } } }
			},
			{
				"id": "refund",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "refunded": { "type": "boolean" } } }
			}
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
	assertJSON(t, out.Tasks["ship"].Output, `{"$ref": "#/$defs/ship_output"}`)
	assertJSON(t, out.Tasks["refund"].Output, `{"$ref": "#/$defs/refund_output"}`)
}

func TestGenerate_InnerDefsPromotedToRoot(t *testing.T) {
	// input_schema has its own $defs/Address — after flattenNamedSchemas these
	// should be promoted to the root $defs with scoped names.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{"id":"s1","transport":"http","endpoint":"http://x"}],
		"input_schema": {
			"type": "object",
			"$defs": {
				"Address": {
					"type": "object",
					"properties": { "street": { "type": "string" } }
				}
			},
			"properties": {
				"addr": { "$ref": "#/$defs/Address" }
			}
		}
	}`)
	assertJSON(t, out.ProcessInput, `{"$ref": "#/$defs/input"}`)
	// input schema at root $defs — no inner $defs, $ref rewritten to root
	assertJSON(t, out.Defs["input"], `{
		"type": "object",
		"properties": { "addr": { "$ref": "#/$defs/Address" } }
	}`)
	assertJSON(t, out.Defs["Address"], `{
		"type": "object",
		"properties": { "street": { "type": "string" } }
	}`)
}

func TestGenerate_InnerDefsConflictRenamed(t *testing.T) {
	// Both input_schema and charge output_schema have an inner $defs/Item.
	// After promotion both should be in root $defs under distinct names.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"$defs": { "Item": { "type": "string" } },
			"properties": { "x": { "$ref": "#/$defs/Item" } }
		},
		"steps": [{
			"id": "charge",
			"transport": "http", "endpoint": "http://x",
			"output_schema": {
				"type": "object",
				"$defs": { "Item": { "type": "integer" } },
				"properties": { "y": { "$ref": "#/$defs/Item" } }
			}
		}]
	}`)
	// Both "Item" defs must exist in root $defs under different names.
	var itemCount int
	for k := range out.Defs {
		if k == "Item" || strings.HasPrefix(k, "Item_") {
			itemCount++
		}
	}
	if itemCount != 2 {
		t.Errorf("expected 2 Item defs in $defs, found %d (keys: %v)", itemCount, defKeys(out))
	}
}

func TestGenerate_UnusedDefsRemoved(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{
			"id": "charge",
			"transport": "http", "endpoint": "http://x",
			"output_schema": {
				"type": "object",
				"$defs": {
					"Used":   { "type": "string" },
					"Unused": { "type": "integer" }
				},
				"properties": { "x": { "$ref": "#/$defs/Used" } }
			}
		}]
	}`)
	if out.Defs["Used"] == nil {
		t.Error("Used def should be present in $defs")
	}
	if out.Defs["Unused"] != nil {
		t.Error("Unused def should have been removed")
	}
}

func TestGenerate_Input_FirstTaskNoInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{
			"id": "charge",
			"transport": "http", "endpoint": "http://x",
			"output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }
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
			"transport": "http", "endpoint": "http://x",
			"output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }
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
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }
			},
			{
				"id": "notify",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }
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
			{ "id": "log", "transport": "http", "endpoint": "http://x" },
			{
				"id": "notify",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }
			}
		]
	}`)
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_SwitchOnlyStepSkippedInContext(t *testing.T) {
	// A switch-only step (no action) produces no output and should not appear
	// in the accumulated context for subsequent steps.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }
			},
			{
				"id": "route",
				"switch": {"outputs.charge.charged == true": "ship"}
			},
			{
				"id": "ship",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "tracking": { "type": "string" } } }
			}
		]
	}`)
	// ship should see charge's output but not any "route" output (switch-only steps have none)
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
			"transport": "http", "endpoint": "http://x",
			"params": {
				"id":  "input.order_id",
				"sum": "input.amount"
			},
			"output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }
		}]
	}`)
	input := out.Tasks["charge"].Input
	props, _ := input["properties"].(map[string]any)
	if input["type"] != "object" {
		t.Errorf("input type: got %v, want object", input["type"])
	}
	assertJSON(t, props["id"], `{"type": "integer"}`)
	assertJSON(t, props["sum"], `{"type": "number"}`)
}

func TestGenerate_Input_ParamsOnlyTask(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"input_schema": { "type": "object", "properties": { "user_id": { "type": "string" } }, "required": ["user_id"] },
		"steps": [{
			"id": "log",
			"transport": "http", "endpoint": "http://x",
			"params": { "uid": "input.user_id" }
		}]
	}`)
	if _, ok := out.Tasks["log"]; !ok {
		t.Fatal("task with params but no output_schema should appear in tasks")
	}
	assertJSON(t, out.Tasks["log"].Input, `{
		"type": "object",
		"properties": { "uid": { "type": "string" } },
		"required": ["uid"]
	}`)
}

func TestGenerate_Input_Params_OneOfOutputPropertyAccess(t *testing.T) {
	// save_order has a oneOf output (object|string). check_fraud accesses
	// outputs.save_order.valid — the inferred type must be boolean|null, not
	// the full oneOf[object,string].
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "save_order",
				"transport": "http", "endpoint": "http://x",
				"output_schema": {
					"oneOf": [
						{"type":"object","properties":{"valid":{"type":"boolean"}}},
						{"type":"string"}
					]
				}
			},
			{
				"id": "check_fraud",
				"transport": "http", "endpoint": "http://x",
				"params": { "result": "outputs.save_order.valid" }
			}
		]
	}`)
	input := out.Tasks["check_fraud"].Input
	props, _ := input["properties"].(map[string]any)
	assertJSON(t, props["result"], `{"type":["boolean","null"]}`)
}

func TestGenerate_Switch_SelfExpressionTypeChecked(t *testing.T) {
	// Switch expressions with "self" should be type-checked against the step's
	// own OutputSchema.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"transport": "http", "endpoint": "http://x",
				"output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				},
				"switch": {
					"self.charged == true": "ship",
					"self.charged == false": "refund"
				}
			},
			{ "id": "ship",   "transport": "http", "endpoint": "http://x" },
			{ "id": "refund", "transport": "http", "endpoint": "http://x" }
		]
	}`)
	// All steps present; no error means type inference succeeded
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
}

func TestGenerate_Switch_OutputsExpressionTypeChecked(t *testing.T) {
	// Switch expressions can also reference prior outputs without "self".
	runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"transport": "http", "endpoint": "http://x",
				"output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				},
				"switch": {"outputs.charge.charged == true": "notify"}
			},
			{ "id": "notify", "transport": "http", "endpoint": "http://x" }
		]
	}`)
}

func TestGenerate_RecursiveStep_OwnOutputOptionalInParams(t *testing.T) {
	// A step that switches back to itself (final loop) must be able to reference
	// its own previous output in params. The output is optional (nullable) because
	// it doesn't exist on the first iteration.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"properties": { "tasks": { "type": "array", "items": { "type": "string" } } },
			"required": ["tasks"]
		},
		"steps": [{
			"id": "loop",
			"transport": "http", "endpoint": "http://x",
			"params": {
				"tasks": "input.tasks",
				"task_index": "outputs.loop.finished_index ? outputs.loop.finished_index : 0"
			},
			"output_schema": {
				"type": "object",
				"properties": {
					"finished_index": { "type": "number" },
					"done": { "type": "boolean" }
				},
				"required": ["finished_index", "done"]
			},
			"switch": { "!self.done": "loop" },
			"final": true
		}]
	}`)
	input := out.Tasks["loop"].Input
	props, _ := input["properties"].(map[string]any)
	if props == nil {
		t.Fatal("loop input should have properties")
	}
	// task_index is a conditional: outputs.loop.finished_index (nullable number) or 0 (integer)
	if props["task_index"] == nil {
		t.Error("task_index param should be inferred")
	}
	// tasks comes from required input field → non-nullable array
	if props["tasks"] == nil {
		t.Error("tasks param should be inferred")
	}
}

func TestGenerate_FinalStep_NextStepNotReachableViaFallthrough(t *testing.T) {
	// A step with final:true does not fall through to the next step in the list.
	// The next step's required context must not include the final step's output
	// unless it is also reachable from a non-final predecessor.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "decide",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } }, "required": ["ok"] },
				"switch": { "self.ok": "work" },
				"final": true
			},
			{
				"id": "work",
				"transport": "http", "endpoint": "http://x",
				"params": { "flag": "outputs.decide.ok" },
				"output_schema": { "type": "object", "properties": { "done": { "type": "boolean" } } }
			}
		]
	}`)
	// work is only reachable via decide's switch, so decide's output is required for work.
	input := out.Tasks["work"].Input
	props, _ := input["properties"].(map[string]any)
	if props == nil {
		t.Fatal("work input should have properties")
	}
	assertJSON(t, props["flag"], `{"type": "boolean"}`)
}

func TestGenerate_InvalidRef(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p", "version": 1,
		"steps": [{"id":"s1","transport":"http","endpoint":"http://x"}],
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
