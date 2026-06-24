package logview

import (
	"testing"
	"time"
)

func TestLabel(t *testing.T) {
	cases := map[string]string{
		"inst_created":     "input",
		"action_started":      "request",
		"action_succeeded":    "result",
		"action_failed":       "error",
		"inst_completed":   "output",
		"child_spawned":    "data", // events without a payload label fall back to "data"
	}
	for event, want := range cases {
		if got := Label(event); got != want {
			t.Errorf("Label(%q) = %q, want %q", event, got, want)
		}
	}
}

func TestParseModeAndIncludesData(t *testing.T) {
	for _, s := range []string{"basic", "detail", "json"} {
		if _, err := ParseMode(s); err != nil {
			t.Errorf("ParseMode(%q) errored: %v", s, err)
		}
	}
	if _, err := ParseMode("raw"); err == nil {
		t.Error("ParseMode(raw) should error")
	}
	if ModeBasic.IncludesData() {
		t.Error("basic should not include data")
	}
	if !ModeDetail.IncludesData() {
		t.Error("detail should include data")
	}
	if !ModeJSON.IncludesData() {
		t.Error("json should include data")
	}
}

func TestDetail(t *testing.T) {
	rec := Record{
		Event: "action_succeeded", ID: "i1", Task: "fetch",
		Data: `{"slept":5000}`, Meta: map[string]any{"status": float64(200)},
	}
	// id/task are columns, not detail; basic omits the data body, detail appends it
	// under the event's label.
	basic := rec.Detail(ModeBasic)
	if hasKey(basic, "result") || hasKey(basic, "id") || hasKey(basic, "task") {
		t.Errorf("basic detail should be just meta: %v", basic)
	}
	detail := rec.Detail(ModeDetail)
	if !hasKey(detail, "result") || !hasKey(detail, "status") {
		t.Errorf("detail missing expected keys: %v", detail)
	}
	if hasKey(detail, "id") || hasKey(detail, "task") {
		t.Errorf("id/task are columns, not detail: %v", detail)
	}
}

func TestRenderEvent(t *testing.T) {
	rec := Record{Event: "action_succeeded", Data: `{"slept":5000}`, Meta: map[string]any{"status": float64(200)}}
	ts := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

	// With id (tree): fixed-width columns, JSON body rendered raw, status from meta.
	got := RenderEvent(ts, "info", "abc123", rec.Event, "fetch", rec.Detail(ModeDetail), true)
	want := "15:04:05  INFO   abc123  action_succeeded  fetch           status=200 result={\"slept\":5000}"
	if got != want {
		t.Errorf("RenderEvent(tree):\n got %q\nwant %q", got, want)
	}

	// basic mode drops the data body.
	got = RenderEvent(ts, "info", "abc123", rec.Event, "fetch", rec.Detail(ModeBasic), true)
	want = "15:04:05  INFO   abc123  action_succeeded  fetch           status=200"
	if got != want {
		t.Errorf("RenderEvent(basic):\n got %q\nwant %q", got, want)
	}
}

func TestRenderFree(t *testing.T) {
	ts := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	got := RenderFree(ts, "info", "engine started", []Field{{"worker", "w1"}})
	want := "15:04:05  INFO   msg=\"engine started\" worker=w1"
	if got != want {
		t.Errorf("RenderFree:\n got %q\nwant %q", got, want)
	}
}

func TestRenderJSON(t *testing.T) {
	ts := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

	// An audit record: event/id/task plus the detail fields become object keys.
	got := RenderJSON(ts, "INFO", "action_succeeded", true, "abc123", "fetch",
		[]Field{{"status", float64(200)}, {"result", `{"slept":5000}`}})
	want := `{"event":"action_succeeded","id":"abc123","level":"info","result":"{\"slept\":5000}","status":200,"task":"fetch","time":"2026-01-02T15:04:05Z"}`
	if got != want {
		t.Errorf("RenderJSON(audit):\n got %s\nwant %s", got, want)
	}

	// An operational record: the message is carried as msg, no event/task.
	got = RenderJSON(ts, "INFO", "engine started", false, "", "", []Field{{"worker", "w1"}})
	want = `{"level":"info","msg":"engine started","time":"2026-01-02T15:04:05Z","worker":"w1"}`
	if got != want {
		t.Errorf("RenderJSON(free):\n got %s\nwant %s", got, want)
	}
}

func TestFormatVal(t *testing.T) {
	cases := map[any]string{
		`{"a":1}`:   `{"a":1}`,   // JSON body raw (not quoted despite quotes)
		"→ next":    `"→ next"`,  // free text with a space → quoted
		"http.500":  "http.500",  // plain token raw
		float64(200): "200",      // integer, no decimal
	}
	for in, want := range cases {
		if got := FormatVal(in); got != want {
			t.Errorf("FormatVal(%v) = %q, want %q", in, got, want)
		}
	}
}

func hasKey(fs []Field, key string) bool {
	for _, f := range fs {
		if f.Key == key {
			return true
		}
	}
	return false
}
