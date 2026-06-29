package schema

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// pathStep is one segment of a dot-path expression.
type pathStep struct {
	prop  string
	index int
}

// parsePath splits a path like "user.issues[0].value" into typed steps.
func parsePath(path string) ([]pathStep, error) {
	if path == "" {
		return nil, fmt.Errorf("path must not be empty")
	}
	var steps []pathStep
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return nil, fmt.Errorf("invalid path %q: empty segment", path)
		}
		for {
			open := strings.Index(segment, "[")
			if open == -1 {
				break
			}
			close := strings.Index(segment, "]")
			if close == -1 || close < open {
				return nil, fmt.Errorf("invalid path %q: unmatched '[' in segment %q", path, segment)
			}
			name := segment[:open]
			if name != "" {
				steps = append(steps, pathStep{prop: name})
			}
			idx, err := strconv.Atoi(segment[open+1 : close])
			if err != nil {
				return nil, fmt.Errorf("invalid path %q: non-integer index in %q", path, segment)
			}
			steps = append(steps, pathStep{index: idx})
			segment = segment[close+1:]
		}
		if segment != "" {
			steps = append(steps, pathStep{prop: segment})
		}
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("invalid path %q: no steps", path)
	}
	return steps, nil
}

// Navigate navigates a dot-path expression from the root of s, resolving $refs
// against defs, and returns the subschema at the end of the path.
func Navigate(s *SchemaNode, defs map[string]*SchemaNode, path string) (*SchemaNode, error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	return navigateSchema(s, defs, steps)
}

// LookupProperty returns the subschema for a single named property within s.
// Optional properties are returned wrapped as nullable.
func LookupProperty(s *SchemaNode, name string, defs map[string]*SchemaNode) (*SchemaNode, error) {
	resolved, err := Deref(s, defs)
	if err != nil {
		return nil, err
	}

	for _, kw := range []struct {
		name     string
		variants []*SchemaNode
	}{
		{"anyOf", resolved.AnyOf},
		{"oneOf", resolved.OneOf},
	} {
		if kw.variants == nil {
			continue
		}
		results := make([]*SchemaNode, 0, len(kw.variants))
		hadNull := false
		hadMiss := false
		for i, v := range kw.variants {
			if v == nil {
				return nil, fmt.Errorf("cannot access .%s: %s[%d] is nil", name, kw.name, i)
			}
			if IsNullType(v) {
				hadNull = true
				continue
			}
			r, err := LookupProperty(v, name, defs)
			if err != nil {
				hadMiss = true
				hadNull = true
				continue
			}
			results = append(results, r)
		}
		if len(results) == 0 {
			if hadMiss {
				return nil, fmt.Errorf("field %q not found in any %s variant", name, kw.name)
			}
			return &SchemaNode{Type: SchemaType{"null"}}, nil
		}
		var result *SchemaNode
		if allSame(results) {
			result = results[0]
		} else {
			cp := make([]*SchemaNode, len(results))
			copy(cp, results)
			if kw.name == "oneOf" {
				result = &SchemaNode{OneOf: cp}
			} else {
				result = &SchemaNode{AnyOf: cp}
			}
		}
		if hadNull {
			return WithNull(result), nil
		}
		return result, nil
	}

	if resolved.Properties == nil {
		return nil, fmt.Errorf("cannot access .%s: schema has no properties", name)
	}
	prop, ok := resolved.Properties[name]
	if !ok {
		return nil, fmt.Errorf("field %q not found in schema", name)
	}
	result, err := Deref(prop, defs)
	if err != nil {
		return nil, err
	}
	if !isRequired(resolved, name) {
		return WithNull(result), nil
	}
	return result, nil
}

// InferIndex returns the (nullable) element type for array index access on s.
// Always nullable because the index may be out of bounds at runtime.
func InferIndex(s *SchemaNode, defs map[string]*SchemaNode) (*SchemaNode, error) {
	resolved, err := Deref(s, defs)
	if err != nil {
		return nil, err
	}
	resolved = StripNull(resolved)

	for _, variants := range [][]*SchemaNode{resolved.AnyOf, resolved.OneOf} {
		if variants == nil {
			continue
		}
		results := make([]*SchemaNode, 0, len(variants))
		hadNull := false
		for _, v := range variants {
			if v == nil {
				continue
			}
			if IsNullType(v) {
				hadNull = true
				continue
			}
			r, err := InferIndex(v, defs)
			if err != nil {
				hadNull = true
				continue
			}
			results = append(results, r)
		}
		if len(results) == 0 {
			return &SchemaNode{Type: SchemaType{"null"}}, nil
		}
		var result *SchemaNode
		if allSame(results) {
			result = results[0]
		} else {
			result = &SchemaNode{AnyOf: results}
		}
		if hadNull && !HasNullType(result) {
			return WithNull(result), nil
		}
		return result, nil
	}

	if !resolved.Type.Contains("array") {
		t := ""
		if len(resolved.Type) > 0 {
			t = resolved.Type[0]
		}
		return nil, fmt.Errorf("index access [n] requires an array schema, got type %q", t)
	}
	if resolved.Items == nil {
		return &SchemaNode{}, nil
	}
	return WithNull(resolved.Items), nil
}

// Deref follows a $ref pointer if present, looking it up in defs.
// Returns s unchanged if no $ref is present.
func Deref(s *SchemaNode, defs map[string]*SchemaNode) (*SchemaNode, error) {
	if s == nil || s.Ref == "" {
		return s, nil
	}
	if defs == nil {
		return nil, fmt.Errorf("cannot resolve $ref %q: no defs available", s.Ref)
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(s.Ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref %q: only #/$defs/<name> is supported", s.Ref)
	}
	target, ok := defs[strings.TrimPrefix(s.Ref, prefix)]
	if !ok || target == nil {
		return nil, fmt.Errorf("$ref %q not found in defs", s.Ref)
	}
	return target, nil
}

// IsSecret reports whether s is a secret value (marked secret:true), looking
// through nullable / single-variant union wrappers so an optional or wrapped
// secret is still recognised.
func IsSecret(s *SchemaNode) bool {
	if s == nil {
		return false
	}
	if s.Secret {
		return true
	}
	for _, v := range s.OneOf {
		if IsSecret(v) {
			return true
		}
	}
	for _, v := range s.AnyOf {
		if IsSecret(v) {
			return true
		}
	}
	return false
}

// Taint returns a copy of s marked secret:true. It is used to taint the result of
// an expression that reads a secret value (conservatively, the whole value).
func Taint(s *SchemaNode) *SchemaNode {
	if s == nil {
		return &SchemaNode{Secret: true}
	}
	if s.Secret {
		return s
	}
	n := *s
	n.Secret = true
	return &n
}

// PathHitsSecret reports whether navigating path from s passes through (or ends
// at) a node marked secret — reading from inside a secret object is itself
// secret. Returns false if the path cannot be resolved.
func PathHitsSecret(s *SchemaNode, defs map[string]*SchemaNode, path string) bool {
	steps, err := parsePath(path)
	if err != nil {
		return false
	}
	cur, err := Deref(s, defs)
	if err != nil {
		return false
	}
	if IsSecret(cur) {
		return true
	}
	for _, step := range steps {
		if step.prop != "" {
			cur, err = LookupProperty(cur, step.prop, defs)
		} else {
			cur, err = InferIndex(cur, defs)
		}
		if err != nil {
			return false
		}
		if IsSecret(cur) {
			return true
		}
	}
	return false
}

// CollectSecrets appends to *out the string form of every value in value whose
// schema is marked secret, walking objects and arrays against node (resolving
// $refs and looking through nullable wrappers). It is the gather half of log
// redaction: the collected values are then scrubbed from free-form log text.
func CollectSecrets(value any, node *SchemaNode, defs map[string]*SchemaNode, out *[]string) {
	if node == nil || value == nil {
		return
	}
	resolved, err := Deref(node, defs)
	if err != nil {
		return
	}
	if IsSecret(resolved) {
		if s := fmt.Sprintf("%v", value); s != "" {
			*out = append(*out, s)
		}
		return
	}
	if len(resolved.Properties) == 0 && resolved.Items == nil && (len(resolved.OneOf) > 0 || len(resolved.AnyOf) > 0) {
		if stripped := StripNull(resolved); stripped != resolved {
			if d, derr := Deref(stripped, defs); derr == nil {
				resolved = d
			}
		}
	}
	switch v := value.(type) {
	case map[string]any:
		for k, val := range v {
			if prop, ok := resolved.Properties[k]; ok {
				CollectSecrets(val, prop, defs, out)
			}
		}
	case []any:
		if resolved.Items != nil {
			for _, el := range v {
				CollectSecrets(el, resolved.Items, defs, out)
			}
		}
	}
}

// Redact returns value with every field whose schema is marked secret replaced by
// "***", walking objects and arrays against node (resolving $refs and looking
// through nullable wrappers). Non-secret values pass through unchanged. Used to
// scrub secret-derived values before they cross a public boundary (API, logs).
func Redact(value any, node *SchemaNode, defs map[string]*SchemaNode) any {
	if node == nil || value == nil {
		return value
	}
	resolved, err := Deref(node, defs)
	if err != nil {
		return value
	}
	if IsSecret(resolved) {
		return "***"
	}
	// Look through a nullable / single-variant wrapper to the concrete shape.
	if len(resolved.Properties) == 0 && resolved.Items == nil && (len(resolved.OneOf) > 0 || len(resolved.AnyOf) > 0) {
		if stripped := StripNull(resolved); stripped != resolved {
			if d, derr := Deref(stripped, defs); derr == nil {
				resolved = d
			}
		}
	}
	switch v := value.(type) {
	case map[string]any:
		if len(resolved.Properties) == 0 {
			return value
		}
		out := make(map[string]any, len(v))
		for k, val := range v {
			if prop, ok := resolved.Properties[k]; ok {
				out[k] = Redact(val, prop, defs)
			} else {
				out[k] = val
			}
		}
		return out
	case []any:
		if resolved.Items == nil {
			return value
		}
		out := make([]any, len(v))
		for i, el := range v {
			out[i] = Redact(el, resolved.Items, defs)
		}
		return out
	default:
		return value
	}
}

// IsNullType reports whether s is exactly {type:"null"}.
func IsNullType(s *SchemaNode) bool {
	return s != nil && len(s.Type) == 1 && s.Type[0] == "null"
}

// HasNullType reports whether null is a possible type for s.
func HasNullType(s *SchemaNode) bool {
	if s == nil {
		return false
	}
	if s.Type.Contains("null") {
		return true
	}
	for _, v := range s.OneOf {
		if IsNullType(v) {
			return true
		}
	}
	for _, v := range s.AnyOf {
		if IsNullType(v) {
			return true
		}
	}
	return false
}

// WithNull makes s nullable. Simple types produce {type:[T,"null"]};
// complex schemas are wrapped in {oneOf:[s,{type:"null"}]}.
func WithNull(s *SchemaNode) *SchemaNode {
	if s == nil || isEmptyNode(s) {
		return s
	}
	if s.Type.Contains("null") {
		return s
	}
	for _, v := range s.OneOf {
		if IsNullType(v) {
			return s
		}
	}
	for _, v := range s.AnyOf {
		if IsNullType(v) {
			return s
		}
	}
	// Simple type without properties — widen type array to include null.
	if len(s.Type) >= 1 && s.Properties == nil {
		n := *s
		n.Type = make(SchemaType, len(s.Type)+1)
		copy(n.Type, s.Type)
		n.Type[len(s.Type)] = "null"
		return &n
	}
	return &SchemaNode{OneOf: []*SchemaNode{s, {Type: SchemaType{"null"}}}}
}

// StripNull removes null from a schema's possible types.
func StripNull(s *SchemaNode) *SchemaNode {
	if s == nil {
		return s
	}
	if len(s.Type) > 0 {
		var nonNull SchemaType
		for _, t := range s.Type {
			if t != "null" {
				nonNull = append(nonNull, t)
			}
		}
		if len(nonNull) == len(s.Type) {
			return s
		}
		n := *s
		n.Type = nonNull
		return &n
	}
	if len(s.OneOf) > 0 {
		var nonNull []*SchemaNode
		for _, v := range s.OneOf {
			if !IsNullType(v) {
				nonNull = append(nonNull, v)
			}
		}
		if len(nonNull) == len(s.OneOf) {
			return s
		}
		if len(nonNull) == 1 {
			return nonNull[0]
		}
		n := *s
		n.OneOf = nonNull
		return &n
	}
	if len(s.AnyOf) > 0 {
		var nonNull []*SchemaNode
		for _, v := range s.AnyOf {
			if !IsNullType(v) {
				nonNull = append(nonNull, v)
			}
		}
		if len(nonNull) == len(s.AnyOf) {
			return s
		}
		if len(nonNull) == 1 {
			return nonNull[0]
		}
		n := *s
		n.AnyOf = nonNull
		return &n
	}
	return s
}

func isEmptyNode(s *SchemaNode) bool {
	return s == nil || (len(s.Type) == 0 && s.Properties == nil && s.Required == nil &&
		s.Items == nil && s.OneOf == nil && s.AnyOf == nil && s.AllOf == nil &&
		s.Enum == nil && s.Ref == "" && s.Defs == nil && s.Minimum == nil &&
		s.Maximum == nil && s.MinLength == nil && s.MaxLength == nil &&
		s.MinItems == nil && s.MaxItems == nil)
}

func navigateSchema(s *SchemaNode, defs map[string]*SchemaNode, steps []pathStep) (*SchemaNode, error) {
	current := s
	for _, step := range steps {
		var err error
		if step.prop != "" {
			current, err = LookupProperty(current, step.prop, defs)
		} else {
			current, err = InferIndex(current, defs)
		}
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func isRequired(s *SchemaNode, name string) bool {
	for _, r := range s.Required {
		if r == name {
			return true
		}
	}
	return false
}

func allSame(schemas []*SchemaNode) bool {
	if len(schemas) == 0 {
		return true
	}
	first, _ := json.Marshal(schemas[0])
	for _, s := range schemas[1:] {
		other, _ := json.Marshal(s)
		if string(first) != string(other) {
			return false
		}
	}
	return true
}
