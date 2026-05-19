package api

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/openapi31"
)

// Spec returns the OpenAPI 3.0 spec as JSON, built from the action registry.
// Can be called without starting the server — useful for generating static spec files.
func Spec() []byte { return buildSpec() }

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

const swaggerUI = `<!DOCTYPE html>
<html>
<head>
  <title>gent API</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  SwaggerUIBundle({
    url: "/openapi.json",
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
    layout: "BaseLayout",
    deepLinking: true,
  })
</script>
</body>
</html>`
