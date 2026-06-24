package model

import "time"

// LogLevel mirrors slog levels for a persisted log entry.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Log event kinds emitted by the engine as it advances an instance. These are
// the stable machine-readable identifiers; the human message lives in Message.
const (
	EventInstanceCreated = "inst_created"
	EventTaskStarted     = "task_started"      // a worker picked the instance up and began advancing it
	EventActionStarted   = "action_started"   // an action call is about to be sent (request)
	EventActionSucceeded = "action_succeeded" // an action call returned successfully (response)
	EventActionFailed    = "action_failed"    // an action call returned an error (status + error body)
	EventTaskCompleted   = "task_completed"
	EventRetryScheduled  = "retry_scheduled"
	EventErrorRoute      = "error_routed"
	EventErrorCompleted  = "error_handled"
	EventCancelSkipRetry = "cancel_skipped"
	EventInstanceDone    = "inst_completed"
	EventInstanceFailed  = "inst_failed"
	EventInstanceSettled = "inst_settled"
	EventCancelled       = "inst_cancelled"
	EventChildrenSpawned = "child_spawned"
	EventChildrenCollect = "child_collected"
	EventDelayArmed      = "delay_armed"
	EventExternalArmed   = "extern_armed"
	EventExternalResolved = "extern_resolved"
	EventExternalTimeout  = "extern_timeout"
)

// LogEntry is one persisted line of an instance's execution audit trail.
//
// Data carries the single raw payload an event is about — a process/task input,
// output, or request/response/error body — as a JSON snippet that MAY be truncated
// (so it is not guaranteed to parse). Meta carries small, complete, structured
// metadata about the event (e.g. {"url":…} / {"status":200}) and is always valid
// JSON. Message is the human-readable summary; the same fact may appear in both
// Message (prose) and Meta (structured) by design. Small facts with no payload
// (attempt counts, goto target, child counts) live in Message.
type LogEntry struct {
	ID         string         `json:"id"`
	InstanceID string         `json:"instance_id"`
	Level      LogLevel       `json:"level"`
	Event      string         `json:"event"`
	TaskID     string         `json:"task_id,omitempty"`
	Message    string         `json:"message,omitempty"`
	Code       string         `json:"code,omitempty"`
	Data       string         `json:"data,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	// Depth is the instance's distance from the queried subtree root; only set by
	// ListTreeLogs (0 for single-instance queries). Not persisted.
	Depth int `json:"-"`
}
