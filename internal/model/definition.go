package model

// StepType identifies the kind of step.
type StepType string

const (
	StepTypeTask        StepType = "task"
	StepTypeConditional StepType = "conditional"
)

// Transport identifies how the engine communicates with a service.
type Transport string

const (
	TransportHTTP Transport = "http"
	TransportTCP  Transport = "tcp"
	TransportUDS  Transport = "uds"
)

// Step is a single unit of work in a process definition.
// For task steps, Transport/Endpoint/TimeoutMs/Retries are used.
// For conditional steps, Condition/Then/Else are used.
type Step struct {
	Type      StepType  `json:"type"`
	ID        string    `json:"id"`
	Transport Transport `json:"transport,omitempty"`
	Endpoint  string    `json:"endpoint,omitempty"`
	TimeoutMs int       `json:"timeout_ms,omitempty"`
	Retries   int       `json:"retries,omitempty"`
	Condition string    `json:"condition,omitempty"`
	Then      []*Step   `json:"then,omitempty"`
	Else      []*Step   `json:"else,omitempty"`
}

// ProcessDefinition is the immutable versioned blueprint for a process.
// Once published, a version must never be mutated — create a new version instead.
type ProcessDefinition struct {
	Name    string  `json:"name"`
	Version int     `json:"version"`
	Steps   []*Step `json:"steps"`
}
