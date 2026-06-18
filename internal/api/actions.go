package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"gent/internal/model"
	"gent/internal/schema"
)

// actionDef is the single source of truth for one API action.
// It drives HTTP routing (Method + Path) and OpenAPI documentation
// (schemas reflected from Go types).
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
				Name: "order_pipeline",
				InputSchema: &schema.SchemaNode{
					Type: schema.SchemaType{"object"},
					Properties: map[string]*schema.SchemaNode{
						"order_id": {Type: schema.SchemaType{"integer"}},
					},
					Required: []string{"order_id"},
				},
				Tasks: []*model.Task{
					{
						ID: "charge",
						Action: &model.Action{
							Type:     model.ActionTypeREST,
							Endpoint: "http://localhost:9001/charge",
							ResultSchema: &schema.SchemaNode{
								Type: schema.SchemaType{"object"},
								Properties: map[string]*schema.SchemaNode{
									"charged": {Type: schema.SchemaType{"boolean"}},
								},
							},
						},
						TimeoutMs: 5000, OnError: []model.ErrorCase{{Retries: 3}},
						Switch: model.SwitchMap{
							{Case: "self.output.charged == true", Goto: "$ship"},
							{Goto: "$refund"},
						},
					},
					{
						ID:        "ship",
						Action:      &model.Action{Type: model.ActionTypeREST, Endpoint: "http://localhost:9002/ship"},
						Switch:    model.SwitchMap{{Goto: model.GotoEnd}},
						TimeoutMs: 3000, OnError: []model.ErrorCase{{Retries: 2}},
					},
					{
						ID:        "refund",
						Action:      &model.Action{Type: model.ActionTypeREST, Endpoint: "http://localhost:9003/refund"},
						Switch:    model.SwitchMap{{Goto: model.GotoEnd}},
						TimeoutMs: 3000, OnError: []model.ErrorCase{{Retries: 1}},
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
				Status string `query:"status" enum:"running,completed,failing,failed,cancelling,cancelled" description:"Filter by status"`
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
			Name:    "put_definitions_batch",
			Method:  http.MethodPut,
			Path:    "/definitions/batch",
			Summary: "Apply process definitions to a channel with optional auto-update of dependents",
			Tags:    []string{"Definitions"},
			Req: PutDefinitionsBatchReq{
				Channel:           "latest",
				AutoUpdateParents: false,
				Definitions: []model.ProcessDefinition{
					{
						Name:  "child_process",
						Tasks: []*model.Task{{ID: "run", Action: &model.Action{Type: model.ActionTypeREST, Endpoint: "http://localhost:9001/run"}}},
					},
				},
			},
			Resp: []BatchApplyResult{{Name: "child_process", Version: 1, Saved: true}},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putDefinitions(env.Payload)
			},
		},
		{
			Name:    "put_channel",
			Method:  http.MethodPut,
			Path:    "/channels",
			Summary: "Set a channel pointer to a specific process version",
			Tags:    []string{"Channels"},
			Req:     PutChannelReq{Name: "order_pipeline", Channel: "stable", Version: 3},
			Resp:    map[string]any{"name": "order_pipeline", "channel": "stable", "version": 3},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.putChannel(env.Payload)
			},
		},
		{
			Name:    "delete_channel",
			Method:  http.MethodDelete,
			Path:    "/channels",
			Summary: "Remove a channel pointer from a process",
			Tags:    []string{"Channels"},
			Req:     DeleteChannelReq{Name: "order_pipeline", Channel: "stable"},
			Resp:    map[string]any{"deleted": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.deleteChannel(env.Payload)
			},
		},
		{
			Name:    "list_channels",
			Method:  http.MethodGet,
			Path:    "/channels",
			Summary: "List all channels for a process",
			Tags:    []string{"Channels"},
			PathQuery: struct {
				Name string `query:"name" description:"Process name"`
			}{},
			Resp: []ChannelEntry{{Channel: "latest", Version: 2}, {Channel: "stable", Version: 1}},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListChannelsReq{Name: r.URL.Query().Get("name")})
				return Envelope{Action: "list_channels", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listChannels(env.Payload)
			},
		},
		{
			Name:    "promote_channel",
			Method:  http.MethodPost,
			Path:    "/channels/promote",
			Summary: "Copy all channel pointers from one channel to another (optionally scoped to a process subtree)",
			Tags:    []string{"Channels"},
			Req:     PromoteChannelReq{From: "staging", To: "latest"},
			Resp:    map[string]any{"from": "staging", "to": "latest", "promoted": []any{}},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.promoteChannel(env.Payload)
			},
		},
		{
			Name:    "channel_status",
			Method:  http.MethodPost,
			Path:    "/channels/status",
			Summary: "Report stale child references within a channel",
			Tags:    []string{"Channels"},
			Req:     ChannelStatusReq{Channel: "latest"},
			Resp:    []ChannelStatusItem{},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.channelStatus(env.Payload)
			},
		},
		{
			Name:    "validate_definitions",
			Method:  http.MethodPost,
			Path:    "/definitions/validate",
			Summary: "Validate process definitions and return inferred JSON schemas (no save)",
			Tags:    []string{"Definitions"},
			Req: []model.ProcessDefinition{
				{
					Name:  "order_pipeline",
					Tasks: []*model.Task{{ID: "charge", Action: &model.Action{Type: model.ActionTypeREST, Endpoint: "http://localhost:9001/charge"}}},
				},
			},
			Resp: []map[string]any{{"process": "order_pipeline", "version": 1}},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.validateDefinitions(env.Payload)
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
		{
			Name:    "list_instance_logs",
			Method:  http.MethodGet,
			Path:    "/instances/{id}/logs",
			Summary: "Get the execution audit trail for a process instance (oldest first)",
			Tags:    []string{"Instances"},
			PathQuery: struct {
				ID      string `path:"id" format:"uuid"`
				Level   string `query:"level" enum:"debug,info,warn,error" description:"Filter by log level"`
				Since   int64  `query:"since" description:"Only logs at/after this unix-millis timestamp"`
				Limit   int    `query:"limit" description:"Page size (default 200)"`
				AfterTs int64  `query:"after_ts" description:"Keyset cursor: created_at of the previous page's last row"`
				AfterID string `query:"after_id" description:"Keyset cursor: id of the previous page's last row"`
				Tree    bool   `query:"tree" description:"Include the whole process subtree, keyed on the root instance"`
			}{},
			Resp: []LogEntryResp{{
				Time: "2026-06-14T12:00:00Z", Level: model.LogInfo, Event: model.EventTaskSucceeded, Task: "charge",
			}},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				q := r.URL.Query()
				since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
				limit, _ := strconv.Atoi(q.Get("limit"))
				afterTs, _ := strconv.ParseInt(q.Get("after_ts"), 10, 64)
				tree, _ := strconv.ParseBool(q.Get("tree"))
				b, _ := json.Marshal(ListLogsReq{
					Level:   q.Get("level"),
					Since:   since,
					Limit:   limit,
					AfterTs: afterTs,
					AfterID: q.Get("after_id"),
					Tree:    tree,
				})
				return Envelope{Action: "list_instance_logs", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listInstanceLogs(env.ID, env.Payload)
			},
		},
		{
			Name:    "cancel_instance",
			Method:  http.MethodPost,
			Path:    "/instances/{id}/cancel",
			Summary: "Cancel a running root process instance and its entire descendant tree",
			Tags:    []string{"Instances"},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Resp: map[string]any{"cancelled": true},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				return Envelope{Action: "cancel_instance", ID: r.PathValue("id")}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.cancelInstance(env.ID)
			},
		},
		{
			Name:    "retry_instance",
			Method:  http.MethodPost,
			Path:    "/instances/{id}/retry",
			Summary: "Retry a failed or cancelled root process instance, resuming its tree where it was interrupted",
			Tags:    []string{"Instances"},
			PathQuery: struct {
				ID    string `path:"id" format:"uuid"`
				Force bool   `query:"force" description:"Override only_once retry protection"`
			}{},
			Resp: map[string]any{"retried": true},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
				b, _ := json.Marshal(RetryInstanceReq{Force: force})
				return Envelope{Action: "retry_instance", ID: r.PathValue("id"), Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.retryInstance(env.ID, env.Payload)
			},
		},
		{
			Name:    "tick",
			Method:  http.MethodPost,
			Path:    "/tick",
			Summary: "Manually trigger one engine poll cycle (useful when started with -poll 0); optionally shift the server clock forward first to expire leases and retry timers without real waits (testing only)",
			Tags:    []string{"Debug"},
			Req:     TickReq{AdvanceMs: 12_000},
			Resp:    map[string]any{"count": 0},
			handle:  func(h *Handlers, env Envelope) Reply { return h.tick(env.Payload) },
		},
	}
}()
