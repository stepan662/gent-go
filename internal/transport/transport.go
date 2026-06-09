package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gent/internal/model"
)

// Request is the message the engine sends to a service.
type Request struct {
	InstanceID string                 `json:"instance_id"`
	StepID     string                 `json:"step_id"`
	Data       map[string]interface{} `json:"data"`
}

// Response carries the result of a Send call.
// ErrorCode is non-empty on failure ("http.404", "script.1", "output.parse", "start.error", etc.).
// ErrorMessage is a human-readable description of the failure (may include trimmed response body).
// Body holds the raw decoded JSON body on success.
type Response struct {
	Body         any
	ErrorCode    string
	ErrorMessage string
}

// Send dispatches a request to the appropriate endpoint based on the step's call config.
// headers contains pre-resolved header values (for rest calls).
func Send(ctx context.Context, call *model.Call, headers map[string]string, req Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	switch call.Type {
	case model.CallTypeREST:
		return sendHTTP(ctx, call.Endpoint, call.AcceptedStatus, headers, body)
	case model.CallTypeScript:
		return sendScript(ctx, call.Exec, body)
	default:
		return nil, fmt.Errorf("unknown call type: %q", call.Type)
	}
}

func sendHTTP(ctx context.Context, endpoint string, acceptedStatus []string, headers map[string]string, body []byte) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err // caller uses ClassifyGoError
	}
	defer resp.Body.Close()

	if !matchAcceptedStatus(resp.StatusCode, acceptedStatus) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = fmt.Sprintf("request failed with status %d without response body", resp.StatusCode)
		}
		return &Response{ErrorCode: fmt.Sprintf("http.%d", resp.StatusCode), ErrorMessage: msg}, nil
	}

	var b any
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return &Response{ErrorCode: "output.parse"}, nil
	}
	return &Response{Body: b}, nil
}

// matchAcceptedStatus reports whether code is covered by patterns.
// Patterns may be "2xx"/"3xx"/"4xx"/"5xx" (hundred-range) or exact 3-digit strings like "404".
// Empty patterns defaults to accepting any 2xx.
func matchAcceptedStatus(code int, patterns []string) bool {
	if len(patterns) == 0 {
		return code >= 200 && code <= 299
	}
	for _, p := range patterns {
		if len(p) == 3 && p[1] == 'x' && p[2] == 'x' {
			hundreds := int(p[0]-'0') * 100
			if code >= hundreds && code <= hundreds+99 {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(p); err == nil && n == code {
			return true
		}
	}
	return false
}

// sendScript runs exec via sh -c, writes newline-terminated JSON to stdin,
// and reads a newline-terminated JSON response from stdout.
func sendScript(ctx context.Context, command string, body []byte) (*Response, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = bytes.NewReader(append(body, '\n'))

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &Response{ErrorCode: fmt.Sprintf("script.%d", exitErr.ExitCode())}, nil
		}
		return nil, err // context deadline, launch failure — caller uses ClassifyScriptError
	}

	var b any
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&b); err != nil {
		return &Response{ErrorCode: "output.parse"}, nil
	}
	return &Response{Body: b}, nil
}

// ClassifyGoError maps a transport-level Go error to an error code.
// Used for REST call failures that never received an HTTP response.
//
// Returns pre.timeout or pre.error when the failure happened during the
// TCP dial phase (the server never received the request). Returns http.timeout
// when the connection was established but no response arrived in time.
func ClassifyGoError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		var netErr *net.OpError
		if errors.As(err, &netErr) && netErr.Op == "dial" {
			return "pre.timeout"
		}
		return "http.timeout"
	}
	return "pre.error"
}

// ClassifyScriptError maps a script-level Go error to an error code.
// Used for script failures that are not exec.ExitError.
//
// Returns pre.exec when the process failed to launch (command not found,
// permission denied, etc.). Returns script.timeout for context cancellations
// where the process may have already started.
func ClassifyScriptError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "script.timeout"
	}
	return "pre.exec"
}

// RetryDelay returns the backoff duration for a given retry attempt (exponential, capped at 5 min).
func RetryDelay(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// SQLLikeMatch reports whether s matches the SQL LIKE pattern p.
// '%' matches any sequence of characters; '_' matches exactly one character.
func SQLLikeMatch(p, s string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			p = p[1:]
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if SQLLikeMatch(p, s[i:]) {
					return true
				}
			}
			return false
		case '_':
			if len(s) == 0 {
				return false
			}
			p, s = p[1:], s[1:]
		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p, s = p[1:], s[1:]
		}
	}
	return len(s) == 0
}

// ValidLikePattern reports whether p is a valid SQL LIKE pattern (only printable ASCII, no raw %).
// We allow any non-empty string — validation is just a non-empty check for now.
func ValidLikePattern(p string) bool {
	return len(strings.TrimSpace(p)) > 0
}
