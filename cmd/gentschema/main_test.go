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
	assertJSON(t, out.ProcessInput, `{
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
	assertJSON(t, out.Tasks["charge"].Output, `{
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
	assertJSON(t, out.Tasks["ship"].Output, `{
		"type": "object",
		"properties": { "tracking": { "type": "string" } }
	}`)
	assertJSON(t, out.Tasks["refund"].Output, `{
		"type": "object",
		"properties": { "refunded": { "type": "boolean" } }
	}`)
	if _, ok := out.Tasks["check"]; ok {
		t.Error("conditional step should not appear in tasks")
	}
}

func TestGenerate_NormalizesRefs(t *testing.T) {
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
	assertJSON(t, out.ProcessInput, `{
		"type": "object",
		"$defs": {
			"Address": { "type": "object", "properties": { "street": { "type": "string" } } }
		},
		"properties": { "addr": { "$ref": "#/$defs/Address" } }
	}`)
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
	assertJSON(t, out.Tasks["charge"].Output, `{
		"type": "object",
		"$defs": { "Used": { "type": "string" } },
		"properties": { "x": { "$ref": "#/$defs/Used" } }
	}`)
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
			"input": { "type": "object", "properties": { "order_id": { "type": "integer" } } },
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
	// charge is first — no preceding tasks
	assertJSON(t, out.Tasks["charge"].Context, `{
		"type": "object",
		"properties": { "outputs": { "type": "object" } }
	}`)
	// notify sees charge's output
	assertJSON(t, out.Tasks["notify"].Context, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": {
					"charge": { "type": "object", "properties": { "charged": { "type": "boolean" } } }
				}
			}
		}
	}`)
}

func TestGenerate_Context_TaskWithNoOutputSkippedInContext(t *testing.T) {
	// "log" has no output_schema — it should not appear in notify's context outputs
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
	// After a conditional, the next task sees the union of both branches.
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
	// ship sees only charge before it
	assertJSON(t, out.Tasks["ship"].Context, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": {
					"charge": { "type": "object", "properties": { "charged": { "type": "boolean" } } }
				}
			}
		}
	}`)
	// notify sees charge + union of ship/refund
	assertJSON(t, out.Tasks["notify"].Context, `{
		"type": "object",
		"properties": {
			"outputs": {
				"type": "object",
				"properties": {
					"charge":  { "type": "object", "properties": { "charged":  { "type": "boolean" } } },
					"ship":    { "type": "object", "properties": { "tracking": { "type": "string"  } } },
					"refund":  { "type": "object", "properties": { "refunded": { "type": "boolean" } } }
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
