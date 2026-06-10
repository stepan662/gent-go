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

// GotoEnd is the internal sentinel for process termination. It is stored in
// SwitchCase.Next and ErrorCase.Next to signal that the instance should complete.
// On the wire it appears as "end" (no prefix); step references use the "$" prefix.
const GotoEnd = "$end"

// CallType identifies how the engine invokes a step's action.
type CallType string

const (
	CallTypeREST         CallType = "rest"
	CallTypeScript       CallType = "script"
	CallTypeChildProcess CallType = "child_process"
)

// ChildProcessEntry describes a single process to run within a "child_process" call.
type ChildProcessEntry struct {
	Name    string            `json:"name"            description:"Name of the child process to invoke."`
	Version int               `json:"version,omitempty" description:"Version to run; 0 means latest published version."`
	Input   map[string]string `json:"input,omitempty" description:"Expression map evaluated against the current context to build the child's input payload."`
}

// Call describes how to invoke a step's action. It is a discriminated union on Type.
//   - "rest":          Endpoint (required), Headers (optional), AcceptedStatus (optional), OutputSchema (optional)
//   - "script":        Exec (required), OutputSchema (optional)
//   - "child_process": Processes (required), ChildOutputSchema (optional per-child schema)
//
// OutputSchema (rest/script): when set, the response body is validated against the schema
// and stored in context for downstream steps. Without it, the body is available only as
// "self" within the same step's switch and is not persisted.
//
// AcceptedStatus (rest only): HTTP status patterns treated as non-errors. Supports "2xx"
// style patterns and exact codes like "404". Defaults to any 2xx when omitted.
//
// ChildOutputSchema (child_process): when set, each child's computed output is
// validated before the parent resumes.
type Call struct {
	Type             CallType            `json:"type"`
	Endpoint         string              `json:"endpoint,omitempty"`           // rest
	Headers          map[string]string   `json:"headers,omitempty"`            // rest, values are expressions
	AcceptedStatus   []string            `json:"accepted_status,omitempty"`    // rest: HTTP status patterns accepted as non-errors
	Exec             string              `json:"exec,omitempty"`               // script
	OutputSchema     *schema.SchemaNode  `json:"output_schema,omitempty"`      // rest/script: validate & persist output
	Processes        []ChildProcessEntry `json:"processes,omitempty"`          // child_process
	ChildOutputSchema *schema.SchemaNode `json:"child_output_schema,omitempty"` // child_process: validate each child's output
}

// JSONSchemaBytes returns the JSON Schema for Call as a discriminated union
// so that OpenAPI reflection produces a proper oneOf instead of a flat object.
func (Call) JSONSchemaBytes() ([]byte, error) {
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
				"description": "Child-process call — runs one or more named processes as sub-instances.",
				"properties": {
					"type": {"type": "string", "const": "child_process"},
					"processes": {
						"type": "array",
						"description": "List of child processes to run. All run concurrently; the step waits for all to complete.",
						"items": {
							"type": "object",
							"properties": {
								"name":    {"type": "string", "description": "Name of the child process to invoke."},
								"version": {"type": "integer", "description": "Version to run; 0 means latest published version."},
								"input":   {"type": "object", "additionalProperties": {"type": "string"}, "description": "Expression map evaluated against the current context to build the child's input payload."}
							},
							"required": ["name"],
							"additionalProperties": false
						},
						"minItems": 1
					},
					"child_output_schema": {"type": "object", "additionalProperties": true, "description": "JSON Schema applied to each child's computed output before the parent resumes."}
				},
				"required": ["type", "processes"],
				"additionalProperties": false
			}
		],
		"discriminator": {"propertyName": "type"}
	}`), nil
}

// SwitchCase is a single entry in a Step's switch list: a boolean expression
// evaluated against the process context (and this step's own output as "self"),
// and the ID of the step to jump to when the expression is true.
// An empty Case means "catch-all" — it matches unconditionally and must be last.
type SwitchCase struct {
	Case string
	Next string
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
		Next string `json:"next"`
	}
	items := make([]wireCase, len(s))
	for i, c := range s {
		nextWire := c.Next
		if nextWire == GotoEnd {
			nextWire = "end"
		} else {
			nextWire = "$" + nextWire
		}
		items[i] = wireCase{Case: c.Case, Next: nextWire}
	}
	return json.Marshal(items)
}

func (s *SwitchMap) UnmarshalJSON(data []byte) error {
	var items []struct {
		Case string `json:"case"`
		Next string `json:"next"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("switch: %w", err)
	}
	*s = (*s)[:0]
	for _, item := range items {
		if item.Next == "" {
			return fmt.Errorf("switch: next is required")
		}
		next := item.Next
		if next == "end" {
			next = GotoEnd
		} else if strings.HasPrefix(next, "$") {
			next = next[1:]
		} else {
			return fmt.Errorf("switch: next %q must be \"end\" or a step reference like \"$step-id\"", next)
		}
		*s = append(*s, SwitchCase{Case: item.Case, Next: next})
	}
	return nil
}

// JSONSchemaBytes returns the JSON Schema for SwitchMap so that OpenAPI
// reflection produces the correct schema for its wire format.
func (SwitchMap) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{"type":"array","description":"Ordered routing rules. Cases are evaluated in order; first match wins. Omit 'case' on the last entry for a catch-all. Use \"end\" as 'next' to terminate the instance.","items":{"type":"object","properties":{"case":{"type":"string","description":"Boolean expression evaluated against the context (and 'self' for this step's output). Omit for a catch-all that matches unconditionally; must be the last item."},"next":{"type":"string","description":"Step to jump to, prefixed with '$' (e.g. \"$my-step\"), or \"end\" to complete the instance."}},"required":["next"],"additionalProperties":false}}`), nil
}

// ErrorCase is a single error-routing rule evaluated when a step's call fails.
// Rules are evaluated in order; the first match applies.
// An empty Code list is a catch-all matching any error.
type ErrorCase struct {
	Code        []string `json:"code,omitempty"        description:"SQL LIKE patterns matched against the error code. '%' = any chars, '_' = one char. Empty = catch-all. Known codes — REST: http.NNN (e.g. http.500), http.timeout, pre.error, pre.timeout, output.parse, output.invalid; Script: script.N (exit code, e.g. script.1), script.timeout, pre.exec, output.parse; Child process: child.failed, output.invalid. pre.* codes mean the call never reached the remote."`
	Retries     int      `json:"retries,omitempty"     description:"Number of retries before following Next or failing. 0 = no retries. On only_once:true steps only pre.* codes (or rules with not_reached:true) may have retries > 0."`
	Next        string   `json:"next,omitempty"        description:"Step to route to when retries are exhausted. '$step-id' or 'end'. Omit to fail the instance."`
	NotReached  *bool    `json:"not_reached,omitempty" description:"Assert that this error code means the remote call was never reached. When true, retries are allowed even on only_once:true steps. Omit to use the engine's default classification (pre.* = not reached, everything else = potentially reached)."`
}

func (e ErrorCase) MarshalJSON() ([]byte, error) {
	type wire struct {
		Code       []string `json:"code,omitempty"`
		Retries    int      `json:"retries,omitempty"`
		Next       string   `json:"next,omitempty"`
		NotReached *bool    `json:"not_reached,omitempty"`
	}
	w := wire{Code: e.Code, Retries: e.Retries, NotReached: e.NotReached}
	if e.Next != "" {
		if e.Next == GotoEnd {
			w.Next = "end"
		} else {
			w.Next = "$" + e.Next
		}
	}
	return json.Marshal(w)
}

func (e *ErrorCase) UnmarshalJSON(data []byte) error {
	type wire struct {
		Code       []string `json:"code,omitempty"`
		Retries    int      `json:"retries,omitempty"`
		Next       string   `json:"next,omitempty"`
		NotReached *bool    `json:"not_reached,omitempty"`
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	e.Code = w.Code
	e.Retries = w.Retries
	e.NotReached = w.NotReached
	if w.Next == "" {
		e.Next = ""
	} else if w.Next == "end" {
		e.Next = GotoEnd
	} else if strings.HasPrefix(w.Next, "$") {
		e.Next = w.Next[1:]
	} else {
		return fmt.Errorf("on_error: next %q must be \"end\" or a step reference like \"$step-id\"", w.Next)
	}
	return nil
}

// Step is a single unit of work in a process definition.
// Each step may have a call, a switch, or both — but at least one is required.
//
//   - Call-only (Call set, Switch empty): executes the call and advances to the
//     next step in the list. If it is the last step, the instance completes.
//   - Switch-only (Call nil, Switch non-empty): evaluates the switch to determine
//     the next step without performing any external call.
//   - Both: executes the call first, then evaluates the switch.
//
// When Switch is present it must contain a "default" case as the last entry.
// Switch cases are evaluated in order; the first matching expression wins.
// The "default" case always matches and must be present to make control flow explicit.
// A Next value of GotoEnd ("end" on the wire) terminates the instance rather than jumping to a step.
// Switch expressions have access to the full context and to this step's own action
// output under the name "self".
type Step struct {
	ID        string            `json:"id"                 validate:"required" description:"Unique step identifier within this process. Used as a next target in switch cases."`
	Call      *Call             `json:"call,omitempty"                        description:"Describes the action to perform. Omit for switch-only (routing) steps."`
	TimeoutMs int               `json:"timeout_ms,omitempty"                  description:"Maximum execution time in milliseconds. 0 means no timeout."`
	OnlyOnce  *bool             `json:"only_once,omitempty"                   description:"When true, the engine guarantees at-most-once execution: retries are only allowed for pre.* errors (remote never reached) or on_error rules with not_reached:true. Defaults to false (retryable)."`
	OnError   []ErrorCase       `json:"on_error,omitempty"                    description:"Ordered error-routing rules evaluated when the call fails. First match wins."`
	Params    map[string]string `json:"params,omitempty"                      description:"Expression map evaluated against the current context to build the call's input. Keys become input field names."`
	Switch    SwitchMap         `json:"switch,omitempty"                      description:"Ordered routing rules evaluated after the call. Omit 'case' on the last entry for a catch-all. Required if the step has no call or needs non-linear flow."`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Versions are assigned by the server on apply; never include a version when submitting definitions.
type ProcessDefinition struct {
	Name        string             `json:"name"         validate:"required" description:"Unique process identifier."`
	Steps       []*Step            `json:"steps"        validate:"required,min=1,dive" description:"Ordered list of execution steps. Control advances linearly unless a switch case redirects."`
	InputSchema *schema.SchemaNode `json:"input_schema,omitempty"          description:"JSON Schema used to validate the input payload when starting a new instance."`
	Output      map[string]string  `json:"output,omitempty"                description:"Expression map evaluated at completion to produce the process output. Keys become output field names."`
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
		if s.Call == nil {
			continue
		}
		if s.Call.OutputSchema != nil {
			normalized, err := schema.Normalize(s.Call.OutputSchema)
			if err != nil {
				return fmt.Errorf("step %q call.output_schema: %w", s.ID, err)
			}
			s.Call.OutputSchema = normalized
		}
		if s.Call.ChildOutputSchema != nil {
			normalized, err := schema.Normalize(s.Call.ChildOutputSchema)
			if err != nil {
				return fmt.Errorf("step %q call.child_output_schema: %w", s.ID, err)
			}
			s.Call.ChildOutputSchema = normalized
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
	for _, s := range d.Steps {
		if err := validateStep(s, stepIDs); err != nil {
			return err
		}
	}
	return nil
}

func validateStep(s *Step, stepIDs map[string]struct{}) error {
	hasAction := s.Call != nil
	hasSwitch := len(s.Switch) > 0

	if !hasAction && !hasSwitch {
		return fmt.Errorf("step %q must have a call or a switch", s.ID)
	}
	if hasAction {
		switch s.Call.Type {
		case CallTypeREST:
			if s.Call.Endpoint == "" {
				return fmt.Errorf("step %q: call.endpoint is required for type %q", s.ID, s.Call.Type)
			}
		case CallTypeScript:
			if s.Call.Exec == "" {
				return fmt.Errorf("step %q: call.exec is required for type %q", s.ID, s.Call.Type)
			}
		case CallTypeChildProcess:
			if len(s.Call.Processes) == 0 {
				return fmt.Errorf("step %q: call.processes is required for type %q", s.ID, s.Call.Type)
			}
			for i, p := range s.Call.Processes {
				if p.Name == "" {
					return fmt.Errorf("step %q: call.processes[%d].name is required", s.ID, i)
				}
			}
		default:
			return fmt.Errorf("step %q: call.type must be one of: rest, script, child_process", s.ID)
		}
	}
	if hasSwitch {
		for i, c := range s.Switch {
			isLast := i == len(s.Switch)-1
			if c.Case == "" && !isLast {
				return fmt.Errorf("step %q switch: catch-all at index %d must be the last case (unreachable cases after it)", s.ID, i)
			}
			if c.Next != GotoEnd {
				if _, ok := stepIDs[c.Next]; !ok {
					return fmt.Errorf("step %q switch: next %q is not a known step", s.ID, c.Next)
				}
			}
		}
		if s.Switch[len(s.Switch)-1].Case != "" {
			return fmt.Errorf("step %q switch: last case must be a catch-all (omit 'case' to match unconditionally)", s.ID)
		}
	}
	onlyOnce := s.OnlyOnce != nil && *s.OnlyOnce
	for i, ec := range s.OnError {
		for _, pat := range ec.Code {
			if !validLikePattern(pat) {
				return fmt.Errorf("step %q on_error[%d]: code pattern must not be empty", s.ID, i)
			}
		}
		isLast := i == len(s.OnError)-1
		if len(ec.Code) == 0 && !isLast {
			return fmt.Errorf("step %q on_error[%d]: catch-all must be the last rule (unreachable rules after it)", s.ID, i)
		}
		if ec.Next != "" && ec.Next != GotoEnd {
			if _, ok := stepIDs[ec.Next]; !ok {
				return fmt.Errorf("step %q on_error[%d]: next %q is not a known step", s.ID, i, ec.Next)
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
	if s.Call != nil && s.Call.Type == CallTypeREST {
		for _, pat := range s.Call.AcceptedStatus {
			if !validAcceptedStatusPattern(pat) {
				return fmt.Errorf("step %q: accepted_status %q must be \"2xx\"/\"3xx\"/\"4xx\"/\"5xx\" or a 3-digit code", s.ID, pat)
			}
		}
	}
	if s.Call != nil {
		if err := checkSchemaDoc(fmt.Sprintf("step %q call.output_schema", s.ID), s.Call.OutputSchema); err != nil {
			return err
		}
		if err := checkSchemaDoc(fmt.Sprintf("step %q call.child_output_schema", s.ID), s.Call.ChildOutputSchema); err != nil {
			return err
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
func (c *Call) ValidateOutput(output any) error {
	return validateSchema(c.OutputSchema, output)
}

// ValidateChildOutput checks a single child's output against call.ChildOutputSchema. No-op if unset.
func (c *Call) ValidateChildOutput(output any) error {
	return validateSchema(c.ChildOutputSchema, output)
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
