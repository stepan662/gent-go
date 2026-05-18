package api

import (
	"gent/internal/model"
)

// actionDef is the single source of truth for one API action.
// Adding an entry here automatically updates both the request dispatcher and Swagger docs.
type actionDef struct {
	Name    string
	Summary string
	// Req is a concrete example of the request payload (nil = no payload).
	// Used for Swagger examples and JSON schema inference.
	// Use realistic values — this is what shows up in the docs.
	Req interface{}
	// ReqID is set for actions that identify a resource by ID instead of a payload.
	ReqID string
	// Resp is a concrete example of the data field in the Reply.
	Resp interface{}
	// handle is the actual handler.
	handle func(h *Handlers, env Envelope) Reply
}

// registry is the authoritative list of all actions.
// Order here determines order in Swagger.
var registry = func() []actionDef {
	v1 := 1
	return []actionDef{
		{
			Name:    "put_definition",
			Summary: "Register or update a process definition",
			Req: model.ProcessDefinition{
				Name:    "order_pipeline",
				Version: 1,
				Steps: []*model.Step{
					{
						Type:      model.StepTypeTask,
						ID:        "charge",
						Transport: model.TransportHTTP,
						Endpoint:  "http://localhost:9001/charge",
						TimeoutMs: 5000,
						Retries:   3,
					},
					{
						Type:      model.StepTypeConditional,
						ID:        "check_payment",
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
			Resp: map[string]interface{}{"name": "order_pipeline", "version": 1, "saved": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putDefinition(env.Payload)
			},
		},
		{
			Name:    "list_definitions",
			Summary: "List all registered process definitions",
			Resp:    []DefinitionSummary{{Name: "order_pipeline", Version: 1}},
			handle: func(h *Handlers, _ Envelope) Reply {
				return h.listDefinitions()
			},
		},
		{
			Name:    "start_instance",
			Summary: "Start a new process instance (omit version for latest)",
			Req: StartInstanceReq{
				Process: "order_pipeline",
				Version: &v1,
				Input:   map[string]interface{}{"order_id": 42},
			},
			Resp: StartInstanceResp{
				ID:      "550e8400-e29b-41d4-a716-446655440000",
				Process: "order_pipeline",
				Version: 1,
				Status:  model.StatusRunning,
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.startInstance(env.Payload)
			},
		},
		{
			Name:    "get_instance",
			Summary: "Get status of a process instance",
			ReqID:   "550e8400-e29b-41d4-a716-446655440000",
			Resp: InstanceStatusResp{
				ID:      "550e8400-e29b-41d4-a716-446655440000",
				Process: "order_pipeline",
				Version: 1,
				Status:  model.StatusCompleted,
				Context: map[string]interface{}{"order_id": 42, "charged": true},
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.getInstance(env.ID)
			},
		},
		{
			Name:    "list_instances",
			Summary: "List process instances (optional status filter: running, completed, failed)",
			Req:     ListInstancesReq{Status: "running"},
			Resp:    []InstanceStatusResp{},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listInstances(env.Payload)
			},
		},
	}
}()
