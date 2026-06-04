package schema

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// pathStep is one segment of a dot-path expression.
type pathStep struct {
	prop  string // non-empty for a named property access
	index int    // used when prop == "" (array index access)
}

// parsePath splits a path like "user.issues[0].value" into typed steps.
// Brackets immediately follow a segment name: "items[0]" → prop "items" then index 0.
func parsePath(path string) ([]pathStep, error) {
	if path == "" {
		return nil, fmt.Errorf("path must not be empty")
	}
	var steps []pathStep
	for _, segment := range strings.Split(path, ".") {
		if segment == "" {
			return nil, fmt.Errorf("invalid path %q: empty segment", path)
		}
		// Split off trailing [N] bracket(s).
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

// Navigate navigates a dot-path expression (e.g. "user.issues[0].value") from
// the root of s, resolving $refs against defs, and returns the subschema at
// the end of the path. The schema should be normalized before calling Navigate.
func Navigate(s map[string]any, defs map[string]any, path string) (map[string]any, error) {
	steps, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	return navigateSchema(s, defs, steps)
}

// LookupProperty returns the subschema for a single named property within s,
// resolving any $ref first. For anyOf/oneOf schemas it navigates each non-null
// variant and merges the results. Optional properties (absent from "required")
// are returned wrapped as nullable.
func LookupProperty(s map[string]any, name string, defs map[string]any) (map[string]any, error) {
	return lookupProperty(s, name, defs)
}

// InferIndex returns the (nullable) element type for array index access on s.
// Always nullable because the index may be out of bounds at runtime.
func InferIndex(s map[string]any, defs map[string]any) (map[string]any, error) {
	return inferIndex(s, defs)
}

// Deref follows a $ref pointer if present, looking it up in defs.
// Returns s unchanged if no $ref is present.
func Deref(s map[string]any, defs map[string]any) (map[string]any, error) {
	return derefNav(s, defs)
}

// navigateSchema walks steps through schema, resolving $refs against defs,
// and returns the subschema reached at the end of the path.
func navigateSchema(s map[string]any, defs map[string]any, steps []pathStep) (map[string]any, error) {
	current := s
	for _, step := range steps {
		var err error
		if step.prop != "" {
			current, err = lookupProperty(current, step.prop, defs)
		} else {
			current, err = inferIndex(current, defs)
		}
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

// lookupProperty looks up a named property within a (possibly ref-resolved) schema.
// For anyOf/oneOf it navigates each non-null variant and merges the results.
// Optional properties (not in "required") are returned wrapped as nullable.
func lookupProperty(s map[string]any, name string, defs map[string]any) (map[string]any, error) {
	resolved, err := derefNav(s, defs)
	if err != nil {
		return nil, err
	}

	for _, kw := range []string{"anyOf", "oneOf"} {
		variants, ok := resolved[kw].([]any)
		if !ok {
			continue
		}
		results := make([]any, 0, len(variants))
		hadNull := false
		hadMiss := false
		for i, v := range variants {
			varSchema, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("cannot access .%s: %s[%d] is not a schema object", name, kw, i)
			}
			if isNullType(varSchema) {
				hadNull = true
				continue
			}
			r, err := lookupProperty(varSchema, name, defs)
			if err != nil {
				hadMiss = true
				hadNull = true
				continue
			}
			results = append(results, r)
		}
		if len(results) == 0 {
			if hadMiss {
				return nil, fmt.Errorf("field %q not found in any %s variant", name, kw)
			}
			return map[string]any{"type": "null"}, nil
		}
		var result map[string]any
		if allSame(results) {
			result = results[0].(map[string]any)
		} else {
			result = map[string]any{kw: results}
		}
		if hadNull {
			return withNull(result), nil
		}
		return result, nil
	}

	props, _ := resolved["properties"].(map[string]any)
	if props == nil {
		return nil, fmt.Errorf("cannot access .%s: schema has no properties", name)
	}
	prop, ok := props[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("field %q not found in schema", name)
	}
	result, err := derefNav(prop, defs)
	if err != nil {
		return nil, err
	}
	if !isRequired(resolved, name) {
		return withNull(result), nil
	}
	return result, nil
}

// inferIndex returns the (nullable) element type for array index access.
// Always nullable because the index may be out of bounds at runtime.
func inferIndex(s map[string]any, defs map[string]any) (map[string]any, error) {
	resolved, err := derefNav(s, defs)
	if err != nil {
		return nil, err
	}
	resolved = stripNull(resolved)

	for _, kw := range []string{"anyOf", "oneOf"} {
		variants, ok := resolved[kw].([]any)
		if !ok {
			continue
		}
		results := make([]any, 0, len(variants))
		hadNull := false
		for _, v := range variants {
			varSchema, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if isNullType(varSchema) {
				hadNull = true
				continue
			}
			r, err := inferIndex(varSchema, defs)
			if err != nil {
				hadNull = true
				continue
			}
			results = append(results, r)
		}
		if len(results) == 0 {
			return map[string]any{"type": "null"}, nil
		}
		var result map[string]any
		if allSame(results) {
			result = results[0].(map[string]any)
		} else {
			result = map[string]any{kw: results}
		}
		if hadNull && !hasNullType(result) {
			return withNull(result), nil
		}
		return result, nil
	}

	t, _ := resolved["type"].(string)
	if t != "array" {
		return nil, fmt.Errorf("index access [n] requires an array schema, got type %q", t)
	}
	items, _ := resolved["items"].(map[string]any)
	if items == nil {
		return map[string]any{}, nil
	}
	return withNull(items), nil
}

// derefNav follows a $ref if present.
func derefNav(s map[string]any, defs map[string]any) (map[string]any, error) {
	ref, ok := s["$ref"].(string)
	if !ok {
		return s, nil
	}
	if defs == nil {
		return nil, fmt.Errorf("cannot resolve $ref %q: no defs available", ref)
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref %q: only #/$defs/<name> is supported", ref)
	}
	target, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q not found in defs", ref)
	}
	return target, nil
}

func isNullType(s map[string]any) bool {
	t, _ := s["type"].(string)
	return t == "null"
}

func hasNullType(s map[string]any) bool {
	switch t := s["type"].(type) {
	case string:
		return t == "null"
	case []any:
		for _, v := range t {
			if v == "null" {
				return true
			}
		}
	}
	return false
}

func isRequired(s map[string]any, name string) bool {
	req, _ := s["required"].([]any)
	for _, r := range req {
		if r == name {
			return true
		}
	}
	return false
}

// withNull makes a schema nullable. Simple types produce {"type":[X,"null"]};
// complex schemas are wrapped in {"oneOf":[schema,{"type":"null"}]}.
func withNull(s map[string]any) map[string]any {
	if len(s) == 0 {
		return s // {} = any, already includes null
	}
	if types, ok := s["type"].([]any); ok {
		for _, t := range types {
			if t == "null" {
				return s
			}
		}
	}
	if t, ok := s["type"].(string); ok && t != "null" {
		if _, hasProps := s["properties"]; !hasProps {
			result := make(map[string]any, len(s))
			for k, v := range s {
				result[k] = v
			}
			result["type"] = []any{t, "null"}
			return result
		}
	}
	return map[string]any{"oneOf": []any{s, map[string]any{"type": "null"}}}
}

// stripNull removes null from a schema's possible types.
func stripNull(s map[string]any) map[string]any {
	if types, ok := s["type"].([]any); ok {
		var nonNull []any
		for _, t := range types {
			if t != "null" {
				nonNull = append(nonNull, t)
			}
		}
		if len(nonNull) == len(types) {
			return s
		}
		result := make(map[string]any, len(s))
		for k, v := range s {
			result[k] = v
		}
		if len(nonNull) == 1 {
			result["type"] = nonNull[0]
		} else {
			result["type"] = nonNull
		}
		return result
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		variants, ok := s[kw].([]any)
		if !ok {
			continue
		}
		var nonNull []any
		for _, v := range variants {
			vs, ok := v.(map[string]any)
			if !ok {
				nonNull = append(nonNull, v)
				continue
			}
			if !isNullType(vs) {
				nonNull = append(nonNull, vs)
			}
		}
		if len(nonNull) == len(variants) {
			return s
		}
		if len(nonNull) == 1 {
			return nonNull[0].(map[string]any)
		}
		result := make(map[string]any, len(s))
		for k, v := range s {
			result[k] = v
		}
		result[kw] = nonNull
		return result
	}
	return s
}

func allSame(schemas []any) bool {
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
