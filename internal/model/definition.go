package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gent/internal/schema"

	"github.com/go-playground/validator/v10"
	"github.com/xeipuuv/gojsonschema"
)

// GotoEnd is the reserved switch Goto target that terminates the process instance.
// Use it as the target of a switch case (including "default") to signal completion.
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
	Name    string            `json:"name"`
	Version int               `json:"version,omitempty"` // 0 = latest
	Input   map[string]string `json:"input,omitempty"`   // expression map, evaluated like Step.Params
}

// Call describes how to invoke a step's action. It is a discriminated union on Type.
//   - "rest":          Endpoint (required), Headers (optional), OutputSchema (optional)
//   - "script":        Exec (required), OutputSchema (optional)
//   - "child_process": Processes (required), ChildOutputSchema (optional per-child schema)
//
// OutputSchema (rest/script): when set, the call response is validated and stored in
// context for downstream steps. Without it, the output is only available as "self"
// within the same step's switch and is not persisted.
//
// ChildOutputSchema (child_process): when set, each child's computed output is
// validated before the parent resumes.
type Call struct {
	Type      CallType          `json:"type"`
	Endpoint  string            `json:"endpoint,omitempty"`           // rest
	Headers   map[string]string `json:"headers,omitempty"`            // rest, values are expressions
	Exec      string            `json:"exec,omitempty"`               // script
	OutputSchema map[string]any `json:"output_schema,omitempty"`      // rest/script: validate & persist output
	Processes []ChildProcessEntry `json:"processes,omitempty"`        // child_process
	ChildOutputSchema map[string]any `json:"child_output_schema,omitempty"` // child_process: validate each child's output
}

// JSONSchemaBytes returns the JSON Schema for Call as a discriminated union
// so that OpenAPI reflection produces a proper oneOf instead of a flat object.
func (Call) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{
		"oneOf": [
			{
				"type": "object",
				"properties": {
					"type":          {"type": "string", "const": "rest"},
					"endpoint":      {"type": "string"},
					"headers":       {"type": "object", "additionalProperties": {"type": "string"}},
					"output_schema": {"type": "object"}
				},
				"required": ["type", "endpoint"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"properties": {
					"type":          {"type": "string", "const": "script"},
					"exec":          {"type": "string"},
					"output_schema": {"type": "object"}
				},
				"required": ["type", "exec"],
				"additionalProperties": false
			},
			{
				"type": "object",
				"properties": {
					"type": {"type": "string", "const": "child_process"},
					"processes": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"name":    {"type": "string"},
								"version": {"type": "integer"},
								"input":   {"type": "object", "additionalProperties": {"type": "string"}}
							},
							"required": ["name"],
							"additionalProperties": false
						},
						"minItems": 1
					},
					"child_output_schema": {"type": "object"}
				},
				"required": ["type", "processes"],
				"additionalProperties": false
			}
		],
		"discriminator": {"propertyName": "type"}
	}`), nil
}

// SwitchCase is a single entry in a Step's switch list: an expression evaluated
// against the process context (and this step's own output as "self"), and the ID
// of the step to jump to when the expression is true.
type SwitchCase struct {
	When string
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
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, c := range s {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(c.When)
		gotoWire := c.Goto
		if gotoWire != GotoEnd {
			gotoWire = "#" + gotoWire
		}
		val, _ := json.Marshal(gotoWire)
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func (s *SwitchMap) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("switch: expected object, got %v", t)
	}
	*s = (*s)[:0]
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		when, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("switch: key must be a string")
		}
		var goto_ string
		if err := dec.Decode(&goto_); err != nil {
			return err
		}
		if goto_ != GotoEnd {
			if !strings.HasPrefix(goto_, "#") {
				return fmt.Errorf("switch: goto %q must be %q or a step reference like \"#step-id\"", goto_, GotoEnd)
			}
			goto_ = goto_[1:]
		}
		*s = append(*s, SwitchCase{When: when, Goto: goto_})
	}
	_, err = dec.Token() // closing '}'
	return err
}

// JSONSchemaBytes returns the JSON Schema for SwitchMap so that OpenAPI
// reflection produces the correct schema for its wire format.
func (SwitchMap) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{"type":"object","additionalProperties":{"type":"string"}}`), nil
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
// A Goto value of GotoEnd ("$end") terminates the instance rather than jumping to a step.
// Switch expressions have access to the full context and to this step's own action
// output under the name "self".
type Step struct {
	ID        string            `json:"id"           validate:"required"`
	Call      *Call             `json:"call,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
	Retries   int               `json:"retries,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	Switch    SwitchMap         `json:"switch,omitempty"`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Once published, a version must never be mutated — create a new version instead.
type ProcessDefinition struct {
	Name        string            `json:"name"                 validate:"required"`
	Version     int               `json:"version"              validate:"min=1"`
	Steps       []*Step           `json:"steps"                validate:"required,min=1,dive"`
	InputSchema map[string]any    `json:"input_schema,omitempty"`
	Output      map[string]string `json:"output,omitempty"`        // expression map evaluated at completion, like Step.Params
	OutputSchema map[string]any   `json:"output_schema,omitempty"` // JSON Schema validating the computed output
}

// Normalize normalizes InputSchema and all step OutputSchemas in-place using the
// schema package (flattens $defs to root, removes unused definitions, rewrites $refs).
func (d *ProcessDefinition) Normalize() error {
	if len(d.InputSchema) > 0 {
		normalized, err := schema.Normalize(d.InputSchema)
		if err != nil {
			return fmt.Errorf("input_schema: %w", err)
		}
		d.InputSchema = normalized
	}
	if len(d.OutputSchema) > 0 {
		normalized, err := schema.Normalize(d.OutputSchema)
		if err != nil {
			return fmt.Errorf("output_schema: %w", err)
		}
		d.OutputSchema = normalized
	}
	for _, s := range d.Steps {
		if s.Call == nil {
			continue
		}
		if len(s.Call.OutputSchema) > 0 {
			normalized, err := schema.Normalize(s.Call.OutputSchema)
			if err != nil {
				return fmt.Errorf("step %q call.output_schema: %w", s.ID, err)
			}
			s.Call.OutputSchema = normalized
		}
		if len(s.Call.ChildOutputSchema) > 0 {
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
	if err := checkSchemaDoc("output_schema", d.OutputSchema); err != nil {
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
		last := s.Switch[len(s.Switch)-1]
		if last.When != "default" {
			return fmt.Errorf("step %q switch: last case must be \"default\"", s.ID)
		}
		for _, c := range s.Switch {
			if c.Goto != GotoEnd {
				if _, ok := stepIDs[c.Goto]; !ok {
					return fmt.Errorf("step %q switch: goto %q is not a known step", s.ID, c.Goto)
				}
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

func checkSchemaDoc(field string, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	if _, err := gojsonschema.NewSchema(gojsonschema.NewGoLoader(schema)); err != nil {
		return fmt.Errorf("%s is not a valid JSON Schema: %w", field, err)
	}
	return nil
}

// ValidateInput checks input data against InputSchema. No-op if InputSchema is nil.
func (d *ProcessDefinition) ValidateInput(input any) error {
	return validateSchema(d.InputSchema, input)
}

// ValidateOutput checks output data against OutputSchema. No-op if OutputSchema is nil.
func (d *ProcessDefinition) ValidateOutput(output any) error {
	return validateSchema(d.OutputSchema, output)
}

// ValidateOutput checks output data against call.OutputSchema. No-op if unset.
func (c *Call) ValidateOutput(output any) error {
	return validateSchema(c.OutputSchema, output)
}

// ValidateChildOutput checks a single child's output against call.ChildOutputSchema. No-op if unset.
func (c *Call) ValidateChildOutput(output any) error {
	return validateSchema(c.ChildOutputSchema, output)
}

func validateSchema(schema map[string]any, data any) error {
	if len(schema) == 0 {
		return nil
	}
	result, err := gojsonschema.Validate(
		gojsonschema.NewGoLoader(schema),
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
