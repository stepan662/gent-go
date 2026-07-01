// Package schema provides a normalizer and type helpers for a strict subset of JSON Schema.
//
// Supported keywords: type, properties, required, items, oneOf, anyOf, enum,
// minimum, maximum, minLength, maxLength, minItems, maxItems, $ref, $defs, $anchor, $id.
// Any other keyword causes an unmarshal error.
//
// allOf is deliberately NOT accepted: it is an intersection that schema navigation
// (LookupProperty / InferIndex, and thus type inference and secret detection) cannot
// resolve a member through, so accepting it would be a half-supported keyword. The
// AllOf struct field remains only as an internal vehicle for bundling refs during
// normalization (see flattenNamedSchemas); it is never populated from user JSON.
package schema

import (
	"encoding/json"
	"fmt"
)

// SchemaType holds one or more JSON Schema type strings.
// A single type marshals as a JSON string; multiple types marshal as a JSON array.
type SchemaType []string

func (t SchemaType) MarshalJSON() ([]byte, error) {
	if len(t) == 1 {
		return json.Marshal(t[0])
	}
	return json.Marshal([]string(t))
}

func (t *SchemaType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*t = SchemaType{s}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("schema type must be a string or array of strings: %w", err)
	}
	*t = arr
	return nil
}

// Contains reports whether t includes the given type string.
func (t SchemaType) Contains(s string) bool {
	for _, v := range t {
		if v == s {
			return true
		}
	}
	return false
}

// allowedKeywords is the set of JSON Schema keywords accepted by SchemaNode.
// "default" is the standard annotation; "secret" is a genroc extension that is only
// meaningful inside a process config_schema (it drives log redaction) and ignored
// elsewhere.
var allowedKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "items": true,
	"oneOf": true, "anyOf": true, "enum": true,
	"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"minItems": true, "maxItems": true,
	"$ref": true, "$defs": true, "$anchor": true, "$id": true,
	"default": true, "secret": true,
	// "allOf" is intentionally omitted — see the package doc.
}

// SchemaNode is the typed representation of the supported JSON Schema subset.
// Any JSON key absent from allowedKeywords causes an UnmarshalJSON error.
type SchemaNode struct {
	Type       SchemaType              `json:"type,omitempty"`
	Properties map[string]*SchemaNode  `json:"properties,omitempty"`
	Required   []string                `json:"required,omitempty"`
	Items      *SchemaNode             `json:"items,omitempty"`
	OneOf      []*SchemaNode           `json:"oneOf,omitempty"`
	AnyOf      []*SchemaNode           `json:"anyOf,omitempty"`
	AllOf      []*SchemaNode           `json:"allOf,omitempty"`
	Enum       []any                   `json:"enum,omitempty"`
	Minimum    *float64                `json:"minimum,omitempty"`
	Maximum    *float64                `json:"maximum,omitempty"`
	MinLength  *int                    `json:"minLength,omitempty"`
	MaxLength  *int                    `json:"maxLength,omitempty"`
	MinItems   *int                    `json:"minItems,omitempty"`
	MaxItems   *int                    `json:"maxItems,omitempty"`
	Ref        string                  `json:"$ref,omitempty"`
	Defs       map[string]*SchemaNode  `json:"$defs,omitempty"`
	Anchor     string                  `json:"$anchor,omitempty"`
	ID         string                  `json:"$id,omitempty"`
	Default    any                     `json:"default,omitempty"`
	Secret     bool                    `json:"secret,omitempty"`
}

// UnmarshalJSON implements strict decoding: any JSON key not in allowedKeywords
// returns an error.
func (n *SchemaNode) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range raw {
		if !allowedKeywords[k] {
			return fmt.Errorf("unsupported schema keyword %q", k)
		}
	}
	type alias SchemaNode
	return json.Unmarshal(data, (*alias)(n))
}

// Schema is an immutable JSON Schema value backed by a SchemaNode.
// Every transformation method returns a new instance; the receiver is never modified.
//
// A Schema also carries the root $defs against which $refs resolve. Sub-schemas
// produced by navigation (Infer / At) keep the same root defs, so path-scoped
// operations — ValidateAt, SecretAt, Infer — resolve $refs even though the
// sub-node itself holds no $defs. This is what lets the class operate uniformly
// on any subpath, not just the root object.
type Schema struct {
	node *SchemaNode
	defs map[string]*SchemaNode
}

// defsOf returns a node's own $defs (its root defs when the node is a root).
func defsOf(n *SchemaNode) map[string]*SchemaNode {
	if n == nil {
		return nil
	}
	return n.Defs
}

// FromNode wraps a SchemaNode. The caller must not modify it after calling FromNode.
func FromNode(n *SchemaNode) Schema {
	return Schema{node: n, defs: defsOf(n)}
}

// Node returns the underlying SchemaNode. The caller must not modify it.
func (s Schema) Node() *SchemaNode {
	return s.node
}

// Parse parses a JSON-encoded schema, enforcing the strict keyword allowlist.
func Parse(data []byte) (Schema, error) {
	var n SchemaNode
	if err := json.Unmarshal(data, &n); err != nil {
		return Schema{}, err
	}
	return Schema{node: &n, defs: n.Defs}, nil
}

// Load wraps a raw schema map. Unrecognised keywords are silently dropped via a
// JSON roundtrip. Intended for programmatic schema construction and compatibility;
// use Parse for user-supplied schemas.
func Load(raw map[string]any) Schema {
	if len(raw) == 0 {
		return Schema{node: &SchemaNode{}}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return Schema{node: &SchemaNode{}}
	}
	type alias SchemaNode // bypass strict UnmarshalJSON
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return Schema{node: &SchemaNode{}}
	}
	n := SchemaNode(a)
	return Schema{node: &n, defs: n.Defs}
}

// Raw returns the schema as a plain map. Intended for compatibility and testing;
// avoid in new code.
func (s Schema) Raw() map[string]any {
	if s.node == nil {
		return nil
	}
	b, err := json.Marshal(s.node)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// Normalize returns a normalized copy with all $defs flattened to the root.
// The receiver is not modified.
func (s Schema) Normalize() (Schema, error) {
	cloned, err := deepClone(s.node)
	if err != nil {
		return Schema{}, err
	}
	out, err := Normalize(cloned)
	if err != nil {
		return Schema{}, err
	}
	return Schema{node: out, defs: out.Defs}, nil
}

// Infer navigates a dot-path expression (e.g. "user.issues[0].value") and
// returns the subschema for the value at that path, carrying the same root $defs
// so the result stays navigable/validatable. The schema should be normalized
// before calling Infer so that $refs are resolvable.
func (s Schema) Infer(path string) (Schema, error) {
	result, err := Navigate(s.node, s.defs, path)
	if err != nil {
		return Schema{}, err
	}
	return Schema{node: result, defs: s.defs}, nil
}

// At is an alias for Infer, reading better where the intent is "the schema at
// this subpath" rather than "the inferred type of this expression".
func (s Schema) At(path string) (Schema, error) {
	return s.Infer(path)
}

// IsSubset reports whether every value valid under s is also valid under super.
// Both schemas must be normalized.
func (s Schema) IsSubset(super Schema) bool {
	return IsSubset(s.node, super.node)
}

// WithDef returns a new Schema with the given definition added under $defs.
func (s Schema) WithDef(name string, def Schema) Schema {
	cloned, _ := deepClone(s.node)
	if cloned == nil {
		cloned = &SchemaNode{}
	}
	if cloned.Defs == nil {
		cloned.Defs = make(map[string]*SchemaNode)
	} else {
		newDefs := make(map[string]*SchemaNode, len(cloned.Defs)+1)
		for k, v := range cloned.Defs {
			newDefs[k] = v
		}
		cloned.Defs = newDefs
	}
	cloned.Defs[name] = def.node
	return Schema{node: cloned, defs: cloned.Defs}
}

// IsSecret reports whether this schema (the value at the root) is marked secret,
// looking through nullable / single-variant union wrappers.
func (s Schema) IsSecret() bool {
	return IsSecret(s.node)
}

// SecretAt reports whether the value at path is secret — either the path passes
// through a node marked secret, or it ends at one. Reading from inside a secret
// object is itself secret. Returns false if the path cannot be resolved.
func (s Schema) SecretAt(path string) bool {
	return PathHitsSecret(s.node, s.defs, path)
}

// Redact returns data with every field whose schema is marked secret replaced by
// "***", descending via the same navigation the type inference uses.
func (s Schema) Redact(data any) any {
	return Redact(data, s.node, s.defs)
}

// CollectSecrets returns the string form of every value in data whose schema is
// marked secret — the gather half of log redaction.
func (s Schema) CollectSecrets(data any) []string {
	var out []string
	CollectSecrets(data, s.node, s.defs, &out)
	return out
}

// MarshalJSON implements json.Marshaler.
func (s Schema) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.node)
}

// JSONSchemaBytes returns a permissive JSON Schema for OpenAPI reflection.
// The actual keyword restrictions are enforced at parse/unmarshal time, not at
// the spec level — keeping the API surface broad so callers can write schemas
// in standard JSON Schema syntax without TypeScript type errors.
func (SchemaNode) JSONSchemaBytes() ([]byte, error) {
	return []byte(`{"type":"object","additionalProperties":true}`), nil
}

// deepClone returns a fully independent copy via JSON roundtrip.
func deepClone(n *SchemaNode) (*SchemaNode, error) {
	if n == nil {
		return nil, nil
	}
	b, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	// Use alias to avoid the strict UnmarshalJSON on a round-trip of already-valid data.
	type alias SchemaNode
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	result := SchemaNode(a)
	return &result, nil
}
