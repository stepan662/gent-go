package main_test

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
				"switch": {"{{self.charged == true}}": "#ship"}
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
	// input_schema has its own $defs/Address — after flattenNamedSchemas these
	// should be promoted to the root $defs with scoped names.
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
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
				"type": "object",
				"$defs": { "Item": { "type": "integer" } },
				"properties": { "y": { "$ref": "#/$defs/Item" } }
			}}
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
	// A switch-only step (no action) produces no output and should not appear
	// in the accumulated context for subsequent steps.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "charge",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }}
			},
			{
				"id": "route",
				"switch": {"{{outputs.charge.charged == true}}": "#ship"}
			},
			{
				"id": "ship",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "tracking": { "type": "string" } } }}
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
			"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }},
			"params": {
				"id":  "{{input.order_id}}",
				"sum": "{{input.amount}}"
			}
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
			"call": {"type": "rest", "endpoint": "http://x"},
			"params": { "uid": "{{input.user_id}}" }
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
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				}},
				"switch": {
					"{{self.charged == true}}": "#ship",
					"{{self.charged == false}}": "#refund"
				}
			},
			{ "id": "ship",   "call": {"type": "rest", "endpoint": "http://x"} },
			{ "id": "refund", "call": {"type": "rest", "endpoint": "http://x"} }
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
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } },
					"required": ["charged"]
				}},
				"switch": {"{{outputs.charge.charged == true}}": "#notify"}
			},
			{ "id": "notify", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
}

func TestGenerate_RecursiveStep_OwnOutputOptionalInParams(t *testing.T) {
	// A step that switches back to itself must be able to reference its own previous
	// output in params. The output is optional (nullable) because it doesn't exist
	// on the first iteration.
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
			"switch": { "{{!self.done}}": "#loop", "default": "$end" }
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

func TestGenerate_SwitchStep_NextStepNotReachableViaFallthrough(t *testing.T) {
	// A step with a switch does not fall through to the next step in the list.
	// The next step's required context must not include that step's output unless
	// it is also reachable from a non-switch predecessor.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "decide",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } }, "required": ["ok"] }},
				"switch": { "{{self.ok}}": "#work", "default": "$end" }
			},
			{
				"id": "work",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": { "type": "object", "properties": { "done": { "type": "boolean" } } }},
				"params": { "flag": "{{outputs.decide.ok}}" }
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

// ─── context-set / CFG tests ──────────────────────────────────────────────────
//
// These tests verify that computeContextSets correctly classifies step outputs as
// required (always present before a step runs) or optional (present only on some
// paths), and that param type inference reflects that distinction:
//   - required output → field access is non-nullable
//   - optional output → field access is nullable (anyOf / type array with null)

func TestGenerate_ContextSets_LinearChain_RequiredOutputNonNullable(t *testing.T) {
	// A always runs before B (no branching). B's param accesses a required field
	// of A's output; the inferred type must be non-nullable.
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
	props, _ := out.Tasks["B"].Input["properties"].(map[string]any)
	if props == nil {
		t.Fatal("B input should have properties")
	}
	// A is required before B → outputs.A is required → .ok is non-nullable boolean.
	assertJSON(t, props["flag"], `{"type": "boolean"}`)
}

func TestGenerate_ContextSets_ExclusiveBranch_SkippedStepOutputNullable(t *testing.T) {
	// gate(switch) → fast  (one path)
	//              → slow  (other path, default)
	// fast falls through to slow; slow falls through to merge.
	//
	// fast is only on the gate→fast→slow→merge path, not the gate→slow→merge path,
	// so its output is optional at merge. Accessing it in params must yield a
	// nullable type.
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
				"switch": { "{{input.take_fast}}": "#fast", "default": "#slow" }
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
	props, _ := out.Tasks["merge"].Input["properties"].(map[string]any)
	if props == nil {
		t.Fatal("merge input should have properties")
	}
	// fast is optional at merge → outputs.fast is nullable → .speed is nullable number.
	assertJSON(t, props["s"], `{"type": ["number", "null"]}`)
}

func TestGenerate_ContextSets_PreBranchStepRequiredAtAllMergePoints(t *testing.T) {
	// pre always runs before gate; gate branches to path_a or path_b (default);
	// path_a falls through to path_b; path_b falls through to post.
	//
	// Every path to post goes through pre, so pre's output must be required
	// (non-nullable) in post's params even though a branch intervenes.
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
				"switch": { "{{outputs.pre.id == 1}}": "#path_a", "default": "#path_b" }
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
	props, _ := out.Tasks["post"].Input["properties"].(map[string]any)
	if props == nil {
		t.Fatal("post input should have properties")
	}
	// pre is required on every path to post → outputs.pre.id is non-nullable integer.
	assertJSON(t, props["pre_id"], `{"type": "integer"}`)
}

func TestGenerate_ContextSets_DefaultEndSwitch_SuccessorRequiredNotOptional(t *testing.T) {
	// decide has switch: self.ok→work, default→$end.
	// work is reachable only from decide's explicit switch case.
	// Because every path to work goes through decide, decide's output is required
	// (not optional) at work — the $end branch does not create an edge to work.
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
				"switch": { "{{self.ok}}": "#work", "default": "$end" }
			},
			{
				"id": "work",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": { "flag": "{{outputs.decide.ok}}" }
			}
		]
	}`)
	props, _ := out.Tasks["work"].Input["properties"].(map[string]any)
	if props == nil {
		t.Fatal("work input should have properties")
	}
	// decide is required for work (only path: decide→work); output is non-nullable.
	assertJSON(t, props["flag"], `{"type": "boolean"}`)
}

func TestGenerate_Switch_OneOfAllBooleanAccepted(t *testing.T) {
	// A oneOf/anyOf schema where every variant is boolean is still boolean —
	// isType must recurse into variants rather than just checking the top-level key.
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
			"switch": { "{{self.ok}}": "#next", "default": "$end" }
		},
		{ "id": "next", "call": {"type": "rest", "endpoint": "http://x"} }]
	}`)
}

func TestGenerate_Switch_OneOfBooleanOptionalFieldRejected(t *testing.T) {
	// oneOf:[boolean] on a non-required field becomes nullable after withNull wrapping,
	// so it must be rejected even though the schema itself contains only boolean variants.
	err := runGenerateErr(t, `{
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "oneOf": [{"type": "boolean"}] } }
		},
		"steps": [
			{
				"id": "route",
				"switch": { "{{input.go_then}}": "#next", "default": "$end" }
			},
			{ "id": "next", "call": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
	if err == nil {
		t.Fatal("expected error for nullable oneOf boolean switch expression, got nil")
	}
}

func TestGenerate_Switch_NullableBooleanRejected(t *testing.T) {
	// input.go_then is not in "required", so its inferred type is boolean|null.
	// A nullable expression is not acceptable as a switch condition.
	err := runGenerateErr(t, `{
		"name": "p", "version": 1,
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "type": "boolean" } }
		},
		"steps": [
			{
				"id": "route",
				"switch": { "{{input.go_then}}": "#work", "default": "$end" }
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
	// A mixed template like "{{self.ok}}_" produces a string, not a boolean.
	// gentschema must reject it at schema-generation time.
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
				"switch": { "{{self.ok}}_": "#next", "default": "$end" }
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
