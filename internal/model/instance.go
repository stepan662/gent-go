package model

import "time"

// Status represents the lifecycle state of a process instance.
//
// failing and cancelling are draining states: the outcome is decided but
// descendants are still settling. A node only becomes failed/cancelled once
// all its direct children are terminal, so a terminal root implies the whole
// tree has settled — which is what makes failed/cancelled roots retryable.
type Status string

const (
	StatusRunning    Status = "running"
	StatusCompleted  Status = "completed"
	StatusFailing    Status = "failing" // doomed by an error, draining descendants
	StatusFailed     Status = "failed"
	StatusCancelling Status = "cancelling" // cancel requested, draining descendants
	StatusCancelled  Status = "cancelled"
)

// WaitState tracks where a parent instance is in the child-process lifecycle.
type WaitState string

const (
	WaitStateNone       WaitState = ""           // not in a child-process wait cycle
	WaitStateWaiting    WaitState = "waiting"    // children spawned, waiting for them
	WaitStateCollecting WaitState = "collecting" // all children terminal, collect their outputs
	WaitStateExternal   WaitState = "external"   // parked on an external task, waiting for a submitted result (or timeout)
)

// Private context_data keys used by the external-task lifecycle. Underscore-prefixed
// like _children / _spawn_* so they are clearly engine-internal bookkeeping.
const (
	// CtxExternal holds the parked external task's metadata: {task_id, token, input}.
	// The queue endpoint reads token+input from here; never exposed as process output.
	CtxExternal = "_external"
	// CtxExternalResult holds a submitted, validated result placed by the resolve API.
	// Its presence is how the engine tells "result arrived" from "first arrival".
	CtxExternalResult = "_external_result"
)

// ProcessInstance is a single running execution of a ProcessDefinition.
// ProcessVersion is pinned at creation — process definition changes
// never affect existing instances.
type ProcessInstance struct {
	ID             string
	ProcessName    string
	ProcessVersion int

	// TaskQueue holds the remaining tasks to execute, serialized as JSON.
	// A switch goto replaces this slice with the target task and all tasks after it.
	TaskQueue []*Task

	// ContextData is the accumulated key/value state passed between tasks.
	ContextData map[string]any

	// ParentID is set when this instance was started by a child_process task.
	// Empty string means this is a root instance.
	ParentID string

	// SpawnTaskID is the ID of the parent task that spawned this instance.
	// Empty string for root instances. Scopes sibling queries to one spawn batch
	// so consecutive spawn tasks under the same parent never mix.
	SpawnTaskID string

	// CallStack is the ordered list of ancestor instance IDs (root first).
	// Used for O(1) ancestor lookup during error cascade.
	CallStack []string

	RetryCount    int
	WakeAt   *time.Time
	Status        Status
	WaitState     WaitState
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	WorkerID      *string
	LeaseExpiresAt *time.Time

	// Config is the configuration namespace resolved from the OS environment at
	// the start of each tick (see ProcessDefinition.ResolveConfig). It is exposed
	// to expressions as "config" but is transient: never persisted to the DB and
	// never returned over the API, so secret values stay out of stored state.
	Config map[string]any `json:"-"`

	// ConfigSecrets holds the resolved values of config vars marked secret, used
	// to redact them from audit-log payloads. Transient, like Config.
	ConfigSecrets []string `json:"-"`

	// ReclaimedExpired is a transient, non-persisted flag set by ClaimInstances
	// when this instance was reclaimed from an expired lease (its prior worker_id
	// was non-null) rather than picked up at a clean task boundary. It signals that
	// the current task may have been interrupted mid-execution on the previous owner.
	ReclaimedExpired bool
}

// InstanceSummary is the lightweight projection of a ProcessInstance used by list
// endpoints. It deliberately omits the heavy JSON blobs (context_data, task_queue,
// call_stack) so listing many instances never fetches or unmarshals a potentially
// huge context — those are only loaded for single-instance detail (GetInstance).
type InstanceSummary struct {
	ID             string
	ProcessName    string
	ProcessVersion int
	RetryCount     int
	Status         Status
	WaitState      WaitState
	Error          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
