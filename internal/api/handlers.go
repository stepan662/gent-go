package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"time"

	"genroc/internal/db"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/validation"
)

const defaultChannel = "latest"

// engineService is the slice of the engine the API depends on: triggering a tick
// and recording the instance_created audit milestone for a root instance.
type engineService interface {
	Tick(ctx context.Context) (int, error)
	ManualTick() bool
	AuditCreated(inst *model.ProcessInstance)
	NotifyWork()
}

// Handlers holds business logic for all API operations.
type Handlers struct {
	db     *db.DB
	engine engineService
}

func NewHandlers(database *db.DB, eng engineService) *Handlers {
	return &Handlers{db: database, engine: eng}
}

// --- Request / Response types ---

// Pagination is the common sort/cursor query surface embedded in every list
// request. Order is "asc"|"desc"|"" (empty = the endpoint's default direction).
// after/before are opaque cursors from a previous page's page object; before pages
// backward. Empty after+before = the first page.
type Pagination struct {
	Sort   string `json:"sort,omitempty"`
	Order  string `json:"order,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	After  string `json:"after,omitempty"`
	Before string `json:"before,omitempty"`
}

// page maps the request surface to a db.PageReq. Order "" leaves Desc nil so the
// listing's default direction applies.
func (p Pagination) page() db.PageReq {
	req := db.PageReq{Sort: p.Sort, Limit: p.Limit, After: p.After, Before: p.Before}
	switch p.Order {
	case "asc":
		desc := false
		req.Desc = &desc
	case "desc":
		desc := true
		req.Desc = &desc
	}
	return req
}

// PageResp is the envelope every list endpoint returns: a page of items plus the
// page object (total, has-next/prev, and the cursors to move either way).
type PageResp[T any] struct {
	Items []T         `json:"items"`
	Page  db.PageInfo `json:"page"`
}

type PutDefinitionReq struct {
	model.ProcessDefinition
}

type StartInstanceReq struct {
	Process string  `json:"process"`
	Version *int    `json:"version,omitempty"` // explicit version; takes priority over Channel
	Channel *string `json:"channel,omitempty"` // resolve to version via channel; fallback to latest
	Input   *any    `json:"input,omitempty"`
}

type PutDefinitionsBatchReq struct {
	Definitions       []model.ProcessDefinition `json:"definitions"`
	Channel           string                    `json:"channel"` // default "latest"
	AutoUpdateParents bool                      `json:"auto_update_parents"`
}

type ChannelEntry struct {
	Channel string `json:"channel"`
	Version int    `json:"version"`
}

type PutChannelReq struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Version int    `json:"version"`
}

type DeleteChannelReq struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
}

type ListChannelsReq struct {
	Name string `json:"name"`
	Pagination
}

type PromoteChannelReq struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	Process *string `json:"process,omitempty"` // nil = all processes on the channel
}

type ChannelStatusReq struct {
	Channel string `json:"channel"`
}

type StaleRef struct {
	TaskID         string `json:"task_id"`
	ChildName      string `json:"child_name"`
	BakedVersion   int    `json:"baked_version"`
	ChannelVersion int    `json:"channel_version"`
}

type ChannelStatusItem struct {
	Name      string     `json:"name"`
	Version   int        `json:"version"`
	StaleRefs []StaleRef `json:"stale_refs,omitempty"`
}

type StartInstanceResp struct {
	ID      string       `json:"id"`
	Process string       `json:"process"`
	Version int          `json:"version"`
	Status  model.Status `json:"status"`
}

type ListDefinitionsReq struct {
	Pagination
}

type ListInstancesReq struct {
	Status string `json:"status"` // optional filter: running, completed, failing, failed, cancelling, cancelled
	Pagination
}

type RetryInstanceReq struct {
	Force bool `json:"force"` // override only_once retry protection
}

type ListExternalTasksReq struct {
	Process string `json:"process"` // optional: filter by process name
	Version int    `json:"version"` // optional: filter by process version (0 = any)
	Task    string `json:"task"`    // optional: filter by task id
	Pagination
}

// ExternalTaskResp is one entry in the external-task queue. It exposes only the task's
// snapshotted input + the result_schema the resolver must satisfy, plus the resolve
// token — never the process context.
type ExternalTaskResp struct {
	Token        string             `json:"token"` // pass back to /external-tasks/resolve
	Process      string             `json:"process"`
	Version      int                `json:"version"`
	TaskID       string             `json:"task_id"`
	Input        any                `json:"input"`                   // the task's evaluated input snapshot
	ResultSchema *schema.SchemaNode `json:"result_schema,omitempty"` // JSON Schema the submitted result must satisfy
	WaitingSince string             `json:"waiting_since"`           // RFC3339 park time
}

type ResolveExternalTaskReq struct {
	Token  string `json:"token"`  // the token from the external-task queue
	Result any    `json:"result"` // the result payload, validated against the task's result_schema
}

type SignalInstanceReq struct {
	TaskID string `json:"task_id"` // the external task to deliver to (addressed, not by token)
	Result any    `json:"result"`  // the result, validated against the task's result_schema
}

type ListLogsReq struct {
	Level     string `json:"level"`     // optional filter: debug, info, warn, error
	Since     int64  `json:"since"`     // optional: only logs at/after this unix-millis timestamp
	Recursive bool   `json:"recursive"` // include the whole process subtree, keyed on the root instance
	Resolve   bool   `json:"resolve"`   // inline full externalized payloads instead of preview + data_ref
	Pagination
}

type TickReq struct {
	AdvanceMs int64 `json:"advance_ms"` // shift the server clock forward (milliseconds) before ticking (testing only)
}

type DefinitionSummary struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type BatchApplyResult struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Saved   bool   `json:"saved"`
}

type InstanceStatusResp struct {
	ID         string          `json:"id"`
	Process    string          `json:"process"`
	Version    int             `json:"version"`
	Status     model.Status    `json:"status"`
	WaitState  model.WaitState `json:"wait_state,omitempty"`
	RetryCount int             `json:"retry_count"`
	Context    map[string]any  `json:"context"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
}

// InstanceSummaryResp is the per-row shape returned by the instance list. It is
// InstanceStatusResp without the (potentially large) context — listing many
// instances should stay light; fetch a single instance for its full context.
type InstanceSummaryResp struct {
	ID         string          `json:"id"`
	Process    string          `json:"process"`
	Version    int             `json:"version"`
	Status     model.Status    `json:"status"`
	WaitState  model.WaitState `json:"wait_state,omitempty"`
	RetryCount int             `json:"retry_count"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
}

type LogEntryResp struct {
	Time     string         `json:"time"`
	Instance string         `json:"instance"`
	Depth    int            `json:"depth"` // distance from the queried subtree root (0 = the queried node)
	Level    model.LogLevel `json:"level"`
	Event    string         `json:"event"`
	Task     string         `json:"task,omitempty"`
	Message  string         `json:"message,omitempty"`
	Code     string         `json:"code,omitempty"`
	Data     string         `json:"data,omitempty"`     // inline payload (input/output/request/response body); empty when externalized — see DataRef — unless ?resolve=true inlines the full value
	DataRef  *LogDataRef    `json:"data_ref,omitempty"` // set when the full payload was externalized to an object; fetch via /instances/{id}/objects/{ref} or pass ?resolve=true
	Meta     map[string]any `json:"meta,omitempty"`     // small, complete, parseable metadata (e.g. {"url":…}, {"status":200})
}

// LogDataRef points at an externalized log payload; the full (pre-redacted) value is
// retrievable from the log-object endpoint.
type LogDataRef struct {
	Ref  string `json:"ref"`
	Size int64  `json:"size"`
}

// decodeLogData unpacks the stored log-data envelope into the API view: a small inline
// payload comes back as its string value; an externalized one comes back as a bare
// reference with no inline data (the full value is fetched on demand via the log-object
// endpoint, or inlined by the caller with ?resolve=true). A non-envelope value is
// returned verbatim.
func decodeLogData(raw string) (string, *LogDataRef) {
	if raw == "" {
		return "", nil
	}
	var env model.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return raw, nil
	}
	if env.IsRef() {
		return "", &LogDataRef{Ref: env.Refs[0].Ref, Size: env.Refs[0].Size}
	}
	s, _ := env.Data.(string)
	return s, nil
}

// --- Envelope ---

type Envelope struct {
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
	// For GET-style actions that only need an ID.
	ID string `json:"id,omitempty"`
}

type Reply struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Handle dispatches an incoming Envelope and returns a Reply.
// This is the single entry-point used by all transports (HTTP, TCP, UDS).
// Actions are defined in actions.go — add a new entry there to register a new action.
func (h *Handlers) Handle(env Envelope) Reply {
	for i := range registry {
		if registry[i].Name == env.Action {
			return registry[i].handle(h, env)
		}
	}
	return errReply(fmt.Errorf("unknown action %q", env.Action))
}

func (h *Handlers) putDefinition(raw json.RawMessage) Reply {
	var req PutDefinitionReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if err := req.Validate(); err != nil {
		return errReply(err)
	}
	latestV, _ := h.db.LatestVersion(req.Name)
	version := latestV + 1
	if _, err := validation.Generate(&req.ProcessDefinition); err != nil {
		return errReply(err)
	}
	if err := validation.ValidateChildProcessRefs(&req.ProcessDefinition, version, h.db); err != nil {
		return errReply(err)
	}
	// Reject registration if a required config var has no value in the server
	// environment, the same rule ResolveConfig enforces at instance start — so a
	// missing GENROC_<PROCESS>_<NAME> surfaces here rather than on first start.
	if _, err := req.ResolveConfig(os.LookupEnv); err != nil {
		return errReply(err)
	}
	if err := h.db.SaveDefinition(&req.ProcessDefinition, version, nil, "", defaultChannel); err != nil {
		return errReply(fmt.Errorf("save: %w", err))
	}
	return okReply(map[string]interface{}{"saved": true, "name": req.Name, "version": version})
}

func (h *Handlers) startInstance(raw json.RawMessage) Reply {
	var req StartInstanceReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Process == "" {
		return errReply(fmt.Errorf("process name is required"))
	}

	version := 0
	switch {
	case req.Version != nil:
		version = *req.Version
	case req.Channel != nil:
		v, err := h.db.GetChannel(req.Process, *req.Channel)
		if err != nil {
			return errReply(err)
		}
		version = v
	default:
		v, err := h.resolveDefaultVersion(req.Process)
		if err != nil {
			return errReply(err)
		}
		version = v
	}

	def, err := h.db.GetDefinition(req.Process, version)
	if err != nil {
		return errReply(err)
	}

	var input any
	if req.Input != nil {
		input = *req.Input
	}

	input, err = def.ValidateInput(input)
	if err != nil {
		return errReply(fmt.Errorf("input validation: %w", err))
	}

	// Resolve config up front so a missing required var or bad value rejects the
	// start request rather than producing an instance that fails on first tick.
	if _, err := def.ResolveConfig(os.LookupEnv); err != nil {
		return errReply(fmt.Errorf("config: %w", err))
	}

	inst := &model.ProcessInstance{
		ID:             idgen.New(),
		ProcessName:    def.Name,
		ProcessVersion: version,
		Task:           def.Tasks[0].ID,
		ContextData:    map[string]any{"input": input, "outputs": map[string]any{}, "error": nil},
		Status:         model.StatusRunning,
		CreatedAt:      time.Now(),
	}

	if err := h.db.SaveInstance(inst); err != nil {
		return errReply(fmt.Errorf("save instance: %w", err))
	}
	if h.engine != nil {
		h.engine.AuditCreated(inst) // bookend: instance_created with the process input
		h.engine.NotifyWork()       // start advancing now instead of waiting for the next poll tick
	}

	return okReply(StartInstanceResp{
		ID:      inst.ID,
		Process: inst.ProcessName,
		Version: inst.ProcessVersion,
		Status:  inst.Status,
	})
}

func (h *Handlers) listDefinitions(raw json.RawMessage) Reply {
	var req ListDefinitionsReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	defs, info, err := h.db.ListDefinitions(req.page())
	if err != nil {
		return errReply(err)
	}
	summaries := make([]DefinitionSummary, len(defs))
	for i, d := range defs {
		summaries[i] = DefinitionSummary{Name: d.Def.Name, Version: d.Version}
	}
	return okReply(PageResp[DefinitionSummary]{Items: summaries, Page: info})
}

func (h *Handlers) listInstances(raw json.RawMessage) Reply {
	var req ListInstancesReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	instances, info, err := h.db.ListInstances(req.Status, req.page())
	if err != nil {
		return errReply(err)
	}
	resp := make([]InstanceSummaryResp, len(instances))
	for i, inst := range instances {
		resp[i] = instanceSummaryToResp(inst)
	}
	return okReply(PageResp[InstanceSummaryResp]{Items: resp, Page: info})
}

func (h *Handlers) getInstance(id string, resolve bool) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	// By default, externalized value-slots are left as {ref, size} references — a
	// detail read should stay light and not pull large blobs out of the object store.
	// With resolve=true the caller opts into materializing every slot inline (and then
	// redacting), the way the full context used to always be returned.
	if resolve {
		if err := h.db.HydrateContext(inst); err != nil {
			return errReply(err)
		}
	}
	resp := instanceToResp(inst)
	// Redact secret-derived values from the returned context (the DB still holds
	// them plainly; they are just not exposed over the API).
	if def, derr := h.db.GetDefinition(inst.ProcessName, inst.ProcessVersion); derr == nil {
		if sf, gerr := validation.Generate(def); gerr == nil {
			resp.Context = orderedContext(validation.RedactContext(inst.ContextData, sf))
		}
	}
	return okReply(resp)
}

func (h *Handlers) listInstanceLogs(id string, raw json.RawMessage) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	var req ListLogsReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	opts := db.LogQuery{
		Level: req.Level,
		Since: req.Since,
		Page:  req.page(),
	}
	var (
		logs []*model.LogEntry
		info db.PageInfo
		err  error
	)
	if req.Recursive {
		logs, info, err = h.db.ListTreeLogs(id, opts)
	} else {
		logs, info, err = h.db.ListLogs(id, opts)
	}
	if err != nil {
		return errReply(err)
	}
	resp := make([]LogEntryResp, len(logs))
	for i, l := range logs {
		data, ref := decodeLogData(l.Data)
		// With resolve=true, replace the preview + data_ref with the full payload
		// inline. The object is owned by the log's own instance (l.InstanceID), which
		// differs from the queried root for subtree logs. Log objects are stored
		// pre-redacted, so serving them inline leaks nothing the data_ref didn't.
		if req.Resolve && ref != nil {
			if content, oerr := h.db.GetLogObject(l.InstanceID, ref.Ref); oerr == nil {
				data, ref = content, nil
			}
		}
		resp[i] = LogEntryResp{
			Time:     l.CreatedAt.Format(time.RFC3339Nano),
			Instance: l.InstanceID,
			Depth:    l.Depth,
			Level:    l.Level,
			Event:    l.Event,
			Task:     l.TaskID,
			Message:  l.Message,
			Code:     l.Code,
			Data:     data,
			DataRef:  ref,
			Meta:     l.Meta,
		}
	}
	return okReply(PageResp[LogEntryResp]{Items: resp, Page: info})
}

// getLogObject returns the full payload of an externalized log entry (referenced by a
// log row's data_ref). Only log objects are served — they are stored pre-redacted, so
// returning the raw content leaks no secrets.
func (h *Handlers) getLogObject(id, hash string) Reply {
	if id == "" || hash == "" {
		return errReply(fmt.Errorf("id and ref are required"))
	}
	content, err := h.db.GetLogObject(id, hash)
	if err != nil {
		return errReply(fmt.Errorf("log payload not found"))
	}
	return okReply(map[string]any{"data": content})
}

func (h *Handlers) cancelInstance(id string) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	if err := h.db.CancelProcess(context.Background(), id); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"cancelled": true})
}

func (h *Handlers) retryInstance(id string, raw json.RawMessage) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	var req RetryInstanceReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if err := h.db.RetryProcess(context.Background(), id, req.Force); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"retried": true})
}

func (h *Handlers) listExternalTasks(raw json.RawMessage) Reply {
	var req ListExternalTasksReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	instances, info, err := h.db.ListExternalTasks(req.Process, req.Version, req.Task, req.page())
	if err != nil {
		return errReply(err)
	}
	resp := make([]ExternalTaskResp, 0, len(instances))
	for _, inst := range instances {
		task, err := h.db.CurrentTask(inst)
		if err != nil || task == nil {
			// Not a resolvable external task (no current task), which a concurrent
			// transition could momentarily produce — skip it.
			continue
		}
		resp = append(resp, externalTaskToResp(inst, task))
	}
	return okReply(PageResp[ExternalTaskResp]{Items: resp, Page: info})
}

// externalTaskToResp projects a parked external instance and its current task to a
// queue entry.
func externalTaskToResp(inst *model.ProcessInstance, task *model.Task) ExternalTaskResp {
	ext, _ := inst.ContextData[model.CtxExternal].(map[string]any)
	token, _ := ext["token"].(string)
	var resultSchema *schema.SchemaNode
	if task.Action != nil {
		resultSchema = task.Action.ResultSchema
	}
	return ExternalTaskResp{
		Token:        token,
		Process:      inst.ProcessName,
		Version:      inst.ProcessVersion,
		TaskID:       task.ID,
		Input:        ext["input"],
		ResultSchema: resultSchema,
		WaitingSince: inst.UpdatedAt.Format(time.RFC3339),
	}
}

func (h *Handlers) resolveExternalTask(raw json.RawMessage) Reply {
	var req ResolveExternalTaskReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Token == "" {
		return errReply(fmt.Errorf("token is required"))
	}
	// The token is instanceID.nonce; instance ids are UUIDs (no '.'), so the part before
	// the first '.' is the instance id for a PK lookup. The exact-token check happens
	// under lock in ResolveExternalTask.
	instanceID, _, ok := strings.Cut(req.Token, ".")
	if !ok || instanceID == "" {
		return errReply(fmt.Errorf("invalid token"))
	}
	inst, err := h.db.GetInstance(instanceID)
	if err != nil {
		return errReply(err)
	}
	task, err := h.db.CurrentTask(inst)
	if err != nil {
		return errReply(err)
	}
	if inst.Status != model.StatusRunning || inst.WaitState != model.WaitStateExternal || task == nil {
		return errReply(fmt.Errorf("task is not waiting for an external result"))
	}
	// Validate the submitted result against the parked task's result_schema (no-op when
	// absent). The task definition is immutable, so validating the pre-lock snapshot is
	// safe; ResolveExternalTask re-checks the parked state + token atomically.
	if task.Action != nil {
		normalized, err := task.Action.ValidateOutput(req.Result)
		if err != nil {
			return errReply(fmt.Errorf("result validation: %w", err))
		}
		req.Result = normalized
	}
	if err := h.db.ResolveExternalTask(context.Background(), instanceID, req.Token, req.Result); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"resolved": true})
}

func (h *Handlers) signalInstance(id string, raw json.RawMessage) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	var req SignalInstanceReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.TaskID == "" {
		return errReply(fmt.Errorf("task_id is required"))
	}
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	if inst.Status != model.StatusRunning {
		return errReply(fmt.Errorf("instance is not running (status %s)", inst.Status))
	}
	// Resolve the target external task from the pinned definition — it may be a wait point
	// reached later, not the current front task. The definition (and its result_schema) is
	// immutable for this version, so validating against it before the atomic deliver is safe.
	def, err := h.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return errReply(err)
	}
	var target *model.Task
	for _, t := range def.Tasks {
		if t.ID == req.TaskID {
			target = t
			break
		}
	}
	if target == nil {
		return errReply(fmt.Errorf("no task %q in %s v%d", req.TaskID, inst.ProcessName, inst.ProcessVersion))
	}
	if target.Action == nil || target.Action.Type != model.ActionTypeExternal {
		return errReply(fmt.Errorf("task %q is not an external task", req.TaskID))
	}
	normalized, err := target.Action.ValidateOutput(req.Result)
	if err != nil {
		return errReply(fmt.Errorf("result validation: %w", err))
	}
	req.Result = normalized
	delivered, err := h.db.DeliverSignal(context.Background(), id, req.TaskID, idgen.New(), req.Result)
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"delivered": delivered, "buffered": !delivered})
}

func (h *Handlers) tick(raw json.RawMessage) Reply {
	if h.engine == nil {
		return errReply(fmt.Errorf("engine not available"))
	}
	if !h.engine.ManualTick() {
		return errReply(fmt.Errorf("tick is only available in manual mode; start the server with --poll 0"))
	}
	var req TickReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	if req.AdvanceMs < 0 {
		return errReply(fmt.Errorf("advance_ms must not be negative"))
	}
	if req.AdvanceMs > 0 {
		db.AdvanceClock(time.Duration(req.AdvanceMs) * time.Millisecond)
	}
	n, err := h.engine.Tick(context.Background())
	if err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"count": n})
}

func instanceToResp(inst *model.ProcessInstance) InstanceStatusResp {
	return InstanceStatusResp{
		ID:         inst.ID,
		Process:    inst.ProcessName,
		Version:    inst.ProcessVersion,
		Status:     inst.Status,
		WaitState:  inst.WaitState,
		RetryCount: inst.RetryCount,
		Context:    orderedContext(inst.ContextData),
		Error:      inst.Error,
		CreatedAt:  inst.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  inst.UpdatedAt.Format(time.RFC3339),
	}
}

func instanceSummaryToResp(s *model.InstanceSummary) InstanceSummaryResp {
	return InstanceSummaryResp{
		ID:         s.ID,
		Process:    s.ProcessName,
		Version:    s.ProcessVersion,
		Status:     s.Status,
		WaitState:  s.WaitState,
		RetryCount: s.RetryCount,
		Error:      s.Error,
		CreatedAt:  s.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  s.UpdatedAt.Format(time.RFC3339),
	}
}

// orderedContext returns a copy of contextData with outputs serialized in task
// completion order (tracked by "output_order"), hiding the order key itself.
func orderedContext(ctxData map[string]any) map[string]any {
	result := make(map[string]any, len(ctxData))
	for k, v := range ctxData {
		if k != "output_order" {
			result[k] = v
		}
	}

	outputs, _ := ctxData["outputs"].(map[string]any)
	if len(outputs) == 0 {
		return result
	}

	var order []string
	switch v := ctxData["output_order"].(type) {
	case []string:
		order = v
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, key := range order {
		val, ok := outputs[key]
		if !ok {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		keyBytes, _ := json.Marshal(key)
		valBytes, _ := json.Marshal(val)
		buf.Write(keyBytes)
		buf.WriteByte(':')
		buf.Write(valBytes)
		first = false
	}
	buf.WriteByte('}')

	result["outputs"] = json.RawMessage(buf.Bytes())
	return result
}

// resolveDefaultVersion returns the version a bare process reference (no explicit
// version or channel) resolves to: the version the "latest" channel points at —
// i.e. what apply most recently published. ensureLatestChannel guarantees "latest"
// exists from the first apply, so the fallback to the highest version number is
// only a safety net for definitions registered before that invariant.
func (h *Handlers) resolveDefaultVersion(process string) (int, error) {
	if v, err := h.db.GetChannel(process, defaultChannel); err == nil {
		return v, nil
	}
	return h.db.LatestVersion(process)
}

// ensureLatestChannel guarantees the "latest" channel exists for a process — it is
// created (pointing at version) on the first apply even when applied to another
// channel, so a bare process reference always resolves via a channel. It only
// creates "latest" when absent; an apply targeting "latest" updates it through the
// normal path.
func (h *Handlers) ensureLatestChannel(name string, version int) error {
	if _, err := h.db.GetChannel(name, defaultChannel); err == nil {
		return nil
	}
	return h.db.SaveChannel(name, defaultChannel, version)
}

// ProcessSpec returns the full OpenAPI spec with the input schema for POST /instances
// patched to match the specific process definition. Input stays as `any` when the
// process has no input_schema.
func (h *Handlers) ProcessSpec(name string, version int) ([]byte, error) {
	if version == 0 {
		v, err := h.resolveDefaultVersion(name)
		if err != nil {
			return nil, err
		}
		version = v
	}
	def, err := h.db.GetDefinition(name, version)
	if err != nil {
		return nil, err
	}

	// Deep-copy the shared spec so we can mutate freely.
	var spec map[string]any
	if err := json.Unmarshal(buildSpec(), &spec); err != nil {
		return nil, err
	}

	// Update info to reflect the specific process.
	spec["info"] = map[string]any{
		"title":   fmt.Sprintf("%s v%d", def.Name, version),
		"version": fmt.Sprintf("%d", version),
	}

	// Patch ApiStartInstanceReq.properties.input with the process's input_schema.
	if def.InputSchema != nil {
		if err := patchInputSchema(spec, def.InputSchema); err != nil {
			return nil, err
		}
	}

	return json.MarshalIndent(spec, "", "  ")
}

func patchInputSchema(spec map[string]any, inputSchema any) error {
	components, ok := spec["components"].(map[string]any)
	if !ok {
		return fmt.Errorf("spec missing components")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		return fmt.Errorf("spec missing components.schemas")
	}
	reqSchema, ok := schemas["ApiStartInstanceReq"].(map[string]any)
	if !ok {
		return fmt.Errorf("spec missing ApiStartInstanceReq schema")
	}
	props, ok := reqSchema["properties"].(map[string]any)
	if !ok {
		return fmt.Errorf("ApiStartInstanceReq missing properties")
	}

	// Marshal the typed schema node to a plain map for OpenAPI spec injection.
	b, err := json.Marshal(inputSchema)
	if err != nil {
		return fmt.Errorf("marshal input schema: %w", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(b, &asMap); err != nil {
		return fmt.Errorf("unmarshal input schema: %w", err)
	}
	if asMap["$id"] == nil {
		asMap = maps.Clone(asMap)
		asMap["$id"] = "instance_input_schema"
	}
	props["input"] = asMap
	reqSchema["required"] = []string{"process", "input"}
	return nil
}

// batchGetter resolves definitions from an in-memory batch first, then falls back to the DB.
// This lets child-process references within the same batch validate correctly.
type batchGetter struct {
	batch    []*model.ProcessDefinition
	versions map[string]int // server-assigned versions for batch items
	db       *db.DB
}

func (g *batchGetter) GetDefinition(name string, version int) (*model.ProcessDefinition, error) {
	for _, d := range g.batch {
		if d.Name == name && (version == 0 || g.versions[d.Name] == version) {
			return d, nil
		}
	}
	return g.db.GetDefinition(name, version)
}

func (g *batchGetter) LatestVersion(name string) (int, error) {
	if v, ok := g.versions[name]; ok {
		return v, nil
	}
	return g.db.LatestVersion(name)
}

func (h *Handlers) putDefinitions(raw json.RawMessage) Reply {
	var req PutDefinitionsBatchReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Channel == "" {
		req.Channel = defaultChannel
	}
	results, err := h.applyBatch(req.Definitions, req.Channel, req.AutoUpdateParents)
	if err != nil {
		return errReply(err)
	}
	return okReply(results)
}

// applyBatch is the core implementation for channel-aware batch apply.
func (h *Handlers) applyBatch(defs []model.ProcessDefinition, channel string, autoUpdateParents bool) ([]BatchApplyResult, error) {
	ptrs := make([]*model.ProcessDefinition, len(defs))
	for i := range defs {
		ptrs[i] = &defs[i]
	}

	sorted, err := topoSort(ptrs)
	if err != nil {
		return nil, err
	}

	// batchVersions tracks the resolved version for each process in this batch.
	batchVersions := make(map[string]int, len(sorted))
	// oldChannelVersions records what the channel pointed to before this apply,
	// used later to find parents that need cascading updates.
	oldChannelVersions := make(map[string]int, len(sorted))

	var results []BatchApplyResult

	for _, def := range sorted {
		// Normalize schemas to canonical form before any comparison or storage.
		if err := def.Normalize(); err != nil {
			return nil, fmt.Errorf("%s: normalize: %w", def.Name, err)
		}

		// Server assigns the next version; user-supplied value is ignored.
		latestV, _ := h.db.LatestVersion(def.Name)
		newVersion := latestV + 1

		// Build resolved deps without mutating def (raw def is stored as-is).
		newDeps, err := h.buildResolvedDeps(def, newVersion, channel, batchVersions)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}

		// Track old channel pointer for cascade detection.
		if currentV, chErr := h.db.GetChannel(def.Name, channel); chErr == nil {
			oldChannelVersions[def.Name] = currentV
		}

		// Content dedup: compute hash and look up any existing version with identical content.
		rawNew, _ := json.Marshal(def)
		hash := contentHash(rawNew, newDeps)
		if v, err := h.db.FindVersionByHash(def.Name, hash); err == nil {
			if err := h.db.SaveChannel(def.Name, channel, v); err != nil {
				return nil, fmt.Errorf("channel %s: %w", def.Name, err)
			}
			if err := h.ensureLatestChannel(def.Name, v); err != nil {
				return nil, fmt.Errorf("ensure latest %s: %w", def.Name, err)
			}
			batchVersions[def.Name] = v
			results = append(results, BatchApplyResult{Name: def.Name, Version: v, Saved: false})
			continue
		}

		// Build a validation copy with baked-in versions for validation.
		defForValidation := applyDepsToDefCopy(def, newDeps)
		getter := &batchGetter{batch: sorted, versions: batchVersions, db: h.db}
		if err := def.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}
		if _, err := validation.Generate(defForValidation); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}
		if err := validation.ValidateChildProcessRefs(defForValidation, newVersion, getter); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}
		// Reject if a required config var is unset in the server environment, the
		// same rule ResolveConfig enforces at instance start.
		if _, err := def.ResolveConfig(os.LookupEnv); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}

		if err := h.db.SaveDefinition(def, newVersion, newDeps, hash, channel); err != nil {
			return nil, fmt.Errorf("save %s: %w", def.Name, err)
		}
		if err := h.ensureLatestChannel(def.Name, newVersion); err != nil {
			return nil, fmt.Errorf("ensure latest %s: %w", def.Name, err)
		}
		batchVersions[def.Name] = newVersion
		results = append(results, BatchApplyResult{Name: def.Name, Version: newVersion, Saved: true})
	}

	if autoUpdateParents {
		// Include all submitted processes so cascade fires even when child deduplicates.
		// FindStaleParents filters to only actually-stale parents, so this is safe.
		cascadeResults, err := h.cascadeUpdate(channel, maps.Clone(batchVersions), batchVersions)
		if err != nil {
			return nil, err
		}
		results = append(results, cascadeResults...)
	}

	return results, nil
}

// buildResolvedDeps returns dependency rows for all child/child_parallel tasks in def,
// resolving version=0 refs via batchVersions or the channel.
// Self-references are excluded — the engine always runs them at the caller's own version.
// It does not mutate def — the raw definition is stored as-is.
func (h *Handlers) buildResolvedDeps(def *model.ProcessDefinition, selfVersion int, channel string, batchVersions map[string]int) ([]db.DependencyRow, error) {
	var deps []db.DependencyRow
	for _, task := range def.Tasks {
		if task.Action == nil {
			continue
		}
		switch task.Action.Type {
		case model.ActionTypeChild:
			entry := model.ChildEntry{Name: task.Action.Name, Version: task.Action.Version}
			if entry.Name == def.Name && (entry.Version == 0 || entry.Version == selfVersion) {
				continue
			}
			version, err := h.resolveChildVersion(entry.Name, entry.Version, task.ID, "", channel, batchVersions)
			if err != nil {
				return nil, err
			}
			deps = append(deps, db.DependencyRow{
				ParentName:    def.Name,
				ParentVersion: selfVersion,
				TaskID:        task.ID,
				ChildKey:      "",
				ChildName:     entry.Name,
				ChildVersion:  version,
			})
		case model.ActionTypeChildParallel:
			for key, entry := range task.Action.Children {
				if entry.Name == def.Name && (entry.Version == 0 || entry.Version == selfVersion) {
					continue
				}
				version, err := h.resolveChildVersion(entry.Name, entry.Version, task.ID, key, channel, batchVersions)
				if err != nil {
					return nil, err
				}
				deps = append(deps, db.DependencyRow{
					ParentName:    def.Name,
					ParentVersion: selfVersion,
					TaskID:        task.ID,
					ChildKey:      key,
					ChildName:     entry.Name,
					ChildVersion:  version,
				})
			}
		}
	}
	return deps, nil
}

func (h *Handlers) resolveChildVersion(childName string, childVersion int, taskID, childKey, channel string, batchVersions map[string]int) (int, error) {
	if childVersion != 0 {
		return childVersion, nil
	}
	if v, ok := batchVersions[childName]; ok {
		return v, nil
	}
	v, err := h.db.GetChannel(childName, channel)
	if err != nil {
		label := childName
		if childKey != "" {
			label = fmt.Sprintf("%s[%q]", childName, childKey)
		}
		return 0, fmt.Errorf("task %q child %s: not on channel %q (%w)", taskID, label, channel, err)
	}
	return v, nil
}

// cascadeUpdate finds all processes on channel whose deps point to old versions
// of any process in changedVersions, creates new versions, and repeats until fixpoint.
// allUpdated accumulates all resolved versions from the originating batch.
func (h *Handlers) cascadeUpdate(channel string, changedVersions map[string]int, allUpdated map[string]int) ([]BatchApplyResult, error) {
	var results []BatchApplyResult

	var lastCurrent []db.VersionedDef
	for {
		stale, current, err := h.db.FindParentsOf(channel, allUpdated)
		if err != nil {
			return nil, fmt.Errorf("cascade: find parents: %w", err)
		}
		lastCurrent = current

		foundUpdate := false
		for _, vd := range stale {
			if _, alreadyUpdated := allUpdated[vd.Def.Name]; alreadyUpdated {
				continue
			}

			latestV, err := h.db.LatestVersion(vd.Def.Name)
			if err != nil {
				latestV = 0
			}
			newVersion := latestV + 1

			newDeps, err := h.buildResolvedDeps(vd.Def, newVersion, channel, allUpdated)
			if err != nil {
				return nil, fmt.Errorf("auto-update %s: %w", vd.Def.Name, err)
			}

			// Content dedup via hash: reuse any existing version with identical snapshot.
			rawNew, _ := json.Marshal(vd.Def)
			hash := contentHash(rawNew, newDeps)
			if reuseV, err := h.db.FindVersionByHash(vd.Def.Name, hash); err == nil {
				if err := h.db.SaveChannel(vd.Def.Name, channel, reuseV); err != nil {
					return nil, fmt.Errorf("auto-update channel %s: %w", vd.Def.Name, err)
				}
				allUpdated[vd.Def.Name] = reuseV
				results = append(results, BatchApplyResult{Name: vd.Def.Name, Version: reuseV, Saved: false})
				foundUpdate = true
				continue
			}

			defForValidation := applyDepsToDefCopy(vd.Def, newDeps)
			getter := &batchGetter{db: h.db}
			if _, err := validation.Generate(defForValidation); err != nil {
				return nil, fmt.Errorf("auto-update %s: schema incompatible after child upgrade: %w", vd.Def.Name, err)
			}
			if err := validation.ValidateChildProcessRefs(defForValidation, newVersion, getter); err != nil {
				return nil, fmt.Errorf("auto-update %s: child input incompatible after upgrade: %w", vd.Def.Name, err)
			}

			if err := h.db.SaveDefinition(vd.Def, newVersion, newDeps, hash, channel); err != nil {
				return nil, fmt.Errorf("auto-update save %s: %w", vd.Def.Name, err)
			}

			allUpdated[vd.Def.Name] = newVersion
			results = append(results, BatchApplyResult{Name: vd.Def.Name, Version: newVersion, Saved: true})
			foundUpdate = true
		}

		if !foundUpdate {
			break
		}
	}

	// Report up-to-date parents from the final iteration so they appear in output.
	reported := make(map[string]bool, len(results))
	for _, r := range results {
		reported[r.Name] = true
	}
	for _, vd := range lastCurrent {
		if !reported[vd.Def.Name] {
			results = append(results, BatchApplyResult{Name: vd.Def.Name, Version: vd.Version, Saved: false})
		}
	}

	return results, nil
}

func (h *Handlers) putChannel(raw json.RawMessage) Reply {
	var req PutChannelReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Name == "" || req.Channel == "" || req.Version < 1 {
		return errReply(fmt.Errorf("name, channel, and version (≥1) are required"))
	}
	if _, err := h.db.GetDefinition(req.Name, req.Version); err != nil {
		return errReply(fmt.Errorf("definition %q v%d not found", req.Name, req.Version))
	}
	if err := h.db.SaveChannel(req.Name, req.Channel, req.Version); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"name": req.Name, "channel": req.Channel, "version": req.Version})
}

func (h *Handlers) deleteChannel(raw json.RawMessage) Reply {
	var req DeleteChannelReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Name == "" || req.Channel == "" {
		return errReply(fmt.Errorf("name and channel are required"))
	}
	if err := h.db.DeleteChannel(req.Name, req.Channel); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"deleted": true})
}

func (h *Handlers) listChannels(raw json.RawMessage) Reply {
	var req ListChannelsReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Name == "" {
		return errReply(fmt.Errorf("name is required"))
	}
	channels, info, err := h.db.ListChannels(req.Name, req.page())
	if err != nil {
		return errReply(err)
	}
	entries := make([]ChannelEntry, len(channels))
	for i, c := range channels {
		entries[i] = ChannelEntry{Channel: c.Channel, Version: c.Version}
	}
	return okReply(PageResp[ChannelEntry]{Items: entries, Page: info})
}

func (h *Handlers) promoteChannel(raw json.RawMessage) Reply {
	var req PromoteChannelReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.From == "" || req.To == "" {
		return errReply(fmt.Errorf("from and to are required"))
	}
	if req.From == req.To {
		return errReply(fmt.Errorf("from and to must differ"))
	}

	defs, err := h.db.LoadDefinitionsOnChannel(req.From)
	if err != nil {
		return errReply(fmt.Errorf("load channel %q: %w", req.From, err))
	}

	// If scoped to a process, collect only its dependency subtree.
	if req.Process != nil {
		defs, err = subtree(defs, *req.Process)
		if err != nil {
			return errReply(err)
		}
	}

	promoted := make([]map[string]any, 0, len(defs))
	for _, vd := range defs {
		if err := h.db.SaveChannel(vd.Def.Name, req.To, vd.Version); err != nil {
			return errReply(fmt.Errorf("promote %s: %w", vd.Def.Name, err))
		}
		promoted = append(promoted, map[string]any{"name": vd.Def.Name, "version": vd.Version})
	}
	return okReply(map[string]any{"from": req.From, "to": req.To, "promoted": promoted})
}

func (h *Handlers) channelStatus(raw json.RawMessage) Reply {
	var req ChannelStatusReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Channel == "" {
		return errReply(fmt.Errorf("channel is required"))
	}

	defs, err := h.db.LoadDefinitionsOnChannel(req.Channel)
	if err != nil {
		return errReply(err)
	}

	staleRows, err := h.db.FindStaleRefs(req.Channel)
	if err != nil {
		return errReply(err)
	}

	type parentKey struct {
		name    string
		version int
	}
	staleByParent := make(map[parentKey][]StaleRef, len(staleRows))
	for _, r := range staleRows {
		k := parentKey{r.ParentName, r.ParentVersion}
		staleByParent[k] = append(staleByParent[k], StaleRef{
			TaskID:         r.TaskID,
			ChildName:      r.ChildName,
			BakedVersion:   r.BakedVersion,
			ChannelVersion: r.ChannelVersion,
		})
	}

	items := make([]ChannelStatusItem, 0, len(defs))
	for _, vd := range defs {
		k := parentKey{vd.Def.Name, vd.Version}
		items = append(items, ChannelStatusItem{
			Name:      vd.Def.Name,
			Version:   vd.Version,
			StaleRefs: staleByParent[k],
		})
	}
	return okReply(items)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// topoSort returns definitions sorted leaves-first so child refs are resolved
// before the parents that reference them. Returns an error on cycles.
func topoSort(defs []*model.ProcessDefinition) ([]*model.ProcessDefinition, error) {
	byName := make(map[string]*model.ProcessDefinition, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(defs))
	var sorted []*model.ProcessDefinition

	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("cycle detected involving process %q", name)
		}
		state[name] = visiting
		d := byName[name]
		for _, task := range d.Tasks {
			if task.Action == nil {
				continue
			}
			var childNames []string
			switch task.Action.Type {
			case model.ActionTypeChild:
				childNames = []string{task.Action.Name}
			case model.ActionTypeChildParallel:
				for _, entry := range task.Action.Children {
					childNames = append(childNames, entry.Name)
				}
			}
			for _, childName := range childNames {
				if childName == name {
					continue // self-reference is valid recursion, not a cycle
				}
				if _, inBatch := byName[childName]; inBatch {
					if err := visit(childName); err != nil {
						return err
					}
				}
			}
		}
		state[name] = done
		sorted = append(sorted, d)
		return nil
	}

	for _, d := range defs {
		if err := visit(d.Name); err != nil {
			return nil, err
		}
	}
	return sorted, nil
}

type taskChildKey struct {
	taskID   string
	childKey string
}

// applyDepsToDefCopy returns a deep copy of def with resolved child versions baked in.
// Self-refs (entry.Name == def.Name) keep version=0 since genrocschema handles them
// separately and the engine resolves them via inst.ProcessVersion.
// Used to produce a validation copy for genrocschema — the raw def stored in DB is unchanged.
func applyDepsToDefCopy(def *model.ProcessDefinition, deps []db.DependencyRow) *model.ProcessDefinition {
	data, _ := json.Marshal(def)
	var copy model.ProcessDefinition
	_ = json.Unmarshal(data, &copy)
	lookup := make(map[taskChildKey]int, len(deps))
	for _, d := range deps {
		lookup[taskChildKey{d.TaskID, d.ChildKey}] = d.ChildVersion
	}
	for _, task := range copy.Tasks {
		if task.Action == nil {
			continue
		}
		switch task.Action.Type {
		case model.ActionTypeChild:
			if v, ok := lookup[taskChildKey{task.ID, ""}]; ok {
				task.Action.Version = v
			}
		case model.ActionTypeChildParallel:
			for key := range task.Action.Children {
				if v, ok := lookup[taskChildKey{task.ID, key}]; ok {
					entry := task.Action.Children[key]
					entry.Version = v
					task.Action.Children[key] = entry
				}
			}
		}
	}
	return &copy
}

// contentHash returns a SHA256 hex digest over rawJSON and the sorted deps,
// uniquely identifying a (definition, resolved-children) snapshot.
func contentHash(rawJSON []byte, deps []db.DependencyRow) string {
	h := sha256.New()
	h.Write(rawJSON)
	sorted := append([]db.DependencyRow(nil), deps...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].TaskID != sorted[j].TaskID {
			return sorted[i].TaskID < sorted[j].TaskID
		}
		return sorted[i].ChildKey < sorted[j].ChildKey
	})
	for _, d := range sorted {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%d", d.TaskID, d.ChildKey, d.ChildName, d.ChildVersion)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// subtree collects the definition for rootName and all its dependencies (recursively)
// from the provided slice, following baked-in child refs.
func subtree(defs []db.VersionedDef, rootName string) ([]db.VersionedDef, error) {
	byName := make(map[string]*model.ProcessDefinition, len(defs))
	for _, vd := range defs {
		byName[vd.Def.Name] = vd.Def
	}

	visited := make(map[string]bool)
	var collect func(name string) error
	collect = func(name string) error {
		if visited[name] {
			return nil
		}
		d, ok := byName[name]
		if !ok {
			return nil // dependency not on this channel, skip
		}
		visited[name] = true
		for _, task := range d.Tasks {
			if task.Action == nil {
				continue
			}
			switch task.Action.Type {
			case model.ActionTypeChild:
				if err := collect(task.Action.Name); err != nil {
					return err
				}
			case model.ActionTypeChildParallel:
				for _, entry := range task.Action.Children {
					if err := collect(entry.Name); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := collect(rootName); err != nil {
		return nil, err
	}

	var out []db.VersionedDef
	for _, vd := range defs {
		if visited[vd.Def.Name] {
			out = append(out, vd)
		}
	}
	return out, nil
}

func (h *Handlers) validateDefinitions(raw json.RawMessage) Reply {
	var defs []model.ProcessDefinition
	if err := json.Unmarshal(raw, &defs); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	ptrs := make([]*model.ProcessDefinition, len(defs))
	for i := range defs {
		ptrs[i] = &defs[i]
	}
	getter := &batchGetter{batch: ptrs, versions: map[string]int{}, db: h.db}
	schemas := make([]validation.SchemaFile, 0, len(ptrs))
	for _, def := range ptrs {
		if err := def.Validate(); err != nil {
			return errReply(fmt.Errorf("%s: %w", def.Name, err))
		}
		sf, err := validation.Generate(def)
		if err != nil {
			return errReply(fmt.Errorf("%s: %w", def.Name, err))
		}
		if err := validation.ValidateChildProcessRefs(def, 0, getter); err != nil {
			return errReply(fmt.Errorf("%s: %w", def.Name, err))
		}
		schemas = append(schemas, sf)
	}
	return okReply(schemas)
}

func okReply(v interface{}) Reply {
	data, _ := json.Marshal(v)
	return Reply{OK: true, Data: data}
}

func errReply(err error) Reply {
	return Reply{OK: false, Error: err.Error()}
}
