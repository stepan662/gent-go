package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestInferScalar(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		{"3", int64(3)},
		{"-7", int64(-7)},
		{"1.5", 1.5},
		{"hello", "hello"},
		{"007abc", "007abc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := inferScalar(c.in); got != c.want {
			t.Errorf("inferScalar(%q) = %v (%T), want %v (%T)", c.in, got, got, c.want, c.want)
		}
	}
}

func TestApplySetNesting(t *testing.T) {
	m := map[string]any{}
	for _, kv := range []string{"name=Sam", "user.age=30", "user.admin=true"} {
		if err := applySet(m, kv); err != nil {
			t.Fatalf("applySet(%q): %v", kv, err)
		}
	}
	if got, want := toJSON(t, m), `{"name":"Sam","user":{"admin":true,"age":30}}`; got != want {
		t.Errorf("nested set:\n got %s\nwant %s", got, want)
	}

	// Missing '=' and a dotted path through a non-object are reported.
	if err := applySet(map[string]any{}, "novalue"); err == nil {
		t.Error("expected error for missing '='")
	}
	clash := map[string]any{"a": "scalar"}
	if err := applySet(clash, "a.b=1"); err == nil {
		t.Error("expected error setting through a non-object")
	}
}

func TestBuildInput(t *testing.T) {
	// Neither flag: input is absent.
	if v, present, err := buildInput("", nil); err != nil || present || v != nil {
		t.Errorf("buildInput(empty) = %v, present=%v, err=%v", v, present, err)
	}

	// Relaxed JSON literal (unquoted keys, bare values).
	v, present, err := buildInput("{name: Sam, count: 3}", nil)
	if err != nil || !present {
		t.Fatalf("relaxed input: present=%v err=%v", present, err)
	}
	if got, want := toJSON(t, v), `{"count":3,"name":"Sam"}`; got != want {
		t.Errorf("relaxed input:\n got %s\nwant %s", got, want)
	}

	// --set overrides a field from --input and adds a new one.
	v, _, err = buildInput("{count: 1}", []string{"count=2", "active=true"})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if got, want := toJSON(t, v), `{"active":true,"count":2}`; got != want {
		t.Errorf("override:\n got %s\nwant %s", got, want)
	}

	// --set with no --input builds an object from scratch.
	v, _, err = buildInput("", []string{"x.y=1"})
	if err != nil {
		t.Fatalf("set-only: %v", err)
	}
	if got, want := toJSON(t, v), `{"x":{"y":1}}`; got != want {
		t.Errorf("set-only:\n got %s\nwant %s", got, want)
	}

	// --set requires the base to be an object.
	if _, _, err := buildInput("5", []string{"a=1"}); err == nil {
		t.Error("expected error: --set on a non-object --input")
	}
}

func TestReadInputSourceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.json")
	if err := os.WriteFile(path, []byte(`{"k":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readInputSource("@" + path)
	if err != nil {
		t.Fatalf("read @file: %v", err)
	}
	if string(data) != `{"k":1}` {
		t.Errorf("read @file = %q", data)
	}
	if data, _ := readInputSource("literal"); string(data) != "literal" {
		t.Errorf("literal = %q", data)
	}
}

func TestInputValidationError(t *testing.T) {
	if d, ok := inputValidationError(fmt.Errorf("server: input validation: /count: want integer")); !ok || d != "/count: want integer" {
		t.Errorf("got (%q, %v)", d, ok)
	}
	if _, ok := inputValidationError(fmt.Errorf("server: process not found")); ok {
		t.Error("non-validation error should not match")
	}
}

func TestPayloadLabel(t *testing.T) {
	cases := map[string]string{
		"instance_created":   "input",
		"action_started":     "request",
		"action_succeeded":   "result",
		"action_failed":      "error",
		"instance_completed": "output",
		"children_spawned":   "data", // events without a payload label fall back to "data"
	}
	for event, want := range cases {
		if got := payloadLabel(event); got != want {
			t.Errorf("payloadLabel(%q) = %q, want %q", event, got, want)
		}
	}
}

func TestFormatMeta(t *testing.T) {
	// JSON numbers decode to float64; an integer status must render without a decimal.
	if got := formatMeta(map[string]any{"status": float64(200)}); got != "status=200" {
		t.Errorf("status meta = %q", got)
	}
	if got := formatMeta(map[string]any{"url": "http://x/y"}); got != "url=http://x/y" {
		t.Errorf("url meta = %q", got)
	}
	// Keys are sorted for stable output.
	if got := formatMeta(map[string]any{"status": float64(500), "code": "http.500"}); got != "code=http.500 status=500" {
		t.Errorf("multi meta = %q", got)
	}
	if got := formatMeta(nil); got != "" {
		t.Errorf("nil meta = %q, want empty", got)
	}
}

func TestClip(t *testing.T) {
	// The engine stores payload snippets raw — possibly cropped mid-JSON
	// (…(truncated)) — so the CLI only ever truncates the string, never parses it.
	if got := clip("hello world", 5); got != "hello…" {
		t.Errorf("clip(11,5) = %q", got)
	}
	if got := clip("short", 10); got != "short" {
		t.Errorf("clip(no-trunc) = %q", got)
	}
	if got := clip("anything", 0); got != "anything" {
		t.Errorf("clip(0=unlimited) = %q", got)
	}
	if got := clip("héllo", 2); got != "hé…" { // multibyte rune kept intact
		t.Errorf("clip(multibyte) = %q", got)
	}
}

func TestTimeFormatting(t *testing.T) {
	now := time.Now()
	// shortTime: relative ages for recent timestamps.
	if got := shortTime(now.Add(-5 * time.Minute).Format(time.RFC3339)); got != "5m ago" {
		t.Errorf("shortTime(5m) = %q", got)
	}
	if got := shortTime(now.Add(-3 * time.Hour).Format(time.RFC3339)); got != "3h ago" {
		t.Errorf("shortTime(3h) = %q", got)
	}
	// Unparseable input is returned unchanged.
	if got := shortTime("not-a-time"); got != "not-a-time" {
		t.Errorf("shortTime(garbage) = %q", got)
	}
	// logTime: clock-only for today.
	if got := logTime(now.Format(time.RFC3339)); len(got) != len("15:04:05") {
		t.Errorf("logTime(today) = %q, want HH:MM:SS", got)
	}
}
