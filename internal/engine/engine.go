package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
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
	db               *db.DB
	pollEvery        time.Duration
	immediateRetries bool
	log              *slog.Logger
	sem              chan struct{}
	workerID         string
}

// New creates an Engine. pollEvery controls how often SQLite is checked for work.
// maxConcurrent limits how many instances are processed in parallel and how many
// are fetched per tick. immediateRetries disables exponential backoff (retries fire
// instantly); intended for tests only.
func New(database *db.DB, pollEvery time.Duration, maxConcurrent int, immediateRetries bool, log *slog.Logger) *Engine {
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	return &Engine{
		db:               database,
		pollEvery:        pollEvery,
		immediateRetries: immediateRetries,
		log:              log,
		sem:              make(chan struct{}, maxConcurrent),
		workerID:         workerID,
	}
}

func (e *Engine) retryDelay(attempt int) time.Duration {
	if e.immediateRetries {
		return 0
	}
	return transport.RetryDelay(attempt)
}

// Run starts the engine loop and blocks until ctx is cancelled.
// When pollEvery is zero the engine does not auto-tick; call Tick explicitly.
func (e *Engine) Run(ctx context.Context) {
	e.log.Info("engine started", "poll_interval", e.pollEvery, "max_concurrent", cap(e.sem), "worker_id", e.workerID)

	go e.leaseRenewer(ctx)

	if e.pollEvery == 0 {
		e.log.Info("engine in manual tick mode")
		<-ctx.Done()
		e.log.Info("engine stopped")
		return
	}

	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			e.log.Info("engine stopped")
			return
		case <-ticker.C:
			e.Tick(ctx)
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

// Tick claims pending instances and processes each in its own goroutine.
// It blocks until all goroutines finish, so ticks never overlap and the same
// instance is never advanced twice concurrently. Returns the number of instances
// that were claimed and processed.
func (e *Engine) Tick(ctx context.Context) (int, error) {
	instances, err := e.db.ClaimInstances(e.workerID, leaseDuration, cap(e.sem))
	if err != nil {
		e.log.Error("claim instances", "err", err)
		return 0, err
	}
	var wg sync.WaitGroup
	for _, inst := range instances {
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return 0, ctx.Err()
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
	return len(instances), nil
}

// advance executes the next step in the instance's queue.
// Each step may have a call, a switch, or both.
// The call runs first; then the switch is evaluated with the call's output
// available as "self". A matching switch case jumps to the named step; no match
// advances to the next step in the queue.
func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) error {
	if inst.Status == model.StatusCancelling {
		return e.cancelInstance(inst)
	}

	if len(inst.StepQueue) == 0 {
		inst.Status = model.StatusCompleted
		inst.NextRetryAt = nil
		if err := e.computeOutput(inst); err != nil {
			return e.failInstance(inst, err.Error())
		}
		e.log.Info("instance completed", "id", inst.ID, "process", inst.ProcessName)
		return e.saveAndNotify(inst)
	}

	step := inst.StepQueue[0]
	var selfOutput any

	if step.Call != nil {
		if step.Call.Type == model.CallTypeChild || step.Call.Type == model.CallTypeChildParallel {
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
		return e.saveAndNotify(inst)
	}

	if gotoID == model.GotoNext {
		inst.StepQueue = inst.StepQueue[1:]
	} else {
		// gotoID is a step reference like "$ship" — strip the sigil.
		newQueue, err := e.queueFromStep(inst, gotoID[1:])
		if err != nil {
			return e.failInstance(inst, err.Error())
		}
		inst.StepQueue = newQueue
	}

	inst.RetryCount = 0
	inst.NextRetryAt = nil
	e.log.Info("step completed", "id", inst.ID, "step", step.ID)
	return e.db.UpdateInstanceProgress(inst)
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
		msg := resp.ErrorMessage
		if msg == "" {
			msg = resp.ErrorCode
		}
		return nil, true, e.handleCallError(inst, step, msg, resp.ErrorCode)
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
// of the first case whose Case expression evaluates to true. An empty Case is a
// catch-all that always matches and must be the last entry when present. Returns ""
// when the switch list is empty (should not happen on validated definitions).
func (e *Engine) evalSwitch(inst *model.ProcessInstance, step *model.Step, selfOutput any) (string, error) {
	for _, c := range step.Switch {
		if c.Case == "" {
			return c.Goto, nil
		}
		ok, err := evalBool(c.Case, inst.ContextData, selfOutput)
		if err != nil {
			return "", fmt.Errorf("case %q: %w", c.Case, err)
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

// isRetryAllowed reports whether a retry is safe for the given step and error.
// For idempotent steps (the default) retries are always governed by on_error rules.
// For non-idempotent steps, a retry is only allowed when we know the remote call
// never started: start.* error codes, or an on_error rule with executed:false.
func isRetryAllowed(step *model.Step, errCode string, matched *model.ErrorCase) bool {
	if step.OnlyOnce == nil || !*step.OnlyOnce {
		return true
	}
	if matched != nil && matched.NotReached != nil && *matched.NotReached {
		return true
	}
	return strings.HasPrefix(errCode, "pre.")
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
	// If the process is being cancelled, suppress retries and honour the cancellation
	// unless retries are exhausted / not configured — in that case error takes precedence.
	if inst.Status == model.StatusCancelling {
		matched := matchOnError(step, errCode)
		if matched != nil && inst.RetryCount < matched.Retries && isRetryAllowed(step, errCode, matched) {
			// Retries remain but we're cancelling — skip the retry and cancel cleanly.
			e.log.Info("step failed during cancellation, skipping retry",
				"id", inst.ID, "step", step.ID, "code", errCode)
			return e.cancelInstance(inst)
		}
		// No retries available — error takes precedence over cancellation.
		return e.failInstance(inst, fmt.Sprintf("step %q: %s: %s", step.ID, errCode, errMsg))
	}

	matched := matchOnError(step, errCode)

	if matched != nil && inst.RetryCount < matched.Retries && isRetryAllowed(step, errCode, matched) {
		inst.RetryCount++
		next := time.Now().Add(e.retryDelay(inst.RetryCount))
		inst.NextRetryAt = &next
		e.log.Warn("step failed, scheduling retry",
			"id", inst.ID, "step", step.ID,
			"attempt", inst.RetryCount, "max", matched.Retries,
			"next_retry", next.Format(time.RFC3339),
			"code", errCode, "err", errMsg,
		)
		return e.db.UpdateInstance(inst)
	}

	inst.ContextData["error"] = map[string]any{
		"step":    step.ID,
		"message": errMsg,
		"code":    errCode,
	}

	if matched != nil && matched.Goto != "" {
		if matched.Goto == model.GotoEnd {
			inst.Status = model.StatusCompleted
			inst.NextRetryAt = nil
			e.log.Info("instance completed via error route", "id", inst.ID, "step", step.ID, "code", errCode)
			return e.saveAndNotify(inst)
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
	inst.WaitState = model.WaitStateNone
	inst.Error = reason
	inst.NextRetryAt = nil
	e.log.Error("instance failed", "id", inst.ID, "reason", reason)
	return e.saveAndNotify(inst)
}

func (e *Engine) cancelInstance(inst *model.ProcessInstance) error {
	inst.Status = model.StatusCancelled
	inst.WaitState = model.WaitStateNone
	inst.NextRetryAt = nil
	e.log.Info("instance cancelled", "id", inst.ID)
	return e.saveAndNotify(inst)
}

// saveAndNotify is the single exit point for all terminal instance states.
// For root instances and failed instances it saves directly; for non-failed child
// instances it uses FinishChild, which atomically saves the child and transitions
// the parent to WaitStateCollecting when all siblings are done.
func (e *Engine) saveAndNotify(inst *model.ProcessInstance) error {
	if inst.ParentID == "" {
		return e.db.UpdateInstance(inst)
	}
	if inst.Status == model.StatusFailed {
		return e.db.FailInstanceAndAncestors(inst)
	}
	return e.db.FinishChild(inst)
}

// runChildProcesses handles the two-phase child lifecycle:
//
//  1. WaitStateNone → spawn children, suspend parent (wait_state='waiting').
//  2. WaitStateCollecting → all children are terminal; merge their outputs into
//     parent context and return so advance() continues past this step.
//
// A cancelling parent spawns cancelling children: they self-cancel and call
// FinishChild, which transitions the parent to WaitStateCollecting normally.
func (e *Engine) runChildProcesses(ctx context.Context, inst *model.ProcessInstance, step *model.Step) (any, bool, error) {
	// Phase 2: parent woke up with children done — collect their outputs.
	if inst.WaitState == model.WaitStateCollecting {
		if err := e.db.CollectChildOutputs(ctx, inst, step); err != nil {
			inst.WaitState = model.WaitStateNone
			return nil, true, e.failInstance(inst, fmt.Sprintf("step %q collect: %v", step.ID, err))
		}
		inst.WaitState = model.WaitStateNone
		output := inst.ContextData["outputs"].(map[string]any)[step.ID]
		e.log.Info("parent collected child outputs", "id", inst.ID, "step", step.ID)
		return output, false, nil
	}

	// Phase 1: spawn children.
	childCallStack := append(inst.CallStack, inst.ID)

	if inst.ContextData["outputs"] == nil {
		inst.ContextData["outputs"] = map[string]any{}
	}

	var children []*model.ProcessInstance
	switch step.Call.Type {
	case model.CallTypeChild:
		child, err := e.buildSingleChild(ctx, inst, step, childCallStack)
		if err != nil {
			return nil, true, err
		}
		inst.ContextData["outputs"].(map[string]any)[step.ID] = child.ID
		children = []*model.ProcessInstance{child}
	case model.CallTypeChildParallel:
		parallel, err := e.buildParallelChildren(ctx, inst, step, childCallStack)
		if err != nil {
			return nil, true, err
		}
		placeholder := make(map[string]any, len(parallel))
		for _, c := range parallel {
			key, _ := c.ContextData["_spawn_child_key"].(string)
			placeholder[key] = c.ID
		}
		inst.ContextData["outputs"].(map[string]any)[step.ID] = placeholder
		children = parallel
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

	inst.RetryCount = 0
	inst.NextRetryAt = nil

	if err := e.db.SpawnChildrenAndWait(ctx, inst, children); err != nil {
		return nil, true, e.failInstance(inst, fmt.Sprintf("step %q spawn: %v", step.ID, err))
	}

	e.log.Info("parent waiting for children", "id", inst.ID, "step", step.ID, "children", len(children))
	return nil, true, nil
}

// buildSingleChild resolves the child definition, evaluates input, and constructs
// a ProcessInstance ready to be saved. It does not persist anything.
func (e *Engine) buildSingleChild(ctx context.Context, inst *model.ProcessInstance, step *model.Step, callStack []string) (*model.ProcessInstance, error) {
	name := step.Call.Name
	version := step.Call.Version
	if version == 0 {
		if name == inst.ProcessName {
			version = inst.ProcessVersion
		} else {
			var err error
			version, err = e.db.GetDependencyVersion(inst.ProcessName, inst.ProcessVersion, step.ID, "")
			if err != nil {
				version, err = e.db.LatestVersion(name)
				if err != nil {
					return nil, e.failInstance(inst, fmt.Sprintf("step %q child: %v", step.ID, err))
				}
			}
		}
	}
	def, err := e.db.GetDefinition(name, version)
	if err != nil {
		return nil, e.failInstance(inst, fmt.Sprintf("step %q child: %v", step.ID, err))
	}
	input, err := e.evalChildInput(inst, step.ID, "child", step.Call.Input)
	if err != nil {
		return nil, e.failInstance(inst, err.Error())
	}
	if err := def.ValidateInput(input); err != nil {
		return nil, e.failInstance(inst, fmt.Sprintf("step %q child input validation: %v", step.ID, err))
	}
	childCtx := map[string]any{
		"input":            input,
		"outputs":          map[string]any{},
		"output_order":     []string{},
		"error":            nil,
		"_spawn_call_type": string(model.CallTypeChild),
	}
	if step.Call.OutputSchema != nil {
		if b, err := json.Marshal(step.Call.OutputSchema); err == nil {
			childCtx["_spawn_output_schema"] = string(b)
		}
	}
	return &model.ProcessInstance{
		ID:             uuid.NewString(),
		ProcessName:    def.Name,
		ProcessVersion: version,
		StepQueue:      def.Steps,
		ContextData:    childCtx,
		Status:         model.StatusRunning,
		ParentID:       inst.ID,
		SpawnStepID:    step.ID,
		CallStack:      callStack,
	}, nil
}

// buildParallelChildren resolves definitions, evaluates inputs, and constructs
// ProcessInstances for all parallel children. Does not persist anything.
func (e *Engine) buildParallelChildren(ctx context.Context, inst *model.ProcessInstance, step *model.Step, callStack []string) ([]*model.ProcessInstance, error) {
	keys := make([]string, 0, len(step.Call.Children))
	for key := range step.Call.Children {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	children := make([]*model.ProcessInstance, 0, len(step.Call.Children))
	for _, key := range keys {
		entry := step.Call.Children[key]
		version := entry.Version
		if version == 0 {
			if entry.Name == inst.ProcessName {
				version = inst.ProcessVersion
			} else {
				var err error
				version, err = e.db.GetDependencyVersion(inst.ProcessName, inst.ProcessVersion, step.ID, key)
				if err != nil {
					version, err = e.db.LatestVersion(entry.Name)
					if err != nil {
						return nil, e.failInstance(inst, fmt.Sprintf("step %q child_parallel[%q]: %v", step.ID, key, err))
					}
				}
			}
		}
		def, err := e.db.GetDefinition(entry.Name, version)
		if err != nil {
			return nil, e.failInstance(inst, fmt.Sprintf("step %q child_parallel[%q]: %v", step.ID, key, err))
		}
		input, err := e.evalChildInput(inst, step.ID, fmt.Sprintf("child_parallel[%q]", key), entry.Input)
		if err != nil {
			return nil, e.failInstance(inst, err.Error())
		}
		if err := def.ValidateInput(input); err != nil {
			return nil, e.failInstance(inst, fmt.Sprintf("step %q child_parallel[%q] input validation: %v", step.ID, key, err))
		}
		childCtx := map[string]any{
			"input":            input,
			"outputs":          map[string]any{},
			"output_order":     []string{},
			"error":            nil,
			"_spawn_call_type": string(model.CallTypeChildParallel),
			"_spawn_child_key": key,
		}
		if entry.OutputSchema != nil {
			if b, err := json.Marshal(entry.OutputSchema); err == nil {
				childCtx["_spawn_output_schema"] = string(b)
			}
		}
		children = append(children, &model.ProcessInstance{
			ID:             uuid.NewString(),
			ProcessName:    def.Name,
			ProcessVersion: version,
			StepQueue:      def.Steps,
			ContextData:    childCtx,
			Status:         model.StatusRunning,
			ParentID:       inst.ID,
			SpawnStepID:    step.ID,
			CallStack:      callStack,
		})
	}
	return children, nil
}

func (e *Engine) evalChildInput(inst *model.ProcessInstance, stepID, label string, inputExprs map[string]string) (map[string]any, error) {
	result := make(map[string]any, len(inputExprs))
	for k, expr := range inputExprs {
		val, err := evalAny(expr, inst.ContextData)
		if err != nil {
			return nil, fmt.Errorf("step %q %s input %q: %v", stepID, label, k, err)
		}
		result[k] = val
	}
	return result, nil
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
