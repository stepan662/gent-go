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

// Transport identifies how the engine communicates with a service.
type Transport string

const (
	TransportHTTP Transport = "http"
	TransportTCP  Transport = "tcp"
	TransportUDS  Transport = "uds"
)

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
// Each step may have an action, a switch, or both — but at least one is required.
//
//   - Action-only (Transport+Endpoint set, Switch empty): executes the action and
//     advances to the next step in the list. If it is the last step, the instance
//     completes.
//   - Switch-only (Transport+Endpoint empty, Switch non-empty): evaluates the switch
//     to determine the next step without performing any external call.
//   - Both: executes the action first, then evaluates the switch.
//
// When Switch is present it must contain a "default" case as the last entry.
// Switch cases are evaluated in order; the first matching expression wins.
// The "default" case always matches and must be present to make control flow explicit.
// A Goto value of GotoEnd ("$end") terminates the instance rather than jumping to a step.
// Switch expressions have access to the full context and to this step's own action
// output under the name "self".
type Step struct {
	ID           string            `json:"id"                  validate:"required"`
	Transport    Transport         `json:"transport,omitempty"`
	Endpoint     string            `json:"endpoint,omitempty"`
	TimeoutMs    int               `json:"timeout_ms,omitempty"`
	Retries      int               `json:"retries,omitempty"`
	OutputSchema map[string]any    `json:"output_schema,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	Switch       SwitchMap         `json:"switch,omitempty"`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Once published, a version must never be mutated — create a new version instead.
type ProcessDefinition struct {
	Name        string         `json:"name"                 validate:"required"`
	Version     int            `json:"version"              validate:"min=1"`
	Steps       []*Step        `json:"steps"                validate:"required,min=1,dive"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
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
	for _, s := range d.Steps {
		if len(s.OutputSchema) > 0 {
			normalized, err := schema.Normalize(s.OutputSchema)
			if err != nil {
				return fmt.Errorf("step %q output_schema: %w", s.ID, err)
			}
			s.OutputSchema = normalized
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
	hasAction := s.Transport != "" || s.Endpoint != ""
	hasSwitch := len(s.Switch) > 0

	if !hasAction && !hasSwitch {
		return fmt.Errorf("step %q must have an action (transport + endpoint) or a switch", s.ID)
	}
	if s.Transport != "" && s.Endpoint == "" {
		return fmt.Errorf("step %q: endpoint is required", s.ID)
	}
	if s.Endpoint != "" && s.Transport == "" {
		return fmt.Errorf("step %q: transport is required", s.ID)
	}
	if s.Transport != "" {
		switch s.Transport {
		case TransportHTTP, TransportTCP, TransportUDS:
		default:
			return fmt.Errorf("step %q: transport must be one of: http, tcp, uds", s.ID)
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
	return checkSchemaDoc(fmt.Sprintf("step %q output_schema", s.ID), s.OutputSchema)
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
func (s *Step) ValidateOutput(output any) error {
	return validateSchema(s.OutputSchema, output)
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
