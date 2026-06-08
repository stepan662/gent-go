package validationtest

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
