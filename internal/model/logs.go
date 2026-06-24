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
	EventInstanceCreated = "instance_created"
	EventActionStarted   = "action_started"   // an action call is about to be sent (request)
	EventActionSucceeded = "action_succeeded" // an action call returned successfully (response)
	EventActionFailed    = "action_failed"    // an action call returned an error (status + error body)
	EventTaskCompleted   = "task_completed"
	EventRetryScheduled  = "retry_scheduled"
	EventErrorRoute      = "routing_to_error_handler"
	EventErrorCompleted  = "error_route_completed"
	EventCancelSkipRetry = "cancel_skip_retry"
	EventInstanceDone    = "instance_completed"
	EventInstanceFailed  = "instance_failed"
	EventInstanceSettled = "instance_settled_failed"
	EventCancelled       = "instance_cancelled"
	EventChildrenSpawned = "children_spawned"
	EventChildrenCollect = "children_collected"
	EventDelayArmed      = "delay_armed"
	EventExternalArmed   = "external_armed"
	EventExternalResolved = "external_resolved"
	EventExternalTimeout  = "external_timeout"
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
