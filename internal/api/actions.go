package api

import (
	"encoding/json"
	"net/http"

	"gent/internal/model"
)

// actionDef is the single source of truth for one API action.
// It drives three things simultaneously:
//   - HTTP routing (Method + Path)
//   - TCP/UDS envelope dispatch (Name)
//   - OpenAPI documentation (schemas reflected from Go types)
type actionDef struct {
	Name    string
	Method  string
	Path    string
	Summary string
	Tags    []string

	// Req is a zero-value of the request body type (nil = no body).
	Req any

	// PathQuery is a struct with path/query tagged fields for OpenAPI parameter generation.
	PathQuery any

	// Resp is a zero-value of the response data type.
	Resp any

	// fromHTTP extracts an Envelope from an HTTP request.
	// nil = default: decode body as JSON payload.
	fromHTTP func(r *http.Request) (Envelope, error)

	// handle is the actual handler, shared by HTTP, TCP, and UDS.
	handle func(h *Handlers, env Envelope) Reply
}

// envelope builds an Envelope from an HTTP request using the action's fromHTTP func.
func (a actionDef) envelope(r *http.Request) (Envelope, error) {
	if a.fromHTTP != nil {
		return a.fromHTTP(r)
	}
	var payload json.RawMessage
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return Envelope{}, err
		}
	}
	return Envelope{Action: a.Name, Payload: payload}, nil
}

// registry is the authoritative list of all actions.
// Order here determines order in Swagger.
var registry = func() []actionDef {
	v1 := 1
	return []actionDef{
		{
			Name:    "put_definition",
			Method:  http.MethodPut,
			Path:    "/definitions",
			Summary: "Register or update a process definition",
			Tags:    []string{"Definitions"},
			Req: model.ProcessDefinition{
				Name:    "order_pipeline",
				Version: 1,
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"order_id": map[string]any{"type": "integer"},
					},
					"required": []string{"order_id"},
				},
				Steps: []*model.Step{
					{
						Type: model.StepTypeTask, ID: "charge",
						Transport: model.TransportHTTP, Endpoint: "http://localhost:9001/charge",
						TimeoutMs: 5000, Retries: 3,
						OutputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"charged": map[string]any{"type": "boolean"},
							},
						},
					},
					{
						Type: model.StepTypeConditional, ID: "check_payment",
						Condition: "context.charged == true",
						Then: []*model.Step{{
							Type: model.StepTypeTask, ID: "ship",
							Transport: model.TransportHTTP, Endpoint: "http://localhost:9002/ship",
							TimeoutMs: 3000, Retries: 2,
						}},
						Else: []*model.Step{{
							Type: model.StepTypeTask, ID: "refund",
							Transport: model.TransportHTTP, Endpoint: "http://localhost:9003/refund",
							TimeoutMs: 3000, Retries: 1,
						}},
					},
				},
			},
			Resp: map[string]any{"name": "order_pipeline", "version": 1, "saved": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putDefinition(env.Payload)
			},
		},
		{
			Name:    "list_definitions",
			Method:  http.MethodGet,
			Path:    "/definitions",
			Summary: "List all registered process definitions",
			Tags:    []string{"Definitions"},
			Resp:    []DefinitionSummary{{Name: "order_pipeline", Version: 1}},
			fromHTTP: func(_ *http.Request) (Envelope, error) {
				return Envelope{Action: "list_definitions"}, nil
			},
			handle: func(h *Handlers, _ Envelope) Reply {
				return h.listDefinitions()
			},
		},
		{
			Name:    "start_instance",
			Method:  http.MethodPost,
			Path:    "/instances",
			Summary: "Start a new process instance (omit version to use latest)",
			Tags:    []string{"Instances"},
			Req: func() StartInstanceReq {
				input := any(map[string]any{"order_id": 42})
				return StartInstanceReq{Process: "order_pipeline", Version: &v1, Input: &input}
			}(),
			Resp: StartInstanceResp{
				ID: "550e8400-e29b-41d4-a716-446655440000", Process: "order_pipeline",
				Version: 1, Status: model.StatusRunning,
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.startInstance(env.Payload)
			},
		},
		{
			Name:    "list_instances",
			Method:  http.MethodGet,
			Path:    "/instances",
			Summary: "List process instances",
			Tags:    []string{"Instances"},
			PathQuery: struct {
				Status string `query:"status" enum:"running,completed,failed" description:"Filter by status"`
			}{},
			Resp: []InstanceStatusResp{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListInstancesReq{Status: r.URL.Query().Get("status")})
				return Envelope{Action: "list_instances", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listInstances(env.Payload)
			},
		},
		{
			Name:    "get_instance",
			Method:  http.MethodGet,
			Path:    "/instances/{id}",
			Summary: "Get status of a process instance",
			Tags:    []string{"Instances"},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Resp: InstanceStatusResp{
				ID: "550e8400-e29b-41d4-a716-446655440000", Process: "order_pipeline",
				Version: 1, Status: model.StatusCompleted,
				Context: map[string]any{"order_id": 42, "charged": true},
			},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				return Envelope{Action: "get_instance", ID: r.PathValue("id")}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.getInstance(env.ID)
			},
		},
	}
}()
