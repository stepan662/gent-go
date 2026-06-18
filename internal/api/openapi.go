package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"gent/internal/model"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi31"
)

// Spec returns the OpenAPI 3.0 spec as JSON, built from the action registry.
// Can be called without starting the server — useful for generating static spec files.
func Spec() []byte { return buildSpec() }

var (
	processSchemaOnce  sync.Once
	processSchemaBytes []byte
)

func buildProcessDefinitionSchema() []byte {
	processSchemaOnce.Do(func() {
		r := jsonschema.Reflector{}
		r.DefaultOptions = append(r.DefaultOptions,
			jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
				if !params.Processed || params.Field.Type == nil || params.ParentSchema == nil {
					return nil
				}
				tag := params.Field.Tag.Get("json")
				if strings.Contains(tag, "omitempty") || params.Field.Type.Kind() == reflect.Ptr {
					return nil
				}
				for _, r := range params.ParentSchema.Required {
					if r == params.Name {
						return nil
					}
				}
				params.ParentSchema.Required = append(params.ParentSchema.Required, params.Name)
				return nil
			}),
			jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
				if !params.Processed || params.PropertySchema == nil {
					return nil
				}
				if desc := params.Field.Tag.Get("description"); desc != "" {
					params.PropertySchema.WithDescription(desc)
				}
				return nil
			}),
		)
		s, err := r.Reflect(model.ProcessDefinition{})
		if err != nil {
			panic(fmt.Sprintf("processDefinitionSchema: %v", err))
		}
		b, _ := json.Marshal(s)
		processSchemaBytes = upgradeToDraft201909(b)
	})
	return processSchemaBytes
}

// upgradeToDraft201909 rewrites a swaggest-generated schema to JSON Schema
// draft 2019-09 so that $ref and description can coexist on the same node
// (in draft-07, $ref silently swallows all sibling keywords).
func upgradeToDraft201909(b []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return b
	}
	if defs, ok := root["definitions"]; ok {
		root["$defs"] = defs
		delete(root, "definitions")
	}
	root["$schema"] = "https://json-schema.org/draft/2019-09/schema"
	// Rewrite all $ref values from #/definitions/ to #/$defs/.
	rewriteRefs(root)
	out, _ := json.Marshal(root)
	return out
}

func rewriteRefs(v any) {
	switch node := v.(type) {
	case map[string]any:
		if ref, ok := node["$ref"].(string); ok {
			node["$ref"] = strings.ReplaceAll(ref, "#/definitions/", "#/$defs/")
		}
		for _, child := range node {
			rewriteRefs(child)
		}
	case []any:
		for _, child := range node {
			rewriteRefs(child)
		}
	}
}

var (
	specOnce  sync.Once
	specBytes []byte
)

func buildSpec() []byte {
	specOnce.Do(func() {
		r := openapi31.Reflector{}
		r.Spec = &openapi31.Spec{Openapi: "3.1.0"}

		desc := "Minimalist business process orchestrator. HTTP endpoints are generated from the action registry."
		r.Spec.Info.Title = "gent"
		r.Spec.Info.Description = &desc
		r.Spec.Info.Version = "1.0.0"

		// Fields are required by default; add json:",omitempty" or use a pointer type to opt out.
		r.DefaultOptions = append(r.DefaultOptions, jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
			if !params.Processed || params.Field.Type == nil || params.ParentSchema == nil {
				return nil
			}
			tag := params.Field.Tag.Get("json")
			if strings.Contains(tag, "omitempty") || params.Field.Type.Kind() == reflect.Ptr {
				return nil
			}
			for _, r := range params.ParentSchema.Required {
				if r == params.Name {
					return nil
				}
			}
			params.ParentSchema.Required = append(params.ParentSchema.Required, params.Name)
			return nil
		}))

		for _, a := range registry {
			addOperation(&r, a)
		}

		b, _ := r.Spec.MarshalJSON()
		// The hand-written Action schema and the Shape recursion reference the Shape
		// def by its process-schema JSON-Pointer (#/$defs/ModelShape). In an OpenAPI
		// 3.1 document, component schemas live under #/components/schemas, so rewrite
		// those refs to resolve here too.
		b = bytes.ReplaceAll(b, []byte("#/$defs/ModelShape"), []byte("#/components/schemas/ModelShape"))
		specBytes = b
	})
	return specBytes
}

func addOperation(r *openapi31.Reflector, a actionDef) {
	op, err := r.NewOperationContext(a.Method, a.Path)
	if err != nil {
		return
	}
	op.SetSummary(a.Summary)
	if len(a.Tags) > 0 {
		op.SetTags(a.Tags...)
	}

	// Request body (POST/PUT) — reflect zero value of the type to avoid
	// runtime values influencing additionalProperties inference.
	if a.Req != nil && a.Method != http.MethodGet {
		op.AddReqStructure(zeroOf(a.Req))
	}

	// Path and query parameters — struct with path/query tagged fields.
	if a.PathQuery != nil {
		op.AddReqStructure(a.PathQuery)
	}

	// Response data — reflect zero value of the type.
	if a.Resp != nil {
		op.AddRespStructure(zeroOf(a.Resp))
		op.AddRespStructure(Reply{}, func(cu *openapi.ContentUnit) {
			cu.HTTPStatus = http.StatusBadRequest
		})
	}

	if err := r.AddOperation(op); err != nil {
		panic(fmt.Sprintf("openapi: %s %s: %v", a.Method, a.Path, err))
	}
}

// zeroOf returns a zero value of the same type as v.
// This prevents runtime map/slice values from polluting the reflected schema
// (e.g. map[string]any{"id": 42} would otherwise infer additionalProperties:{type:integer}).
func zeroOf(v any) any {
	t := reflect.TypeOf(v)
	if t == nil {
		return v
	}
	if t.Kind() == reflect.Ptr {
		return reflect.New(t.Elem()).Interface()
	}
	return reflect.New(t).Elem().Interface()
}

// swaggerUIHTML renders a Swagger UI page pointing at the given spec URL and title.
func swaggerUIHTML(title, specURL string) string {
	return `<!DOCTYPE html>
<html>
<head>
  <title>` + title + `</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  SwaggerUIBundle({
    url: "` + specURL + `",
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true,
  })
</script>
</body>
</html>`
}
