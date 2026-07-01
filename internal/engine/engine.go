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

	"genroc/internal/db"
	"genroc/internal/idgen"
	"genroc/internal/logview"
	"genroc/internal/model"
	"genroc/internal/schema"
	"genroc/internal/transport"
	"genroc/internal/validation"
)

const (
	defaultLeaseDuration      = 10 * time.Second
	defaultLeaseRenewInterval = 3 * time.Second
	defaultPayloadBytes       = 2048
)

// LogConfig controls how much the engine persists to each instance's audit log
// and for how long, plus the verbosity of the unified server console.
type LogConfig struct {
	Payloads     bool          // capture truncated request/response snippets on task events
	PayloadBytes int           // max bytes per captured snippet (<=0 → defaultPayloadBytes)
	Retention    time.Duration // prune audit logs older than this; 0 = keep forever
	Mode         logview.Mode  // console verbosity: basic omits the data body, detail includes it
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
	wake               chan struct{} // buffer-1 nudge: "runnable work may exist, re-scan now" (see signalWork)
	workerID           string
	inflight           sync.Map // instance IDs this worker is currently advancing (detects overwhelm via self-reclaim)
	// schemaCache caches the inferred SchemaFile per (process,version) so logged
	// payloads can be schema-redacted (secret fields → "***") without re-running
	// inference on every log line. Definitions are immutable per version.
	schemaCache sync.Map
}

type schemaKey struct {
	name    string
	version int
}

// schemaFile returns the inferred schemas for the instance's process (cached),
// used to redact secret-derived fields from logged payloads.
func (e *Engine) schemaFile(inst *model.ProcessInstance) (validation.SchemaFile, bool) {
	key := schemaKey{inst.ProcessName, inst.ProcessVersion}
	if cached, ok := e.schemaCache.Load(key); ok {
		return cached.(validation.SchemaFile), true
	}
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return validation.SchemaFile{}, false
	}
	sf, err := validation.Generate(def)
	if err != nil {
		return validation.SchemaFile{}, false
	}
	e.schemaCache.Store(key, sf)
	return sf, true
}

// snippetResult redacts an action's raw result body against its result_schema
// (which may mark response fields secret), then returns the capped JSON snippet.
// The response body is not part of the instance context, so it cannot be scrubbed
// by audit's context-secret pass — it is schema-redacted here instead.
func (e *Engine) snippetResult(task *model.Task, body any) string {
	if e.logCfg.Payloads && task.Action != nil && task.Action.ResultSchema != nil {
		body = schema.Redact(body, task.Action.ResultSchema, task.Action.ResultSchema.Defs)
	}
	return e.snippet(body)
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
	// Dereferenced objects survive on the same horizon as audit logs, so a log that
	// references an object stays resolvable for as long as the log itself lives.
	database.SetObjectRetention(logCfg.Retention)
	return &Engine{
		db:                 database,
		pollEvery:          pollEvery,
		immediateRetries:   immediateRetries,
		leaseDuration:      leaseDuration,
		leaseRenewInterval: leaseRenewInterval,
		logCfg:             logCfg,
		log:                log,
		sem:                make(chan struct{}, maxConcurrent),
		wake:               make(chan struct{}, 1),
		workerID:           workerID,
	}
}

// signalWork nudges the pump to re-scan for runnable work immediately instead of
// waiting out the poll interval. The send is non-blocking and the channel is buffer-1,
// so concurrent nudges coalesce into one pending wake and a nudge with no pump parked
// on it (it is busy, or this is manual/tick mode with no pump) is harmlessly dropped —
// the ticker is still the idle floor.
func (e *Engine) signalWork() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// NotifyWork tells the engine that new runnable work may exist (e.g. a freshly created
// instance), so its pump claims it without waiting for the next poll tick.
func (e *Engine) NotifyWork() { e.signalWork() }

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
	e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine started", Meta: map[string]any{"poll_interval": e.pollEvery, "max_concurrent": cap(e.sem), "worker": e.workerID}})

	go e.leaseRenewer(ctx)

	if e.logCfg.Retention > 0 {
		go e.logPruner(ctx)
	}

	if e.pollEvery == 0 {
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine in manual tick mode"})
		<-ctx.Done()
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine stopped"})
		return nil
	}

	err := e.runPump(ctx)
	if err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: err.Error()})
	} else {
		e.logOnly(logEvent{Level: model.LogInfo, Msg: "engine stopped"})
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
				e.logOnly(logEvent{Level: model.LogError, Msg: "claim instances: " + err.Error()})
			}
			// Nothing claimable right now: wait for the next tick, or wake early when
			// signalWork reports freshly-runnable work (a self-requeued loop, spawned
			// children, an un-parked parent, or a newly created instance).
			select {
			case <-ctx.Done():
				return nil
			case <-e.wake:
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
			e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: "advance instance: " + err.Error()})
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
				e.logOnly(logEvent{Level: model.LogError, Msg: "renew worker leases: " + err.Error()})
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
		e.logOnly(logEvent{Level: model.LogError, Msg: "prune logs: " + err.Error()})
	} else if n > 0 {
		e.logOnly(logEvent{Level: model.LogDebug, Msg: "pruned audit logs", Meta: map[string]any{"count": n, "older_than": e.logCfg.Retention}})
	}
	// Sweep dereferenced/expired objects (log payloads and dropped context values) on
	// the same horizon — their expiration was stamped to now+retention when released.
	if n, err := e.db.DeleteExpiredObjects(db.Now().UnixMilli()); err != nil {
		e.logOnly(logEvent{Level: model.LogError, Msg: "prune objects: " + err.Error()})
	} else if n > 0 {
		e.logOnly(logEvent{Level: model.LogDebug, Msg: "pruned objects", Meta: map[string]any{"count": n}})
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
		e.logOnly(logEvent{Level: model.LogError, Msg: "claim instances: " + err.Error()})
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
				e.logOnly(logEvent{Level: model.LogError, ID: inst.ID, Msg: "advance instance: " + err.Error()})
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
	if err := e.persist(inst, outcome); err != nil {
		return err
	}
	// A persisted advance may have produced immediately-runnable work: this instance
	// again (a running checkpoint), children spawned by a parked parent, or a parent
	// un-parked by this instance finishing. Nudge the pump to re-scan now rather than
	// idle until the next tick. A spurious nudge (nothing actually runnable) costs only
	// one empty claim, so signalling unconditionally keeps this correct and simple.
	e.signalWork()
	return nil
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

	// Load the definition once for the whole tick: it drives config resolution and
	// is the source of truth for the task list (the instance stores only its current
	// task id; successors are implied by definition order). An instance whose
	// definition cannot be loaded cannot run, so fail it with a clear reason.
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return e.failInstance(inst, fmt.Sprintf("load definition: %v", err))
	}

	// Resolve config from the OS environment for this tick. Config is never
	// persisted — it is re-resolved every tick and exposed to expressions as
	// "config". A resolution failure (missing required var, bad coercion) fails
	// the instance with a clear reason.
	if def.ConfigSchema != nil {
		cfg, err := def.ResolveConfig(os.LookupEnv)
		if err != nil {
			return e.failInstance(inst, fmt.Sprintf("config: %v", err))
		}
		inst.Config = cfg
	}

	// Resolve the instance's position in the task list. An empty Task means it has
	// run off the end (nothing left) — the loop below completes it. A non-empty Task
	// that isn't in the definition is a corrupt/mismatched row: fail it.
	idx := taskIndex(def.Tasks, inst.Task)
	if inst.Task != "" && idx < 0 {
		return e.failInstance(inst, fmt.Sprintf("current task %q not found in definition", inst.Task))
	}

	// Lease takeover: this instance was reclaimed from an expired lease, so its
	// current task may have started executing on the previous owner before it
	// crashed/stalled. Re-running is fine for idempotent tasks, but an only_once
	// (non-idempotent) call task cannot be safely re-executed — the call may already
	// have happened — so fail the instance to honour at-most-once.
	if inst.ReclaimedExpired {
		e.logOnly(logEvent{Level: model.LogWarn, ID: inst.ID,
			Msg:  "reclaimed expired lease; previous owner crashed or stalled mid-task",
			Meta: map[string]any{"task": inst.Task, "process": inst.ProcessName}})
		if idx >= 0 {
			s := def.Tasks[idx]
			if s.Action != nil && s.OnlyOnce != nil && *s.OnlyOnce {
				return e.failInstance(inst, fmt.Sprintf(
					"task %q is only_once and was interrupted by a lease takeover; cannot re-execute", s.ID))
			}
		}
	}

	// work_started: a worker has picked this instance up and is about to work its
	// current task. One per work session (a resume after parking emits it again),
	// tagged with the worker so the unified log shows who is doing what.
	if idx >= 0 {
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventWorkStarted, Task: inst.Task, Meta: map[string]any{"worker": e.workerID}})
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
	// resuming from the last persisted task position is deterministic. Durable state
	// only changes at the boundaries (spawn txn, action result, terminal save), each
	// of which writes inst.Task — the current position in the definition's task list.
	const maxInlineTasks = 1000
	for i := 0; ; i++ {
		if idx < 0 || idx >= len(def.Tasks) {
			// Ran off the end of the task list: nothing left to do.
			inst.Task = ""
			inst.Status = model.StatusCompleted
			inst.WakeAt = nil
			if err := e.computeOutput(inst); err != nil {
				return e.failInstance(inst, err.Error())
			}
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceDone, Data: e.outputData(inst)})
			return advanceOutcome{kind: outcomeTerminal}
		}

		task := def.Tasks[idx]
		// Point the instance at the task about to run, so any mid-task persist (park,
		// retry, error route, fail) records this task as the resume point.
		inst.Task = task.ID
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
				out, done := e.runExternal(ctx, inst, task)
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
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceDone, Task: task.ID, Data: e.outputData(inst)})
			return advanceOutcome{kind: outcomeTerminal}
		}

		if gotoID == model.GotoNext {
			idx++
		} else {
			// gotoID is a task reference like "$ship" — strip the sigil.
			if idx = taskIndex(def.Tasks, gotoID[1:]); idx < 0 {
				return e.failInstance(inst, fmt.Sprintf("goto task %q not found in %q v%d", gotoID[1:], inst.ProcessName, inst.ProcessVersion))
			}
		}
		// Reflect the new position (empty once we run past the last task) so a
		// checkpoint here persists the next task to run, not the one just completed.
		inst.Task = taskIDAt(def.Tasks, idx)

		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventTaskCompleted, Task: task.ID, Msg: "→ " + gotoID})

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

	// Resolve the endpoint template (e.g. a base URL from config or input). Secret
	// values it carries are scrubbed from the logged URL/errors in audit().
	endpoint, err := e.resolveEndpoint(inst, task.Action)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q endpoint: %v", task.ID, err)))
	}

	// action_started (debug): message = the action type; data = the request body; and
	// for a REST call meta = {url} so the trail shows which endpoint was hit. Headers
	// are intentionally omitted — they routinely carry secrets and the audit log is
	// persisted.
	var startMeta map[string]any
	if task.Action.Type == model.ActionTypeREST {
		startMeta = map[string]any{"url": endpoint}
	}
	e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionStarted, Task: task.ID, Msg: string(task.Action.Type), Data: e.snippet(data), Meta: startMeta})

	resolvedHeaders, err := e.resolveHeaders(inst, task.Action)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q headers: %v", task.ID, err)))
	}

	resp, err := transport.Send(taskCtx, task.Action, endpoint, resolvedHeaders, req)
	if err != nil {
		code := transport.ClassifyGoError(err)
		// action_failed (debug) records the call failure — error detail in data,
		// code in code — separate from the operational retry/route event that follows.
		// A transport error has no HTTP status, so meta stays absent.
		e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionFailed, Task: task.ID, Code: code, Data: e.snippetRaw(err.Error())})
		return nil, stop(e.handleCallError(inst, task, err.Error(), code))
	}
	if resp.ErrorCode != "" {
		msg := resp.ErrorMessage
		if msg == "" {
			msg = resp.ErrorCode
		}
		// action_failed (debug): error body in data, status in meta, code in code.
		e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionFailed, Task: task.ID, Code: resp.ErrorCode, Data: e.snippetRaw(resp.ErrorMessage), Meta: statusMeta(resp.Status)})
		return nil, stop(e.handleCallError(inst, task, msg, resp.ErrorCode))
	}

	// result_schema validates the raw result; it does not export it. The result is
	// transient — available to this task's own output/switch as self.result. Only an
	// `output` projection adds anything to outputs.<id>.
	if err := task.Action.ValidateOutput(resp.Body); err != nil {
		return nil, stop(e.handleCallError(inst, task, err.Error(), "output.invalid"))
	}
	inst.RetryCount = 0

	// action_succeeded (debug): the response body in data, the HTTP status in meta.
	// Like action_started it carries an action payload, so it is gated behind
	// --level debug rather than cluttering the default info trail.
	e.audit(inst, logEvent{Level: model.LogDebug, Event: model.EventActionSucceeded, Task: task.ID, Data: e.snippetResult(task, resp.Body), Meta: statusMeta(resp.Status)})

	return resp.Body, nil
}

// evalTaskOutput evaluates a task's output map against the context plus self,
// where self.result is the raw action result and self.previous is this task's
// prior output (its value from the last loop iteration, or nil on the first run).
func (e *Engine) evalTaskOutput(inst *model.ProcessInstance, task *model.Task, result, previous any) (any, error) {
	self := map[string]any{"result": result, "previous": previous}
	return e.evalShapeCtx(inst, task.Output.Raw, self)
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
		ok, err := e.evalBoolCtx(inst, c.Case, selfOutput)
		if err != nil {
			return "", fmt.Errorf("case %q: %w", c.Case, err)
		}
		if ok {
			return c.Goto, nil
		}
	}
	return "", nil
}

// taskIndex returns the position of taskID in tasks, or -1 if absent (the empty id —
// "no current task" — is always absent).
func taskIndex(tasks []*model.Task, taskID string) int {
	if taskID == "" {
		return -1
	}
	for i, t := range tasks {
		if t.ID == taskID {
			return i
		}
	}
	return -1
}

// taskIDAt returns the id of the task at idx, or "" when idx is out of range (the
// instance has advanced past the last task).
func taskIDAt(tasks []*model.Task, idx int) string {
	if idx < 0 || idx >= len(tasks) {
		return ""
	}
	return tasks[idx].ID
}

// resolveGoto validates that the instance's definition contains taskID, so the engine
// can point the instance at it. No queue is built — the remaining tasks are implied by
// definition order. Used by the on-error route, which has no definition in scope.
func (e *Engine) resolveGoto(inst *model.ProcessInstance, taskID string) error {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return fmt.Errorf("resolve goto: %w", err)
	}
	if taskIndex(def.Tasks, taskID) < 0 {
		return fmt.Errorf("goto task %q not found in %q v%d", taskID, inst.ProcessName, inst.ProcessVersion)
	}
	return nil
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
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventCancelSkipRetry, Task: task.ID, Msg: errMsg, Code: errCode})
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
		retryMsg := fmt.Sprintf("%s (attempt %d/%d)", errMsg, inst.RetryCount, matched.Retries)
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventRetryScheduled, Task: task.ID, Msg: retryMsg, Code: errCode})
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
			e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventErrorCompleted, Task: task.ID, Msg: errMsg, Code: errCode})
			return advanceOutcome{kind: outcomeTerminal}
		}
		if err := e.resolveGoto(inst, matched.Goto); err != nil {
			return e.failInstance(inst, err.Error())
		}
		inst.Task = matched.Goto
		inst.RetryCount = 0
		inst.WakeAt = nil
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventErrorRoute, Task: task.ID, Msg: errMsg + " → " + matched.Goto, Code: errCode})
		return advanceOutcome{kind: outcomeUpdate}
	}

	return e.failInstance(inst, fmt.Sprintf("task %q: %s: %s", task.ID, errCode, errMsg))
}

func (e *Engine) buildTaskData(inst *model.ProcessInstance, task *model.Task) (any, error) {
	if !task.Action.Input.Present() {
		return map[string]any{}, nil
	}
	return e.evalShapeCtx(inst, task.Action.Input.Raw, nil)
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
		ms, err := e.evalDurationMsCtx(inst, task.Action.Ms)
		if err != nil {
			return stop(e.failInstance(inst, fmt.Sprintf("task %q delay: %v", task.ID, err)))
		}
		wake := db.Now().Add(time.Duration(ms) * time.Millisecond)
		inst.WakeAt = &wake
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventDelayArmed, Task: task.ID, Msg: fmt.Sprintf("%dms", ms)})
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
func (e *Engine) runExternal(ctx context.Context, inst *model.ProcessInstance, task *model.Task) (any, *advanceOutcome) {
	// Phase 2: a result was submitted (the resolve API or a direct signal already un-parked
	// us by storing _external_result).
	if res, ok := inst.ContextData[model.CtxExternalResult]; ok {
		delete(inst.ContextData, model.CtxExternalResult)
		delete(inst.ContextData, model.CtxExternal)
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalResolved, Task: task.ID})
		return res, nil
	}

	// Phase 3: still parked at 'external' — the claim only returns us once the timeout
	// deadline passed, so no result arrived in time.
	if inst.WaitState == model.WaitStateExternal {
		inst.WaitState = model.WaitStateNone
		delete(inst.ContextData, model.CtxExternal)
		e.audit(inst, logEvent{Level: model.LogWarn, Event: model.EventExternalTimeout, Task: task.ID, Msg: "external task timed out", Code: "external.timeout"})
		return nil, stop(e.handleCallError(inst, task, "external task timed out", "external.timeout"))
	}

	// Phase 1: first arrival. Atomically either consume a signal already buffered for this
	// task (the push/webhook case — it raced ahead of the process reaching the task) or
	// park and wait. RetryCount is intentionally left untouched so a re-arm after an
	// external.timeout retry keeps its counter and on_error budgeting terminates.
	input, err := e.buildTaskData(inst, task)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q input: %v", task.ID, err)))
	}
	token := inst.ID + "." + idgen.New()
	var wakeAt *time.Time
	if task.TimeoutMs > 0 {
		wake := db.Now().Add(time.Duration(task.TimeoutMs) * time.Millisecond)
		wakeAt = &wake
	}
	consumed, payload, err := e.db.ArmExternalOrConsumeSignal(ctx, inst, task.ID, token, input, wakeAt)
	if err != nil {
		return nil, stop(e.failInstance(inst, fmt.Sprintf("task %q arm: %v", task.ID, err)))
	}
	if consumed {
		// A buffered signal fed the task immediately. Continue advancing with it as the
		// result; ArmExternalOrConsumeSignal kept this worker's lease, so the normal
		// progress/terminal write at the end of advance releases it — the instance never
		// sits claimable while still in flight.
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalResolved, Task: task.ID, Msg: "buffered"})
		return payload, nil
	}
	// Parked. ArmExternalOrConsumeSignal persisted the parked state and released the lease,
	// so (like a child spawn) advance returns noop and writes nothing further.
	armedMsg := "token=" + token
	if task.TimeoutMs > 0 {
		armedMsg += fmt.Sprintf(" timeout=%dms", task.TimeoutMs)
	}
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventExternalArmed, Task: task.ID, Msg: armedMsg})
	return nil, stop(advanceOutcome{kind: outcomeNoop})
}

// evalDurationMs evaluates a delay expression to a non-negative millisecond
// count. The expression is a template, so a bare literal ("30000") returns the
// string "30000" (parsed here) while a "{{ … }}" expression returns a number.
func evalDurationMs(expr string, ctx, config map[string]any) (int64, error) {
	v, err := evalAny(expr, ctx, config)
	if err != nil {
		return 0, err
	}
	return durationFromValue(expr, v)
}

// evalDurationMsCtx is evalDurationMs against an instance's context, resolving only
// the slots the duration expression references.
func (e *Engine) evalDurationMsCtx(inst *model.ProcessInstance, expr string) (int64, error) {
	v, err := e.evalAnyCtx(inst, expr)
	if err != nil {
		return 0, err
	}
	return durationFromValue(expr, v)
}

// durationFromValue coerces an evaluated delay expression to a non-negative ms count.
func durationFromValue(expr string, v any) (int64, error) {
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

// logEvent is the structured payload of one log line. Level and Event are
// required; the rest are optional. It is shared by audit (console + durable DB
// trail) and logOnly (console only), so both render identically — only persistence
// differs.
type logEvent struct {
	Level model.LogLevel
	Event string
	ID    string // instance id; audit fills this from the instance
	Task  string
	Msg   string // human note (rendered as note=…, since slog uses msg for the event)
	Code  string
	Data  string // body (request/response/input/output/…); shown under its event Label
	Meta  map[string]any
}

// audit records an instance event to the unified console (slog) and the durable
// per-instance trail (the DB). Best-effort on the DB write: a failure is logged and
// swallowed so audit logging can never abort an advance.
func (e *Engine) audit(inst *model.ProcessInstance, ev logEvent) {
	ev.ID = inst.ID
	// Scrub every secret value (config + input + output, identified by the taint
	// schemas) from the log before it is emitted or stored. A single sink here is
	// the robust choice: genroc expressions have no functions, so a secret always
	// appears verbatim (or as a substring) in any logged value — there is no way for
	// it to reach a log line in a form a string-replace would miss.
	if secrets := e.contextSecrets(inst); len(secrets) > 0 {
		ev.Data = redactSecrets(ev.Data, secrets)
		ev.Msg = redactSecrets(ev.Msg, secrets)
		ev.Meta = redactMeta(ev.Meta, secrets)
	}
	// Console shows a capped excerpt regardless of how the full payload is persisted.
	consoleEv := ev
	consoleEv.Data = truncateStr(ev.Data, e.payloadCap())
	e.emit(consoleEv)
	if err := e.db.AppendLog(&model.LogEntry{
		InstanceID: ev.ID,
		Level:      ev.Level,
		Event:      ev.Event,
		TaskID:     ev.Task,
		Message:    ev.Msg,
		Code:       ev.Code,
		Data:       e.encodeLogData(ev.ID, ev.Data),
		Meta:       ev.Meta,
	}); err != nil {
		e.logOnly(logEvent{Level: model.LogError, ID: ev.ID, Msg: "append audit log: " + err.Error()})
	}
}

// contextSecrets gathers every secret value currently in the instance's context —
// config secrets, plus input/output values whose inferred schema is marked secret —
// so audit can scrub them from log text. (The action response body is not in the
// context; it is schema-redacted at its log site via snippetResult.)
//
// It considers only already-materialized values: inline values and slots that were
// resolved earlier this advance (inst.ResolvedObjects). An unresolved *ObjectRef is
// skipped, because a value that was never loaded was never used, so it cannot appear
// in any log line being scrubbed. This relies on the invariant that anything logged
// is derived from a value resolved BEFORE the audit call that logs it (every eval
// path feeds inst.ResolvedObjects via resolveValue first) — preserve that ordering.
func (e *Engine) contextSecrets(inst *model.ProcessInstance) []string {
	def, err := e.db.GetDefinition(inst.ProcessName, inst.ProcessVersion)
	if err != nil {
		return nil
	}
	out := def.SecretConfigValues(inst.Config)
	sf, ok := e.schemaFile(inst)
	if !ok {
		return out
	}
	collect := func(v any, node *schema.SchemaNode) {
		if node == nil {
			return
		}
		if ref, isRef := v.(*model.ObjectRef); isRef {
			cached, ok := inst.ResolvedObjects[ref.Ref]
			if !ok {
				return // never materialized this advance → cannot be in any log line
			}
			v = cached
		}
		schema.CollectSecrets(v, node, sf.Defs, &out)
	}
	if v, ok := inst.ContextData["input"]; ok {
		collect(v, sf.ProcessInput)
	}
	if outs, ok := inst.ContextData["outputs"].(map[string]any); ok {
		for tid, v := range outs {
			if ts, ok := sf.Tasks[tid]; ok {
				collect(v, ts.Output)
			}
		}
	}
	// Scrub the longest value first: when one secret is a prefix/substring of
	// another (e.g. an input array [5, 50, 500]), replacing the shorter one first
	// consumes the shared lead and leaves the longer one's tail exposed ("***0",
	// "***00"). Length-descending order makes each value redacted as a whole.
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// redactSecrets replaces each secret value in s with "***".
func redactSecrets(s string, secrets []string) string {
	for _, sv := range secrets {
		if sv != "" {
			s = strings.ReplaceAll(s, sv, "***")
		}
	}
	return s
}

// redactMeta returns a copy of meta with secret values scrubbed from its string
// values (e.g. the resolved endpoint URL). The original map is left unchanged.
func redactMeta(meta map[string]any, secrets []string) map[string]any {
	if len(meta) == 0 || len(secrets) == 0 {
		return meta
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		if s, ok := v.(string); ok {
			out[k] = redactSecrets(s, secrets)
		} else {
			out[k] = v
		}
	}
	return out
}

// logOnly records a console-only line (server lifecycle / operational events not in
// any instance's durable trail). It carries no Event — it renders free-form (a
// message + fields), distinct from the columnar audit rows.
func (e *Engine) logOnly(ev logEvent) {
	ev.Event = "" // operational: no structured event
	e.emit(ev)
}

// emit renders one record to the server console via slog. It builds the attrs only
// when the level is enabled (most are dropped at the common low-verbosity setting),
// keeping audit's hot path — the DB write — cheap. A record with an Event is a
// structured audit event (marked, so the console renders it in aligned columns); one
// without is operational (rendered free-form). The fields come from logview.Record
// so the console and the CLI show the same fields in the same order.
func (e *Engine) emit(ev logEvent) {
	lvl := slogLevel(ev.Level)
	if !e.log.Enabled(context.Background(), lvl) {
		return
	}
	if ev.Event == "" {
		// operational: message + any id/meta as free-form fields.
		attrs := make([]any, 0, 2+2*len(ev.Meta))
		if ev.ID != "" {
			attrs = append(attrs, "id", ev.ID)
		}
		for _, k := range sortedMetaKeys(ev.Meta) {
			attrs = append(attrs, k, ev.Meta[k])
		}
		e.log.Log(context.Background(), lvl, ev.Msg, attrs...)
		return
	}
	// audit: the event is the slog message; id/task become columns; the rest detail.
	detail := logview.Record{
		Event: ev.Event, Msg: ev.Msg, Code: ev.Code, Data: ev.Data, Meta: ev.Meta,
	}.Detail(e.logCfg.Mode)
	attrs := make([]any, 0, 6+2*len(detail))
	attrs = append(attrs, logview.AuditKey, true, "id", ev.ID, "task", ev.Task)
	for _, f := range detail {
		attrs = append(attrs, f.Key, f.Val)
	}
	e.log.Log(context.Background(), lvl, ev.Event, attrs...)
}

func sortedMetaKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// slogLevel maps an audit level to the matching slog level for console output.
func slogLevel(l model.LogLevel) slog.Level {
	switch l {
	case model.LogDebug:
		return slog.LevelDebug
	case model.LogWarn:
		return slog.LevelWarn
	case model.LogError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// statusMeta wraps an HTTP status as event metadata, or nil for a non-HTTP (status 0)
// transport so the meta field stays absent.
func statusMeta(status int) map[string]any {
	if status == 0 {
		return nil
	}
	return map[string]any{"status": status}
}

// AuditCreated records the instance_created milestone for a freshly created
// instance, capturing its process input (subject to payload-logging config). The
// API calls it for a root instance right after persisting it; the engine calls it
// for each spawned child. It bookends the trail with instance_completed, which
// carries the final output.
func (e *Engine) AuditCreated(inst *model.ProcessInstance) {
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceCreated, Data: e.snippet(inst.ContextData["input"])})
}

// outputData captures the process's final output for the instance_completed event:
// the raw snippet of context_data["output"] (set by computeOutput from the
// definition's output projection), or "" when the process defines no output (or
// payload logging is off).
func (e *Engine) outputData(inst *model.ProcessInstance) string {
	return e.snippet(inst.ContextData["output"])
}

// snippet renders v as JSON for inclusion in an audit detail. It returns the FULL
// payload (no truncation): audit caps it for the console and externalizes anything
// over the payload size to a log object, so the captured value is never lossy.
// Returns "" when payload capture is disabled or v is empty.
func (e *Engine) snippet(v any) string {
	if !e.logCfg.Payloads || v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// snippetRaw returns an already-string payload (e.g. an error response body, raw
// text not a value to JSON-encode) in full; audit caps/externalizes it like snippet.
// Returns "" when payload capture is off or s is empty.
func (e *Engine) snippetRaw(s string) string {
	if !e.logCfg.Payloads {
		return ""
	}
	return s
}

// payloadCap is the configured per-payload size used both as the console truncation
// point and the inline-vs-externalize threshold for log data.
func (e *Engine) payloadCap() int {
	if e.logCfg.PayloadBytes > 0 {
		return e.logCfg.PayloadBytes
	}
	return defaultPayloadBytes
}

// logPreviewBytes is the length of the inline excerpt kept on a log row whose full
// payload was externalized, so a listing can show a snippet without loading the object.
const logPreviewBytes = 512

func truncateStr(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

// encodeLogData renders a (already secret-scrubbed) log payload into the data column:
// a small payload is stored inline as an envelope; a large one is written to a log
// object and stored as a reference plus a short preview, so the high-churn process_logs
// table never holds a huge value. Best-effort: if the object write fails it falls back
// to an inline, truncated preview.
func (e *Engine) encodeLogData(instanceID, full string) string {
	if full == "" {
		return ""
	}
	if len(full) <= e.payloadCap() {
		if b, err := json.Marshal(model.Envelope{Data: full}); err == nil {
			return string(b)
		}
		return ""
	}
	ref, err := e.db.WriteLogObject(instanceID, full)
	if err != nil {
		if b, mErr := json.Marshal(model.Envelope{Data: truncateStr(full, e.payloadCap())}); mErr == nil {
			return string(b)
		}
		return ""
	}
	if b, err := json.Marshal(model.Envelope{Refs: []*model.ObjectRef{ref}, Preview: truncateStr(full, logPreviewBytes)}); err == nil {
		return string(b)
	}
	return ""
}

// failInstance moves the instance to its failed state and returns the terminal
// outcome (persisted by runAdvance via saveAndNotify).
func (e *Engine) failInstance(inst *model.ProcessInstance, reason string) advanceOutcome {
	inst.Status = model.StatusFailed
	inst.WaitState = model.WaitStateNone
	inst.Error = reason
	inst.WakeAt = nil
	e.audit(inst, logEvent{Level: model.LogError, Event: model.EventInstanceFailed, Msg: reason})
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
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventCancelled})
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
	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventInstanceSettled, Msg: inst.Error})
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
		e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventChildrenCollect, Task: task.ID})
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

	e.audit(inst, logEvent{Level: model.LogInfo, Event: model.EventChildrenSpawned, Task: task.ID, Msg: fmt.Sprintf("%d children", len(children))})
	// Each spawned child is its own process: record its creation + input so its
	// subtree trail bookends the same way a root's does.
	for _, c := range children {
		e.AuditCreated(c)
	}
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
		"input":              input,
		"outputs":            map[string]any{},
		"output_order":       []string{},
		"error":              nil,
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
		Task:           def.Tasks[0].ID,
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
			"input":              input,
			"outputs":            map[string]any{},
			"output_order":       []string{},
			"error":              nil,
			"_spawn_action_type": string(model.ActionTypeChildParallel),
			"_spawn_child_key":   key,
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
			Task:           def.Tasks[0].ID,
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
	val, err := e.evalShapeCtx(inst, input.Raw, nil)
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
	out, err := e.evalShapeCtx(inst, def.Output.Raw, nil)
	if err != nil {
		return fmt.Errorf("output: %w", err)
	}
	inst.ContextData["output"] = out
	return nil
}

// resolveEndpoint evaluates the REST endpoint as a template so a base URL can come
// from config or input (e.g. "{{ config.server_url }}/path"), returning the
// resolved URL. Returns "" for actions without an endpoint. Secret values it
// carries are scrubbed from logged URLs/errors by audit().
func (e *Engine) resolveEndpoint(inst *model.ProcessInstance, call *model.Action) (string, error) {
	if call.Endpoint == "" {
		return "", nil
	}
	val, err := e.evalAnyCtx(inst, call.Endpoint)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", val), nil
}

// resolveHeaders evaluates each header value expression against the instance
// context and coerces the result to a string. Returns nil for calls without headers.
func (e *Engine) resolveHeaders(inst *model.ProcessInstance, call *model.Action) (map[string]string, error) {
	if len(call.Headers) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(call.Headers))
	for k, expr := range call.Headers {
		val, err := e.evalAnyCtx(inst, expr)
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", k, err)
		}
		resolved[k] = fmt.Sprintf("%v", val)
	}
	return resolved, nil
}
