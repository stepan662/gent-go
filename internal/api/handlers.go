package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/stepangranat/gent/internal/db"
	"github.com/stepangranat/gent/internal/model"
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
	Process string                 `json:"process"`
	Version *int                   `json:"version"` // nil = latest
	Input   map[string]interface{} `json:"input"`
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
	ID         string                 `json:"id"`
	Process    string                 `json:"process"`
	Version    int                    `json:"version"`
	Status     model.Status           `json:"status"`
	RetryCount int                    `json:"retry_count"`
	Context    map[string]interface{} `json:"context"`
	Error      string                 `json:"error,omitempty"`
	CreatedAt  string                 `json:"created_at"`
	UpdatedAt  string                 `json:"updated_at"`
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
func (h *Handlers) Handle(env Envelope) Reply {
	switch env.Action {
	case "put_definition":
		return h.putDefinition(env.Payload)
	case "start_instance":
		return h.startInstance(env.Payload)
	case "get_instance":
		return h.getInstance(env.ID)
	case "list_definitions":
		return h.listDefinitions()
	case "list_instances":
		return h.listInstances(env.Payload)
	default:
		return errReply(fmt.Errorf("unknown action %q", env.Action))
	}
}

func (h *Handlers) putDefinition(raw json.RawMessage) Reply {
	var req PutDefinitionReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return errReply(fmt.Errorf("decode: %w", err))
	}
	if req.Name == "" || req.Version == 0 || len(req.Steps) == 0 {
		return errReply(fmt.Errorf("name, version, and at least one step are required"))
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

	input := req.Input
	if input == nil {
		input = map[string]interface{}{}
	}

	inst := &model.ProcessInstance{
		ID:             uuid.NewString(),
		ProcessName:    def.Name,
		ProcessVersion: def.Version,
		StepQueue:      def.Steps,
		ContextData:    input,
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
		Context:    inst.ContextData,
		Error:      inst.Error,
		CreatedAt:  inst.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  inst.UpdatedAt.Format(time.RFC3339),
	}
}

func okReply(v interface{}) Reply {
	data, _ := json.Marshal(v)
	return Reply{OK: true, Data: data}
}

func errReply(err error) Reply {
	return Reply{OK: false, Error: err.Error()}
}
