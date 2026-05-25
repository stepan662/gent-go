package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gent/internal/db"
	"gent/internal/model"
	"gent/internal/transport"
)

// Engine is the main orchestration loop. It polls SQLite for pending instances
// and advances each one step at a time.
type Engine struct {
	db        *db.DB
	eval      Evaluator
	pollEvery time.Duration
	log       *slog.Logger
	sem       chan struct{}
}

// New creates an Engine. pollEvery controls how often SQLite is checked for work.
// maxConcurrent limits how many instances are processed in parallel and how many
// are fetched per tick.
func New(database *db.DB, pollEvery time.Duration, maxConcurrent int, log *slog.Logger) *Engine {
	return &Engine{
		db:        database,
		eval:      Evaluator{},
		pollEvery: pollEvery,
		log:       log,
		sem:       make(chan struct{}, maxConcurrent),
	}
}

// Run starts the engine loop and blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	e.log.Info("engine started", "poll_interval", e.pollEvery, "max_concurrent", cap(e.sem))
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

// tick fetches pending instances and processes each in its own goroutine.
// It blocks until all goroutines finish, so ticks never overlap and the same
// instance is never advanced twice concurrently.
func (e *Engine) tick(ctx context.Context) {
	instances, err := e.db.PendingInstances(cap(e.sem))
	if err != nil {
		e.log.Error("poll instances", "err", err)
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
// Each step may have an action (Transport+Endpoint), a switch, or both.
// The action runs first; then the switch is evaluated with the action's output
// available as "self". A matching switch case jumps to the named step; no match
// advances to the next step in the queue.
func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) error {
	if len(inst.StepQueue) == 0 {
		inst.Status = model.StatusCompleted
		inst.NextRetryAt = nil
		e.log.Info("instance completed", "id", inst.ID, "process", inst.ProcessName)
		return e.db.UpdateInstance(inst)
	}

	step := inst.StepQueue[0]
	var selfOutput any

	if step.Transport != "" {
		out, done, err := e.executeAction(ctx, inst, step)
		if done || err != nil {
			return err
		}
		selfOutput = out
	}

	gotoID, err := e.evalSwitch(inst, step, selfOutput)
	if err != nil {
		return e.failInstance(inst, fmt.Sprintf("step %q switch: %v", step.ID, err))
	}

	if gotoID == model.GotoEnd {
		inst.Status = model.StatusCompleted
		inst.RetryCount = 0
		inst.NextRetryAt = nil
		e.log.Info("instance completed", "id", inst.ID, "step", step.ID)
		return e.db.UpdateInstance(inst)
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

	e.log.Debug("executing step", "id", inst.ID, "step", step.ID, "transport", step.Transport)

	resp, err := transport.Send(taskCtx, step, req)
	if err != nil {
		return nil, true, e.retryOrFail(inst, step, err.Error())
	}
	if resp.Status != "ok" {
		return nil, true, e.retryOrFail(inst, step, resp.Error)
	}

	if err := step.ValidateOutput(resp.Output); err != nil {
		return nil, true, e.failInstance(inst, fmt.Sprintf("step %q output validation: %v", step.ID, err))
	}

	// Store output under outputs.<step_id> for unambiguous addressing.
	if inst.ContextData["outputs"] == nil {
		inst.ContextData["outputs"] = map[string]any{}
	}
	inst.ContextData["outputs"].(map[string]any)[step.ID] = resp.Output

	// Track step completion order so outputs can be serialized in step order.
	var order []string
	switch v := inst.ContextData["output_order"].(type) {
	case []string:
		order = v
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				order = append(order, s)
			}
		}
	}
	inst.ContextData["output_order"] = append(order, step.ID)
	inst.RetryCount = 0

	return resp.Output, false, nil
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
		ok, err := e.eval.EvalBool(c.When, inst.ContextData, selfOutput)
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

// retryOrFail either schedules a retry or marks the instance as permanently failed.
func (e *Engine) retryOrFail(inst *model.ProcessInstance, step *model.Step, errMsg string) error {
	if inst.RetryCount < step.Retries {
		inst.RetryCount++
		next := time.Now().Add(transport.RetryDelay(inst.RetryCount))
		inst.NextRetryAt = &next
		e.log.Warn("step failed, scheduling retry",
			"id", inst.ID, "step", step.ID,
			"attempt", inst.RetryCount, "max", step.Retries,
			"next_retry", next.Format(time.RFC3339),
			"err", errMsg,
		)
		return e.db.UpdateInstance(inst)
	}
	return e.failInstance(inst, fmt.Sprintf("step %q failed after %d retries: %s", step.ID, step.Retries, errMsg))
}

func (e *Engine) buildTaskData(inst *model.ProcessInstance, step *model.Step) (map[string]any, error) {
	if len(step.Params) == 0 {
		return map[string]any{}, nil
	}
	result := make(map[string]any, len(step.Params))
	for name, expression := range step.Params {
		val, err := e.eval.EvalAny(expression, inst.ContextData)
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
	return e.db.UpdateInstance(inst)
}
