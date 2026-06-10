package validation

import (
	"fmt"

	"gent/internal/model"
	"gent/internal/schema"
)

// DefinitionGetter looks up process definitions. *db.DB satisfies this interface.
type DefinitionGetter interface {
	GetDefinition(name string, version int) (*model.ProcessDefinition, error)
	LatestVersion(name string) (int, error)
}

// ValidateChildProcessRefs checks every child/child_parallel step in def:
//  1. The referenced process exists (version 0 resolves to latest).
//  2. The schema inferred from the input expressions is a subset of the child's InputSchema.
//
// currentVersion is the server-assigned version of def (used for self-reference detection).
// def must already be normalised (Generate calls Normalize internally, so call this after Generate).
func ValidateChildProcessRefs(def *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	defs, tasks, processInput, err := buildSchemaContext(def)
	if err != nil {
		return err
	}

	required, optional, mustErr, mayErr := computeContextSets(def.Steps)

	for _, s := range def.Steps {
		if s.Call == nil {
			continue
		}
		ctx := contextSchema(required[s.ID], optional[s.ID], tasks, processInput, mustErr[s.ID], mayErr[s.ID])
		if len(defs) > 0 {
			ctx = withDefs(ctx, defs)
		}

		switch s.Call.Type {
		case model.CallTypeChild:
			entry := model.ChildEntry{Name: s.Call.Name, Version: s.Call.Version, Input: s.Call.Input}
			if err := validateChildEntry(s.ID, "child", entry, ctx, defs, def, currentVersion, getter); err != nil {
				return err
			}
		case model.CallTypeChildParallel:
			for key, entry := range s.Call.Children {
				if err := validateChildEntry(s.ID, fmt.Sprintf("children[%q]", key), entry, ctx, defs, def, currentVersion, getter); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateChildEntry(stepID string, label string, p model.ChildEntry, ctx *schema.SchemaNode, defs map[string]*schema.SchemaNode, current *model.ProcessDefinition, currentVersion int, getter DefinitionGetter) error {
	prefix := fmt.Sprintf("step %q: %s", stepID, label)

	var child *model.ProcessDefinition
	var childVersion int
	if p.Name == current.Name && (p.Version == 0 || p.Version == currentVersion) {
		child = current
		childVersion = currentVersion
	} else {
		childVersion = p.Version
		if childVersion == 0 {
			v, err := getter.LatestVersion(p.Name)
			if err != nil {
				return fmt.Errorf("%s: %w", prefix, err)
			}
			childVersion = v
		}
		var err error
		child, err = getter.GetDefinition(p.Name, childVersion)
		if err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
	}

	if child.InputSchema == nil {
		return nil
	}

	inferred, err := inferObjectSchema(p.Input, ctx, func(name string) string {
		return fmt.Sprintf("%s input %q", prefix, name)
	})
	if err != nil {
		return err
	}

	if len(defs) > 0 {
		inferred = withDefs(inferred, defs)
	}
	inferred, err = schema.Normalize(inferred)
	if err != nil {
		return fmt.Errorf("%s: normalize inferred input: %w", prefix, err)
	}

	if !schema.IsSubset(inferred, child.InputSchema) {
		return fmt.Errorf("%s: input is not compatible with %q v%d input_schema", prefix, p.Name, childVersion)
	}
	return nil
}
