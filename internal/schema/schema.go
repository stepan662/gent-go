package schema

import (
	"encoding/json"
	"maps"
)

// Schema is an immutable JSON Schema value.
// Every transformation method returns a new instance; the receiver is never modified.
type Schema struct {
	raw map[string]any
}

// Load wraps a raw schema map. The caller must not modify the map after calling Load.
func Load(raw map[string]any) Schema {
	return Schema{raw: raw}
}

// Parse parses a JSON-encoded schema.
func Parse(data []byte) (Schema, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return Schema{}, err
	}
	return Schema{raw: m}, nil
}

// Normalize returns a normalized copy with all $defs flattened to the root.
// The receiver is not modified.
func (s Schema) Normalize() (Schema, error) {
	cloned, err := deepClone(s.raw)
	if err != nil {
		return Schema{}, err
	}
	out, err := Normalize(cloned)
	if err != nil {
		return Schema{}, err
	}
	return Schema{raw: out}, nil
}

// Infer navigates a dot-path expression (e.g. "user.issues[0].value") and
// returns the subschema for the value at that path. The schema should be
// normalized before calling Infer so that $refs are resolvable.
func (s Schema) Infer(path string) (Schema, error) {
	defs, _ := s.raw["$defs"].(map[string]any)
	result, err := Navigate(s.raw, defs, path)
	if err != nil {
		return Schema{}, err
	}
	return Schema{raw: result}, nil
}

// IsSubset reports whether every value valid under s is also valid under super.
// Both schemas must be normalized.
func (s Schema) IsSubset(super Schema) bool {
	return IsSubset(s.raw, super.raw)
}

// WithDef returns a new Schema with the given definition added under $defs.
// The result should be re-normalized if the new def introduces its own $refs.
func (s Schema) WithDef(name string, def Schema) Schema {
	cloned := make(map[string]any, len(s.raw))
	maps.Copy(cloned, s.raw)
	existingDefs, _ := cloned["$defs"].(map[string]any)
	newDefs := make(map[string]any, len(existingDefs)+1)
	maps.Copy(newDefs, existingDefs)
	newDefs[name] = def.raw
	cloned["$defs"] = newDefs
	return Schema{raw: cloned}
}

// Raw returns the underlying map. The caller must not modify it.
func (s Schema) Raw() map[string]any {
	return s.raw
}

// MarshalJSON implements json.Marshaler.
func (s Schema) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.raw)
}

// deepClone returns a fully independent copy via JSON roundtrip.
func deepClone(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
