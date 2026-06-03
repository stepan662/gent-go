package gentschema_test

import (
	"strings"
	"testing"
)

func TestGenerate_NoSchemas(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{"id":"s1","call":{"type":"rest","endpoint":"http://x"}}]
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
		"steps": [{"id":"s1","call":{"type":"rest","endpoint":"http://x"}}],
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
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } }
				}}
			},
			{ "id": "notify", "call": {"type": "rest", "endpoint": "http://x"} }
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
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }},
				"switch": [{"when": "{{self.charged == true}}", "goto": "#ship"}]
			},
			{
				"id": "ship",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "tracking": { "type": "string" } } }}
			},
			{
				"id": "refund",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "refunded": { "type": "boolean" } } }}
			}
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
	assertJSON(t, out.Tasks["ship"].Output, `{"$ref": "#/$defs/ship_output"}`)
	assertJSON(t, out.Tasks["refund"].Output, `{"$ref": "#/$defs/refund_output"}`)
}

func TestGenerate_InnerDefsPromotedToRoot(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{"id":"s1","call":{"type":"rest","endpoint":"http://x"}}],
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
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"$defs": { "Item": { "type": "string" } },
			"properties": { "x": { "$ref": "#/$defs/Item" } }
		},
		"steps": [{
			"id": "charge",
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
				"type": "object",
				"$defs": { "Item": { "type": "integer" } },
				"properties": { "y": { "$ref": "#/$defs/Item" } }
			}}
		}]
	}`)
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
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
				"type": "object",
				"$defs": {
					"Used":   { "type": "string" },
					"Unused": { "type": "integer" }
				},
				"properties": { "x": { "$ref": "#/$defs/Used" } }
			}}
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
	input, _ := out.Defs["charge_input"].(map[string]any)
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
	input, _ := out.Defs["check_fraud_input"].(map[string]any)
	props, _ := input["properties"].(map[string]any)
	assertJSON(t, props["result"], `{"type":["boolean","null"]}`)
}

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
	input, _ := out.Defs["loop_input"].(map[string]any)
	props, _ := input["properties"].(map[string]any)
	if props == nil {
		t.Fatal("loop input should have properties")
	}
	if props["task_index"] == nil {
		t.Error("task_index param should be inferred")
	}
	if props["tasks"] == nil {
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
	workInput, _ := out.Defs["work_input"].(map[string]any)
	props, _ := workInput["properties"].(map[string]any)
	if props == nil {
		t.Fatal("work input should have properties")
	}
	assertJSON(t, props["flag"], `{"type": "boolean"}`)
}

func TestGenerate_ContextSets_LinearChain_RequiredOutputNonNullable(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "A",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "ok": { "type": "boolean" } },
					"required": ["ok"]
				}}
			},
			{
				"id": "B",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": { "flag": "{{outputs.A.ok}}" }
			}
		]
	}`)
	assertJSON(t, out.Tasks["B"].Input, `{"$ref": "#/$defs/B_input"}`)
	bInput, _ := out.Defs["B_input"].(map[string]any)
	props, _ := bInput["properties"].(map[string]any)
	if props == nil {
		t.Fatal("B input should have properties")
	}
	assertJSON(t, props["flag"], `{"type": "boolean"}`)
}

func TestGenerate_ContextSets_ExclusiveBranch_SkippedStepOutputNullable(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"properties": { "take_fast": { "type": "boolean" } },
			"required": ["take_fast"]
		},
		"steps": [
			{
				"id": "gate",
				"switch": [{"when": "{{input.take_fast}}", "goto": "#fast"}, {"when": "default", "goto": "#slow"}]
			},
			{
				"id": "fast",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "speed": { "type": "number" } },
					"required": ["speed"]
				}}
			},
			{ "id": "slow", "call": {"type": "rest", "endpoint": "http://x"} },
			{
				"id": "merge",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": { "s": "{{outputs.fast.speed}}" }
			}
		]
	}`)
	assertJSON(t, out.Tasks["merge"].Input, `{"$ref": "#/$defs/merge_input"}`)
	mergeInput, _ := out.Defs["merge_input"].(map[string]any)
	props, _ := mergeInput["properties"].(map[string]any)
	if props == nil {
		t.Fatal("merge input should have properties")
	}
	assertJSON(t, props["s"], `{"type": ["number", "null"]}`)
}

func TestGenerate_ContextSets_PreBranchStepRequiredAtAllMergePoints(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "pre",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "id": { "type": "integer" } },
					"required": ["id"]
				}}
			},
			{
				"id": "gate",
				"switch": [{"when": "{{outputs.pre.id == 1}}", "goto": "#path_a"}, {"when": "default", "goto": "#path_b"}]
			},
			{ "id": "path_a", "call": {"type": "rest", "endpoint": "http://x"} },
			{ "id": "path_b", "call": {"type": "rest", "endpoint": "http://x"} },
			{
				"id": "post",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": { "pre_id": "{{outputs.pre.id}}" }
			}
		]
	}`)
	assertJSON(t, out.Tasks["post"].Input, `{"$ref": "#/$defs/post_input"}`)
	postInput, _ := out.Defs["post_input"].(map[string]any)
	props, _ := postInput["properties"].(map[string]any)
	if props == nil {
		t.Fatal("post input should have properties")
	}
	assertJSON(t, props["pre_id"], `{"type": "integer"}`)
}

func TestGenerate_ContextSets_DefaultEndSwitch_SuccessorRequiredNotOptional(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "decide",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "ok": { "type": "boolean" } },
					"required": ["ok"]
				}},
				"switch": [{"when": "{{self.ok}}", "goto": "#work"}, {"when": "default", "goto": "$end"}]
			},
			{
				"id": "work",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": { "flag": "{{outputs.decide.ok}}" }
			}
		]
	}`)
	assertJSON(t, out.Tasks["work"].Input, `{"$ref": "#/$defs/work_input"}`)
	workInput2, _ := out.Defs["work_input"].(map[string]any)
	props, _ := workInput2["properties"].(map[string]any)
	if props == nil {
		t.Fatal("work input should have properties")
	}
	assertJSON(t, props["flag"], `{"type": "boolean"}`)
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

