package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gent/internal/db"
	"gent/internal/idgen"
	"gent/internal/model"
	"gent/internal/transport"
)

const (
	defaultLeaseDuration      = 10 * time.Second
	defaultLeaseRenewInterval = 3 * time.Second
	defaultPayloadBytes       = 2048
)

// LogConfig controls how much the engine persists to each instance's audit log
// and for how long.
type LogConfig struct {
	Payloads     bool          // capture truncated request/response snippets on task events
	PayloadBytes int           // max bytes per captured snippet (<=0 → defaultPayloadBytes)
	Retention    time.Duration // prune audit logs older than this; 0 = keep forever
}

const logPruneInterval = time.Minute

// OverwhelmError is returned by Run when the engine re-claimed an instance it was
// still advancing: the in-flight advance outlived its lease, so lease renewal can't
// keep up. There is no safe recovery — in a multi-worker deployment another worker
// would already have stolen and double-executed the instance — so the pump stops
// claiming, in-flight work is drained, and Run returns this. The binary should log it
// and exit non-zero so the worker is restarted; lowering --max-concurrent or raising
// the lease duration prevents recurrence.
type OverwhelmError struct {
	InstanceID    string
	WorkerID      string
	Lease         time.Duration
	MaxConcurrent int
}

func (e *OverwhelmError) Error() string {
	return fmt.Sprintf("engine overwhelmed: re-claimed instance %s still being advanced by worker %s; "+
		"lease renewal cannot keep up (lease=%s, max_concurrent=%d). Lower --max-concurrent or increase the lease duration.",
		e.InstanceID, e.WorkerID, e.Lease, e.MaxConcurrent)
}

// Engine is the main orchestration loop. It polls the database for pending
// instances and advances each one task at a time.
type Engine struct {
	db                 *db.DB
	pollEvery          time.Duration
	immediateRetries   bool
	leaseDuration      time.Duration // how long a claimed instance is leased to this worker
	leaseRenewInterval time.Duration // how often the renewer re-stamps this worker's leases
	logCfg             LogConfig     // audit-log persistence settings
	log                *slog.Logger
	sem                chan struct{}
	workerID           string
	inflight           sync.Map // instance IDs this worker is currently advancing (detects overwhelm via self-reclaim)
}

// New creates an Engine. pollEvery controls how often SQLite is checked for work.
// maxConcurrent limits how many instances are processed in parallel and how many
// are fetched per tick. immediateRetries disables exponential backoff (retries fire
// instantly); intended for tests only. leaseDuration / leaseRenewInterval control
// lease ownership and renewal cadence; pass 0 for either to use the defaults
// (10s / 3s). The renew interval must be comfortably shorter than the lease so the
// renewer can re-stamp leases before they expire.
func New(database *db.DB, pollEvery time.Duration, maxConcurrent int, immediateRetries bool, leaseDuration, leaseRenewInterval time.Duration, logCfg LogConfig, log *slog.Logger) *Engine {
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	if leaseRenewInterval <= 0 {
		leaseRenewInterval = defaultLeaseRenewInterval
	}
	return &Engine{
		db:                 database,
		pollEvery:          pollEvery,
		immediateRetries:   immediateRetries,
		leaseDuration:      leaseDuration,
		leaseRenewInterval: leaseRenewInterval,
		logCfg:             logCfg,
		log:                log,
		sem:                make(chan struct{}, maxConcurrent),
		workerID:           workerID,
	}
}

func (e *Engine) retryDelay(attempt int) time.Duration {
	if e.immediateRetries {
		return 0
	}
	return transport.RetryDelay(attempt)
}

// Run starts the engine loop and blocks until ctx is cancelled. It returns nil on a
// clean shutdown, or an *OverwhelmError if the pump stopped because the engine could
// not keep up with its leases (in-flight work is drained before it returns either way).
// When pollEvery is zero the engine does not auto-tick; call Tick explicitly.
func (e *Engine) Run(ctx context.Context) error {
	e.log.Info("engine started", "poll_interval", e.pollEvery, "max_concurrent", cap(e.sem), "worker_id", e.workerID)

	go e.leaseRenewer(ctx)

	if e.logCfg.Retention > 0 {
		go e.logPruner(ctx)
	}

	if e.pollEvery == 0 {
		e.log.Info("engine in manual tick mode")
		<-ctx.Done()
		e.log.Info("engine stopped")
		return nil
	}

	err := e.runPump(ctx)
	if err != nil {
		e.log.Error("engine stopped after draining in-flight work", "err", err)
	} else {
		e.log.Info("engine stopped")
	}
	return err
}

// runPump is the continuous claim/dispatch loop used when pollEvery > 0. Unlike
// Tick (a synchronous batch with a wg.Wait barrier, still used by the /tick
// endpoint and manual mode), the pump never waits for a batch to finish: it tops
// up work as worker slots free, so a slow instance never stalls the others.
//
// e.sem doubles as the idle detector and the concurrency bound. Reserving one
// slot blocks exactly when all workers are busy and wakes the instant one frees,
// giving natural backpressure and immediate top-up without a separate counter.
// When a claim finds nothing the pump releases its slot and waits on the ticker —
// the same adaptive cadence the old loop had. A WaitGroup drains in-flight
// advances on shutdown.
func (e *Engine) runPump(ctx context.Context) error {
	ticker := time.NewTicker(e.pollEvery)
	defer ticker.Stop()

	var wg sync.WaitGroup
	defer wg.Wait() // stop claiming, finish in-flight advances, then return

	for {
		// Block for one slot (wakes the instant a worker frees), then grab any
		// other free slots without blocking. Acquiring all slots up front means the
		// dispatch loop below never blocks: combined with the claim's
		// wait_state<>'waiting' filter, that closes the window where an in-flight
		// advance could finish between the claim and the dispatch guard and let a
		// stale snapshot through. slots is the exact claim limit, so in-flight never
		// exceeds maxConcurrent.
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		slots := 1
	fill:
		for slots < cap(e.sem) {
			select {
			case e.sem <- struct{}{}:
				slots++
			default:
				break fill
			}
		}

		insts, err := e.db.ClaimInstances(e.workerID, e.leaseDuration, slots)
		// Release the slots we acquired but won't use (claimed fewer than slots).
		for i := len(insts); i < slots; i++ {
			<-e.sem
		}
		if err != nil || len(insts) == 0 {
			if err != nil {
				e.log.Error("claim instances", "err", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
			continue
		}

		// Each dispatch consumes one pre-acquired slot (released when the advance
		// finishes). If dispatch reports overwhelm, stop claiming and return: the
		// deferred wg.Wait drains the advances already in flight first.
		for _, inst := range insts {
			if err := e.dispatch(ctx, &wg, inst); err != nil {
				return err
			}
		}
	}
}

// dispatch runs one instance's advance in its own goroutine and releases its
// e.sem slot when done. The caller must have already reserved the slot. It returns
// an *OverwhelmError (without starting an advance) if the instance is already
// in-flight on this worker; the caller stops the pump and drains.
func (e *Engine) dispatch(ctx context.Context, wg *sync.WaitGroup, inst *model.ProcessInstance) error {
	// If we just re-claimed an instance this worker is still advancing, its lease
	// expired before the advance finished: lease renewal can't keep up, so the
	// engine is overwhelmed. This is inherent to a lease-based design — in a
	// multi-worker deployment another worker would already have stolen and
	// double-executed the instance. There is no reliable way to recover, so we
	// stop the pump and surface the error rather than silently corrupting state.
	// The operator should lower --max-concurrent or increase the lease duration.
	//
	// This detection is exact: runAdvance drops the marker only just before the
	// write that frees the instance, so an in-flight instance is claimable only once
	// its lease has actually expired. A re-claim that still finds the marker is
	// therefore a genuine overwhelm, never an advance that already finished.
	if _, busy := e.inflight.LoadOrStore(inst.ID, struct{}{}); busy {
		return &OverwhelmError{
			InstanceID:    inst.ID,
			WorkerID:      e.workerID,
			Lease:         e.leaseDuration,
			MaxConcurrent: cap(e.sem),
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { <-e.sem }()
		// runAdvance drops the inflight marker (stored above) before persisting.
		if err := e.runAdvance(ctx, inst); err != nil {
			e.log.Error("advance instance", "id", inst.ID, "err", err)
		}
	}()
	return nil
}

// leaseRenewer renews all leases held by this worker in a single query every
// leaseRenewInterval. Running as its own goroutine means renewals are never
// blocked by a long tick.
func (e *Engine) leaseRenewer(ctx context.Context) {
	ticker := time.NewTicker(e.leaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.db.RenewWorkerLeases(e.workerID, e.leaseDuration); err != nil {
				e.log.Error("renew worker leases", "err", err)
			}
		}
	}
}

// logPruner periodically deletes audit-log rows older than the retention window.
// Only started when retention > 0. The cutoff uses the DB clock so a clock shift
// (e.g. via /tick in tests) expires logs without a real wait.
func (e *Engine) logPruner(ctx context.Context) {
	ticker := time.NewTicker(logPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.pruneLogs()
		}
	}
}

// pruneLogs deletes audit logs past the retention window. No-op when retention
// is disabled. Best-effort: a failure is logged and otherwise ignored.
func (e *Engine) pruneLogs() {
	if e.logCfg.Retention <= 0 {
		return
	}
	cutoff := db.Now().Add(-e.logCfg.Retention).UnixMilli()
	if n, err := e.db.PruneLogs(cutoff); err != nil {
		e.log.Error("prune logs", "err", err)
	} else if n > 0 {
		e.log.Debug("pruned audit logs", "count", n, "older_than", e.logCfg.Retention)
	}
}

// ManualTick reports whether the engine runs in manual-tick mode (pollEvery == 0),
// i.e. it does not auto-advance and must be driven by explicit Tick calls. The
// /tick endpoint is only meaningful in this mode; when the continuous pump is
// running, calling Tick out-of-band would race the pump, so the endpoint refuses.
func (e *Engine) ManualTick() bool { return e.pollEvery == 0 }

// Tick claims pending instances and processes each in its own goroutine.
// It blocks until all goroutines finish, so ticks never overlap and the same
// instance is never advanced twice concurrently. Returns the number of instances
// that were claimed and processed.
func (e *Engine) Tick(ctx context.Context) (int, error) {
	e.pruneLogs()
	instances, err := e.db.ClaimInstances(e.workerID, e.leaseDuration, cap(e.sem))
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
			if err := e.runAdvance(ctx, inst); err != nil {
				e.log.Error("advance instance", "id", inst.ID, "err", err)
			}
		}(inst)
	}
	wg.Wait()
	return len(instances), nil
}

// advanceOutcome is the next persisted state that advance() computes without
// writing it to the DB. runAdvance drops the in-flight marker first, then persist
// applies the outcome — so the lease-releasing write always happens after the
// marker is gone, in one place, and an instance is never simultaneously free in the
// DB and still marked in memory (which dispatch would misread as re-claiming live
// work). advance() is a pure state machine over the instance's own row; the one
// exception is a child spawn, which is a multi-row transaction that parks the parent
// (non-runnable, so the marker order is irrelevant) — it persists itself and returns
// outcomeNoop.
type advanceOutcome struct {
	kind outcomeKind
}

type outcomeKind uint8

const (
	outcomeProgress outcomeKind = iota // running checkpoint        → UpdateInstanceProgress
	outcomeUpdate                      // running, status/error set → UpdateInstance
	outcomeTerminal                    // completed/failed/cancelled → saveAndNotify
	outcomeNoop                        // advance already persisted (child spawn parked the parent)
)

// stop wraps an outcome as a non-nil pointer, the signal call helpers use to tell
// advance to stop the task loop and return this outcome (nil means "continue").
func stop(o advanceOutcome) *advanceOutcome { return &o }

// persist applies an advance outcome to the DB. It is the single place an in-flight
// advance releases its lease; runAdvance calls it after dropping the marker.
func (e *Engine) persist(inst *model.ProcessInstance, o advanceOutcome) error {
	switch o.kind {
	case outcomeTerminal:
		return e.saveAndNotify(inst)
	case outcomeProgress:
		return e.db.UpdateInstanceProgress(inst)
	case outcomeUpdate:
		return e.db.UpdateInstance(inst)
	case outcomeNoop:
		return nil
	default:
		return fmt.Errorf("unknown advance outcome %d", o.kind)
	}
}

// runAdvance advances one instance, then drops its in-flight marker before
// persisting the resulting state. Doing the delete before the store closes the
// window where the instance is free in the DB but still marked in memory. (For Tick,
// which keeps no marker, the delete is a harmless no-op.)
func (e *Engine) runAdvance(ctx context.Context, inst *model.ProcessInstance) error {
	outcome := e.advance(ctx, inst)
	e.inflight.Delete(inst.ID)
	return e.persist(inst, outcome)
}

// advance executes the next task in the instance's queue, returning the outcome to
// persist (it performs no lease-releasing write itself — runAdvance does).
// Each task may have a call, a switch, or both.
// The call runs first; then the switch is evaluated with the call's output
// available as "self". A matching switch case jumps to the named task; no match
// advances to the next task in the queue.
func (e *Engine) advance(ctx context.Context, inst *model.ProcessInstance) advanceOutcome {
	if inst.Status == model.StatusFailing {
		return e.settleFailing(inst)
	}
	if inst.Status == model.StatusCancelling {
		return e.cancelInstance(inst)
	}

	// Lease takeover: this instance was reclaimed from an expired lease, so its
	// front task (TaskQueue[0]) may have started executing on the previous owner
	// before it crashed/stalled. Re-running is fine for idempotent tasks, but an
	// only_once (non-idempotent) call task cannot be safely re-executed — the call
	// may already have happened — so fail the instance to honour at-most-once.
	if inst.ReclaimedExpired {
		taskID := ""
		if len(inst.TaskQueue) > 0 {
			taskID = inst.TaskQueue[0].ID
		}
		e.log.Warn("reclaimed instance with expired lease; previous owner crashed or stalled mid-task",
			"id", inst.ID, "process", inst.ProcessName, "task", taskID)
		if len(inst.TaskQueue) > 0 {
			s := inst.TaskQueue[0]
			if s.Action != nil && s.OnlyOnce != nil && *s.OnlyOnce {
				return e.failInstance(inst, fmt.Sprintf(
					"task %q is only_once and was interrupted by a lease takeover; cannot re-execute", s.ID))
			}
		}
	}

	// Process tasks in a loop. A call-less task (pure switch/routing) has no
	// external side effects, so once it resolves its goto we continue to the next
	// task in-memory without persisting — collapsing a chain of switch-only tasks
	// into a single claim and a single DB write at the boundary. We stop and
	// persist at the first task that has a call (child spawn or remote action), at
	// a terminal state, or after maxInlineTasks transitions (a guard against a
	// pathological all-switch loop holding the goroutine/lease forever).
	//
	// This is crash-safe: skipping persistence between call-less tasks is fine
	// because they only re-evaluate switches against already-persisted context, so
	// resuming from the last persisted task queue is deterministic. Durable state
	// only changes at the boundaries (spawn txn, action result, terminal save),
	// each of which writes the live task queue.
	const maxInlineTasks = 1000
	for i := 0; ; i++ {
		if len(inst.TaskQueue) == 0 {
			inst.Status = model.StatusCompleted
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, err.Error())
			}
			e.log.Info("instance completed", "id", inst.ID, "process", inst.ProcessName)
			e.audit(inst, model.LogInfo, model.EventInstanceDone, "", "", "", nil)
			return advanceOutcome{kind: outcomeTerminal}
		}

		task := inst.TaskQueue[0]
		hasCall := task.Action != nil
		var actionResult any

		// Capture this task's prior output before the action can overwrite it, so an
		// output map may reference self.previous (the value from the last loop iteration).
		var priorOutput any
		if task.Output.Present() {
			if outs, ok := inst.ContextData["outputs"].(map[string]any); ok {
				priorOutput = outs[task.ID]
			}
		}

		if hasCall {
			switch task.Action.Type {
			case model.ActionTypeChild, model.ActionTypeChildParallel:
				out, done := e.runChildProcesses(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			case model.ActionTypeDelay:
				if done := e.runDelay(inst, task); done != nil {
					return *done
				}
				// Timer fired: fall through to the switch with no action result.
			case model.ActionTypeExternal:
				out, done := e.runExternal(inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			default: // rest, script
				out, done := e.executeAction(ctx, inst, task)
				if done != nil {
					return *done
				}
				actionResult = out
			}
		}

		// The output projection (if any) is the only thing exported (outputs.taskID).
		// The raw result is never stored; it is exposed transiently to this task's own
		// output/switch as self.result.
		var taskOutput any
		hasOutput := task.Output.Present()
		if hasOutput {
			remapped, err := e.evalTaskOutput(inst, task, actionResult, priorOutput)
			if err != nil {
				return e.failInstance(inst, fmt.Sprintf("task %q output: %v", task.ID, err))
			}
			e.setTaskOutput(inst, task.ID, remapped)
			taskOutput = remapped
		}

		// self is this task's transient scope: result (raw action result) and
		// previous (its own prior output), plus output (the projection) only when one
		// is defined. None of these but the projection persist beyond this task.
		self := map[string]any{"result": actionResult, "previous": priorOutput}
		if hasOutput {
			self["output"] = taskOutput
		}
		gotoID, err := e.evalSwitch(inst, task, self)
		if err != nil {
			return e.failInstance(inst, fmt.Sprintf("task %q switch: %v", task.ID, err))
		}
		if gotoID == "" {
			// Validation requires a catch-all case, but legacy rows in the DB may
			// predate that rule — fail the instance rather than panic on gotoID[1:].
			return e.failInstance(inst, fmt.Sprintf("task %q switch: no case matched", task.ID))
		}

		if gotoID == model.GotoEnd {
			inst.Status = model.StatusCompleted
			inst.RetryCount = 0
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, err.Error())
			}
			e.log.Info("instance completed", "id", inst.ID, "task", task.ID)
			e.audit(inst, model.LogInfo, model.EventInstanceDone, task.ID, "", "", nil)
			return advanceOutcome{kind: outcomeTerminal}
		}

		if gotoID == model.GotoNext {
			inst.TaskQueue = inst.TaskQueue[1:]
		} else {
			// gotoID is a task reference like "$ship" — strip the sigil.
			newQueue, err := e.queueFromTask(inst, gotoID[1:])
			if err != nil {
				return e.failInstance(inst, err.Error())
			}
			inst.TaskQueue = newQueue
		}

		inst.RetryCount = 0
		inst.WakeAt = nil
		e.log.Info("task completed", "id", inst.ID, "task", task.ID)
		e.audit(inst, model.LogInfo, model.EventTaskCompleted, task.ID, "", "", map[string]any{"goto": gotoID})

		// A task with a call has just executed a side effect — checkpoint and yield.
		// A call-less routing task had none, so continue in-memory to the next task
		// unless we've hit the inline-task guard.
		if hasCall || i >= maxInlineTasks {
			return advanceOutcome{kind: outcomeProgress}
		}
	}
}

// executeAction sends a request to the task's endpoint and returns (output, done):
//   - done=nil: action succeeded; output is the task result.
//   - done!=nil: the task loop should stop and persist this outcome (retry, error
//     route, or permanent fail).
func (e *Engine) executeAction(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	timeout := time.Duration(task.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data, err := e.buildTaskData(inst, task)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q input: %v", task.ID, err)))
	}

	req := transport.Request{
		InstanceID: inst.ID,
		TaskID:     task.ID,
		Data:       data,
	}

	e.log.Debug("executing task", "id", inst.ID, "task", task.ID, "action_type", task.Action.Type)
	startDetail := map[string]any{"action_type": string(task.Action.Type)}
	if req := e.snippet(data); req != "" {
		startDetail["request"] = req
	}
	e.audit(inst, model.LogDebug, model.EventTaskStarted, task.ID, "", "", startDetail)

	resolvedHeaders, err := e.resolveHeaders(inst, task.Action)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q headers: %v", task.ID, err)))
	}

	resp, err := transport.Send(taskCtx, task.Action, resolvedHeaders, req)
	if err != nil {
		code := transport.ClassifyGoError(err)
		if task.Action.Type == model.ActionTypeScript {
			code = transport.ClassifyScriptError(err)
		}
		return nil, stop(e.handleCallError(inst, task, err.Error(), code))
	}
	if resp.ErrorCode != "" {
		msg := resp.ErrorMessage
		if msg == "" {
			msg = resp.ErrorCode
		}
		return nil, stop(e.handleCallError(inst, task, msg, resp.ErrorCode))
	}

	// result_schema validates the raw result; it does not export it. The result is
	// transient — available to this task's own output/switch as self.result. Only an
	// `output` projection adds anything to outputs.<id>.
	if err := task.Action.ValidateOutput(resp.Body); err != nil {
		return nil, stop(e.handleCallError(inst, task, err.Error(), "output.invalid"))
	}
	inst.RetryCount = 0

	var okDetail map[string]any
	if body := e.snippet(resp.Body); body != "" {
		okDetail = map[string]any{"response": body}
	}
	e.audit(inst, model.LogInfo, model.EventTaskSucceeded, task.ID, "", "", okDetail)

	return resp.Body, nil
}

// evalTaskOutput evaluates a task's output map against the context plus self,
// where self.result is the raw action result and self.previous is this task's
// prior output (its value from the last loop iteration, or nil on the first run).
func (e *Engine) evalTaskOutput(inst *model.ProcessInstance, task *model.Task, result, previous any) (any, error) {
	self := map[string]any{"result": result, "previous": previous}
	return evalShape(task.Output.Raw, evalEnv(inst.ContextData, self))
}

// setTaskOutput stores value as the task's exported output (outputs.taskID),
// recording the task in output_order the first time it produces output (a loop
// re-execution overwrites the value without re-appending).
func (e *Engine) setTaskOutput(inst *model.ProcessInstance, taskID string, value any) {
	if inst.ContextData["outputs"] == nil {
		inst.ContextData["outputs"] = map[string]any{}
	}
	outs := inst.ContextData["outputs"].(map[string]any)
	_, existed := outs[taskID]
	outs[taskID] = value
	if existed {
		return
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
	inst.ContextData["output_order"] = append(order, taskID)
}

// evalSwitch walks the task's switch cases in order and returns the Goto target
// of the first case whose Case expression evaluates to true. An empty Case is a
// catch-all that always matches and must be the last entry when present. Returns ""
// when the switch list is empty (should not happen on validated definitions).
func (e *Engine) evalSwitch(inst *model.ProcessInstance, task *model.Task, selfOutput any) (string, error) {
	for _, c := range task.Switch {
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

// queueFromTask looks up the process definition and returns all tasks starting
// from the one with the given ID. Used to implement switch goto jumps (including
// loops back to earlier tasks).
func (e *Engine) queueFromTask(inst *model.ProcessInstance, taskID string) ([]*model.Task, error) {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve goto: %w", err)
	}
	for i, s := range def.Tasks {
		if s.ID == taskID {
			return def.Tasks[i:], nil
		}
	}
	return nil, fmt.Errorf("goto task %q not found in %q v%d", taskID, inst.ProcessName, inst.ProcessVersion)
}

// isRetryAllowed reports whether a retry is safe for the given task and error.
// For idempotent tasks (the default) retries are always governed by on_error rules.
// For non-idempotent tasks, a retry is only allowed when we know the remote call
// never started: start.* error codes, or an on_error rule with executed:false.
func isRetryAllowed(task *model.Task, errCode string, matched *model.ErrorCase) bool {
	if task.OnlyOnce == nil || !*task.OnlyOnce {
		return true
	}
	if matched != nil && matched.NotReached != nil && *matched.NotReached {
		return true
	}
	return strings.HasPrefix(errCode, "pre.")
}

// matchOnError returns the first ErrorCase whose Code patterns match errCode,
// or whose Code list is empty (catch-all). Returns nil when no rule matches.
func matchOnError(task *model.Task, errCode string) *model.ErrorCase {
	for i := range task.OnError {
		c := &task.OnError[i]
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
// context, and routes to the matching goto or fails the instance. It returns the
// outcome to persist (runAdvance writes it).
func (e *Engine) handleCallError(inst *model.ProcessInstance, task *model.Task, errMsg, errCode string) advanceOutcome {
	// If the process is being cancelled, suppress retries and honour the cancellation
	// unless retries are exhausted / not configured — in that case error takes precedence.
	if inst.Status == model.StatusCancelling {
		matched := matchOnError(task, errCode)
		if matched != nil && inst.RetryCount < matched.Retries && isRetryAllowed(task, errCode, matched) {
			// Retries remain but we're cancelling — skip the retry and cancel cleanly.
			e.log.Info("task failed during cancellation, skipping retry",
				"id", inst.ID, "task", task.ID, "code", errCode)
			e.audit(inst, model.LogInfo, model.EventCancelSkipRetry, task.ID, errMsg, errCode, nil)
			return e.cancelInstance(inst)
		}
		// No retries available — error takes precedence over cancellation.
		return e.failInstance(inst, fmt.Sprintf("task %q: %s: %s", task.ID, errCode, errMsg))
	}

	matched := matchOnError(task, errCode)

	if matched != nil && inst.RetryCount < matched.Retries && isRetryAllowed(task, errCode, matched) {
		inst.RetryCount++
		next := db.Now().Add(e.retryDelay(inst.RetryCount))
		inst.WakeAt = &next
		e.log.Warn("task failed, scheduling retry",
			"id", inst.ID, "task", task.ID,
			"attempt", inst.RetryCount, "max", matched.Retries,
			"next_retry", next.Format(time.RFC3339),
			"code", errCode, "err", errMsg,
		)
		e.audit(inst, model.LogWarn, model.EventRetryScheduled, task.ID, errMsg, errCode, map[string]any{
			"attempt":    inst.RetryCount,
			"max":        matched.Retries,
			"next_retry": next.Format(time.RFC3339),
		})
		return advanceOutcome{kind: outcomeUpdate}
	}

	inst.ContextData["error"] = map[string]any{
		"task":    task.ID,
		"message": errMsg,
		"code":    errCode,
	}

	if matched != nil && matched.Goto != "" {
		if matched.Goto == model.GotoEnd {
			inst.Status = model.StatusCompleted
			inst.WakeAt = nil
			e.log.Info("instance completed via error route", "id", inst.ID, "task", task.ID, "code", errCode)
			e.audit(inst, model.LogInfo, model.EventErrorCompleted, task.ID, errMsg, errCode, nil)
			return advanceOutcome{kind: outcomeTerminal}
		}
		newQueue, err := e.queueFromTask(inst, matched.Goto)
		if err != nil {
			return e.failInstance(inst, err.Error())
		}
		inst.TaskQueue = newQueue
		inst.RetryCount = 0
		inst.WakeAt = nil
		e.log.Info("routing to error handler",
			"id", inst.ID, "task", task.ID, "goto", matched.Goto, "code", errCode)
		e.audit(inst, model.LogInfo, model.EventErrorRoute, task.ID, errMsg, errCode, map[string]any{"goto": matched.Goto})
		return advanceOutcome{kind: outcomeUpdate}
	}

	return e.failInstance(inst, fmt.Sprintf("task %q: %s: %s", task.ID, errCode, errMsg))
}

func (e *Engine) buildTaskData(inst *model.ProcessInstance, task *model.Task) (any, error) {
	if !task.Action.Input.Present() {
		return map[string]any{}, nil
	}
	return evalShape(task.Action.Input.Raw, evalEnv(inst.ContextData, nil))
}

// runDelay implements the delay action. On first entry — WakeAt is nil
// because every task transition resets it — it evaluates the duration and parks the
// instance by stamping wake_at (persisted via the progress outcome, which releases
// the worker); the normal claim loop re-claims it once the timer elapses. On
// re-entry (WakeAt set, so the claim guarantees the timer is due) it returns nil and
// advance continues to the task's switch. Returns a non-nil outcome when it parked
// or failed (the caller stops and persists it).
func (e *Engine) runDelay(inst *model.ProcessInstance, task *model.Task) *advanceOutcome {
	if inst.WakeAt == nil {
		ms, err := evalDurationMs(task.Action.Ms, inst.ContextData)
		if err != nil {
			return stop(e.failInstance(inst, fmt.Sprintf("task %q delay: %v", task.ID, err)))
		}
		wake := db.Now().Add(time.Duration(ms) * time.Millisecond)
		inst.WakeAt = &wake
		e.log.Info("instance delaying", "id", inst.ID, "task", task.ID, "ms", ms)
		e.audit(inst, model.LogInfo, model.EventDelayArmed, task.ID, "", "", map[string]any{"ms": ms})
		return stop(advanceOutcome{kind: outcomeProgress})
	}
	return nil
}

// runExternal implements the external (pull/callback) task. It has three entry
// states, told apart by wait_state and the presence of a submitted result:
//
//  1. First arrival (wait_state none, no _external_result): evaluate and snapshot the
//     input, mint a per-occurrence token, and park the instance (wait_state='external');
//     if timeout_ms>0 also stamp wake_at as the timeout deadline. No worker is held while
//     parked, and the claim will not return it again until the result arrives (which
//     clears wait_state) or the timeout fires.
//  2. Result submitted (wait_state none, _external_result present): the resolve API
//     cleared wait_state and stored the validated result; consume it as self.result and
//     continue to the task's output/switch.
//  3. Timeout (wait_state still 'external'): the claim only returns a parked external
//     instance once its wake_at deadline passed, so reaching here with wait_state still
//     'external' means no result arrived in time → external.timeout via on_error. Retries
//     on that code re-arm the wait with a fresh token.
//
// Returns (result, nil) to continue advancing, or (nil, outcome) to stop and persist.
func (e *Engine) runExternal(inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	// Phase 2: a result was submitted (the resolve API already un-parked us).
	if res, ok := inst.ContextData[model.CtxExternalResult]; ok {
		delete(inst.ContextData, model.CtxExternalResult)
		delete(inst.ContextData, model.CtxExternal)
		e.log.Info("external task resolved", "id", inst.ID, "task", task.ID)
		e.audit(inst, model.LogInfo, model.EventExternalResolved, task.ID, "", "", nil)
		return res, nil
	}

	// Phase 3: still parked at 'external' — the claim only returns us once the timeout
	// deadline passed, so no result arrived in time.
	if inst.WaitState == model.WaitStateExternal {
		inst.WaitState = model.WaitStateNone
		delete(inst.ContextData, model.CtxExternal)
		e.log.Warn("external task timed out", "id", inst.ID, "task", task.ID)
		e.audit(inst, model.LogWarn, model.EventExternalTimeout, task.ID, "external task timed out", "external.timeout", nil)
		return nil, stop(e.handleCallError(inst, task, "external task timed out", "external.timeout"))
	}

	// Phase 1: first arrival — snapshot input, mint a per-occurrence token, and park.
	// RetryCount is intentionally left untouched: a re-arm after an external.timeout
	// retry must keep its counter so on_error retry budgeting terminates.
	input, err := e.buildTaskData(inst, task)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q input: %v", task.ID, err)))
	}
	token := inst.ID + "." + idgen.New()
	inst.ContextData[model.CtxExternal] = map[string]any{
		"task_id": task.ID,
		"token":   token,
		"input":   input,
	}
	inst.WaitState = model.WaitStateExternal
	if task.TimeoutMs > 0 {
		wake := db.Now().Add(time.Duration(task.TimeoutMs) * time.Millisecond)
		inst.WakeAt = &wake
	} else {
		inst.WakeAt = nil
	}
	e.log.Info("external task armed", "id", inst.ID, "task", task.ID, "timeout_ms", task.TimeoutMs)
	detail := map[string]any{"token": token}
	if task.TimeoutMs > 0 {
		detail["timeout_ms"] = task.TimeoutMs
	}
	e.audit(inst, model.LogInfo, model.EventExternalArmed, task.ID, "", "", detail)
	return nil, stop(advanceOutcome{kind: outcomeProgress})
}

// evalDurationMs evaluates a delay expression to a non-negative millisecond
// count. The expression is a template, so a bare literal ("30000") returns the
// string "30000" (parsed here) while a "{{ … }}" expression returns a number.
func evalDurationMs(expr string, ctx map[string]any) (int64, error) {
	v, err := evalAny(expr, ctx)
	if err != nil {
		return 0, err
	}
	var ms int64
	switch n := v.(type) {
	case int:
		ms = int64(n)
	case int64:
		ms = n
	case float64:
		ms = int64(n)
	case string:
		parsed, perr := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("ms %q is not a number", expr)
		}
		ms = parsed
	default:
		return 0, fmt.Errorf("ms must evaluate to a number, got %T", v)
	}
	if ms < 0 {
		return 0, fmt.Errorf("ms must be non-negative, got %d", ms)
	}
	return ms, nil
}

// audit appends one event to the instance's persistent execution log. It is
// best-effort: a write failure is logged and swallowed so audit logging can
// never abort an advance. The structured slog output at each call site is left
// intact for operational logging; this is the durable, queryable trail.
func (e *Engine) audit(inst *model.ProcessInstance, level model.LogLevel, event, task, msg, code string, detail map[string]any) {
	if err := e.db.AppendLog(&model.LogEntry{
		InstanceID: inst.ID,
		Level:      level,
		Event:      event,
		TaskID:     task,
		Message:    msg,
		Code:       code,
		Detail:     detail,
	}); err != nil {
		e.log.Error("append audit log", "id", inst.ID, "event", event, "err", err)
	}
}

// snippet renders v as JSON capped to the configured payload size for inclusion
// in an audit detail. Returns "" when payload capture is disabled or v is empty.
func (e *Engine) snippet(v any) string {
	if !e.logCfg.Payloads || v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	max := e.logCfg.PayloadBytes
	if max <= 0 {
		max = defaultPayloadBytes
	}
	if len(b) > max {
		return string(b[:max]) + "…(truncated)"
	}
	return string(b)
}

// failInstance moves the instance to its failed state and returns the terminal
// outcome (persisted by runAdvance via saveAndNotify).
func (e *Engine) failInstance(inst *model.ProcessInstance, reason string) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.Error = reason
	inst.WakeAt = nil
	e.log.Error("instance failed", "id", inst.ID, "reason", reason)
	e.audit(inst, model.LogError, model.EventInstanceFailed, "", reason, "", nil)
	return advanceOutcome{kind: outcomeTerminal}
}

// cancelInstance moves the instance to its cancelled state and returns the terminal
// outcome (persisted by runAdvance via saveAndNotify).
func (e *Engine) cancelInstance(inst *model.ProcessInstance) advanceOutcome {
	inst.Status = model.StatusCancelled
	inst.WaitState = model.WaitStateNone
	// A retry-backoff parks with RetryCount > 0; clear its timer so a later retry
	// runs immediately. A delay parks with RetryCount == 0; keep wake_at so the
	// retry resumes toward the delay's original deadline.
	if inst.RetryCount > 0 {
		inst.WakeAt = nil
	}
	e.log.Info("instance cancelled", "id", inst.ID)
	e.audit(inst, model.LogInfo, model.EventCancelled, "", "", "", nil)
	return advanceOutcome{kind: outcomeTerminal}
}

// settleFailing finalises a draining 'failing' instance once its children have
// settled (it only becomes claimable then). The error was already recorded when
// the failure propagated up; saveAndNotify (via the terminal outcome) cascades the
// settlement one level up.
func (e *Engine) settleFailing(inst *model.ProcessInstance) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.WakeAt = nil
	e.log.Info("instance settled as failed", "id", inst.ID, "reason", inst.Error)
	e.audit(inst, model.LogInfo, model.EventInstanceSettled, "", inst.Error, "", nil)
	return advanceOutcome{kind: outcomeTerminal}
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
//     parent context and return so advance() continues past this task.
//
// A cancelling parent spawns cancelling children: they self-cancel and call
// FinishChild, which transitions the parent to WaitStateCollecting normally.
func (e *Engine) runChildProcesses(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	// Phase 2: parent woke up with children done — collect their outputs into the
	// action result (self.result). It is exported only if the task projects it.
	if inst.WaitState == model.WaitStateCollecting {
		output, err := e.collectChildOutputs(ctx, inst, task)
		if err != nil {
			inst.WaitState = model.WaitStateNone
			return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q collect: %v", task.ID, err)))
		}
		inst.WaitState = model.WaitStateNone
		e.log.Info("parent collected child outputs", "id", inst.ID, "task", task.ID)
		e.audit(inst, model.LogInfo, model.EventChildrenCollect, task.ID, "", "", nil)
		return output, nil
	}

	// Phase 1: spawn children. Record the spawned child IDs under the internal
	// "_children" key (keyed by task, then by child key for parallel) so observers
	// can correlate a parent task with its children. This is metadata only — child
	// results flow to self.result at collection, not into outputs.
	childCallStack := append(inst.CallStack, inst.ID)
	if inst.ContextData["_children"] == nil {
		inst.ContextData["_children"] = map[string]any{}
	}
	spawned := inst.ContextData["_children"].(map[string]any)

	var children []*model.ProcessInstance
	switch task.Action.Type {
	case model.ActionTypeChild:
		child, fail := e.buildSingleChild(ctx, inst, task, childCallStack)
		if fail != nil {
			return nil, fail
		}
		spawned[task.ID] = child.ID
		children = []*model.ProcessInstance{child}
	case model.ActionTypeChildParallel:
		parallel, fail := e.buildParallelChildren(ctx, inst, task, childCallStack)
		if fail != nil {
			return nil, fail
		}
		ids := make(map[string]any, len(parallel))
		for _, c := range parallel {
			key, _ := c.ContextData["_spawn_child_key"].(string)
			ids[key] = c.ID
		}
		spawned[task.ID] = ids
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
	inst.ContextData["output_order"] = append(order, task.ID)

	inst.RetryCount = 0
	inst.WakeAt = nil

	// A child spawn is a multi-row transaction that parks the parent atomically, so
	// it persists itself here rather than through runAdvance. The parent ends
	// 'waiting' (non-runnable), so dropping the marker after this write is harmless;
	// it reports outcomeNoop so runAdvance does no further write. On failure it
	// transitions to the terminal outcome instead.
	if err := e.db.SpawnChildrenAndWait(ctx, inst, children); err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q spawn: %v", task.ID, err)))
	}

	e.log.Info("parent waiting for children", "id", inst.ID, "task", task.ID, "children", len(children))
	e.audit(inst, model.LogInfo, model.EventChildrenSpawned, task.ID, "", "", map[string]any{"children": len(children)})
	return nil, stop(advanceOutcome{kind: outcomeNoop})
}

// buildSingleChild resolves the child definition, evaluates input, and constructs
// a ProcessInstance ready to be saved. It does not persist anything; a non-nil
// outcome means the parent failed and the caller should stop and persist it.
func (e *Engine) buildSingleChild(ctx context.Context, inst *model.ProcessInstance, task *model.Task, callStack []string) (*model.ProcessInstance, *advanceOutcome) {
	name := task.Action.Name
	version := task.Action.Version
	if version == 0 {
		if name == inst.ProcessName {
			version = inst.ProcessVersion
		} else {
			var err error
			version, err = e.db.GetDependencyVersion(inst.ProcessName, inst.ProcessVersion, task.ID, "")
			if err != nil {
				version, err = e.db.LatestVersion(name)
				if err != nil {
					return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q child: %v", task.ID, err)))
				}
			}
		}
	}
	def, err := e.db.GetDefinition(name, version)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q child: %v", task.ID, err)))
	}
	input, err := e.evalChildInput(inst, task.ID, "child", task.Action.Input)
	if err != nil {
		return nil, stop(e.failInstance(inst, err.Error()))
	}
	if err := def.ValidateInput(input); err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q child input validation: %v", task.ID, err)))
	}
	childCtx := map[string]any{
		"input":            input,
		"outputs":          map[string]any{},
		"output_order":     []string{},
		"error":            nil,
		"_spawn_action_type": string(model.ActionTypeChild),
	}
	if task.Action.ResultSchema != nil {
		if b, err := json.Marshal(task.Action.ResultSchema); err == nil {
			childCtx["_spawn_result_schema"] = string(b)
		}
	}
	return &model.ProcessInstance{
		ID:             idgen.ChildBase(inst.ID).String(), // sorts after the parent
		ProcessName:    def.Name,
		ProcessVersion: version,
		TaskQueue:      def.Tasks,
		ContextData:    childCtx,
		Status:         model.StatusRunning,
		ParentID:       inst.ID,
		SpawnTaskID:    task.ID,
		CallStack:      callStack,
	}, nil
}

// buildParallelChildren resolves definitions, evaluates inputs, and constructs
// ProcessInstances for all parallel children. Does not persist anything; a non-nil
// outcome means the parent failed and the caller should stop and persist it.
func (e *Engine) buildParallelChildren(ctx context.Context, inst *model.ProcessInstance, task *model.Task, callStack []string) ([]*model.ProcessInstance, *advanceOutcome) {
	keys := make([]string, 0, len(task.Action.Children))
	for key := range task.Action.Children {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// One base id (guaranteed to sort after the parent); siblings are base, base+1,
	// … in sorted-key order, so the whole batch sorts after the parent and among
	// itself in spawn order.
	base := idgen.ChildBase(inst.ID)

	children := make([]*model.ProcessInstance, 0, len(task.Action.Children))
	for i, key := range keys {
		entry := task.Action.Children[key]
		version := entry.Version
		if version == 0 {
			if entry.Name == inst.ProcessName {
				version = inst.ProcessVersion
			} else {
				var err error
				version, err = e.db.GetDependencyVersion(inst.ProcessName, inst.ProcessVersion, task.ID, key)
				if err != nil {
					version, err = e.db.LatestVersion(entry.Name)
					if err != nil {
						return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q child_parallel[%q]: %v", task.ID, key, err)))
					}
				}
			}
		}
		def, err := e.db.GetDefinition(entry.Name, version)
		if err != nil {
			return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q child_parallel[%q]: %v", task.ID, key, err)))
		}
		input, err := e.evalChildInput(inst, task.ID, fmt.Sprintf("child_parallel[%q]", key), entry.Input)
		if err != nil {
			return nil, stop(e.failInstance(inst, err.Error()))
		}
		if err := def.ValidateInput(input); err != nil {
			return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q child_parallel[%q] input validation: %v", task.ID, key, err)))
		}
		childCtx := map[string]any{
			"input":            input,
			"outputs":          map[string]any{},
			"output_order":     []string{},
			"error":            nil,
			"_spawn_action_type": string(model.ActionTypeChildParallel),
			"_spawn_child_key": key,
		}
		if entry.ResultSchema != nil {
			if b, err := json.Marshal(entry.ResultSchema); err == nil {
				childCtx["_spawn_result_schema"] = string(b)
			}
		}
		children = append(children, &model.ProcessInstance{
			ID:             idgen.Add(base, uint64(i)).String(),
			ProcessName:    def.Name,
			ProcessVersion: version,
			TaskQueue:      def.Tasks,
			ContextData:    childCtx,
			Status:         model.StatusRunning,
			ParentID:       inst.ID,
			SpawnTaskID:    task.ID,
			CallStack:      callStack,
		})
	}
	return children, nil
}

func (e *Engine) evalChildInput(inst *model.ProcessInstance, taskID, label string, input *model.Shape) (any, error) {
	if !input.Present() {
		return map[string]any{}, nil
	}
	val, err := evalShape(input.Raw, evalEnv(inst.ContextData, nil))
	if err != nil {
		return nil, fmt.Errorf("task %q %s input: %v", taskID, label, err)
	}
	return val, nil
}

// computeOutput evaluates the process definition's Output expression map against
// the final context and stores the result in context_data["output"]. No-op if
// the definition has no Output map.
func (e *Engine) computeOutput(inst *model.ProcessInstance) error {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return fmt.Errorf("load definition for output: %w", err)
	}
	if !def.Output.Present() {
		return nil
	}
	out, err := evalShape(def.Output.Raw, evalEnv(inst.ContextData, nil))
	if err != nil {
		return fmt.Errorf("output: %w", err)
	}
	inst.ContextData["output"] = out
	return nil
}

// resolveHeaders evaluates each header value expression against the instance
// context and coerces the result to a string. Returns nil for calls without headers.
func (e *Engine) resolveHeaders(inst *model.ProcessInstance, call *model.Action) (map[string]string, error) {
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
