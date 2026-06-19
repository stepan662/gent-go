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

// pageQuery is the common sort/cursor query-parameter surface embedded in every
// list action's PathQuery, so the OpenAPI spec documents them uniformly.
type pageQuery struct {
	Sort   string `query:"sort" description:"Sort key (per-endpoint whitelist; omit for the default)"`
	Order  string `query:"order" enum:"asc,desc" description:"Sort direction (omit for the endpoint default)"`
	Limit  int    `query:"limit" description:"Page size (default 200, cap 1000)"`
	After  string `query:"after" description:"Cursor from a previous page's page.next_cursor — fetch the next page"`
	Before string `query:"before" description:"Cursor from a previous page's page.previous_cursor — fetch the previous page"`
}

// paginationFrom reads the common sort/cursor query parameters from a request.
func paginationFrom(r *http.Request) Pagination {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return Pagination{
		Sort:   q.Get("sort"),
		Order:  q.Get("order"),
		Limit:  limit,
		After:  q.Get("after"),
		Before: q.Get("before"),
	}
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
			Name:      "list_definitions",
			Method:    http.MethodGet,
			Path:      "/definitions",
			Summary:   "List all registered process definitions",
			Tags:      []string{"Definitions"},
			PathQuery: struct{ pageQuery }{},
			Resp:      PageResp[DefinitionSummary]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListDefinitionsReq{Pagination: paginationFrom(r)})
				return Envelope{Action: "list_definitions", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listDefinitions(env.Payload)
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
				pageQuery
			}{},
			Resp: PageResp[InstanceStatusResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListInstancesReq{
					Status:     r.URL.Query().Get("status"),
					Pagination: paginationFrom(r),
				})
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
				pageQuery
			}{},
			Resp: PageResp[ChannelEntry]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				b, _ := json.Marshal(ListChannelsReq{
					Name:       r.URL.Query().Get("name"),
					Pagination: paginationFrom(r),
				})
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
				ID    string `path:"id" format:"uuid"`
				Level string `query:"level" enum:"debug,info,warn,error" description:"Filter by log level"`
				Since int64  `query:"since" description:"Only logs at/after this unix-millis timestamp"`
				Tree  bool   `query:"tree" description:"Include the whole process subtree, keyed on the root instance"`
				pageQuery
			}{},
			Resp: PageResp[LogEntryResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				q := r.URL.Query()
				since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
				tree, _ := strconv.ParseBool(q.Get("tree"))
				b, _ := json.Marshal(ListLogsReq{
					Level:      q.Get("level"),
					Since:      since,
					Tree:       tree,
					Pagination: paginationFrom(r),
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
			Name:    "list_external_tasks",
			Method:  http.MethodGet,
			Path:    "/external-tasks",
			Summary: "List instances parked on an external task (the external-task queue); never exposes process context",
			Tags:    []string{"External Tasks"},
			PathQuery: struct {
				Process string `query:"process" description:"Filter by process name"`
				Version int    `query:"version" description:"Filter by process version (0 = any)"`
				Task    string `query:"task" description:"Filter by task id"`
				pageQuery
			}{},
			Resp: PageResp[ExternalTaskResp]{},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				q := r.URL.Query()
				version, _ := strconv.Atoi(q.Get("version"))
				b, _ := json.Marshal(ListExternalTasksReq{
					Process:    q.Get("process"),
					Version:    version,
					Task:       q.Get("task"),
					Pagination: paginationFrom(r),
				})
				return Envelope{Action: "list_external_tasks", Payload: b}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.listExternalTasks(env.Payload)
			},
		},
		{
			Name:    "resolve_external_task",
			Method:  http.MethodPost,
			Path:    "/external-tasks/resolve",
			Summary: "Submit a result for a waiting external task; validates it against the task's result_schema and resumes the process",
			Tags:    []string{"External Tasks"},
			Req: ResolveExternalTaskReq{
				Token:  "550e8400-e29b-41d4-a716-446655440000.6ba7b810-9dad-11d1-80b4-00c04fd430c8",
				Result: map[string]any{"approved": true},
			},
			Resp: map[string]any{"resolved": true},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.resolveExternalTask(env.Payload)
			},
		},
		{
			Name:    "signal_instance",
			Method:  http.MethodPost,
			Path:    "/instances/{id}/signal",
			Summary: "Deliver a signal (result) to an external task by id: resolves it if armed now, else buffers FIFO until the task next arms",
			Tags:    []string{"External Tasks"},
			PathQuery: struct {
				ID string `path:"id" format:"uuid"`
			}{},
			Req:  SignalInstanceReq{TaskID: "approval", Payload: map[string]any{"approved": true}},
			Resp: map[string]any{"delivered": true, "buffered": false},
			fromHTTP: func(r *http.Request) (Envelope, error) {
				var payload json.RawMessage
				if r.ContentLength != 0 {
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						return Envelope{}, err
					}
				}
				return Envelope{Action: "signal_instance", ID: r.PathValue("id"), Payload: payload}, nil
			},
			handle: func(h *Handlers, env Envelope) Reply {
				return h.signalInstance(env.ID, env.Payload)
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
