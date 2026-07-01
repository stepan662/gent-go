// Package logview is the single source of truth for how an instance's audit trail
// is presented, shared by the two surfaces that show it: the server console
// (engine → slog, streaming) and genctl logs (CLI → batch). Both build the same
// fields and render them through the same fixed-width column layout, so a row looks
// identical in either place. The CLI adds a header (it has the whole page); the
// streaming server can't, and its operational (non-event) logs render free-form.
package logview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode is how a record is rendered: basic shows the bounded columns/fields, detail
// adds the (variable-width) data body, and json emits one JSON object per line.
type Mode string

const (
	ModeBasic  Mode = "basic"
	ModeDetail Mode = "detail"
	ModeJSON   Mode = "json"
)

// ParseMode validates a mode string.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeBasic, ModeDetail, ModeJSON:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("invalid log mode %q (want basic, detail, or json)", s)
	}
}

// IncludesData reports whether the mode carries the data body: detail appends it to
// the columns and json emits every field including it; basic omits it.
func (m Mode) IncludesData() bool { return m == ModeDetail || m == ModeJSON }

// AuditKey is the slog attr that marks a record as a structured, DB-persisted audit
// event (so the console renders it in columns). Operational logs lack it and render
// free-form. The handler strips it from output.
const AuditKey = "_audit"

// Fixed column widths for the aligned layout (the server streams one record at a
// time, so widths can't be computed from the batch — they're fixed). A value wider
// than its column just pushes the rest right on that row.
const (
	colTime  = 8  // 15:04:05
	colLevel = 5  // DEBUG
	colID    = 6  // short instance id
	colEvent = 16 // longest event (action_succeeded)
	colTask  = 14 // user-defined task id; the last column before the detail fields
)

// Label is the display name for an event's data body — what the body is, framed by
// the event that produced it. Events without a payload fall back to "data".
func Label(event string) string {
	switch event {
	case "inst_created":
		return "input"
	case "action_started":
		return "request"
	case "action_succeeded":
		return "result"
	case "action_failed":
		return "error"
	case "inst_completed":
		return "output"
	default:
		return "data"
	}
}

// Field is one rendered key/value of a log line.
type Field struct {
	Key string
	Val any
}

// Record is the layout-independent content of one audit event.
type Record struct {
	Event string
	ID    string
	Task  string
	Msg   string // human note
	Code  string
	Data  string // body (request/response/input/output/…)
	Meta  map[string]any
}

// Detail returns the trailing key=value fields for an event — everything that isn't
// a fixed column: msg, code, meta (keys sorted), and the data body under its Label
// when the mode includes it. Both surfaces use this, so they show the same fields in
// the same order.
func (r Record) Detail(mode Mode) []Field {
	fs := make([]Field, 0, 3+len(r.Meta))
	if r.Msg != "" {
		fs = append(fs, Field{"msg", r.Msg})
	}
	if r.Code != "" {
		fs = append(fs, Field{"code", r.Code})
	}
	for _, k := range sortedKeys(r.Meta) {
		fs = append(fs, Field{k, r.Meta[k]})
	}
	if r.Data != "" && mode.IncludesData() {
		fs = append(fs, Field{Label(r.Event), r.Data})
	}
	return fs
}

// RenderEvent renders an audit event as a fixed-width column line. The id column is
// shown only when withID (always on the server, where instances interleave; on the
// CLI only in --tree):
//
//	15:04:05  INFO   2559a9  action_started    first         msg=rest url=… request={…}
func RenderEvent(t time.Time, level, id, event, task string, detail []Field, withID bool) string {
	line := columnPrefix(t.Format("15:04:05"), strings.ToUpper(level), id, event, task, withID)
	if d := renderFields(detail); d != "" {
		line += "  " + d
	}
	return strings.TrimRight(line, " ")
}

// RenderFree renders an operational record (no event) free-form — time, level, then
// the message as a leading msg= field and the rest as key=value, so its human text
// reads the same way (msg="…") as a columnar audit row's. It deliberately doesn't fit
// the columns; these happen at startup or on something unexpected.
//
//	15:04:05  INFO   msg="engine started" max_concurrent=200 worker=…
func RenderFree(t time.Time, level, message string, fields []Field) string {
	if message != "" {
		fields = append([]Field{{"msg", message}}, fields...)
	}
	line := fmt.Sprintf("%-*s  %-*s", colTime, t.Format("15:04:05"), colLevel, strings.ToUpper(level))
	if d := renderFields(fields); d != "" {
		line += "  " + d
	}
	return strings.TrimRight(line, " ")
}

// RenderJSON renders a record as one compact JSON object (JSONL): every field,
// untruncated and jq-friendly. It's the json mode on either surface. The columns
// (id/event/task) and the trailing detail fields all become object keys — an audit
// record carries event (+task), an operational one carries msg.
func RenderJSON(t time.Time, level, message string, isAudit bool, id, task string, fields []Field) string {
	obj := make(map[string]any, 5+len(fields))
	obj["time"] = t.Format(time.RFC3339Nano)
	obj["level"] = strings.ToLower(level)
	if id != "" {
		obj["id"] = id
	}
	if isAudit {
		obj["event"] = message
		if task != "" {
			obj["task"] = task
		}
	} else if message != "" {
		obj["msg"] = message
	}
	for _, f := range fields {
		obj[f.Key] = f.Val
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(b)
}

// Header is the column header line for the CLI (the streaming server has none).
func Header(withID bool) string {
	return strings.TrimRight(columnPrefix("TIME", "LEVEL", "ID", "EVENT", "TASK", withID), " ")
}

func columnPrefix(t, level, id, event, task string, withID bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  ", colTime, t, colLevel, level)
	if withID {
		fmt.Fprintf(&b, "%-*s  ", colID, id)
	}
	fmt.Fprintf(&b, "%-*s  %-*s", colEvent, event, colTask, task)
	return b.String()
}

func renderFields(fs []Field) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		parts = append(parts, f.Key+"="+FormatVal(f.Val))
	}
	return strings.Join(parts, " ")
}

// FormatVal renders a field value compactly: JSON bodies ({…}/[…]) and plain tokens
// raw, free text with spaces quoted, integers without a trailing decimal.
func FormatVal(v any) string {
	s := valToString(v)
	switch {
	case s == "":
		return `""`
	case strings.HasPrefix(s, "{"), strings.HasPrefix(s, "["): // JSON body — keep raw/readable
		return s
	case strings.ContainsAny(s, " \t"):
		return strconv.Quote(s)
	default:
		return s
	}
}

func valToString(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64: // JSON numbers; integers print without a decimal point
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'g', -1, 64)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case bool:
		return strconv.FormatBool(n)
	case fmt.Stringer:
		return n.String()
	default:
		return fmt.Sprint(v)
	}
}

// ShortID is the compact, distinguishing id tag shown in the ID column — the id's
// random tail, not its timestamp-prefixed head, so a parent and a same-millisecond
// child differ. The full id is available via the API / genctl logs --json.
func ShortID(id string) string {
	if len(id) > colID {
		return id[len(id)-colID:]
	}
	return id
}

func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NewHandler builds the server console slog handler. In basic/detail modes, records
// carrying AuditKey render as aligned columns (event = the slog message, plus id/task
// and the detail fields) and everything else (operational logs, plain slog calls)
// renders free-form; in json mode every record is one JSON object per line.
func NewHandler(w io.Writer, level slog.Level, mode Mode) slog.Handler {
	return &consoleHandler{w: w, level: level, mode: mode, mu: &sync.Mutex{}}
}

type consoleHandler struct {
	w     io.Writer
	level slog.Level
	mode  Mode
	attrs []slog.Attr
	mu    *sync.Mutex
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	isAudit := false
	var id, task string
	detail := make([]Field, 0, 8)
	collect := func(a slog.Attr) {
		switch a.Key {
		case AuditKey:
			isAudit = true
		case "id":
			id = a.Value.String()
		case "task":
			task = a.Value.String()
		default:
			detail = append(detail, Field{a.Key, a.Value.Any()})
		}
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool { collect(a); return true })

	var line string
	switch {
	case h.mode == ModeJSON:
		line = RenderJSON(r.Time, r.Level.String(), r.Message, isAudit, id, task, detail)
	case isAudit:
		line = RenderEvent(r.Time, r.Level.String(), ShortID(id), r.Message, task, detail, true)
	default:
		if id != "" { // an operational log about an instance keeps its id as a field
			detail = append([]Field{{"id", id}}, detail...)
		}
		line = RenderFree(r.Time, r.Level.String(), r.Message, detail)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

func (h *consoleHandler) WithAttrs(as []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &nh
}

func (h *consoleHandler) WithGroup(string) slog.Handler { return h } // groups unused
