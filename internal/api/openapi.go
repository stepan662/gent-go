package api

func spec() map[string]interface{} {
	return map[string]interface{}{
		"openapi": "3.0.3",
		"info": map[string]interface{}{
			"title":       "gent",
			"description": "Minimalist business process orchestrator. Versioned JSON process definitions, SQLite persistence, multi-transport API (HTTP / TCP / UDS).",
			"version":     "1.0.0",
		},
		"paths": map[string]interface{}{
			"/definitions": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List all registered process definitions",
					"operationId": "listDefinitions",
					"tags":        []string{"Definitions"},
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content": jsonContent(map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"$ref": "#/components/schemas/DefinitionSummary"},
							}),
						},
					},
				},
				"put": map[string]interface{}{
					"summary":     "Register or update a process definition",
					"operationId": "putDefinition",
					"tags":        []string{"Definitions"},
					"requestBody": map[string]interface{}{
						"required": true,
						"content":  jsonContent(map[string]interface{}{"$ref": "#/components/schemas/ProcessDefinition"}),
					},
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "Saved",
							"content": jsonContent(map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"name":    map[string]interface{}{"type": "string"},
									"version": map[string]interface{}{"type": "integer"},
									"saved":   map[string]interface{}{"type": "boolean"},
								},
							}),
						},
						"400": errResponse(),
					},
				},
			},
			"/instances": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List process instances",
					"operationId": "listInstances",
					"tags":        []string{"Instances"},
					"parameters": []interface{}{
						map[string]interface{}{
							"name":        "status",
							"in":          "query",
							"description": "Filter by status",
							"required":    false,
							"schema": map[string]interface{}{
								"type": "string",
								"enum": []string{"running", "completed", "failed"},
							},
						},
					},
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content": jsonContent(map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"$ref": "#/components/schemas/InstanceStatus"},
							}),
						},
					},
				},
				"post": map[string]interface{}{
					"summary":     "Start a new process instance",
					"operationId": "startInstance",
					"tags":        []string{"Instances"},
					"requestBody": map[string]interface{}{
						"required": true,
						"content":  jsonContent(map[string]interface{}{"$ref": "#/components/schemas/StartInstanceReq"}),
					},
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "Instance created",
							"content":     jsonContent(map[string]interface{}{"$ref": "#/components/schemas/StartInstanceResp"}),
						},
						"400": errResponse(),
					},
				},
			},
			"/instances/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Get status of a process instance",
					"operationId": "getInstance",
					"tags":        []string{"Instances"},
					"parameters": []interface{}{
						map[string]interface{}{
							"name": "id", "in": "path", "required": true,
							"schema": map[string]interface{}{"type": "string", "format": "uuid"},
						},
					},
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content":     jsonContent(map[string]interface{}{"$ref": "#/components/schemas/InstanceStatus"}),
						},
						"404": errResponse(),
					},
				},
			},
		},
		"components": map[string]interface{}{
			"schemas": map[string]interface{}{
				"DefinitionSummary": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":    map[string]interface{}{"type": "string", "example": "order_pipeline"},
						"version": map[string]interface{}{"type": "integer", "example": 1},
					},
				},
				"Step": map[string]interface{}{
					"type":     "object",
					"required": []string{"type", "id"},
					"properties": map[string]interface{}{
						"type":       map[string]interface{}{"type": "string", "enum": []string{"task", "conditional"}},
						"id":         map[string]interface{}{"type": "string"},
						"transport":  map[string]interface{}{"type": "string", "enum": []string{"http", "tcp", "uds"}},
						"endpoint":   map[string]interface{}{"type": "string"},
						"timeout_ms": map[string]interface{}{"type": "integer", "example": 5000},
						"retries":    map[string]interface{}{"type": "integer", "example": 3},
						"condition":  map[string]interface{}{"type": "string", "example": "context.payment_success == true"},
						"then":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/components/schemas/Step"}},
						"else":       map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/components/schemas/Step"}},
					},
				},
				"ProcessDefinition": map[string]interface{}{
					"type":     "object",
					"required": []string{"name", "version", "steps"},
					"properties": map[string]interface{}{
						"name":    map[string]interface{}{"type": "string", "example": "order_pipeline"},
						"version": map[string]interface{}{"type": "integer", "example": 1},
						"steps": map[string]interface{}{
							"type":  "array",
							"items": map[string]interface{}{"$ref": "#/components/schemas/Step"},
						},
					},
				},
				"StartInstanceReq": map[string]interface{}{
					"type":     "object",
					"required": []string{"process"},
					"properties": map[string]interface{}{
						"process": map[string]interface{}{"type": "string", "example": "order_pipeline"},
						"version": map[string]interface{}{"type": "integer", "description": "Omit to use latest version"},
						"input":   map[string]interface{}{"type": "object", "additionalProperties": true},
					},
				},
				"StartInstanceResp": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"id":      map[string]interface{}{"type": "string", "format": "uuid"},
						"process": map[string]interface{}{"type": "string"},
						"version": map[string]interface{}{"type": "integer"},
						"status":  map[string]interface{}{"type": "string"},
					},
				},
				"InstanceStatus": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"id":          map[string]interface{}{"type": "string", "format": "uuid"},
						"process":     map[string]interface{}{"type": "string"},
						"version":     map[string]interface{}{"type": "integer"},
						"status":      map[string]interface{}{"type": "string", "enum": []string{"running", "completed", "failed"}},
						"retry_count": map[string]interface{}{"type": "integer"},
						"context":     map[string]interface{}{"type": "object", "additionalProperties": true},
						"error":       map[string]interface{}{"type": "string"},
						"created_at":  map[string]interface{}{"type": "string", "format": "date-time"},
						"updated_at":  map[string]interface{}{"type": "string", "format": "date-time"},
					},
				},
				"ErrorResp": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"ok":    map[string]interface{}{"type": "boolean", "example": false},
						"error": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
	}
}

func jsonContent(schema interface{}) map[string]interface{} {
	return map[string]interface{}{
		"application/json": map[string]interface{}{"schema": schema},
	}
}

func errResponse() map[string]interface{} {
	return map[string]interface{}{
		"description": "Error",
		"content":     jsonContent(map[string]interface{}{"$ref": "#/components/schemas/ErrorResp"}),
	}
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
