package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/stepangranat/gent/internal/model"
)

// Request is the message the engine sends to a service.
type Request struct {
	InstanceID string                 `json:"instance_id"`
	StepID     string                 `json:"step_id"`
	Data       map[string]interface{} `json:"data"`
}

// Response is the message the engine expects back from a service.
type Response struct {
	Status string                 `json:"status"` // "ok" or "error"
	Output map[string]interface{} `json:"output,omitempty"`
	Error  string                 `json:"error,omitempty"`
}

// Send dispatches a request to the appropriate endpoint based on the step's transport.
func Send(ctx context.Context, step *model.Step, req Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	switch step.Transport {
	case model.TransportHTTP:
		return sendHTTP(ctx, step.Endpoint, body)
	case model.TransportTCP:
		return sendStream(ctx, "tcp", step.Endpoint, body)
	case model.TransportUDS:
		return sendStream(ctx, "unix", step.Endpoint, body)
	default:
		return nil, fmt.Errorf("unknown transport: %q", step.Transport)
	}
}

func sendHTTP(ctx context.Context, endpoint string, body []byte) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	var r Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode http response: %w", err)
	}
	return &r, nil
}

// sendStream handles both TCP and Unix Domain Socket transports.
// Protocol: write newline-terminated JSON, read newline-terminated JSON.
func sendStream(ctx context.Context, network, address string, body []byte) (*Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("dial %s %s: %w", network, address, err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	var r Response
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("decode stream response: %w", err)
	}
	return &r, nil
}

// RetryDelay returns the backoff duration for a given retry attempt (exponential, capped at 5 min).
func RetryDelay(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}
