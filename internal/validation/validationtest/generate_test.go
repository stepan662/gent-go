package validationtest

import (
	"testing"
)

func TestGenerate_NoSchemas(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [{"id":"s1","action":{"type":"rest","endpoint":"http://x"}}]
	}`)
	if out.Process != "p" {
		t.Errorf("metadata: got process=%q", out.Process)
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
		"name": "order",
		"tasks": [{"id":"s1","action":{"type":"rest","endpoint":"http://x"}}],
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
  "name": "p",
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "charged": {
              "type": "boolean"
            }
          }
        }
      },
      "switch": "next",
      "output": "{{ self.result }}"
    },
    {
      "id": "notify",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      },
      "switch": "end"
    }
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
  "name": "p",
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "charged": {
              "type": "boolean"
            }
          }
        }
      },
      "switch": [
        {
          "case": "self.output.charged == true",
          "goto": "$ship"
        },
        {
          "goto": "$refund"
        }
      ],
      "output": "{{ self.result }}"
    },
    {
      "id": "ship",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "tracking": {
              "type": "string"
            }
          }
        }
      },
      "switch": "end",
      "output": "{{ self.result }}"
    },
    {
      "id": "refund",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "refunded": {
              "type": "boolean"
            }
          }
        }
      },
      "switch": "end",
      "output": "{{ self.result }}"
    }
  ]
}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
	assertJSON(t, out.Tasks["ship"].Output, `{"$ref": "#/$defs/ship_output"}`)
	assertJSON(t, out.Tasks["refund"].Output, `{"$ref": "#/$defs/refund_output"}`)
}

func TestGenerate_InnerDefsPromotedToRoot(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [{"id":"s1","action":{"type":"rest","endpoint":"http://x"}}],
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
	// Two distinct, same-named recursive $defs (one in input_schema, one inferred
	// from a task output) must be uniquified into two root defs. Recursive defs
	// survive normalization (they cannot be inlined), so they reach the conflict.
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "$defs": {
      "Item": {
        "type": "object",
        "properties": { "a": { "type": "string" }, "next": { "$ref": "#/$defs/Item" } },
        "required": ["a"]
      }
    },
    "properties": {
      "x": {
        "$ref": "#/$defs/Item"
      }
    },
    "required": ["x"]
  },
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "$defs": {
            "Item": {
              "type": "object",
              "properties": { "b": { "type": "integer" }, "next": { "$ref": "#/$defs/Item" } },
              "required": ["b"]
            }
          },
          "properties": {
            "y": {
              "$ref": "#/$defs/Item"
            }
          },
          "required": ["y"]
        }
      },
      "output": "{{ self.result }}"
    }
  ]
}`)
	// The two distinct recursive defs coexist: input's keeps the name "Item", the
	// task's is carried under its output def name "charge_output" — no clobbering.
	if out.Defs["Item"] == nil {
		t.Errorf("input's recursive Item def should be present (keys: %v)", defKeys(out))
	}
	if out.Defs["charge_output"] == nil {
		t.Errorf("charge's recursive output def should be present and distinct (keys: %v)", defKeys(out))
	}
}

func TestGenerate_Child_WithOutputSchema_ExposesTypedOutput(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child",
        "name": "worker",
        "result_schema": {
          "type": "object",
          "properties": {
            "count": {
              "type": "integer"
            }
          },
          "required": [
            "count"
          ]
        }
      },
      "switch": "end",
      "output": "{{ self.result }}"
    }
  ]
}`)
	// spawn should appear in tasks with a typed output
	if out.Tasks["spawn"].Output == nil {
		t.Fatal("spawn should have a typed output in tasks")
	}
	assertJSON(t, out.Defs["spawn_output"], `{
		"type": "object",
		"properties": { "count": { "type": "integer" } },
		"required": ["count"]
	}`)
}

func TestGenerate_Child_WithoutOutputSchema_NoOutput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [{
			"id": "spawn",
			"action": { "type": "child", "name": "worker" },
			"switch": "end"
		}]
	}`)
	if _, ok := out.Tasks["spawn"]; ok {
		t.Error("spawn without result_schema should not appear in tasks")
	}
	if out.Defs["spawn_output"] != nil {
		t.Error("spawn_output def should be absent")
	}
}

func TestGenerate_Child_OutputAvailableInDownstreamStep(t *testing.T) {
	// outputs.spawn.count should be typed as integer in a subsequent step's params.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child",
        "name": "worker",
        "result_schema": {
          "type": "object",
          "properties": {
            "count": {
              "type": "integer"
            }
          },
          "required": [
            "count"
          ]
        }
      },
      "switch": "next",
      "output": "{{ self.result }}"
    },
    {
      "id": "report",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      },
      "params": {
        "n": "{{outputs.spawn.count}}"
      }
    }
  ]
}`)
	reportInput := out.Defs["report_input"]
	if reportInput == nil || reportInput.Properties == nil {
		t.Fatal("report input should have properties")
	}
	assertJSON(t, reportInput.Properties["n"], `{"type": "integer"}`)
}

func TestGenerate_ChildParallel_WithOutputSchemas_ExposesKeyedOutput(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_parallel",
        "children": {
          "left": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          },
          "right": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          }
        }
      },
      "switch": "end",
      "output": "{{ self.result }}"
    }
  ]
}`)
	// spawn should appear in tasks
	if out.Tasks["spawn"].Output == nil {
		t.Fatal("spawn should have a typed output in tasks")
	}
	// spawn_output should be an object with left/right keys
	spawnOutput := out.Defs["spawn_output"]
	if spawnOutput == nil {
		t.Fatal("spawn_output def missing")
	}
	if spawnOutput.Properties == nil {
		t.Fatal("spawn_output should have properties")
	}
	if spawnOutput.Properties["left"] == nil {
		t.Error("spawn_output should have property 'left'")
	}
	if spawnOutput.Properties["right"] == nil {
		t.Error("spawn_output should have property 'right'")
	}
}

func TestGenerate_ChildParallel_KeyedOutputAvailableInDownstreamStep(t *testing.T) {
	// outputs.spawn.left.num should be typed as integer in a subsequent step.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_parallel",
        "children": {
          "left": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          },
          "right": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "num": {
                  "type": "integer"
                }
              },
              "required": [
                "num"
              ]
            }
          }
        }
      },
      "switch": "next",
      "output": "{{ self.result }}"
    },
    {
      "id": "aggregate",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      },
      "params": {
        "a": "{{outputs.spawn.left.num}}",
        "b": "{{outputs.spawn.right.num}}"
      }
    }
  ]
}`)
	aggInput := out.Defs["aggregate_input"]
	if aggInput == nil || aggInput.Properties == nil {
		t.Fatal("aggregate input should have properties")
	}
	assertJSON(t, aggInput.Properties["a"], `{"type": "integer"}`)
	assertJSON(t, aggInput.Properties["b"], `{"type": "integer"}`)
}

func TestGenerate_ChildParallel_MixedOutputSchemas_UntypedKeyIsObject(t *testing.T) {
	// A child without result_schema should still produce a key but typed as plain object.
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "spawn",
      "action": {
        "type": "child_parallel",
        "children": {
          "typed": {
            "name": "worker",
            "result_schema": {
              "type": "object",
              "properties": {
                "ok": {
                  "type": "boolean"
                }
              },
              "required": [
                "ok"
              ]
            }
          },
          "untyped": {
            "name": "other"
          }
        }
      },
      "switch": "end",
      "output": "{{ self.result }}"
    }
  ]
}`)
	spawnOutput := out.Defs["spawn_output"]
	if spawnOutput == nil || spawnOutput.Properties == nil {
		t.Fatal("spawn_output def missing or has no properties")
	}
	if spawnOutput.Properties["typed"] == nil {
		t.Error("spawn_output should have property 'typed'")
	}
	if spawnOutput.Properties["untyped"] == nil {
		t.Error("spawn_output should have property 'untyped' even without result_schema")
	}
}

func TestGenerate_UnusedDefsRemoved(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"$defs": {
				"Used":   { "type": "string" },
				"Unused": { "type": "integer" }
			},
			"properties": { "x": { "$ref": "#/$defs/Used" } }
		},
		"tasks": [{"id":"s1","action":{"type":"rest","endpoint":"http://x"}}]
	}`)
	if out.Defs["Used"] == nil {
		t.Error("Used def should be present in $defs")
	}
	if out.Defs["Unused"] != nil {
		t.Error("Unused def should have been removed")
	}
}
