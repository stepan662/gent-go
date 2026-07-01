package schema

import "fmt"

// CheckDoc reports whether node is a well-formed schema document in the supported
// subset: every $ref resolves against the root $defs, combinator and property
// entries are non-nil, and any paired numeric/length/item bounds are ordered.
//
// Keyword validity is already guaranteed by SchemaNode's strict UnmarshalJSON, so
// CheckDoc only catches the structural errors that survive parsing — chiefly an
// unresolvable $ref. It replaces the previous gojsonschema.NewSchema compile step.
func CheckDoc(node *SchemaNode) error {
	if node == nil {
		return nil
	}
	return checkDoc(node, node.Defs, map[*SchemaNode]bool{})
}

func checkDoc(node *SchemaNode, defs map[string]*SchemaNode, seen map[*SchemaNode]bool) error {
	if node == nil || seen[node] {
		return nil
	}
	seen[node] = true

	if node.Ref != "" {
		if _, err := Deref(node, defs); err != nil {
			return err
		}
	}
	if node.Minimum != nil && node.Maximum != nil && *node.Minimum > *node.Maximum {
		return fmt.Errorf("minimum %v exceeds maximum %v", *node.Minimum, *node.Maximum)
	}
	if node.MinLength != nil && node.MaxLength != nil && *node.MinLength > *node.MaxLength {
		return fmt.Errorf("minLength %d exceeds maxLength %d", *node.MinLength, *node.MaxLength)
	}
	if node.MinItems != nil && node.MaxItems != nil && *node.MinItems > *node.MaxItems {
		return fmt.Errorf("minItems %d exceeds maxItems %d", *node.MinItems, *node.MaxItems)
	}

	for name, p := range node.Properties {
		if p == nil {
			return fmt.Errorf("property %q is null", name)
		}
		if err := checkDoc(p, defs, seen); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := checkDoc(node.Items, defs, seen); err != nil {
		return fmt.Errorf("items: %w", err)
	}
	for i, v := range node.OneOf {
		if v == nil {
			return fmt.Errorf("oneOf[%d] is null", i)
		}
		if err := checkDoc(v, defs, seen); err != nil {
			return err
		}
	}
	for i, v := range node.AnyOf {
		if v == nil {
			return fmt.Errorf("anyOf[%d] is null", i)
		}
		if err := checkDoc(v, defs, seen); err != nil {
			return err
		}
	}
	for name, d := range node.Defs {
		if err := checkDoc(d, defs, seen); err != nil {
			return fmt.Errorf("$defs.%s: %w", name, err)
		}
	}
	return nil
}
