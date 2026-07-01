package engine

import (
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/schema"
)

// A result_schema field marked secret is redacted from the logged response body
// (action_succeeded), so a "secret server-response key" never reaches the trail.
func TestSnippetResultRedactsSecret(t *testing.T) {
	e := &Engine{logCfg: LogConfig{Payloads: true}}
	task := &model.Task{Action: &model.Action{
		Type: model.ActionTypeREST,
		ResultSchema: &schema.SchemaNode{
			Type: schema.SchemaType{"object"},
			Properties: map[string]*schema.SchemaNode{
				"token": {Type: schema.SchemaType{"string"}, Secret: true},
				"name":  {Type: schema.SchemaType{"string"}},
			},
		},
	}}
	body := map[string]any{"token": "s3cr3t-token", "name": "public"}

	got := e.snippetResult(task, body)
	if strings.Contains(got, "s3cr3t-token") {
		t.Errorf("secret leaked into log payload: %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("secret not redacted: %s", got)
	}
	if !strings.Contains(got, "public") {
		t.Errorf("non-secret value lost: %s", got)
	}
}

// With payload logging off, nothing is rendered at all (no leak, no work).
func TestSnippetResultEmptyWhenPayloadsOff(t *testing.T) {
	e := &Engine{logCfg: LogConfig{Payloads: false}}
	task := &model.Task{Action: &model.Action{Type: model.ActionTypeREST}}
	if got := e.snippetResult(task, map[string]any{"x": 1}); got != "" {
		t.Errorf("want empty snippet when payloads off, got %q", got)
	}
}
