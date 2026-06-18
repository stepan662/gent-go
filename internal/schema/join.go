package schema

import "sort"

// Equal reports whether a and b denote the same type, compared in canonical form.
func Equal(a, b *SchemaNode) bool {
	return nodeCanonJSON(Canonicalize(a)) == nodeCanonJSON(Canonicalize(b))
}

// Join returns the least upper bound of a and b: the narrowest type that admits
// every value of either. Two objects are merged property-by-property (a key
// present on only one side becomes nullable, since it may be absent); anything
// else becomes a union. Nullability is preserved (the result is nullable if
// either input is). The result is canonical, which — together with Equal — gives
// the recursive-inference fixpoint a monotone, terminating accumulation step.
func Join(a, b *SchemaNode) *SchemaNode {
	if a == nil {
		return Canonicalize(b)
	}
	if b == nil {
		return Canonicalize(a)
	}
	if Equal(a, b) {
		return Canonicalize(a)
	}
	// A pure-null operand only contributes nullability; stripping it would leave an
	// empty schema, so handle it directly: join(x, null) = x made nullable.
	if IsNullType(a) {
		return Canonicalize(WithNull(b))
	}
	if IsNullType(b) {
		return Canonicalize(WithNull(a))
	}

	nullable := HasNullType(a) || HasNullType(b)
	na, nb := StripNull(a), StripNull(b)

	var res *SchemaNode
	switch {
	case isObjectType(na) && isObjectType(nb):
		res = joinObjects(na, nb)
	default:
		res = &SchemaNode{OneOf: []*SchemaNode{na, nb}}
	}
	if nullable {
		res = WithNull(res)
	}
	return Canonicalize(res)
}

func isObjectType(s *SchemaNode) bool {
	return s != nil && (s.Type.Contains("object") || s.Properties != nil)
}

// joinObjects merges two object schemas property-wise. A key present on both is
// joined; a key on only one side is kept but made nullable (it can be absent in
// the other). A property is required only when both sides require it.
func joinObjects(a, b *SchemaNode) *SchemaNode {
	keys := make(map[string]struct{}, len(a.Properties)+len(b.Properties))
	for k := range a.Properties {
		keys[k] = struct{}{}
	}
	for k := range b.Properties {
		keys[k] = struct{}{}
	}

	props := make(map[string]*SchemaNode, len(keys))
	for k := range keys {
		av, bv := a.Properties[k], b.Properties[k]
		switch {
		case av == nil:
			props[k] = WithNull(Canonicalize(bv))
		case bv == nil:
			props[k] = WithNull(Canonicalize(av))
		default:
			props[k] = Join(av, bv)
		}
	}

	var required []string
	for k := range keys {
		if a.Properties[k] != nil && b.Properties[k] != nil && isRequired(a, k) && isRequired(b, k) {
			required = append(required, k)
		}
	}
	sort.Strings(required)

	out := &SchemaNode{Type: SchemaType{"object"}, Properties: props}
	if len(required) > 0 {
		out.Required = required
	}
	return out
}
