package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"gent/internal/db"
	"gent/internal/model"
	"gent/internal/transport"

	"github.com/google/uuid"
)

const (
	leaseDuration      = 10 * time.Second
	leaseRenewInterval = 3 * time.Second
)

// Engine is the main orchestration loop. It polls the database for pending
// instances and advances each one step at a time.
type Engine struct {
	db        *db.DB
	pollEvery time.Duration
	log       *slog.Logger
	sem       chan struct{}
	workerID  string
}

// New creates an Engine. pollEvery controls how often SQLite is checked for work.
// maxConcurrent limits how many instances are processed in parallel and how many
// are fetched per tick.
func New(database *db.DB, pollEvery time.Duration, maxConcurrent int, log *slog.Logger) *Engine {
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	return &Engine{
		db:        database,
		pollEvery: pollEvery,
		log:       log,
		sem:       make(chan struct{}, maxConcurrent),
		workerID:  workerID,
	}
}

// Run starts the engine loop and blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	e.log.Info("engine started", "poll_interval", e.pollEvery, "max_concurrent", cap(e.sem), "worker_id", e.workerID)

	go e.leaseRenewer(ctx)

	for {
		select {
		case <-ctx.Done():
			e.log.Info("engine stopped")
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

// leaseRenewer renews all leases held by this worker in a single query every
// leaseRenewInterval. Running as its own goroutine means renewals are never
// blocked by a long tick.
func (e *Engine) leaseRenewer(ctx context.Context) {
	ticker := time.NewTicker(leaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.db.RenewWorkerLeases(e.workerID, leaseDuration); err != nil {
				e.log.Error("renew worker leases", "err", err)
			}
		}
	}
}

// tick claims pending instances and processes each in its own goroutine.
// It blocks until all goroutines finish, so ticks never overlap and the same
// instance is never advanced twice concurrently.
func (e *Engine) tick(ctx context.Context) {
	instances, err := e.db.ClaimInstances(e.workerID, leaseDuration, cap(e.sem))
	if err != nil {
		e.log.Error("claim instances", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, inst := range instances {
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(inst *model.ProcessInstance) {
			defer wg.Done()
			defer func() { <-e.sem }()
			if err := e.advance(ctx, inst); err != nil {
				e.log.Error("advance instance", "id", inst.ID, "err", err)
			}
		}(inst)
	}
	wg.Wait()
}

// advance executes the next step in the instance's queue.
// Each step may have a call, a switch, or both.
// The call runs first; then the switch is evaluated with the call's output
// available as "self". A matching switch case jumps to the named step; no match
// advances to the next step in the queue.
func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) error {
	if len(inst.StepQueue) == 0 {
		inst.Status = model.StatusCompleted
		inst.NextRetryAt = nil
		if err := e.computeOutput(inst); err != nil {
			return e.failInstance(inst, err.Error())
		}
		e.log.Info("instance completed", "id", inst.ID, "process", inst.ProcessName)
		if err := e.db.UpdateInstance(inst); err != nil {
			return err
		}
		return e.notifyParent(inst)
	}

	step := inst.StepQueue[0]
	var selfOutput any

	if step.Call != nil {
		if step.Call.Type == model.CallTypeChildProcess {
			out, done, err := e.runChildProcesses(ctx, inst, step)
			if done || err != nil {
				return err
			}
			selfOutput = out
		} else {
			out, done, err := e.executeAction(ctx, inst, step)
			if done || err != nil {
				return err
			}
			selfOutput = out
		}
	}

	gotoID, err := e.evalSwitch(inst, step, selfOutput)
	if err != nil {
		return e.failInstance(inst, fmt.Sprintf("step %q switch: %v", step.ID, err))
	}

	if gotoID == model.GotoEnd {
		inst.Status = model.StatusCompleted
		inst.RetryCount = 0
		inst.NextRetryAt = nil
		if err := e.computeOutput(inst); err != nil {
			return e.failInstance(inst, err.Error())
		}
		e.log.Info("instance completed", "id", inst.ID, "step", step.ID)
		if err := e.db.UpdateInstance(inst); err != nil {
			return err
		}
		return e.notifyParent(inst)
	}

	if gotoID == "" {
		inst.StepQueue = inst.StepQueue[1:]
	} else {
		newQueue, err := e.queueFromStep(inst, gotoID)
		if err != nil {
			return e.failInstance(inst, err.Error())
		}
		inst.StepQueue = newQueue
	}

	inst.RetryCount = 0
	inst.NextRetryAt = nil
	e.log.Info("step completed", "id", inst.ID, "step", step.ID)
	return e.db.UpdateInstance(inst)
}

// executeAction sends a request to the step's endpoint and stores the output in
// the instance context. Returns (output, done, err):
//   - done=false, err=nil: action succeeded; output is the step result.
//   - done=true: instance already persisted (retry scheduled or permanent fail);
//     caller should return err directly.
func (e *Engine) executeAction(ctx context.Context, inst *model.ProcessInstance, step *model.Step) (any, bool, error) {
	timeout := time.Duration(step.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data, err := e.buildTaskData(inst, step)
	if err != nil {
		return nil, true, e.failInstance(inst, fmt.Sprintf("step %q params: %v", step.ID, err))
	}

	req := transport.Request{
		InstanceID: inst.ID,
		StepID:     step.ID,
		Data:       data,
	}

	e.log.Debug("executing step", "id", inst.ID, "step", step.ID, "call_type", step.Call.Type)

	resolvedHeaders, err := e.resolveHeaders(inst, step.Call)
	if err != nil {
		return nil, true, e.failInstance(inst, fmt.Sprintf("step %q headers: %v", step.ID, err))
	}

	resp, err := transport.Send(taskCtx, step.Call, resolvedHeaders, req)
	if err != nil {
		code := transport.ClassifyGoError(err)
		if step.Call.Type == model.CallTypeScript {
			code = transport.ClassifyScriptError(err)
		}
		return nil, true, e.handleCallError(inst, step, err.Error(), code)
	}
	if resp.ErrorCode != "" {
		return nil, true, e.handleCallError(inst, step, resp.ErrorCode, resp.ErrorCode)
	}

	if err := step.Call.ValidateOutput(resp.Body); err != nil {
		return nil, true, e.handleCallError(inst, step, err.Error(), "output.invalid")
	}

	// Only persist output to context when output_schema is declared.
	// Without it the output is only available as "self" within this step's switch.
	if step.Call.OutputSchema != nil {
		if inst.ContextData["outputs"] == nil {
			inst.ContextData["outputs"] = map[string]any{}
		}
		inst.ContextData["outputs"].(map[string]any)[step.ID] = resp.Body

		var order []string
		switch v := inst.ContextData["output_order"].(type) {
		case []string:
			order = v
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					order = append(order, s)
				}
			}
		}
		inst.ContextData["output_order"] = append(order, step.ID)
	}
	inst.RetryCount = 0

	return resp.Body, false, nil
}

// evalSwitch walks the step's switch cases in order and returns the Goto target
// of the first case whose When expression evaluates to true. The "default" case
// always matches and must be the last entry when present. Returns "" when the
// switch list is empty or no case matches (fall-through to next step).
func (e *Engine) evalSwitch(inst *model.ProcessInstance, step *model.Step, selfOutput any) (string, error) {
	for _, c := range step.Switch {
		if c.When == "default" {
			return c.Goto, nil
		}
		ok, err := evalBool(c.When, inst.ContextData, selfOutput)
		if err != nil {
			return "", fmt.Errorf("when %q: %w", c.When, err)
		}
		if ok {
			return c.Goto, nil
		}
	}
	return "", nil
}

// queueFromStep looks up the process definition and returns all steps starting
// from the one with the given ID. Used to implement switch goto jumps (including
// loops back to earlier steps).
func (e *Engine) queueFromStep(inst *model.ProcessInstance, stepID string) ([]*model.Step, error) {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve goto: %w", err)
	}
	for i, s := range def.Steps {
		if s.ID == stepID {
			return def.Steps[i:], nil
		}
	}
	return nil, fmt.Errorf("goto step %q not found in %q v%d", stepID, inst.ProcessName, inst.ProcessVersion)
}

// matchOnError returns the first ErrorCase whose Code patterns match errCode,
// or whose Code list is empty (catch-all). Returns nil when no rule matches.
func matchOnError(step *model.Step, errCode string) *model.ErrorCase {
	for i := range step.OnError {
		c := &step.OnError[i]
		if len(c.Code) == 0 {
			return c
		}
		for _, pat := range c.Code {
			if transport.SQLLikeMatch(pat, errCode) {
				return c
			}
		}
	}
	return nil
}

// handleCallError evaluates on_error rules, retries if allowed, injects $error
// context, and routes to the matching goto or fails the instance.
func (e *Engine) handleCallError(inst *model.ProcessInstance, step *model.Step, errMsg, errCode string) error {
	matched := matchOnError(step, errCode)

	if matched != nil && inst.RetryCount < matched.Retries {
		inst.RetryCount++
		next := time.Now().Add(transport.RetryDelay(inst.RetryCount))
		inst.NextRetryAt = &next
		e.log.Warn("step failed, scheduling retry",
			"id", inst.ID, "step", step.ID,
			"attempt", inst.RetryCount, "max", matched.Retries,
			"next_retry", next.Format(time.RFC3339),
			"code", errCode, "err", errMsg,
		)
		return e.db.UpdateInstance(inst)
	}

	inst.ContextData["$error"] = map[string]any{
		"step":    step.ID,
		"message": errMsg,
		"code":    errCode,
	}

	if matched != nil && matched.Goto != "" {
		if matched.Goto == model.GotoEnd {
			inst.Status = model.StatusCompleted
			inst.NextRetryAt = nil
			e.log.Info("instance completed via error route", "id", inst.ID, "step", step.ID, "code", errCode)
			if err := e.db.UpdateInstance(inst); err != nil {
				return err
			}
			return e.notifyParent(inst)
		}
		newQueue, err := e.queueFromStep(inst, matched.Goto)
		if err != nil {
			return e.failInstance(inst, err.Error())
		}
		inst.StepQueue = newQueue
		inst.RetryCount = 0
		inst.NextRetryAt = nil
		e.log.Info("routing to error handler",
			"id", inst.ID, "step", step.ID, "goto", matched.Goto, "code", errCode)
		return e.db.UpdateInstance(inst)
	}

	return e.failInstance(inst, fmt.Sprintf("step %q: %s: %s", step.ID, errCode, errMsg))
}

func (e *Engine) buildTaskData(inst *model.ProcessInstance, step *model.Step) (map[string]any, error) {
	if len(step.Params) == 0 {
		return map[string]any{}, nil
	}
	result := make(map[string]any, len(step.Params))
	for name, expression := range step.Params {
		val, err := evalAny(expression, inst.ContextData)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", name, err)
		}
		result[name] = val
	}
	return result, nil
}

func (e *Engine) failInstance(inst *model.ProcessInstance, reason string) error {
	inst.Status = model.StatusFailed
	inst.Error = reason
	inst.NextRetryAt = nil
	e.log.Error("instance failed", "id", inst.ID, "reason", reason)
	if err := e.db.UpdateInstance(inst); err != nil {
		return err
	}
	return e.notifyParent(inst)
}

// runChildProcesses starts a child instance for each entry in a child_process call,
// then suspends the parent by setting its status to waiting. The parent resumes
// (via db.TryWakeParent) once all children complete.
func (e *Engine) runChildProcesses(ctx context.Context, inst *model.ProcessInstance, step *model.Step) (any, bool, error) {
	// If the step output was already enriched by TryWakeParent (items are maps
	// with {id, output}, not plain string IDs), the children are done — return
	// the existing results so the engine's normal dequeue path advances the queue.
	if outputs, ok := inst.ContextData["outputs"].(map[string]any); ok {
		if existing, ok := outputs[step.ID]; ok {
			if items, ok := existing.([]any); ok {
				if len(items) == 0 {
					return existing, false, nil
				}
				if _, isMap := items[0].(map[string]any); isMap {
					return existing, false, nil
				}
			}
		}
	}

	childCallStack := append(inst.CallStack, inst.ID)
	ids := make([]string, 0, len(step.Call.Processes))

	for i, entry := range step.Call.Processes {
		version := entry.Version
		if version == 0 {
			if entry.Name == inst.ProcessName {
				// Self-reference: always run at the caller's own version.
				version = inst.ProcessVersion
			} else {
				var err error
				version, err = e.db.GetDependencyVersion(inst.ProcessName, inst.ProcessVersion, step.ID, i)
				if err != nil {
					// Backwards-compat: old defs without deps rows fall back to latest.
					version, err = e.db.LatestVersion(entry.Name)
					if err != nil {
						return nil, true, e.failInstance(inst, fmt.Sprintf("step %q child_process[%d]: %v", step.ID, i, err))
					}
				}
			}
		}
		def, err := e.db.GetDefinition(entry.Name, version)
		if err != nil {
			return nil, true, e.failInstance(inst, fmt.Sprintf("step %q child_process[%d]: %v", step.ID, i, err))
		}
		input := make(map[string]any, len(entry.Input))
		for k, expr := range entry.Input {
			val, err := evalAny(expr, inst.ContextData)
			if err != nil {
				return nil, true, e.failInstance(inst, fmt.Sprintf("step %q child_process[%d] input %q: %v", step.ID, i, k, err))
			}
			input[k] = val
		}
		if err := def.ValidateInput(input); err != nil {
			return nil, true, e.failInstance(inst, fmt.Sprintf("step %q child_process[%d] input validation: %v", step.ID, i, err))
		}
		child := &model.ProcessInstance{
			ID:             uuid.NewString(),
			ProcessName:    def.Name,
			ProcessVersion: version,
			StepQueue:      def.Steps,
			ContextData:    map[string]any{"input": input, "outputs": map[string]any{}, "output_order": []string{}, "_spawn_step_id": step.ID},
			Status:         model.StatusRunning,
			ParentID:       inst.ID,
			CallStack:      childCallStack,
		}
		if err := e.db.SaveInstance(child); err != nil {
			return nil, true, e.failInstance(inst, fmt.Sprintf("step %q child_process[%d] save: %v", step.ID, i, err))
		}
		e.log.Info("started child process", "parent", inst.ID, "child", child.ID, "process", child.ProcessName)
		ids = append(ids, child.ID)
	}

	// Store metadata TryWakeParent needs: spawn order (placeholder) and child output schema.
	if inst.ContextData["outputs"] == nil {
		inst.ContextData["outputs"] = map[string]any{}
	}
	// Placeholder — TryWakeParent replaces this with the enriched array on wake.
	inst.ContextData["outputs"].(map[string]any)[step.ID] = ids
	if step.Call.ChildOutputSchema != nil {
		if schemaBytes, err := json.Marshal(step.Call.ChildOutputSchema); err == nil {
			inst.ContextData["_spawn_child_output_schema"] = string(schemaBytes)
		}
	} else {
		delete(inst.ContextData, "_spawn_child_output_schema")
	}

	var order []string
	switch v := inst.ContextData["output_order"].(type) {
	case []string:
		order = v
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	}
	inst.ContextData["output_order"] = append(order, step.ID)

	// Suspend parent until all children complete.
	inst.Status = model.StatusWaiting
	inst.RetryCount = 0
	inst.NextRetryAt = nil
	if err := e.db.UpdateInstance(inst); err != nil {
		return nil, true, err
	}
	e.log.Info("parent waiting for children", "id", inst.ID, "step", step.ID, "children", len(ids))
	return ids, true, nil
}

// computeOutput evaluates the process definition's Output expression map against
// the final context and stores the result in context_data["output"]. No-op if
// the definition has no Output map.
func (e *Engine) computeOutput(inst *model.ProcessInstance) error {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return fmt.Errorf("load definition for output: %w", err)
	}
	if len(def.Output) == 0 {
		return nil
	}
	result := make(map[string]any, len(def.Output))
	for k, expr := range def.Output {
		val, err := evalAny(expr, inst.ContextData)
		if err != nil {
			return fmt.Errorf("output %q: %w", k, err)
		}
		result[k] = val
	}
	inst.ContextData["output"] = result
	return nil
}

// notifyParent tells the DB to check whether all siblings of inst are done and,
// if so, wake or fail the parent. A no-op for root instances (ParentID == "").
func (e *Engine) notifyParent(inst *model.ProcessInstance) error {
	if inst.ParentID == "" {
		return nil
	}
	return e.db.TryWakeParent(inst)
}

// resolveHeaders evaluates each header value expression against the instance
// context and coerces the result to a string. Returns nil for calls without headers.
func (e *Engine) resolveHeaders(inst *model.ProcessInstance, call *model.Call) (map[string]string, error) {
	if len(call.Headers) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(call.Headers))
	for k, expr := range call.Headers {
		val, err := evalAny(expr, inst.ContextData)
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", k, err)
		}
		resolved[k] = fmt.Sprintf("%v", val)
	}
	return resolved, nil
}
