// Package schema provides a normalizer for a strict subset of JSON Schema.
//
// Supported subset:
//   - $defs at any nesting level (collected and flattened to root)
//   - $ref must start with "#/$defs/" — absolute, internal only
//   - No external refs, no relative paths, no $anchor
//
// Normalizer guarantees on output:
//   - $defs appear only at the root
//   - Only definitions reachable from the root are kept
//   - All $ref values are rewritten to "#/$defs/<name>"
package schema

import (
	"fmt"
	"strings"
)

type normContext struct {
	definitions map[string]*Def
	references  []*Ref
}

// Def holds a collected definition and its eventual flattened name.
type Def struct {
	OriginalName string
	NewName      string
	Schema       map[string]any
	Used         bool
}

// Ref holds a collected $ref and a pointer to the schema object containing it
// so the value can be rewritten in place later.
type Ref struct {
	RefValue string
	Schema   map[string]any
}

// ErrUnsupportedRef is returned when a $ref is outside the supported subset.
type ErrUnsupportedRef struct{ Ref string }

func (e ErrUnsupportedRef) Error() string {
	return fmt.Sprintf("unsupported $ref %q: only \"#/$defs/<name>\" refs are supported", e.Ref)
}

// Normalize flattens all $defs to the root, removes unused definitions,
// and rewrites $ref values to point to the new flat locations.
func Normalize(schema map[string]any) (map[string]any, error) {
	ctx := &normContext{
		definitions: make(map[string]*Def),
		references:  make([]*Ref, 0),
	}

	// Phase 1: collect all definitions and references from the whole tree.
	walkTree(schema, nil, func(node map[string]any, path []string) {
		if len(path) >= 2 && path[len(path)-2] == "$defs" {
			key := strings.Join(path, "/")
			if _, exists := ctx.definitions[key]; !exists {
				ctx.definitions[key] = &Def{OriginalName: path[len(path)-1], Schema: node}
			}
		}
		if refVal, ok := node["$ref"].(string); ok {
			ctx.references = append(ctx.references, &Ref{RefValue: refVal, Schema: node})
		}
	})

	// Phase 2: resolve each ref to its target definition and mark it as used.
	for _, ref := range ctx.references {
		targetPath, err := resolveRefPath(ref.RefValue)
		if err != nil {
			return nil, err
		}
		if def := ctx.resolveDef(targetPath); def != nil {
			def.Used = true
		}
	}

	// Phase 3: clean $defs and $id from every node in the tree.
	walkTree(schema, nil, func(node map[string]any, _ []string) {
		delete(node, "$id")
		delete(node, "$defs")
	})
	for _, def := range ctx.definitions {
		walkTree(def.Schema, nil, func(node map[string]any, _ []string) {
			delete(node, "$id")
			delete(node, "$defs")
		})
	}

	// Build root $defs from used definitions, resolving name collisions.
	rootDefs := make(map[string]any)
	for _, def := range ctx.definitions {
		if !def.Used {
			continue
		}
		def.NewName = getUniqueName(def.OriginalName, rootDefs)
		rootDefs[def.NewName] = def.Schema
	}

	// Rewrite $ref values to the new flat paths.
	for _, ref := range ctx.references {
		targetPath, _ := resolveRefPath(ref.RefValue)
		if def := ctx.resolveDef(targetPath); def != nil && def.Used {
			ref.Schema["$ref"] = "#/$defs/" + def.NewName
		}
	}

	if len(rootDefs) > 0 {
		schema["$defs"] = rootDefs
	}
	return schema, nil
}

// walkTree calls fn(node, path) for the given node and all nested schema nodes,
// covering every JSON Schema keyword that can contain sub-schemas.
func walkTree(schema map[string]any, path []string, fn func(map[string]any, []string)) {
	if schema == nil {
		return
	}
	fn(schema, path)

	for _, key := range []string{"$defs", "properties"} {
		if next, ok := schema[key].(map[string]any); ok {
			for name, v := range next {
				if sub, ok := v.(map[string]any); ok {
					walkTree(sub, append(cp(path), key, name), fn)
				}
			}
		}
	}
	for _, key := range []string{"items", "not", "additionalProperties", "if", "then", "else"} {
		if sub, ok := schema[key].(map[string]any); ok {
			walkTree(sub, append(cp(path), key), fn)
		}
	}
	for _, key := range []string{"oneOf", "anyOf", "allOf", "prefixItems"} {
		if arr, ok := schema[key].([]any); ok {
			for i, item := range arr {
				if sub, ok := item.(map[string]any); ok {
					walkTree(sub, append(cp(path), key, fmt.Sprintf("%d", i)), fn)
				}
			}
		}
	}
}

// resolveDef finds a definition by its absolute path, falling back to suffix
// matching for nested defs referenced by a short path (e.g. "$defs/Item" matches
// "$defs/Order/$defs/Item" when no root-level Item exists).
func (ctx *normContext) resolveDef(path string) *Def {
	if def, ok := ctx.definitions[path]; ok {
		return def
	}
	for key, def := range ctx.definitions {
		if strings.HasSuffix(key, "/"+path) {
			return def
		}
	}
	return nil
}

// resolveRefPath returns the path component after "#/" from a $ref value.
// Only refs starting with "#/$defs/" are accepted.
func resolveRefPath(ref string) (string, error) {
	if !strings.HasPrefix(ref, "#/$defs/") {
		return "", ErrUnsupportedRef{Ref: ref}
	}
	return strings.TrimPrefix(ref, "#/"), nil
}

func getUniqueName(name string, existing map[string]any) string {
	newName := name
	for i := 1; existing[newName] != nil; i++ {
		newName = fmt.Sprintf("%s_%d", name, i)
	}
	return newName
}

// cp returns a shallow copy of a string slice to avoid append aliasing.
func cp(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}
