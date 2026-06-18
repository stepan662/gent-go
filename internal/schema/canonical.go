package schema

import (
	"encoding/json"
	"sort"
)

// Canonicalize returns a structurally-canonical copy of s, suitable for
// type-equality by JSON comparison (the equality test used by the recursive
// type-inference fixpoint). Two schemas that denote the same type canonicalize
// to byte-identical JSON.
//
// It: canonicalizes Properties values and Items recursively; for each of
// oneOf/anyOf/allOf, canonicalizes the variants, flattens nested compositions of
// the *same* kind, and deduplicates and sorts them; collapses a single-variant
// composition to that variant; and, for a union (oneOf/anyOf) whose variants are
// all "simple" (a single primitive type, no other constraints) — including the
// nullable case — merges them into one sorted {type:[...]} array (allOf is an
// intersection and is never merged this way). Type and Required arrays are
// sorted and deduped. It is idempotent.
func Canonicalize(s *SchemaNode) *SchemaNode {
	if s == nil {
		return nil
	}
	n := *s

	if s.Properties != nil {
		props := make(map[string]*SchemaNode, len(s.Properties))
		for k, v := range s.Properties {
			props[k] = Canonicalize(v)
		}
		n.Properties = props
	}
	if s.Items != nil {
		n.Items = Canonicalize(s.Items)
	}
	n.Type = SchemaType(sortDedupStrings([]string(s.Type)))
	n.Required = sortDedupStrings(s.Required)
	n.OneOf = canonVariants(s.OneOf, kindOneOf)
	n.AnyOf = canonVariants(s.AnyOf, kindAnyOf)
	n.AllOf = canonVariants(s.AllOf, kindAllOf)

	return collapse(&n)
}

type compositionKind int

const (
	kindOneOf compositionKind = iota
	kindAnyOf
	kindAllOf
)

// canonVariants canonicalizes each variant, flattens a variant that is itself a
// pure composition of the same kind (oneOf-in-oneOf, allOf-in-allOf, …), then
// dedups and sorts by canonical JSON for a stable order.
func canonVariants(vs []*SchemaNode, kind compositionKind) []*SchemaNode {
	if len(vs) == 0 {
		return nil
	}
	flat := make([]*SchemaNode, 0, len(vs))
	for _, v := range vs {
		cv := Canonicalize(v)
		if cv == nil {
			continue
		}
		if inner, ok := pureComposition(cv, kind); ok {
			flat = append(flat, inner...)
		} else {
			flat = append(flat, cv)
		}
	}
	seen := make(map[string]struct{}, len(flat))
	out := make([]*SchemaNode, 0, len(flat))
	for _, v := range flat {
		key := nodeCanonJSON(v)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return nodeCanonJSON(out[i]) < nodeCanonJSON(out[j]) })
	return out
}

// collapse reduces a node that is purely a single composition (no other
// constraints) toward its simplest equivalent form: a single variant unwraps;
// for a union (oneOf/anyOf) whose variants are all simple primitives — including
// "null" — the variants merge into one sorted {type:[...]} array. An allOf is an
// intersection, so it only unwraps a singleton.
func collapse(n *SchemaNode) *SchemaNode {
	// Unions (oneOf/anyOf): a singleton unwraps and an all-simple union merges into
	// a {type:[...]} array; otherwise n already carries the canonical variants.
	if vs, ok := pureComposition(n, kindOneOf); ok {
		return collapseUnion(n, vs)
	}
	if vs, ok := pureComposition(n, kindAnyOf); ok {
		return collapseUnion(n, vs)
	}
	// allOf is an intersection: only a singleton unwraps; never merge simple variants.
	if vs, ok := pureComposition(n, kindAllOf); ok {
		if len(vs) == 1 {
			return vs[0]
		}
		return n
	}
	return n
}

func collapseUnion(n *SchemaNode, variants []*SchemaNode) *SchemaNode {
	if len(variants) == 1 {
		return variants[0]
	}
	if merged, ok := mergeSimpleVariants(variants); ok {
		return merged
	}
	return n
}

// pureComposition returns the variants of s if s carries exactly the given
// composition keyword and no other type-constraining fields, else (nil, false).
func pureComposition(s *SchemaNode, kind compositionKind) ([]*SchemaNode, bool) {
	if s == nil {
		return nil, false
	}
	if len(s.Type) > 0 || s.Properties != nil || s.Items != nil || len(s.Required) > 0 ||
		len(s.Enum) > 0 || s.Ref != "" {
		return nil, false
	}
	one, any, all := len(s.OneOf) > 0, len(s.AnyOf) > 0, len(s.AllOf) > 0
	switch kind {
	case kindOneOf:
		if one && !any && !all {
			return s.OneOf, true
		}
	case kindAnyOf:
		if any && !one && !all {
			return s.AnyOf, true
		}
	case kindAllOf:
		if all && !one && !any {
			return s.AllOf, true
		}
	}
	return nil, false
}

// mergeSimpleVariants merges a union whose every variant is one or more primitive
// types with no other constraints into one {type:[...]} node (sorted, deduped).
// Returns (nil, false) if any variant is not simple.
func mergeSimpleVariants(variants []*SchemaNode) (*SchemaNode, bool) {
	types := make([]string, 0, len(variants))
	for _, v := range variants {
		if !isSimpleType(v) {
			return nil, false
		}
		types = append(types, v.Type...)
	}
	return &SchemaNode{Type: SchemaType(sortDedupStrings(types))}, true
}

// isSimpleType reports whether s is one or more primitive types with no other
// type-constraining fields (the shape mergeSimpleVariants can fold into a type
// array — including an already-merged multi-entry {type:[...]}).
func isSimpleType(s *SchemaNode) bool {
	if s == nil || len(s.Type) == 0 {
		return false
	}
	return s.Properties == nil && s.Items == nil && len(s.Required) == 0 &&
		len(s.OneOf) == 0 && len(s.AnyOf) == 0 && len(s.AllOf) == 0 &&
		len(s.Enum) == 0 && s.Ref == ""
}

func sortDedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:0]
	var last string
	for i, s := range cp {
		if i == 0 || s != last {
			out = append(out, s)
			last = s
		}
	}
	return out
}

func nodeCanonJSON(s *SchemaNode) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Size is the byte length of s's canonical JSON — a cheap proxy for type
// complexity, used to bound the recursive-inference fixpoint against a
// non-converging type that grows without limit.
func Size(s *SchemaNode) int {
	return len(nodeCanonJSON(Canonicalize(s)))
}
