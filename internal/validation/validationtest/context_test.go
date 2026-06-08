package validationtest

import (
	"testing"
)

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
	bInput := out.Defs["B_input"]
	if bInput == nil || bInput.Properties == nil {
		t.Fatal("B input should have properties")
	}
	assertJSON(t, bInput.Properties["flag"], `{"type": "boolean"}`)
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
	mergeInput := out.Defs["merge_input"]
	if mergeInput == nil || mergeInput.Properties == nil {
		t.Fatal("merge input should have properties")
	}
	assertJSON(t, mergeInput.Properties["s"], `{"type": ["number", "null"]}`)
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
	postInput := out.Defs["post_input"]
	if postInput == nil || postInput.Properties == nil {
		t.Fatal("post input should have properties")
	}
	assertJSON(t, postInput.Properties["pre_id"], `{"type": "integer"}`)
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
	workInput := out.Defs["work_input"]
	if workInput == nil || workInput.Properties == nil {
		t.Fatal("work input should have properties")
	}
	assertJSON(t, workInput.Properties["flag"], `{"type": "boolean"}`)
}

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
