package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"time"

	"gent/internal/db"
	"gent/internal/model"
	"gent/internal/validation"

	"github.com/google/uuid"
)

const defaultChannel = "latest"

type tickProvider interface {
	Tick(ctx context.Context) (int, error)
}

// Handlers holds business logic for all API operations.
type Handlers struct {
	db     *db.DB
	engine tickProvider
}

func NewHandlers(database *db.DB, eng tickProvider) *Handlers {
	return &Handlers{db: database, engine: eng}
}

// --- Request / Response types ---

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
	Channel           string                    `json:"channel"`            // default "latest"
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
	StepID         string `json:"step_id"`
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

type ListInstancesReq struct {
	Status string `json:"status"` // optional filter: running, completed, failed
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
	ID         string           `json:"id"`
	Process    string           `json:"process"`
	Version    int              `json:"version"`
	Status     model.Status     `json:"status"`
	WaitState  model.WaitState  `json:"wait_state,omitempty"`
	RetryCount int              `json:"retry_count"`
	Context    map[string]any   `json:"context"`
	Error      string           `json:"error,omitempty"`
	CreatedAt  string           `json:"created_at"`
	UpdatedAt  string           `json:"updated_at"`
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
	if err := h.db.SaveDefinition(&req.ProcessDefinition, version, nil, "", ""); err != nil {
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
		ProcessVersion: version,
		StepQueue:      def.Steps,
		ContextData:    map[string]any{"input": input, "outputs": map[string]any{}, "error": nil},
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
		summaries[i] = DefinitionSummary{Name: d.Def.Name, Version: d.Version}
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

func (h *Handlers) cancelInstance(id string) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	if err := h.db.CancelProcess(context.Background(), id); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"cancelled": true})
}

func (h *Handlers) retryInstance(id string) Reply {
	if id == "" {
		return errReply(fmt.Errorf("id is required"))
	}
	if err := h.db.RetryProcess(context.Background(), id); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"retried": true})
}

func (h *Handlers) tick() Reply {
	if h.engine == nil {
		return errReply(fmt.Errorf("engine not available"))
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

		if err := h.db.SaveDefinition(def, newVersion, newDeps, hash, channel); err != nil {
			return nil, fmt.Errorf("save %s: %w", def.Name, err)
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

// buildResolvedDeps returns dependency rows for all child/child_parallel steps in def,
// resolving version=0 refs via batchVersions or the channel.
// Self-references are excluded — the engine always runs them at the caller's own version.
// It does not mutate def — the raw definition is stored as-is.
func (h *Handlers) buildResolvedDeps(def *model.ProcessDefinition, selfVersion int, channel string, batchVersions map[string]int) ([]db.DependencyRow, error) {
	var deps []db.DependencyRow
	for _, step := range def.Steps {
		if step.Call == nil {
			continue
		}
		switch step.Call.Type {
		case model.CallTypeChild:
			entry := model.ChildEntry{Name: step.Call.Name, Version: step.Call.Version}
			if entry.Name == def.Name && (entry.Version == 0 || entry.Version == selfVersion) {
				continue
			}
			version, err := h.resolveChildVersion(entry.Name, entry.Version, step.ID, "", channel, batchVersions)
			if err != nil {
				return nil, err
			}
			deps = append(deps, db.DependencyRow{
				ParentName:    def.Name,
				ParentVersion: selfVersion,
				StepID:        step.ID,
				ChildKey:      "",
				ChildName:     entry.Name,
				ChildVersion:  version,
			})
		case model.CallTypeChildParallel:
			for key, entry := range step.Call.Children {
				if entry.Name == def.Name && (entry.Version == 0 || entry.Version == selfVersion) {
					continue
				}
				version, err := h.resolveChildVersion(entry.Name, entry.Version, step.ID, key, channel, batchVersions)
				if err != nil {
					return nil, err
				}
				deps = append(deps, db.DependencyRow{
					ParentName:    def.Name,
					ParentVersion: selfVersion,
					StepID:        step.ID,
					ChildKey:      key,
					ChildName:     entry.Name,
					ChildVersion:  version,
				})
			}
		}
	}
	return deps, nil
}

func (h *Handlers) resolveChildVersion(childName string, childVersion int, stepID, childKey, channel string, batchVersions map[string]int) (int, error) {
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
		return 0, fmt.Errorf("step %q child %s: not on channel %q (%w)", stepID, label, channel, err)
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
	channels, err := h.db.ListChannels(req.Name)
	if err != nil {
		return errReply(err)
	}
	entries := make([]ChannelEntry, 0, len(channels))
	for ch, v := range channels {
		entries = append(entries, ChannelEntry{Channel: ch, Version: v})
	}
	return okReply(entries)
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
			StepID:         r.StepID,
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
		for _, step := range d.Steps {
			if step.Call == nil {
				continue
			}
			var childNames []string
			switch step.Call.Type {
			case model.CallTypeChild:
				childNames = []string{step.Call.Name}
			case model.CallTypeChildParallel:
				for _, entry := range step.Call.Children {
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

type stepChildKey struct {
	stepID   string
	childKey string
}

// applyDepsToDefCopy returns a deep copy of def with resolved child versions baked in.
// Self-refs (entry.Name == def.Name) keep version=0 since gentschema handles them
// separately and the engine resolves them via inst.ProcessVersion.
// Used to produce a validation copy for gentschema — the raw def stored in DB is unchanged.
func applyDepsToDefCopy(def *model.ProcessDefinition, deps []db.DependencyRow) *model.ProcessDefinition {
	data, _ := json.Marshal(def)
	var copy model.ProcessDefinition
	_ = json.Unmarshal(data, &copy)
	lookup := make(map[stepChildKey]int, len(deps))
	for _, d := range deps {
		lookup[stepChildKey{d.StepID, d.ChildKey}] = d.ChildVersion
	}
	for _, step := range copy.Steps {
		if step.Call == nil {
			continue
		}
		switch step.Call.Type {
		case model.CallTypeChild:
			if v, ok := lookup[stepChildKey{step.ID, ""}]; ok {
				step.Call.Version = v
			}
		case model.CallTypeChildParallel:
			for key := range step.Call.Children {
				if v, ok := lookup[stepChildKey{step.ID, key}]; ok {
					entry := step.Call.Children[key]
					entry.Version = v
					step.Call.Children[key] = entry
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
		if sorted[i].StepID != sorted[j].StepID {
			return sorted[i].StepID < sorted[j].StepID
		}
		return sorted[i].ChildKey < sorted[j].ChildKey
	})
	for _, d := range sorted {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%d", d.StepID, d.ChildKey, d.ChildName, d.ChildVersion)
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
		for _, step := range d.Steps {
			if step.Call == nil {
				continue
			}
			switch step.Call.Type {
			case model.CallTypeChild:
				if err := collect(step.Call.Name); err != nil {
					return err
				}
			case model.CallTypeChildParallel:
				for _, entry := range step.Call.Children {
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
