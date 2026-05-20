package main_test

import (
	"strings"
	"testing"
)

func TestGenerate_NoSchemas(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{"type":"task","id":"s1","transport":"http","endpoint":"http://x"}]
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
		"steps": [{"type":"task","id":"s1","transport":"http","endpoint":"http://x"}],
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
				"type": "task", "id": "charge",
				"transport": "http", "endpoint": "http://x",
				"output_schema": {
					"type": "object",
					"properties": { "charged": { "type": "boolean" } }
				}
			},
			{ "type": "task", "id": "notify", "transport": "http", "endpoint": "http://x" }
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

func TestGenerate_NestedSteps(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{
			"type": "conditional", "id": "check", "condition": "true",
			"then": [{
				"type": "task", "id": "ship",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "tracking": { "type": "string" } } }
			}],
			"else": [{
				"type": "task", "id": "refund",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "refunded": { "type": "boolean" } } }
			}]
		}]
	}`)
	assertJSON(t, out.Tasks["ship"].Output, `{"$ref": "#/$defs/ship_output"}`)
	assertJSON(t, out.Defs["ship_output"], `{
		"type": "object",
		"properties": { "tracking": { "type": "string" } }
	}`)
	assertJSON(t, out.Tasks["refund"].Output, `{"$ref": "#/$defs/refund_output"}`)
	if _, ok := out.Tasks["check"]; ok {
		t.Error("conditional step should not appear in tasks")
	}
}

func TestGenerate_InnerDefsPromotedToRoot(t *testing.T) {
	// input_schema has its own $defs/Address — after flattenNamedSchemas these
	// should be promoted to the root $defs with scoped names.
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{"type":"task","id":"s1","transport":"http","endpoint":"http://x"}],
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
			"type": "task", "id": "charge",
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
			"type": "task", "id": "charge",
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

func TestGenerate_Context_FirstTaskNoInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [{
			"type": "task", "id": "charge",
			"transport": "http", "endpoint": "http://x",
			"output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }
		}]
	}`)
	assertJSON(t, out.Tasks["charge"].Context, `{
		"type": "object",
		"properties": {
			"outputs": { "type": "object" }
		}
	}`)
}

func TestGenerate_Context_WithProcessInput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"input_schema": { "type": "object", "properties": { "order_id": { "type": "integer" } } },
		"steps": [{
			"type": "task", "id": "charge",
			"transport": "http", "endpoint": "http://x",
			"output_schema": { "type": "object", "properties": { "ok": { "type": "boolean" } } }
		}]
	}`)
	assertJSON(t, out.Tasks["charge"].Context, `{
		"type": "object",
		"properties": {
			"input": { "$ref": "#/$defs/input" },
			"outputs": { "type": "object" }
		}
	}`)
}

func TestGenerate_Context_PrecedingTaskOutput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"type": "task", "id": "charge",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }
			},
			{
				"type": "task", "id": "notify",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }
			}
		]
	}`)
	assertJSON(t, out.Tasks["charge"].Context, `{
		"type": "object",
		"properties": { "outputs": { "type": "object" } }
	}`)
	assertJSON(t, out.Tasks["notify"].Context, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": {
					"charge": { "$ref": "#/$defs/charge_output" }
				}
			}
		}
	}`)
}

func TestGenerate_Context_TaskWithNoOutputSkippedInContext(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{ "type": "task", "id": "log", "transport": "http", "endpoint": "http://x" },
			{
				"type": "task", "id": "notify",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }
			}
		]
	}`)
	assertJSON(t, out.Tasks["notify"].Context, `{
		"type": "object",
		"properties": { "outputs": { "type": "object" } }
	}`)
}

func TestGenerate_Context_ConditionalBranchUnion(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"type": "task", "id": "charge",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "charged": { "type": "boolean" } } }
			},
			{
				"type": "conditional", "id": "check", "condition": "true",
				"then": [{
					"type": "task", "id": "ship",
					"transport": "http", "endpoint": "http://x",
					"output_schema": { "type": "object", "properties": { "tracking": { "type": "string" } } }
				}],
				"else": [{
					"type": "task", "id": "refund",
					"transport": "http", "endpoint": "http://x",
					"output_schema": { "type": "object", "properties": { "refunded": { "type": "boolean" } } }
				}]
			},
			{
				"type": "task", "id": "notify",
				"transport": "http", "endpoint": "http://x",
				"output_schema": { "type": "object", "properties": { "sent": { "type": "boolean" } } }
			}
		]
	}`)
	assertJSON(t, out.Tasks["ship"].Context, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": { "charge": { "$ref": "#/$defs/charge_output" } }
			}
		}
	}`)
	assertJSON(t, out.Tasks["notify"].Context, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": {
					"charge": { "$ref": "#/$defs/charge_output" },
					"ship":   { "$ref": "#/$defs/ship_output" },
					"refund": { "$ref": "#/$defs/refund_output" }
				}
			}
		}
	}`)
}

func TestGenerate_InvalidRef(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p", "version": 1,
		"steps": [{"type":"task","id":"s1","transport":"http","endpoint":"http://x"}],
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
