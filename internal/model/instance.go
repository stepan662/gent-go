package model

import "time"

// Status represents the lifecycle state of a process instance.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// ProcessInstance is a single running execution of a ProcessDefinition.
// ProcessVersion is pinned at creation — process definition changes
// never affect existing instances.
type ProcessInstance struct {
	ID             string
	ProcessName    string
	ProcessVersion int

	// StepQueue holds the remaining steps to execute, serialized as JSON.
	// Conditional steps expand into their branch inline when evaluated.
	StepQueue []*Step

	// ContextData is the accumulated key/value state passed between steps.
	ContextData map[string]interface{}

	RetryCount  int
	NextRetryAt *time.Time
	Status      Status
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
