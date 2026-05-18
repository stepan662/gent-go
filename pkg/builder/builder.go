// Package builder provides a fluent API for constructing process definitions.
// The resulting definition serialises cleanly to JSON and can be registered
// directly with the gent API.
//
// Example:
//
//	def, err := builder.New("order_pipeline", 1).
//	    Task("charge_card", builder.HTTP("http://localhost:8081/pay"), builder.Timeout(5000), builder.Retries(3)).
//	    If("context.payment_success == true",
//	        func(b *builder.Branch) {
//	            b.Task("ship_goods", builder.UDS("/var/run/shipping.sock"))
//	        },
//	        func(b *builder.Branch) {
//	            b.Task("send_error_email", builder.TCP("127.0.0.1:9000"))
//	        },
//	    ).
//	    Build()
package builder

import (
	"fmt"

	"github.com/stepangranat/gent/internal/model"
)

// ProcessBuilder constructs a ProcessDefinition using method chaining.
type ProcessBuilder struct {
	name    string
	version int
	steps   []*model.Step
	err     error
}

// New starts building a new process definition.
func New(name string, version int) *ProcessBuilder {
	if name == "" {
		return &ProcessBuilder{err: fmt.Errorf("process name cannot be empty")}
	}
	if version < 1 {
		return &ProcessBuilder{err: fmt.Errorf("version must be >= 1")}
	}
	return &ProcessBuilder{name: name, version: version}
}

// Task adds a task step to the process.
func (b *ProcessBuilder) Task(id string, opts ...StepOption) *ProcessBuilder {
	if b.err != nil {
		return b
	}
	s := &model.Step{Type: model.StepTypeTask, ID: id}
	for _, o := range opts {
		o(s)
	}
	if err := validateTask(s); err != nil {
		b.err = fmt.Errorf("step %q: %w", id, err)
		return b
	}
	b.steps = append(b.steps, s)
	return b
}

// If adds a conditional step that evaluates expr and executes the then or else branch.
// Both branches are optional — pass nil to skip a branch.
func (b *ProcessBuilder) If(condition string, then func(*Branch), elseFn func(*Branch)) *ProcessBuilder {
	if b.err != nil {
		return b
	}
	s := &model.Step{
		Type:      model.StepTypeConditional,
		ID:        fmt.Sprintf("cond_%d", len(b.steps)),
		Condition: condition,
	}
	if then != nil {
		tb := &Branch{}
		then(tb)
		if tb.err != nil {
			b.err = tb.err
			return b
		}
		s.Then = tb.steps
	}
	if elseFn != nil {
		eb := &Branch{}
		elseFn(eb)
		if eb.err != nil {
			b.err = eb.err
			return b
		}
		s.Else = eb.steps
	}
	b.steps = append(b.steps, s)
	return b
}

// Build validates and returns the completed ProcessDefinition.
func (b *ProcessBuilder) Build() (*model.ProcessDefinition, error) {
	if b.err != nil {
		return nil, b.err
	}
	if len(b.steps) == 0 {
		return nil, fmt.Errorf("process must have at least one step")
	}
	return &model.ProcessDefinition{
		Name:    b.name,
		Version: b.version,
		Steps:   b.steps,
	}, nil
}

// Branch is a sequence of steps inside a conditional branch.
type Branch struct {
	steps []*model.Step
	err   error
}

// Task adds a task step to the branch.
func (br *Branch) Task(id string, opts ...StepOption) *Branch {
	if br.err != nil {
		return br
	}
	s := &model.Step{Type: model.StepTypeTask, ID: id}
	for _, o := range opts {
		o(s)
	}
	if err := validateTask(s); err != nil {
		br.err = fmt.Errorf("branch step %q: %w", id, err)
		return br
	}
	br.steps = append(br.steps, s)
	return br
}

// If adds a nested conditional step inside the branch.
func (br *Branch) If(condition string, then func(*Branch), elseFn func(*Branch)) *Branch {
	if br.err != nil {
		return br
	}
	sub := &ProcessBuilder{name: "branch", version: 1}
	sub.If(condition, then, elseFn)
	if sub.err != nil {
		br.err = sub.err
		return br
	}
	br.steps = append(br.steps, sub.steps...)
	return br
}

// --- Step options ---

// StepOption configures a task step.
type StepOption func(*model.Step)

func HTTP(endpoint string) StepOption {
	return func(s *model.Step) {
		s.Transport = model.TransportHTTP
		s.Endpoint = endpoint
	}
}

func TCP(addr string) StepOption {
	return func(s *model.Step) {
		s.Transport = model.TransportTCP
		s.Endpoint = addr
	}
}

func UDS(path string) StepOption {
	return func(s *model.Step) {
		s.Transport = model.TransportUDS
		s.Endpoint = path
	}
}

// Timeout sets the step timeout in milliseconds.
func Timeout(ms int) StepOption {
	return func(s *model.Step) { s.TimeoutMs = ms }
}

// Retries sets the maximum number of retry attempts for the step.
func Retries(n int) StepOption {
	return func(s *model.Step) { s.Retries = n }
}

func validateTask(s *model.Step) error {
	if s.ID == "" {
		return fmt.Errorf("step ID cannot be empty")
	}
	if s.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	switch s.Transport {
	case model.TransportHTTP, model.TransportTCP, model.TransportUDS:
	default:
		return fmt.Errorf("transport must be set via HTTP(), TCP(), or UDS()")
	}
	return nil
}
