package engine

import (
	"context"
	"fmt"
	"log/slog"
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
}

// New creates an Engine. pollEvery controls how often SQLite is checked for work.
func New(database *db.DB, pollEvery time.Duration, log *slog.Logger) *Engine {
	return &Engine{
		db:        database,
		eval:      Evaluator{},
		pollEvery: pollEvery,
		log:       log,
	}
}

// Run starts the engine loop and blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	e.log.Info("engine started", "poll_interval", e.pollEvery)
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

func (e *Engine) tick(ctx context.Context) {
	instances, err := e.db.PendingInstances()
	if err != nil {
		e.log.Error("poll instances", "err", err)
		return
	}
	for _, inst := range instances {
		if err := e.advance(ctx, inst); err != nil {
			e.log.Error("advance instance", "id", inst.ID, "err", err)
		}
	}
}

// advance executes the next step in the instance's queue.
func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) error {
	if len(inst.StepQueue) == 0 {
		inst.Status = model.StatusCompleted
		inst.NextRetryAt = nil
		e.log.Info("instance completed", "id", inst.ID, "process", inst.ProcessName)
		return e.db.UpdateInstance(inst)
	}

	step := inst.StepQueue[0]

	switch step.Type {
	case model.StepTypeConditional:
		return e.evalConditional(inst, step)

	case model.StepTypeTask:
		return e.execTask(ctx, inst, step)

	default:
		return fmt.Errorf("unknown step type %q in instance %s", step.Type, inst.ID)
	}
}

// evalConditional resolves a conditional step immediately (no network call).
// The appropriate branch is prepended to the queue and persisted.
func (e *Engine) evalConditional(inst *model.ProcessInstance, step *model.Step) error {
	result, err := e.eval.Eval(step.Condition, inst.ContextData)
	if err != nil {
		return e.failInstance(inst, fmt.Sprintf("eval condition %q: %v", step.Condition, err))
	}

	rest := inst.StepQueue[1:]
	var branch []*model.Step
	if result {
		branch = step.Then
	} else {
		branch = step.Else
	}

	inst.StepQueue = append(branch, rest...)
	inst.NextRetryAt = nil
	e.log.Debug("conditional evaluated", "id", inst.ID, "condition", step.Condition, "result", result)
	return e.db.UpdateInstance(inst)
}

// execTask sends a message to the step's endpoint and handles the response.
func (e *Engine) execTask(ctx context.Context, inst *model.ProcessInstance, step *model.Step) error {
	timeout := time.Duration(step.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data, err := e.buildTaskData(inst, step)
	if err != nil {
		return e.failInstance(inst, fmt.Sprintf("step %q params: %v", step.ID, err))
	}

	req := transport.Request{
		InstanceID: inst.ID,
		StepID:     step.ID,
		Data:       data,
	}

	e.log.Debug("executing step", "id", inst.ID, "step", step.ID, "transport", step.Transport)

	resp, err := transport.Send(taskCtx, step, req)
	if err != nil {
		return e.handleStepError(inst, step, err.Error())
	}
	if resp.Status != "ok" {
		return e.handleStepError(inst, step, resp.Error)
	}

	if err := step.ValidateOutput(resp.Output); err != nil {
		return e.failInstance(inst, fmt.Sprintf("step %q output validation: %v", step.ID, err))
	}

	// Store output under outputs.<step_id> for unambiguous addressing.
	if inst.ContextData["outputs"] == nil {
		inst.ContextData["outputs"] = map[string]any{}
	}
	inst.ContextData["outputs"].(map[string]any)[step.ID] = resp.Output

	// Step succeeded: pop it from the queue.
	inst.StepQueue = inst.StepQueue[1:]
	inst.RetryCount = 0
	inst.NextRetryAt = nil

	e.log.Info("step completed", "id", inst.ID, "step", step.ID)
	return e.db.UpdateInstance(inst)
}

// handleStepError either schedules a retry or marks the instance as failed.
func (e *Engine) handleStepError(inst *model.ProcessInstance, step *model.Step, errMsg string) error {
	maxRetries := step.Retries
	if inst.RetryCount < maxRetries {
		inst.RetryCount++
		next := time.Now().Add(transport.RetryDelay(inst.RetryCount))
		inst.NextRetryAt = &next
		e.log.Warn("step failed, scheduling retry",
			"id", inst.ID, "step", step.ID,
			"attempt", inst.RetryCount, "max", maxRetries,
			"next_retry", next.Format(time.RFC3339),
			"err", errMsg,
		)
		return e.db.UpdateInstance(inst)
	}
	return e.failInstance(inst, fmt.Sprintf("step %q failed after %d retries: %s", step.ID, maxRetries, errMsg))
}

func (e *Engine) buildTaskData(inst *model.ProcessInstance, step *model.Step) (map[string]any, error) {
	if len(step.Params) == 0 {
		return inst.ContextData, nil
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
