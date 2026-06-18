package validationtest

import (
	"strings"
	"testing"
	"time"
)

func TestGenerate_Input_FirstTaskNoInput(t *testing.T) {
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
            "ok": {
              "type": "boolean"
            }
          }
        }
      },
      "output": "{{ self.result }}"
    }
  ]
}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_WithProcessInput(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": {
      "order_id": {
        "type": "integer"
      }
    }
  },
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "ok": {
              "type": "boolean"
            }
          }
        }
      },
      "output": "{{ self.result }}"
    }
  ]
}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_PrecedingTaskOutput(t *testing.T) {
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
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "sent": {
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
	assertJSON(t, out.Tasks["charge"].Input, `{"type": "object"}`)
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_TaskWithNoOutputSkippedInContext(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "log",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      },
      "switch": "next"
    },
    {
      "id": "notify",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "sent": {
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
	assertJSON(t, out.Tasks["notify"].Input, `{"type": "object"}`)
}

func TestGenerate_Input_SwitchOnlyStepSkippedInContext(t *testing.T) {
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
      "id": "route",
      "switch": [
        {
          "case": "outputs.charge.charged == true",
          "goto": "$ship"
        },
        {
          "goto": "end"
        }
      ]
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
      "output": "{{ self.result }}"
    }
  ]
}`)
	if _, ok := out.Tasks["route"]; ok {
		t.Error("switch-only task should not appear in tasks")
	}
	if out.Tasks["ship"].Output == nil {
		t.Error("ship should have an output schema")
	}
}

func TestGenerate_Input_Params(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": {
      "order_id": {
        "type": "integer"
      },
      "amount": {
        "type": "number"
      }
    },
    "required": [
      "order_id",
      "amount"
    ]
  },
  "tasks": [
    {
      "id": "charge",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "ok": {
              "type": "boolean"
            }
          }
        }
      },
      "params": {
        "id": "{{input.order_id}}",
        "sum": "{{input.amount}}"
      },
      "output": "{{ self.result }}"
    }
  ]
}`)
	assertJSON(t, out.Tasks["charge"].Input, `{"$ref": "#/$defs/charge_input"}`)
	input := out.Defs["charge_input"]
	if input == nil {
		t.Fatal("charge_input not found in defs")
	}
	if !input.Type.Contains("object") {
		t.Errorf("input type: got %v, want object", input.Type)
	}
	assertJSON(t, input.Properties["id"], `{"type": "integer"}`)
	assertJSON(t, input.Properties["sum"], `{"type": "number"}`)
}

func TestGenerate_Input_ParamsOnlyTask(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": { "type": "object", "properties": { "user_id": { "type": "string" } }, "required": ["user_id"] },
		"tasks": [{
			"id": "log",
			"action": {"type": "rest", "endpoint": "http://x"},
			"params": { "uid": "{{input.user_id}}" }
		}]
	}`)
	if _, ok := out.Tasks["log"]; !ok {
		t.Fatal("task with params but no result_schema should appear in tasks")
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
  "name": "p",
  "tasks": [
    {
      "id": "save_order",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "oneOf": [
            {
              "type": "object",
              "properties": {
                "valid": {
                  "type": "boolean"
                }
              }
            },
            {
              "type": "string"
            }
          ]
        }
      },
      "switch": "next",
      "output": "{{ self.result }}"
    },
    {
      "id": "check_fraud",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      },
      "params": {
        "result": "{{outputs.save_order.valid}}"
      },
      "switch": "end"
    }
  ]
}`)
	assertJSON(t, out.Tasks["check_fraud"].Input, `{"$ref": "#/$defs/check_fraud_input"}`)
	cfInput := out.Defs["check_fraud_input"]
	if cfInput == nil {
		t.Fatal("check_fraud_input not found in defs")
	}
	assertJSON(t, cfInput.Properties["result"], `{"type":["boolean","null"]}`)
}

func TestGenerate_MixedTemplate_NullableExpressionRejected(t *testing.T) {
	// error is nullable on finale (reachable via both normal and on_error paths).
	// Using it in a mixed template would silently produce "null_null" at runtime.
	err := runGenerateErr(t, `{
		"name": "p",
		"tasks": [
			{
				"id": "start",
				"action": {"type": "rest", "endpoint": "http://x"},
				"on_error": [{"goto": "$finale"}],
				"switch": "next"
			},
			{
				"id": "finale",
				"action": {"type": "rest", "endpoint": "http://x"},
				"params": {"msg": "{{error.code}}_{{error.message}}"},
				"switch": "end"
			}
		]
	}`)
	if err == nil {
		t.Fatal("expected error for nullable expression in mixed template, got nil")
	}
	if !strings.Contains(err.Error(), "??") {
		t.Errorf("error should mention ?? operator, got: %v", err)
	}
}

func TestGenerate_MixedTemplate_NonNullableExpressionAccepted(t *testing.T) {
	// error is required (exclusive error path), so using it in a mixed template is fine.
	runGenerate(t, `{
		"name": "p",
		"tasks": [
			{
				"id": "worker",
				"action": {"type": "rest", "endpoint": "http://x"},
				"switch": "end",
				"on_error": [{"goto": "$handler"}]
			},
			{
				"id": "handler",
				"action": {"type": "rest", "endpoint": "http://x"},
				"params": {"msg": "{{error.code}}_{{error.message}}"},
				"switch": "end"
			}
		]
	}`)
}

func TestGenerate_InvalidRef(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"tasks": [{"id":"s1","action":{"type":"rest","endpoint":"http://x"}}],
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

func TestGenerate_Switch_SelfExpressionTypeChecked(t *testing.T) {
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
          },
          "required": [
            "charged"
          ]
        }
      },
      "switch": [
        {
          "case": "self.result.charged == true",
          "goto": "$ship"
        },
        {
          "case": "self.result.charged == false",
          "goto": "$refund"
        }
      ],
      "output": "{{ self.result }}"
    },
    {
      "id": "ship",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      }
    },
    {
      "id": "refund",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      }
    }
  ]
}`)
	assertJSON(t, out.Tasks["charge"].Output, `{"$ref": "#/$defs/charge_output"}`)
}

func TestGenerate_SelfReferenceRequiresLoop(t *testing.T) {
	// A task that references its own output but never loops back to itself has no
	// prior iteration (it is not its own predecessor), so the self-reference — by
	// outputs.<id> or by self.previous — must be rejected.
	for _, expr := range []string{
		"{{ (outputs.loop.num ?? 0) + 1 }}",
		"{{ (self.previous.num ?? 0) + 1 }}",
	} {
		def := `{
			"name": "p",
			"tasks": [
				{"id":"loop","output":{"num":"` + expr + `"},"switch":[{"goto":"end"}]}
			]
		}`
		if err := runGenerateErr(t, def); err == nil {
			t.Errorf("expected error for non-looping self-reference %q", expr)
		}
	}
}

func TestGenerate_CrossStepMutualRecursion(t *testing.T) {
	// start and loop reference each other's output through a goto loop — a
	// cross-task (mutual) recursion. The joint SCC fixpoint resolves both: loop's
	// output is a plain integer; start mirrors loop and is nullable (null before
	// loop has run on the first pass).
	out := runGenerate(t, `{
		"name": "order-pipeline",
		"input_schema": {"type":"object","properties":{"ttl":{"type":"integer"}},"required":["ttl"]},
		"tasks": [
			{"id":"start","output":{"num":"{{ outputs.loop.num }}"},"switch":"next"},
			{"id":"loop","output":{"num":"{{ (outputs.start.num ?? 0) + 1 }}"},
			 "switch":[{"case":"self.output.num < input.ttl","goto":"$start"},{"goto":"end"}]}
		],
		"output":{"num":"{{ outputs.start.num }}"}
	}`)
	assertJSON(t, out.Defs["loop_output"], `{"type":"object","properties":{"num":{"type":"integer"}},"required":["num"]}`)
	assertJSON(t, out.Defs["start_output"], `{"type":"object","properties":{"num":{"type":["integer","null"]}},"required":["num"]}`)
}

func TestGenerate_UnboundedRecursionFailsFast(t *testing.T) {
	// `result: self.previous ?? input` has no base case: each pass nests the prior
	// output one level deeper, so the type grows without bound (exponentially, as
	// the joined accumulator references the prior estimate). The widening size bound
	// must reject it — and cheaply, before building a multi-megabyte schema.
	done := make(chan error, 1)
	go func() {
		done <- runGenerateErr(t, `{
			"name": "p",
			"input_schema": {
				"type": "object",
				"properties": {"ttl": {"type": "integer"}, "rec": {"$ref": "#/$defs/recursive"}},
				"required": ["ttl"],
				"$defs": {"recursive": {"type": "object","properties": {"num": {"type": "number"},"rec": {"$ref": "#/$defs/recursive"}}}}
			},
			"tasks": [
				{"id":"start","switch":"next"},
				{"id":"loop","output":{"result":"{{ self.previous ?? input }}"},
				 "switch":[{"case":"self.output.result != null","goto":"$start"},{"goto":"end"}]}
			],
			"output":{"num":"{{ outputs.loop }}"}
		}`)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-convergence error, got nil")
		}
		if !strings.Contains(err.Error(), "without converging") {
			t.Errorf("expected a non-convergence error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inference did not fail fast: still running after 5s (widening bound not applied)")
	}
}

func TestGenerate_SelfNamespaceScoping(t *testing.T) {
	// self is the task's transient scope. self.result (the raw action result) and,
	// for a looping output task, self.previous are available in both the output map
	// and the switch. self.output (the projection) is available only in the switch,
	// and only when the task defines an output. Crossing these boundaries — or
	// projecting a field from an untyped raw result — is an error.
	invalid := []struct {
		name string
		def  string
	}{
		{"self.output in a switch without a projection", `{"name":"p","tasks":[
			{"id":"s","action":{"type":"rest","endpoint":"http://x","result_schema":{"type":"object","properties":{"x":{"type":"boolean"}},"required":["x"]}},
			 "switch":[{"case":"self.output.x","goto":"end"},{"goto":"end"}]}]}`},
		{"self.output in an output map", `{"name":"p","tasks":[
			{"id":"s","action":{"type":"rest","endpoint":"http://x","result_schema":{"type":"object","properties":{"x":{"type":"boolean"}},"required":["x"]}},
			 "output":{"y":"{{ self.output.x }}"},"switch":[{"goto":"end"}]}]}`},
		{"self.result.field without result_schema", `{"name":"p","tasks":[
			{"id":"s","action":{"type":"rest","endpoint":"http://x"},"output":{"id":"{{ self.result.job_id }}"},"switch":[{"goto":"end"}]}]}`},
		{"self.previous in a non-looping switch", `{"name":"p","tasks":[
			{"id":"s","action":{"type":"rest","endpoint":"http://x","result_schema":{"type":"object","properties":{"x":{"type":"boolean"}},"required":["x"]}},
			 "output":{"y":"{{ self.result.x }}"},"switch":[{"case":"self.previous.y","goto":"end"},{"goto":"end"}]}]}`},
	}
	for _, c := range invalid {
		if err := runGenerateErr(t, c.def); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}

	valid := []struct {
		name string
		def  string
	}{
		{"self.result in a switch", `{"name":"p","tasks":[
			{"id":"s","action":{"type":"rest","endpoint":"http://x","result_schema":{"type":"object","properties":{"x":{"type":"boolean"}},"required":["x"]}},
			 "switch":[{"case":"self.result.x","goto":"end"},{"goto":"end"}]}]}`},
		{"self.previous in a looping output task's switch", `{"name":"p","tasks":[
			{"id":"loop","output":{"n":"{{ (self.previous.n ?? 0) + 1 }}"},
			 "switch":[{"case":"(self.previous.n ?? 0) < 3","goto":"$loop"},{"goto":"end"}]}]}`},
	}
	for _, c := range valid {
		if err := runGenerateErr(t, c.def); err != nil {
			t.Errorf("%s: expected no error, got %v", c.name, err)
		}
	}
}

func TestGenerate_ThreeStepMutualRecursion(t *testing.T) {
	// A three-node output cycle (a reads c, b reads a, c reads b; closed by a goto
	// loop a->b->c->a) — exercises the joint SCC fixpoint beyond two members. c is
	// the base case (?? 0) so resolves to a plain integer; a and b mirror through
	// the cycle and are nullable (null before the cycle has produced a value).
	out := runGenerate(t, `{
		"name": "p",
		"input_schema": {"type":"object","properties":{"ttl":{"type":"integer"}},"required":["ttl"]},
		"tasks": [
			{"id":"a","output":{"n":"{{ outputs.c.n }}"},"switch":"next"},
			{"id":"b","output":{"n":"{{ outputs.a.n }}"},"switch":"next"},
			{"id":"c","output":{"n":"{{ (outputs.b.n ?? 0) + 1 }}"},
			 "switch":[{"case":"self.output.n < input.ttl","goto":"$a"},{"goto":"end"}]}
		]
	}`)
	assertJSON(t, out.Defs["c_output"], `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	assertJSON(t, out.Defs["a_output"], `{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]}`)
	assertJSON(t, out.Defs["b_output"], `{"type":"object","properties":{"n":{"type":["integer","null"]}},"required":["n"]}`)
}

func TestGenerate_ForwardCrossStepRefRequiresCycle(t *testing.T) {
	// a reads outputs.b, but b runs strictly after a and never loops back — b is not
	// a predecessor of a, so its output is unavailable. The cross-task analogue of
	// the self-reference-requires-loop rule.
	def := `{"name":"p","tasks":[
		{"id":"a","output":{"n":"{{ outputs.b.n }}"},"switch":"next"},
		{"id":"b","output":{"n":"{{ 1 }}"},"switch":[{"goto":"end"}]}
	]}`
	if err := runGenerateErr(t, def); err == nil {
		t.Error("expected error: a cannot read outputs.b when b is not its predecessor")
	}
}

func TestGenerate_AcyclicOutputChain(t *testing.T) {
	// A linear chain of output-map tasks (first -> second -> third), each reading the
	// previous one's output. No cycle, so Tarjan emits singletons in dependency order
	// and each finalizes (non-null) before the next reads it.
	out := runGenerate(t, `{
		"name": "p",
		"tasks": [
			{"id":"first","output":{"n":"{{ 1 }}"},"switch":"next"},
			{"id":"second","output":{"n":"{{ outputs.first.n + 1 }}"},"switch":"next"},
			{"id":"third","output":{"n":"{{ outputs.second.n + 1 }}"},"switch":[{"goto":"end"}]}
		]
	}`)
	for _, id := range []string{"first_output", "second_output", "third_output"} {
		assertJSON(t, out.Defs[id], `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	}
}

func TestGenerate_Switch_OutputsExpressionTypeChecked(t *testing.T) {
	// A later task's switch routes on a prior task's output (outputs.<priorTask>),
	// type-checked against that task's declared output schema.
	runGenerate(t, `{
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
          },
          "required": [
            "charged"
          ]
        }
      },
      "switch": "next",
      "output": "{{ self.result }}"
    },
    {
      "id": "decide",
      "switch": [
        {
          "case": "outputs.charge.charged == true",
          "goto": "$notify"
        },
        {
          "goto": "end"
        }
      ]
    },
    {
      "id": "notify",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      }
    }
  ]
}`)
}

func TestGenerate_RecursiveStep_OwnOutputOptionalInParams(t *testing.T) {
	out := runGenerate(t, `{
  "name": "p",
  "input_schema": {
    "type": "object",
    "properties": {
      "tasks": {
        "type": "array",
        "items": {
          "type": "string"
        }
      }
    },
    "required": [
      "tasks"
    ]
  },
  "tasks": [
    {
      "id": "loop",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "finished_index": {
              "type": "number"
            },
            "done": {
              "type": "boolean"
            }
          },
          "required": [
            "finished_index",
            "done"
          ]
        }
      },
      "params": {
        "tasks": "{{input.tasks}}",
        "task_index": "{{outputs.loop.finished_index ? outputs.loop.finished_index : 0}}"
      },
      "switch": [
        {
          "case": "!self.result.done",
          "goto": "$loop"
        },
        {
          "goto": "end"
        }
      ],
      "output": "{{ self.result }}"
    }
  ]
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
  "name": "p",
  "tasks": [
    {
      "id": "decide",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
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
      "switch": [
        {
          "case": "self.result.ok",
          "goto": "$work"
        },
        {
          "goto": "end"
        }
      ],
      "output": "{{ self.result }}"
    },
    {
      "id": "work",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "done": {
              "type": "boolean"
            }
          }
        }
      },
      "params": {
        "flag": "{{outputs.decide.ok}}"
      },
      "output": "{{ self.result }}"
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
  "name": "p",
  "tasks": [
    {
      "id": "check",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "ok": {
              "oneOf": [
                {
                  "type": "boolean"
                },
                {
                  "type": "boolean"
                }
              ]
            }
          },
          "required": [
            "ok"
          ]
        }
      },
      "switch": [
        {
          "case": "self.result.ok",
          "goto": "$next"
        },
        {
          "goto": "end"
        }
      ],
      "output": "{{ self.result }}"
    },
    {
      "id": "next",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      }
    }
  ]
}`)
}

func TestGenerate_Switch_OneOfBooleanOptionalFieldRejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "oneOf": [{"type": "boolean"}] } }
		},
		"tasks": [
			{
				"id": "route",
				"switch": [{"case": "input.go_then", "goto": "$next"}, {"goto": "end"}]
			},
			{ "id": "next", "action": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
	if err == nil {
		t.Fatal("expected error for nullable oneOf boolean switch expression, got nil")
	}
}

func TestGenerate_Switch_NullableBooleanRejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"input_schema": {
			"type": "object",
			"properties": { "go_then": { "type": "boolean" } }
		},
		"tasks": [
			{
				"id": "route",
				"switch": [{"case": "input.go_then", "goto": "$work"}, {"goto": "end"}]
			},
			{ "id": "work", "action": {"type": "rest", "endpoint": "http://x"} }
		]
	}`)
	if err == nil {
		t.Fatal("expected error for nullable boolean switch expression, got nil")
	}
	if !strings.Contains(err.Error(), "boolean") {
		t.Errorf("error should mention expected type, got: %v", err)
	}
}

func TestGenerate_Switch_StringExpressionRejectsNonBoolean(t *testing.T) {
	err := runGenerateErr(t, `{
  "name": "p",
  "tasks": [
    {
      "id": "check",
      "action": {
        "type": "rest",
        "endpoint": "http://x",
        "result_schema": {
          "type": "object",
          "properties": {
            "label": {
              "type": "string"
            }
          },
          "required": [
            "label"
          ]
        }
      },
      "switch": [
        {
          "case": "self.result.label",
          "goto": "$next"
        },
        {
          "goto": "end"
        }
      ],
      "output": "{{ self.result }}"
    },
    {
      "id": "next",
      "action": {
        "type": "rest",
        "endpoint": "http://x"
      }
    }
  ]
}`)
	if err == nil {
		t.Fatal("expected error for non-boolean switch expression, got nil")
	}
	if !strings.Contains(err.Error(), "boolean") {
		t.Errorf("error should mention expected type, got: %v", err)
	}
}
