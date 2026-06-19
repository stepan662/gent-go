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
	EventTaskStarted     = "task_started"
	EventTaskSucceeded   = "task_succeeded"
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
// Detail carries event-specific structured fields (attempt counts, goto target,
// truncated request/response snippets) and is stored as a JSON blob.
type LogEntry struct {
	ID         string         `json:"id"`
	InstanceID string         `json:"instance_id"`
	Level      LogLevel       `json:"level"`
	Event      string         `json:"event"`
	TaskID     string         `json:"task_id,omitempty"`
	Message    string         `json:"message,omitempty"`
	Code       string         `json:"code,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	// Depth is the instance's distance from the queried subtree root; only set by
	// ListTreeLogs (0 for single-instance queries). Not persisted.
	Depth int `json:"-"`
}
