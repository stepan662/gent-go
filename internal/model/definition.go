package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gent/internal/schema"

	"github.com/go-playground/validator/v10"
	"github.com/xeipuuv/gojsonschema"
)

// GotoEnd signals process termination. Stored verbatim in SwitchCase.Goto and
// compared against the goto value at runtime; on the wire it is literally "end".
const GotoEnd = "end"

// GotoNext signals advance to the next step in the sequence. Valid only on
// non-terminal steps; using it on the last step is a validation error.
const GotoNext = "next"

// ActionType identifies how the engine invokes a step's action.
type ActionType string

const (
	ActionTypeREST          ActionType = "rest"
	ActionTypeScript        ActionType = "script"
	ActionTypeChild         ActionType = "child"
	ActionTypeChildParallel ActionType = "child_parallel"
	ActionTypeDelay         ActionType = "delay"
)

// ChildEntry describes a single named child process in a "child_parallel" call.
type ChildEntry struct {
	Name         string             `json:"name"                    description:"Name of the child process to invoke."`
	Version      int                `json:"version,omitempty"       description:"Version to run; 0 means latest published version."`
	Input        *Shape             `json:"input,omitempty"         description:"Templated value (a string expression or nested object of expressions) evaluated against the current context to build the child's input payload."`
	OutputSchema *schema.SchemaNode `json:"output_schema,omitempty" description:"JSON Schema to validate and expose this child's output."`
}

// Action describes how to invoke a step's action. It is a discriminated union on Type.
//   - "rest":           Endpoint (required), Headers (optional), AcceptedStatus (optional), OutputSchema (optional)
//   - "script":         Exec (required), OutputSchema (optional)
//   - "child":          Name (required), Input (optional), OutputSchema (optional) — single child process
//   - "child_parallel": Children (required, keyed map) — concurrent named child processes
//   - "delay":          Ms (required) — pauses the instance for a duration without holding a worker, then routes via switch
//
// OutputSchema (rest/script/child): when set, the response body is validated and stored in
// context as outputs.stepID. Without it the body is available only as "self" in this step's switch.
//
// AcceptedStatus (rest only): HTTP status patterns treated as non-errors. Defaults to any 2xx.
type Action struct {
	Type           ActionType               `json:"type"`
	Endpoint       string                 `json:"endpoint,omitempty"`        // rest
	Headers        map[string]string      `json:"headers,omitempty"`         // rest, values are expressions
	AcceptedStatus []string               `json:"accepted_status,omitempty"` // rest: HTTP status patterns accepted as non-errors
	Exec           string                 `json:"exec,omitempty"`            // script
	OutputSchema   *schema.SchemaNode     `json:"output_schema,omitempty"`   // rest/script/child: validate & persist output
	Name           string                 `json:"name,omitempty"`            // child
	Version        int                    `json:"version,omitempty"`         // child
	Input          *Shape                 `json:"input,omitempty"`           // child
	Children       map[string]ChildEntry  `json:"children,omitempty"`        // child_parallel
	Ms             string                 `json:"ms,omitempty"`              // delay: milliseconds to pause, as an expression
}

// JSONSchemaBytes returns the JSON Schema for Action as a discriminated union
// so that OpenAPI reflection produces a proper oneOf instead of a flat object.
func (Action) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"oneOf": [
			{
				"type": "object",
				"description": "HTTP call — sends a request to an external endpoint.",
				"properties": {
					"type":            {"type": "string", "const": "rest"},
					"endpoint":        {"type": "string", "description": "URL of the HTTP endpoint to call."},
					"headers":         {"type": "object", "additionalProperties": {"type": "string"}, "description": "HTTP headers to include. Values are expressions evaluated against the current context."},
					"accepted_status": {"type": "array", "items": {"type": "string"}, "description": "HTTP status patterns accepted as non-errors, e.g. \"2xx\" or \"404\". Defaults to any 2xx."},
					"output_schema":   {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and persist the response body. Without it the response is available only as 'self' in this step's switch."}
				},
				"required": ["type", "endpoint"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Script call — executes a shell command or inline script.",
				"properties": {
					"type":          {"type": "string", "const": "script"},
					"exec":          {"type": "string", "description": "Shell command or script body to execute."},
					"output_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and persist stdout. Without it the output is available only as 'self' in this step's switch."}
				},
				"required": ["type", "exec"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Single child-process call — runs one named process as a sub-instance and waits for it to complete. The child's output is available as outputs.stepID.",
				"properties": {
					"type":          {"type": "string", "const": "child"},
					"name":          {"type": "string", "description": "Name of the child process to invoke."},
					"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
					"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value (string expression or nested object) evaluated against the current context to build the child's input payload."},
					"output_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and persist the child's output. Without it the output is not stored in context."}
				},
				"required": ["type", "name"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Parallel child-process call — runs multiple named processes concurrently and waits for all to complete. Each child's output is available as outputs.stepID.childKey.",
				"properties": {
					"type": {"type": "string", "const": "child_parallel"},
					"children": {
						"type": "object",
						"description": "Keyed map of child processes to run concurrently. Keys become the access names in outputs.stepID.",
						"additionalProperties": {
							"type": "object",
							"properties": {
								"name":          {"type": "string", "description": "Name of the child process to invoke."},
								"version":       {"type": "integer", "description": "Version to run; 0 means latest published version."},
								"input":         {"$ref": "#/$defs/ModelShape", "description": "Templated value (string expression or nested object) evaluated against the current context to build the child's input payload."},
								"output_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema to validate and expose this child's output."}
							},
							"required": ["name"],
							"additionalProperties": false
						},
						"minProperties": 1
					}
				},
				"required": ["type", "children"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"description": "Delay action — pauses the instance for a duration without holding a worker, then routes via switch.",
				"properties": {
					"type": {"type": "string", "const": "delay"},
					"ms":   {"type": "string", "description": "Milliseconds to pause, as an expression: a literal such as 30000 or a template such as {{ outputs.x.retry_after }}."}
				},
				"required": ["type", "ms"],
				"additionalProperties": false
			}
		],
		"discriminator": {"propertyName": "type"}
	}`), nil
}

// SwitchCase is a single entry in a Step's switch list: a boolean expression
// evaluated against the process context (and this step's own output as "self"),
// and the routing target when the expression is true.
// An empty Case means "catch-all" — it matches unconditionally and must be last.
// Goto stores the raw wire value: "end", "next", or "$step-id".
type SwitchCase struct {
	Case string
	Goto string
}

// SwitchMap is an ordered list of SwitchCase entries. It marshals as a plain
// JSON object so the wire format is readable:
//
//	{"self.paid == true": "ship", "self.paid == false": "refund"}
//
// JSON object key order is preserved on unmarshal by reading tokens sequentially
// rather than decoding into a map.
type SwitchMap []SwitchCase

func (s SwitchMap) MarshalJSON() ([]byte, error) {
	type wireCase struct {
		Case string `json:"case,omitempty"`
		Goto string `json:"goto"`
	}
	items := make([]wireCase, len(s))
	for i, c := range s {
		items[i] = wireCase{Case: c.Case, Goto: c.Goto}
	}
	return json.Marshal(items)
}

func (s *SwitchMap) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = nil
		return nil
	}
	// Scalar shorthand: "next", "end", or "$step-id" — desugars to a single catch-all.
	if len(data) > 0 && data[0] == '"' {
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return fmt.Errorf("switch: %w", err)
		}
		if v != GotoEnd && v != GotoNext && !strings.HasPrefix(v, "$") {
			return fmt.Errorf("switch: %q must be \"next\", \"end\", or a step reference like \"$step-id\"", v)
		}
		*s = SwitchMap{{Goto: v}}
		return nil
	}
	// Array form.
	var items []struct {
		Case string `json:"case"`
		Goto string `json:"goto"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("switch: %w", err)
	}
	*s = (*s)[:0]
	for _, item := range items {
		if item.Goto == "" {
			return fmt.Errorf("switch: goto is required")
		}
		if item.Goto != GotoEnd && item.Goto != GotoNext && !strings.HasPrefix(item.Goto, "$") {
			return fmt.Errorf("switch: goto %q must be \"end\", \"next\", or a step reference like \"$step-id\"", item.Goto)
		}
		*s = append(*s, SwitchCase{Case: item.Case, Goto: item.Goto})
	}
	return nil
}

// JSONSchemaBytes returns the JSON Schema for SwitchMap so that OpenAPI
// reflection produces the correct schema for its wire format.
func (SwitchMap) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"oneOf": [
			{
				"type": "string",
				"description": "Shorthand for a single unconditional route. \"next\" advances to the next step (not valid on the last step), \"end\" terminates the instance, \"$step-id\" jumps to a named step."
			},
			{
				"type": "array",
				"description": "Ordered routing rules evaluated after the call. Cases are evaluated in order; first match wins. The last entry must be a catch-all (omit 'case').",
				"items": {
					"type": "object",
					"properties": {
						"case": {"type": "string", "description": "Boolean expression. Omit for a catch-all; must be last."},
						"goto": {"type": "string", "description": "\"end\" to terminate, \"next\" to advance, or \"$step-id\" to jump to a step."}
					},
					"required": ["goto"],
					"additionalProperties": false
				},
				"minItems": 1
			}
		]
	}`), nil
}

// Shape is a templated value used by the data-shaping fields (params, output,
// process output, child input). It is recursively either a string expression
// (a {{ }} template — literal text, a single expression preserving type, or a
// mixed string) or an object whose values are themselves Shapes:
//
//	type Shape = string | Record<string, Shape>
//
// Arrays and non-string literals are not allowed structurally — produce them
// from an expression at a string leaf instead (e.g. "{{ 5 }}", "{{ [a, b] }}").
// The authoring structure is string|object, but the evaluated/inferred value can
// be any shape, because a string leaf may evaluate to any type.
type Shape struct {
	Raw any // string | map[string]any (recursively)
}

func (s *Shape) UnmarshalJSON(b []byte) error {
	var raw any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if err := checkShape(raw); err != nil {
		return fmt.Errorf("shape: %w", err)
	}
	s.Raw = raw
	return nil
}

func (s Shape) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Raw)
}

// Present reports whether the shape carries a value. Nil-safe so callers can
// write step.Output.Present() without a separate nil check.
func (s *Shape) Present() bool {
	return s != nil && s.Raw != nil
}

// Strings returns every string leaf in the shape, used to collect outputs.<id>
// references for the output-dependency graph.
func (s *Shape) Strings() []string {
	if s == nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(s.Raw)
	return out
}

// JSONSchemaBytes exposes the recursive Shape schema (string | object of Shape)
// so OpenAPI reflection produces the correct wire format. The self-reference uses
// the JSON-Pointer to swaggest's generated def name (model.Shape -> "ModelShape").
// This is correct for the process schema (defs under #/$defs/); the OpenAPI spec
// builder rewrites #/$defs/ModelShape -> #/components/schemas/ModelShape.
func (Shape) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"oneOf": [
			{"type": "string", "description": "An expression / template ({{ ... }}) or a literal string."},
			{
				"type": "object",
				"description": "Nested object; each value is recursively a Shape.",
				"additionalProperties": {"$ref": "#/$defs/ModelShape"}
			}
		]
	}`), nil
}

// checkShape enforces the string | Record<string, Shape> grammar recursively,
// rejecting arrays and non-string scalar literals with a clear error.
func checkShape(n any) error {
	switch v := n.(type) {
	case string:
		return nil
	case map[string]any:
		for k, c := range v {
			if err := checkShape(c); err != nil {
				return fmt.Errorf("%q: %w", k, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("must be a string expression or a nested object, got %T", n)
	}
}

// ErrorCase is a single error-routing rule evaluated when a step's call fails.
// Rules are evaluated in order; the first match applies.
// An empty Code list is a catch-all matching any error.
type ErrorCase struct {
	Code        []string `json:"code,omitempty"        description:"SQL LIKE patterns matched against the error code. '%' = any chars, '_' = one char. Empty = catch-all. Known codes — REST: http.NNN (e.g. http.500), http.timeout, pre.error, pre.timeout, output.parse, output.invalid; Script: script.N (exit code, e.g. script.1), script.timeout, pre.exec, output.parse; Child process: output.invalid. pre.* codes mean the call never reached the remote. Note: child.failed cannot be caught here — handle errors inside the child process and communicate them via return data."`
	Retries     int      `json:"retries,omitempty"     description:"Number of retries before following goto or failing. 0 = no retries. On only_once:true steps only pre.* codes (or rules with not_reached:true) may have retries > 0."`
	Goto        string   `json:"goto,omitempty"        description:"Step to route to when retries are exhausted. '$step-id' or 'end'. Omit to fail the instance."`
	NotReached  *bool    `json:"not_reached,omitempty" description:"Assert that this error code means the remote call was never reached. When true, retries are allowed even on only_once:true steps. Omit to use the engine's default classification (pre.* = not reached, everything else = potentially reached)."`
}

func (e ErrorCase) MarshalJSON() ([]byte, error) {
	type wire struct {
		Code       []string `json:"code,omitempty"`
		Retries    int      `json:"retries,omitempty"`
		Goto       string   `json:"goto,omitempty"`
		NotReached *bool    `json:"not_reached,omitempty"`
	}
	w := wire{Code: e.Code, Retries: e.Retries, NotReached: e.NotReached}
	if e.Goto != "" {
		if e.Goto == GotoEnd {
			w.Goto = "end"
		} else {
			w.Goto = "$" + e.Goto
		}
	}
	return json.Marshal(w)
}

func (e *ErrorCase) UnmarshalJSON(data []byte) error {
	type wire struct {
		Code       []string `json:"code,omitempty"`
		Retries    int      `json:"retries,omitempty"`
		Goto       string   `json:"goto,omitempty"`
		NotReached *bool    `json:"not_reached,omitempty"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.Code = w.Code
	e.Retries = w.Retries
	e.NotReached = w.NotReached
	if w.Goto == "" {
		e.Goto = ""
	} else if w.Goto == "end" {
		e.Goto = GotoEnd
	} else if strings.HasPrefix(w.Goto, "$") {
		e.Goto = w.Goto[1:]
	} else {
		return fmt.Errorf("on_error: goto %q must be \"end\" or a step reference like \"$step-id\"", w.Goto)
	}
	return nil
}

// Step is a single unit of work in a process definition.
// Every step must have a switch (and optionally a call).
//
//   - Action-only (Action set, Switch present): executes the call, then routes via switch.
//   - Switch-only (Action nil, Switch present): pure routing step with no external call.
//   - Both: executes the call first, then evaluates the switch (with this step's output as "self").
//
// Switch is always required. Use the scalar shorthand ("next", "end", "$step-id") for
// simple linear flow, or an array of cases for conditional branching.
// The last case must always be a catch-all (no "case" expression).
// "end" terminates the instance; "next" advances to the next step in the list
// (invalid on the last step — use "end" instead); "$step-id" jumps to a named step.
type Step struct {
	ID        string            `json:"id"                 validate:"required" description:"Unique step identifier. 'end' and 'next' are reserved and cannot be used."`
	Action      *Action             `json:"action,omitempty"                        description:"Describes the action to perform. Omit for switch-only (routing) steps."`
	TimeoutMs int               `json:"timeout_ms,omitempty"                  description:"Maximum execution time in milliseconds. 0 means no timeout."`
	OnlyOnce  *bool             `json:"only_once,omitempty"                   description:"When true, the engine guarantees at-most-once execution: retries are only allowed for pre.* errors (remote never reached) or on_error rules with not_reached:true. Defaults to false (retryable)."`
	OnError   []ErrorCase       `json:"on_error,omitempty"                    description:"Ordered error-routing rules evaluated when the call fails. First match wins."`
	Params    *Shape            `json:"params,omitempty"                      description:"Templated value (a string expression or nested object of expressions) evaluated against the current context to build the call's input."`
	Output    *Shape            `json:"output,omitempty"                      description:"Templated value that remaps this step's output. Evaluated against the context plus self.result (the action's raw result) and self.previous (this step's prior output). When set, this value is stored as outputs.stepID and seen by the switch as self.output; the raw result is not exported."`
	Switch    SwitchMap         `json:"switch"                                description:"Required. Routing declaration: scalar shorthand (\"next\", \"end\", \"$step-id\") or an ordered list of conditional cases. The last case must be a catch-all (omit 'case')."`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Versions are assigned by the server on apply; never include a version when submitting definitions.
type ProcessDefinition struct {
	Name        string             `json:"name"         validate:"required" description:"Unique process identifier."`
	Steps       []*Step            `json:"steps"        validate:"required,min=1,dive" description:"Ordered list of execution steps. Control advances linearly unless a switch case redirects."`
	InputSchema *schema.SchemaNode `json:"input_schema,omitempty"          description:"JSON Schema used to validate the input payload when starting a new instance."`
	Output      *Shape             `json:"output,omitempty"                description:"Templated value (a string expression or nested object of expressions) evaluated at completion to produce the process output."`
}

// Normalize normalizes InputSchema and all step OutputSchemas in-place using the
// schema package (flattens $defs to root, removes unused definitions, rewrites $refs).
func (d *ProcessDefinition) Normalize() error {
	if d.InputSchema != nil {
		normalized, err := schema.Normalize(d.InputSchema)
		if err != nil {
			return fmt.Errorf("input_schema: %w", err)
		}
		d.InputSchema = normalized
	}
	for _, s := range d.Steps {
		if s.Action == nil {
			continue
		}
		if s.Action.OutputSchema != nil {
			normalized, err := schema.Normalize(s.Action.OutputSchema)
			if err != nil {
				return fmt.Errorf("step %q action.output_schema: %w", s.ID, err)
			}
			s.Action.OutputSchema = normalized
		}
		if s.Action.Type == ActionTypeChildParallel {
			for key, entry := range s.Action.Children {
				if entry.OutputSchema != nil {
					normalized, err := schema.Normalize(entry.OutputSchema)
					if err != nil {
						return fmt.Errorf("step %q action.children[%q].output_schema: %w", s.ID, key, err)
					}
					entry.OutputSchema = normalized
					s.Action.Children[key] = entry
				}
			}
		}
	}
	return nil
}

// Validate checks the definition and all its steps against the struct tag rules,
// and verifies that any attached JSON Schemas are valid schema documents.
// It also checks statically that all switch goto targets name known steps.
func (d *ProcessDefinition) Validate() error {
	if err := fmtValidationErr(v.Struct(d)); err != nil {
		return err
	}
	if err := checkSchemaDoc("input_schema", d.InputSchema); err != nil {
		return err
	}
	stepIDs := make(map[string]struct{}, len(d.Steps))
	for _, s := range d.Steps {
		stepIDs[s.ID] = struct{}{}
	}
	lastIdx := len(d.Steps) - 1
	for i, s := range d.Steps {
		if err := validateStep(s, stepIDs, i, lastIdx); err != nil {
			return err
		}
	}
	return nil
}

func validateStep(s *Step, stepIDs map[string]struct{}, stepIdx, lastIdx int) error {
	// Reserved step IDs.
	if s.ID == GotoEnd || s.ID == GotoNext {
		return fmt.Errorf("step ID %q is reserved", s.ID)
	}

	if s.Action != nil {
		switch s.Action.Type {
		case ActionTypeREST:
			if s.Action.Endpoint == "" {
				return fmt.Errorf("step %q: action.endpoint is required for type %q", s.ID, s.Action.Type)
			}
		case ActionTypeScript:
			if s.Action.Exec == "" {
				return fmt.Errorf("step %q: action.exec is required for type %q", s.ID, s.Action.Type)
			}
		case ActionTypeChild:
			if s.Action.Name == "" {
				return fmt.Errorf("step %q: action.name is required for type %q", s.ID, s.Action.Type)
			}
		case ActionTypeChildParallel:
			if len(s.Action.Children) == 0 {
				return fmt.Errorf("step %q: action.children is required for type %q", s.ID, s.Action.Type)
			}
			for key, entry := range s.Action.Children {
				if entry.Name == "" {
					return fmt.Errorf("step %q: action.children[%q].name is required", s.ID, key)
				}
			}
		case ActionTypeDelay:
			if s.Action.Ms == "" {
				return fmt.Errorf("step %q: action.ms is required for type %q", s.ID, s.Action.Type)
			}
		default:
			return fmt.Errorf("step %q: action.type must be one of: rest, script, child, child_parallel, delay", s.ID)
		}
	}

	if len(s.Switch) == 0 {
		return fmt.Errorf("step %q: switch is required", s.ID)
	}

	for i, c := range s.Switch {
		isLast := i == len(s.Switch)-1
		if c.Case == "" && !isLast {
			return fmt.Errorf("step %q switch: catch-all at index %d must be the last case (unreachable cases after it)", s.ID, i)
		}
		switch {
		case c.Goto == GotoEnd:
			// always valid
		case c.Goto == GotoNext:
			if stepIdx == lastIdx {
				return fmt.Errorf("step %q switch: 'next' is not allowed on the last step; use 'end' to terminate", s.ID)
			}
		case strings.HasPrefix(c.Goto, "$"):
			stepID := c.Goto[1:]
			if _, ok := stepIDs[stepID]; !ok {
				return fmt.Errorf("step %q switch: goto %q is not a known step", s.ID, c.Goto)
			}
		default:
			return fmt.Errorf("step %q switch: goto %q must be \"end\", \"next\", or a step reference like \"$step-id\"", s.ID, c.Goto)
		}
	}
	if s.Switch[len(s.Switch)-1].Case != "" {
		return fmt.Errorf("step %q switch: last case must be a catch-all (omit 'case' to match unconditionally)", s.ID)
	}
	onlyOnce := s.OnlyOnce != nil && *s.OnlyOnce
	for i, ec := range s.OnError {
		for _, pat := range ec.Code {
			if !validLikePattern(pat) {
				return fmt.Errorf("step %q on_error[%d]: code pattern must not be empty", s.ID, i)
			}
			if sqlLikeMatch(pat, "child.failed") {
				return fmt.Errorf("step %q on_error[%d]: catching child.failed is not supported; handle errors inside the child process and communicate them via return data", s.ID, i)
			}
		}
		isLast := i == len(s.OnError)-1
		if len(ec.Code) == 0 && !isLast {
			return fmt.Errorf("step %q on_error[%d]: catch-all must be the last rule (unreachable rules after it)", s.ID, i)
		}
		if ec.Goto != "" && ec.Goto != GotoEnd {
			if _, ok := stepIDs[ec.Goto]; !ok {
				return fmt.Errorf("step %q on_error[%d]: goto %q is not a known step", s.ID, i, ec.Goto)
			}
		}
		if onlyOnce && ec.Retries > 0 {
			// not_reached:true is an explicit user override — allow retries regardless of pattern.
			if ec.NotReached != nil && *ec.NotReached {
				continue
			}
			// Catch-all rules (empty Code) would match any error including reached ones.
			if len(ec.Code) == 0 {
				return fmt.Errorf("step %q on_error[%d]: catch-all rule cannot have retries on an only_once step; restrict to pre.%% or add not_reached:true", s.ID, i)
			}
			for _, pat := range ec.Code {
				if !patternOnlyMatchesPre(pat) {
					return fmt.Errorf("step %q on_error[%d]: pattern %q can match errors where the call may have executed; restrict to pre.%% patterns or add not_reached:true to assert the remote was not reached", s.ID, i, pat)
				}
			}
		}
	}
	if s.Action != nil && s.Action.Type == ActionTypeREST {
		for _, pat := range s.Action.AcceptedStatus {
			if !validAcceptedStatusPattern(pat) {
				return fmt.Errorf("step %q: accepted_status %q must be \"2xx\"/\"3xx\"/\"4xx\"/\"5xx\" or a 3-digit code", s.ID, pat)
			}
		}
	}
	if s.Action != nil {
		if err := checkSchemaDoc(fmt.Sprintf("step %q action.output_schema", s.ID), s.Action.OutputSchema); err != nil {
			return err
		}
		if s.Action.Type == ActionTypeChildParallel {
			for key, entry := range s.Action.Children {
				if err := checkSchemaDoc(fmt.Sprintf("step %q action.children[%q].output_schema", s.ID, key), entry.OutputSchema); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validLikePattern(p string) bool {
	return strings.TrimSpace(p) != ""
}

// patternOnlyMatchesPre reports whether a LIKE pattern can exclusively match
// error codes in the pre.* namespace. It computes the constant prefix before
// the first '%' or '_' wildcard and checks it starts with "pre.". Patterns
// without wildcards must literally start with "pre.".
func patternOnlyMatchesPre(p string) bool {
	for i := 0; i < len(p); i++ {
		if p[i] == '%' || p[i] == '_' {
			return strings.HasPrefix(p[:i], "pre.")
		}
	}
	return strings.HasPrefix(p, "pre.")
}

func sqlLikeMatch(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			p = p[1:]
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if sqlLikeMatch(p, s[i:]) {
					return true
				}
			}
			return false
		case '_':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

func validAcceptedStatusPattern(p string) bool {
	if len(p) == 3 && p[1] == 'x' && p[2] == 'x' && p[0] >= '1' && p[0] <= '5' {
		return true
	}
	if len(p) == 3 {
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	return false
}

func checkSchemaDoc(field string, s *schema.SchemaNode) error {
	if s == nil {
		return nil
	}
	if _, err := gojsonschema.NewSchema(gojsonschema.NewGoLoader(s)); err != nil {
		return fmt.Errorf("%s is not a valid JSON Schema: %w", field, err)
	}
	return nil
}

// ValidateInput checks input data against InputSchema. No-op if InputSchema is nil.
func (d *ProcessDefinition) ValidateInput(input any) error {
	return validateSchema(d.InputSchema, input)
}

// ValidateOutput checks output data against call.OutputSchema. No-op if unset.
func (c *Action) ValidateOutput(output any) error {
	return validateSchema(c.OutputSchema, output)
}


func validateSchema(s *schema.SchemaNode, data any) error {
	if s == nil {
		return nil
	}
	result, err := gojsonschema.Validate(
		gojsonschema.NewGoLoader(s),
		gojsonschema.NewGoLoader(data),
	)
	if err != nil {
		return fmt.Errorf("schema validation error: %w", err)
	}
	if !result.Valid() {
		msgs := make([]string, len(result.Errors()))
		for i, e := range result.Errors() {
			msgs[i] = e.String()
		}
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}

// v is the shared validator, configured to report JSON field names in errors.
var v = func() *validator.Validate {
	val := validator.New()
	val.RegisterTagNameFunc(func(f reflect.StructField) string {
		name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return f.Name
		}
		return name
	})
	return val
}()

// fmtValidationErr converts validator.ValidationErrors into a readable API error.
func fmtValidationErr(err error) error {
	if err == nil {
		return nil
	}
	var ve validator.ValidationErrors
	if !errors.As(err, &ve) {
		return err
	}
	msgs := make([]string, len(ve))
	for i, fe := range ve {
		msgs[i] = describeFieldErr(fe)
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

func describeFieldErr(fe validator.FieldError) string {
	field := fe.Field()
	switch fe.Tag() {
	case "required", "required_if":
		return fmt.Sprintf("%s is required", field)
	case "min":
		return fmt.Sprintf("%s must have at least %s item(s)", field, fe.Param())
	case "oneof":
		return fmt.Sprintf("%s must be one of: %s", field, strings.ReplaceAll(fe.Param(), " ", ", "))
	default:
		return fe.Error()
	}
}

