package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Validate checks data against the schema and returns a normalized copy.
//
// Normalization, relative to the input:
//   - object properties not declared in the schema are dropped;
//   - a declared property that is absent is filled from its `default` if it has
//     one, and otherwise omitted (a missing required property is an error);
//   - every retained value is type- and constraint-checked (enum, minimum/maximum,
//     minLength/maxLength, minItems/maxItems).
//
// Types are checked strictly, with the one concession that JSON decodes all
// numbers to float64: an "integer" schema accepts any number with no fractional
// part. No other coercion is performed. The returned value is freshly built and
// shares no maps or slices with the input.
//
// A nil schema (or an empty {} schema) accepts and returns data unchanged.
// $refs resolve against the schema's own $defs, so the schema should be
// normalized (defs flattened to the root) before calling.
func (s Schema) Validate(data any) (any, error) {
	return conform(s.node, s.defs, data, "")
}

// ValidateAt validates data against the subschema at path (e.g. "outputs.taskA")
// and returns the normalized value. It is Infer(path) followed by Validate, so
// the value is checked against the declared shape at that path with root $defs
// resolved. Optional path segments are treated as nullable, matching Infer.
func (s Schema) ValidateAt(path string, data any) (any, error) {
	sub, err := s.Infer(path)
	if err != nil {
		return nil, err
	}
	return sub.Validate(data)
}

// Validate is the free-function form of Schema.Validate, operating directly on a
// SchemaNode. $defs are read from node (the root), so a normalized schema is
// expected.
func Validate(node *SchemaNode, data any) (any, error) {
	var defs map[string]*SchemaNode
	if node != nil {
		defs = node.Defs
	}
	return conform(node, defs, data, "")
}

// conform is the recursive validator/normalizer. path is the dotted location of
// data within the root value (empty at the root), used only for error messages.
func conform(node *SchemaNode, defs map[string]*SchemaNode, data any, path string) (any, error) {
	resolved, err := Deref(node, defs)
	if err != nil {
		return nil, err
	}
	if resolved == nil || isEmptyNode(resolved) {
		return data, nil // unconstrained — pass through untouched
	}

	// Combinators take precedence: a nullable complex value is modelled as
	// oneOf:[X, {type:null}], and discriminated unions as anyOf/oneOf of objects.
	if len(resolved.AnyOf) > 0 {
		return conformUnion(resolved.AnyOf, defs, data, path, false)
	}
	if len(resolved.OneOf) > 0 {
		return conformUnion(resolved.OneOf, defs, data, path, true)
	}

	if err := checkType(resolved, data, path); err != nil {
		return nil, err
	}
	if len(resolved.Enum) > 0 && !enumContains(resolved.Enum, data) {
		return nil, fmt.Errorf("%svalue is not one of the permitted enum values", at(path))
	}

	switch v := data.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		if isObjectSchema(resolved) {
			return conformObject(resolved, defs, v, path)
		}
		return v, nil
	case []any:
		if resolved.Items != nil || resolved.Type.Contains("array") {
			return conformArray(resolved, defs, v, path)
		}
		return v, nil
	default:
		if err := checkScalar(resolved, data, path); err != nil {
			return nil, err
		}
		return data, nil
	}
}

// conformObject keeps only declared properties, fills defaults for absent
// optionals, errors on absent required, and recurses into present values.
func conformObject(node *SchemaNode, defs map[string]*SchemaNode, v map[string]any, path string) (any, error) {
	required := make(map[string]bool, len(node.Required))
	for _, r := range node.Required {
		required[r] = true
	}
	out := make(map[string]any, len(node.Properties))
	for name, prop := range node.Properties {
		val, present := v[name]
		if !present {
			if required[name] {
				return nil, fmt.Errorf("%srequired property %q is missing", at(path), name)
			}
			if def := propDefault(prop, defs); def != nil {
				out[name] = cloneJSON(def)
			}
			continue // absent optional without a default is omitted
		}
		norm, err := conform(prop, defs, val, join(path, name))
		if err != nil {
			return nil, err
		}
		out[name] = norm
	}
	return out, nil
}

// conformArray validates length bounds and recurses into each element.
func conformArray(node *SchemaNode, defs map[string]*SchemaNode, arr []any, path string) (any, error) {
	if node.MinItems != nil && len(arr) < *node.MinItems {
		return nil, fmt.Errorf("%sarray has %d items, fewer than minItems %d", at(path), len(arr), *node.MinItems)
	}
	if node.MaxItems != nil && len(arr) > *node.MaxItems {
		return nil, fmt.Errorf("%sarray has %d items, more than maxItems %d", at(path), len(arr), *node.MaxItems)
	}
	out := make([]any, len(arr))
	for i, el := range arr {
		norm, err := conform(node.Items, defs, el, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		out[i] = norm
	}
	return out, nil
}

// conformUnion normalizes data against a oneOf/anyOf. anyOf returns the first
// branch that validates; oneOf requires exactly one branch to validate (matching
// zero or several is an error).
func conformUnion(branches []*SchemaNode, defs map[string]*SchemaNode, data any, path string, exactlyOne bool) (any, error) {
	var (
		firstErr error
		match    any
		matches  int
	)
	for _, b := range branches {
		res, err := conform(b, defs, data, path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !exactlyOne {
			return res, nil // anyOf: first match wins
		}
		match = res
		matches++
	}
	if matches == 0 {
		return nil, fmt.Errorf("%svalue does not match any of the permitted variants: %v", at(path), firstErr)
	}
	if matches > 1 {
		return nil, fmt.Errorf("%svalue matches %d oneOf variants; exactly one is required", at(path), matches)
	}
	return match, nil
}

// checkType verifies data's JSON type is permitted by node.Type. An empty Type is
// unconstrained. "integer" accepts an integral number; "number" accepts any number.
func checkType(node *SchemaNode, data any, path string) error {
	if len(node.Type) == 0 {
		return nil
	}
	for _, t := range node.Type {
		if valueHasType(data, t) {
			return nil
		}
	}
	return fmt.Errorf("%sexpected type %s, got %s", at(path), strings.Join(node.Type, "|"), jsonTypeName(data))
}

func valueHasType(data any, t string) bool {
	switch t {
	case "null":
		return data == nil
	case "boolean":
		_, ok := data.(bool)
		return ok
	case "string":
		_, ok := data.(string)
		return ok
	case "object":
		_, ok := data.(map[string]any)
		return ok
	case "array":
		_, ok := data.([]any)
		return ok
	case "number":
		_, ok := asFloat(data)
		return ok
	case "integer":
		return isIntegral(data)
	default:
		return false
	}
}

// checkScalar applies the scalar constraints: numeric range and string length.
func checkScalar(node *SchemaNode, data any, path string) error {
	if f, ok := asFloat(data); ok {
		if node.Minimum != nil && f < *node.Minimum {
			return fmt.Errorf("%svalue %v is less than minimum %v", at(path), f, *node.Minimum)
		}
		if node.Maximum != nil && f > *node.Maximum {
			return fmt.Errorf("%svalue %v is greater than maximum %v", at(path), f, *node.Maximum)
		}
	}
	if s, ok := data.(string); ok {
		n := utf8.RuneCountInString(s)
		if node.MinLength != nil && n < *node.MinLength {
			return fmt.Errorf("%sstring length %d is less than minLength %d", at(path), n, *node.MinLength)
		}
		if node.MaxLength != nil && n > *node.MaxLength {
			return fmt.Errorf("%sstring length %d is greater than maxLength %d", at(path), n, *node.MaxLength)
		}
	}
	return nil
}

// propDefault returns the default for a property, following a lone $ref to its
// target if the property node itself carries none.
func propDefault(prop *SchemaNode, defs map[string]*SchemaNode) any {
	if prop == nil {
		return nil
	}
	if prop.Default != nil {
		return prop.Default
	}
	if prop.Ref != "" {
		if target, err := Deref(prop, defs); err == nil && target != nil {
			return target.Default
		}
	}
	return nil
}

// isObjectSchema reports whether node describes an object (so a map value should
// be pruned to declared properties rather than passed through).
func isObjectSchema(node *SchemaNode) bool {
	return node.Type.Contains("object") || node.Properties != nil || node.Required != nil
}

// enumContains reports whether data equals any enum member, comparing by
// canonical JSON encoding so 1 and 1.0 (and nested values) compare equal.
func enumContains(enum []any, data any) bool {
	db, err := json.Marshal(data)
	if err != nil {
		return false
	}
	for _, e := range enum {
		if eb, err := json.Marshal(e); err == nil && string(eb) == string(db) {
			return true
		}
	}
	return false
}

// asFloat returns data as a float64 if it is any numeric kind.
func asFloat(data any) (float64, bool) {
	switch n := data.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// isIntegral reports whether data is a number with no fractional part.
func isIntegral(data any) bool {
	switch n := data.(type) {
	case int, int64, int32:
		return true
	case float64:
		return !math.IsInf(n, 0) && !math.IsNaN(n) && n == math.Trunc(n)
	case float32:
		f := float64(n)
		return f == math.Trunc(f)
	case json.Number:
		if _, err := n.Int64(); err == nil {
			return true
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f)
	default:
		return false
	}
}

// jsonTypeName names data's JSON type for error messages.
func jsonTypeName(data any) string {
	switch data.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		if _, ok := asFloat(data); ok {
			if isIntegral(data) {
				return "integer"
			}
			return "number"
		}
		return fmt.Sprintf("%T", data)
	}
}

// cloneJSON deep-copies a JSON value so a schema default can be handed out
// without sharing mutable state with the schema.
func cloneJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// at renders a path prefix for an error message ("" at the root).
func at(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}

// join extends a path with a child property name.
func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
