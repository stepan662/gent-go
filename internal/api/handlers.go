package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"

	"gent/internal/db"
	"gent/internal/model"
	"time"

	"github.com/google/uuid"
)

// Handlers holds business logic for all API operations.
type Handlers struct {
	db *db.DB
}

func NewHandlers(database *db.DB) *Handlers {
	return &Handlers{db: database}
}

// --- Request / Response types ---

type PutDefinitionReq struct {
	model.ProcessDefinition
}

type StartInstanceReq struct {
	Process string `json:"process"`
	Version *int   `json:"version"` // nil = latest
	Input   *any   `json:"input,omitempty"`
}

type StartInstanceResp struct {
	ID      string       `json:"id"`
	Process string       `json:"process"`
	Version int          `json:"version"`
	Status  model.Status `json:"status"`
}

type ListInstancesReq struct {
	Status string `json:"status"` // optional filter: running, completed, failed
}

type DefinitionSummary struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type InstanceStatusResp struct {
	ID         string         `json:"id"`
	Process    string         `json:"process"`
	Version    int            `json:"version"`
	Status     model.Status   `json:"status"`
	RetryCount int            `json:"retry_count"`
	Context    map[string]any `json:"context"`
	Error      string         `json:"error,omitempty"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
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
	if err := req.Normalize(); err != nil {
		return errReply(err)
	}
	if err := req.Validate(); err != nil {
		return errReply(err)
	}
	if err := h.db.SaveDefinition(&req.ProcessDefinition); err != nil {
		return errReply(fmt.Errorf("save: %w", err))
	}
	return okReply(map[string]interface{}{"saved": true, "name": req.Name, "version": req.Version})
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
	if req.Version != nil {
		version = *req.Version
	} else {
		v, err := h.db.LatestVersion(req.Process)
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

	if err := def.ValidateInput(input); err != nil {
		return errReply(fmt.Errorf("input validation: %w", err))
	}

	inst := &model.ProcessInstance{
		ID:             uuid.NewString(),
		ProcessName:    def.Name,
		ProcessVersion: def.Version,
		StepQueue:      def.Steps,
		ContextData:    map[string]any{"input": input, "outputs": map[string]any{}},
		Status:         model.StatusRunning,
		CreatedAt:      time.Now(),
	}

	if err := h.db.SaveInstance(inst); err != nil {
		return errReply(fmt.Errorf("save instance: %w", err))
	}

	return okReply(StartInstanceResp{
		ID:      inst.ID,
		Process: inst.ProcessName,
		Version: inst.ProcessVersion,
		Status:  inst.Status,
	})
}

func (h *Handlers) listDefinitions() Reply {
	defs, err := h.db.ListDefinitions()
	if err != nil {
		return errReply(err)
	}
	summaries := make([]DefinitionSummary, len(defs))
	for i, d := range defs {
		summaries[i] = DefinitionSummary{Name: d.Name, Version: d.Version}
	}
	return okReply(summaries)
}

func (h *Handlers) listInstances(raw json.RawMessage) Reply {
	var req ListInstancesReq
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	instances, err := h.db.ListInstances(req.Status)
	if err != nil {
		return errReply(err)
	}
	resp := make([]InstanceStatusResp, len(instances))
	for i, inst := range instances {
		resp[i] = instanceToResp(inst)
	}
	return okReply(resp)
}

func (h *Handlers) getInstance(id string) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	inst, err := h.db.GetInstance(id)
	if err != nil {
		return errReply(err)
	}
	return okReply(instanceToResp(inst))
}

func instanceToResp(inst *model.ProcessInstance) InstanceStatusResp {
	return InstanceStatusResp{
		ID:         inst.ID,
		Process:    inst.ProcessName,
		Version:    inst.ProcessVersion,
		Status:     inst.Status,
		RetryCount: inst.RetryCount,
		Context:    orderedContext(inst.ContextData),
		Error:      inst.Error,
		CreatedAt:  inst.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  inst.UpdatedAt.Format(time.RFC3339),
	}
}

// orderedContext returns a copy of contextData with outputs serialized in step
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

// ProcessSpec returns the full OpenAPI spec with the input schema for POST /instances
// patched to match the specific process definition. Input stays as `any` when the
// process has no input_schema.
func (h *Handlers) ProcessSpec(name string, version int) ([]byte, error) {
	if version == 0 {
		v, err := h.db.LatestVersion(name)
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
		"title":   fmt.Sprintf("%s v%d", def.Name, def.Version),
		"version": fmt.Sprintf("%d", def.Version),
	}

	// Patch ApiStartInstanceReq.properties.input with the process's input_schema.
	if def.InputSchema != nil {
		if err := patchInputSchema(spec, def.InputSchema); err != nil {
			return nil, err
		}
	}

	return json.MarshalIndent(spec, "", "  ")
}

func patchInputSchema(spec map[string]any, inputSchema map[string]any) error {
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

	if inputSchema["$id"] == nil {
		inputSchema = maps.Clone(inputSchema)
		inputSchema["$id"] = "instance_input_schema"
	}
	props["input"] = inputSchema
	reqSchema["required"] = []string{"process", "input"}
	return nil
}

func okReply(v interface{}) Reply {
	data, _ := json.Marshal(v)
	return Reply{OK: true, Data: data}
}

func errReply(err error) Reply {
	return Reply{OK: false, Error: err.Error()}
}
