package api

import (
	"encoding/json"
	"sync"
)

var (
	specOnce  sync.Once
	specBytes []byte
)

// buildSpec generates the OpenAPI spec from the action registry.
// No manual maintenance needed — edit actions.go to change the docs.
func buildSpec() []byte {
	specOnce.Do(func() {
		spec := map[string]interface{}{
			"openapi": "3.0.3",
			"info": map[string]interface{}{
				"title":       "gent",
				"description": "Minimalist business process orchestrator. All actions use the same JSON envelope over HTTP, TCP, and Unix sockets.",
				"version":     "1.0.0",
			},
			"paths": map[string]interface{}{
				"/": map[string]interface{}{
					"post": map[string]interface{}{
						"summary":     "Send an action",
						"description": "All actions share the same envelope: `action` selects the operation, `payload` carries the input, `id` is used for single-resource lookups.",
						"tags":        []string{"Actions"},
						"requestBody": map[string]interface{}{
							"required": true,
							"content": map[string]interface{}{
								"application/json": map[string]interface{}{
									"schema":   buildEnvelopeSchema(),
									"examples": buildExamples(),
								},
							},
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "Result of the action",
								"content": map[string]interface{}{
									"application/json": map[string]interface{}{
										"schema":   buildReplySchema(),
										"examples": buildRespExamples(),
									},
								},
							},
						},
					},
				},
			},
		}
		specBytes, _ = json.Marshal(spec)
	})
	return specBytes
}

// buildExamples generates request examples from the registry.
func buildExamples() map[string]interface{} {
	out := make(map[string]interface{}, len(registry))
	for _, a := range registry {
		env := map[string]interface{}{"action": a.Name}
		if a.Req != nil {
			env["payload"] = jsonRoundtrip(a.Req)
		}
		if a.ReqID != "" {
			env["id"] = a.ReqID
		}
		out[a.Name] = map[string]interface{}{
			"summary": a.Summary,
			"value":   env,
		}
	}
	return out
}

// buildRespExamples generates response examples from the registry.
func buildRespExamples() map[string]interface{} {
	out := make(map[string]interface{}, len(registry))
	for _, a := range registry {
		if a.Resp == nil {
			continue
		}
		out[a.Name] = map[string]interface{}{
			"summary": a.Summary,
			"value": map[string]interface{}{
				"ok":   true,
				"data": jsonRoundtrip(a.Resp),
			},
		}
	}
	return out
}

func buildEnvelopeSchema() map[string]interface{} {
	names := make([]string, len(registry))
	for i, a := range registry {
		names[i] = a.Name
	}
	return map[string]interface{}{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]interface{}{
			"action":  map[string]interface{}{"type": "string", "enum": names},
			"payload": map[string]interface{}{"type": "object"},
			"id":      map[string]interface{}{"type": "string", "format": "uuid"},
		},
	}
}

func buildReplySchema() map[string]interface{} {
	return map[string]interface{}{
		"type":     "object",
		"required": []string{"ok"},
		"properties": map[string]interface{}{
			"ok":    map[string]interface{}{"type": "boolean"},
			"data":  map[string]interface{}{"description": "Action result"},
			"error": map[string]interface{}{"type": "string"},
		},
	}
}

// jsonRoundtrip marshals v to JSON and back to a plain interface{} so it
// serialises cleanly into the spec without Go-specific type wrappers.
func jsonRoundtrip(v interface{}) interface{} {
	b, _ := json.Marshal(v)
	var out interface{}
	json.Unmarshal(b, &out)
	return out
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
