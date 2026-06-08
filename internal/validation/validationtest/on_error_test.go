package validationtest

import (
	"testing"
)

// TestGenerate_OnError_MixedPath_FailingStepOutputNullable verifies that when a step
// is reachable via both the normal fallthrough and an on_error route, the failing
// step's output becomes nullable and the error context becomes nullable too.
func TestGenerate_OnError_MixedPath_FailingStepOutputNullable(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "start",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": {"ok": {"type": "boolean"}},
					"required": ["ok"]
				}},
				"on_error": [{"goto": "#finale"}]
			},
			{
				"id": "finale",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": {"val": "{{outputs.start.ok}}", "errCode": "{{error.code}}"}
			}
		]
	}`)
	finaleInput := out.Defs["finale_input"]
	if finaleInput == nil || finaleInput.Properties == nil {
		t.Fatal("finale input should have properties")
	}
	// On the normal path start produced output; on the error path it did not.
	assertJSON(t, finaleInput.Properties["val"], `{"type": ["boolean", "null"]}`)
	// error is only present on the error path, so it is nullable.
	assertJSON(t, finaleInput.Properties["errCode"], `{"type": ["string", "null"]}`)
}

// TestGenerate_OnError_ExclusivePath_ErrorRequiredOutputAbsent verifies that at a
// step reachable ONLY via on_error, the error context is required (non-nullable)
// and the failing step's output is not available at all.
func TestGenerate_OnError_ExclusivePath_ErrorRequiredOutputAbsent(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "worker",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": {"result": {"type": "string"}},
					"required": ["result"]
				}},
				"switch": [{"when": "default", "goto": "$end"}],
				"on_error": [{"goto": "#handler"}]
			},
			{
				"id": "handler",
				"call": {"type": "rest", "endpoint": "http://x"},
				"params": {"code": "{{error.code}}"}
			}
		]
	}`)
	handlerInput := out.Defs["handler_input"]
	if handlerInput == nil || handlerInput.Properties == nil {
		t.Fatal("handler input should have properties")
	}
	// Every path to handler is an error path, so error.code is always a string.
	assertJSON(t, handlerInput.Properties["code"], `{"type": "string"}`)
}

// TestGenerate_OnError_EndTerminal_RecognisedAsTerminal verifies that a step whose
// on_error routes to $end is counted as a terminal path by outputContextSets, so
// the process output can be inferred without error.
func TestGenerate_OnError_EndTerminal_RecognisedAsTerminal(t *testing.T) {
	// runGenerate fails the test on any Generate error, so a clean return is sufficient.
	runGenerate(t, `{
		"name": "p", "version": 1,
		"steps": [
			{
				"id": "step",
				"call": {"type": "rest", "endpoint": "http://x", "output_schema": {
					"type": "object",
					"properties": {"result": {"type": "string"}},
					"required": ["result"]
				}},
				"on_error": [{"goto": "$end"}]
			}
		],
		"output": {"result": "{{outputs.step.result}}"}
	}`)
}
