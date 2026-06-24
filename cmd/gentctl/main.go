// gentctl is a command-line gateway to a running gent server, inspired by kubectl.
// It reads process definition files (YAML or JSON, multi-document via ---) and
// forwards them to the server in a single API call.
//
// Usage:
//
//	gentctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest] [--auto-update-parents]
//	gentctl validate -f file.yaml [-f file2.yaml ...]
//	gentctl run      <process> [--channel C | --version N] [--input <json|@file|->] [--set k=v ...]
//	gentctl instances [--status <status>] [--sort updated|created] [--limit <n>] [--all]
//	gentctl get      <instance-id> [--json]
//	gentctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--tree] [--mode basic|detail|json] <instance-id>
//	gentctl cancel   <instance-id>
//	gentctl retry    [--force] <instance-id>
//	gentctl channel list   <process>
//	gentctl channel set    <process> <channel> <version>
//	gentctl channel delete <process> <channel>
//	gentctl promote  --from <channel> --to <channel> [--process <name>]
//	gentctl status   --channel <channel>
//
// Environment:
//
//	GENT_SERVER  base URL of the gent server (default: http://localhost:8448)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gent/internal/logview"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cfg := loadConfig()
	server := os.Getenv("GENT_SERVER")
	if server == "" {
		server = cfg.Server
	}
	if server == "" {
		server = "http://localhost:8448"
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "apply":
		runApplyCmd(server, args)
	case "validate":
		runValidateCmd(server, args)
	case "run":
		runRunCmd(server, args)
	case "get":
		runGetCmd(server, args)
	case "channel":
		runChannelCmd(server, args)
	case "promote":
		runPromoteCmd(server, args)
	case "status":
		runStatusCmd(server, args)
	case "instances":
		runInstancesCmd(server, args)
	case "logs":
		runLogsCmd(server, args)
	case "cancel":
		runCancelCmd(server, args)
	case "retry":
		runRetryCmd(server, args)
	case "config":
		runConfigCmd(args)
	default:
		fmt.Fprintf(os.Stderr, "gentctl: unknown command %q\n", cmd)
		usage()
		os.Exit(1)
	}
}

func runApplyCmd(server string, args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	var files multiFlag
	fs.Var(&files, "f", "definition file (YAML or JSON); repeat for multiple files")
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	channelFlag := fs.String("channel", "latest", "channel to apply definitions to")
	autoUpdateFlag := fs.Bool("auto-update-parents", false, "auto-update parent processes on the same channel")
	fs.Parse(args)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "gentctl: -f is required")
		os.Exit(1)
	}

	defs, err := loadDefs(files)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{
		"channel":             *channelFlag,
		"auto_update_parents": *autoUpdateFlag,
		"definitions":         defs,
	}

	var resp []struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
		Saved   bool   `json:"saved"`
	}
	if err := call(*serverFlag+"/definitions/batch", http.MethodPut, body, &resp); err != nil {
		fatal("%v", err)
	}
	for _, r := range resp {
		status := "saved"
		if !r.Saved {
			status = "unchanged"
		}
		fmt.Printf("%s: %s@v%d\n", status, r.Name, r.Version)
	}
}

func runValidateCmd(server string, args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var files multiFlag
	fs.Var(&files, "f", "definition file (YAML or JSON); repeat for multiple files")
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	fs.Parse(args)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "gentctl: -f is required")
		os.Exit(1)
	}

	defs, err := loadDefs(files)
	if err != nil {
		fatal("%v", err)
	}

	var raw json.RawMessage
	if err := call(*serverFlag+"/definitions/validate", http.MethodPost, defs, &raw); err != nil {
		fatal("%v", err)
	}
	var buf bytes.Buffer
	json.Indent(&buf, raw, "", "  ")
	os.Stdout.Write(buf.Bytes())
	os.Stdout.Write([]byte("\n"))
}

func runChannelCmd(server string, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: gentctl channel <list|set|delete> ...")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("channel", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	fs.Parse(args[1:])
	rest := fs.Args()

	sub := args[0]
	switch sub {
	case "list":
		if len(rest) < 1 {
			fatal("usage: gentctl channel list <process>")
		}
		type channelRow struct {
			Channel string `json:"channel"`
			Version int    `json:"version"`
		}
		listURL := *serverFlag + "/channels?name=" + url.QueryEscape(rest[0])
		resp, err := listAll[channelRow](listURL)
		if err != nil {
			fatal("%v", err)
		}
		for _, e := range resp {
			fmt.Printf("%s -> v%d\n", e.Channel, e.Version)
		}

	case "set":
		if len(rest) < 3 {
			fatal("usage: gentctl channel set <process> <channel> <version>")
		}
		v, err := strconv.Atoi(rest[2])
		if err != nil || v < 1 {
			fatal("version must be a positive integer")
		}
		if err := call(*serverFlag+"/channels", http.MethodPut,
			map[string]any{"name": rest[0], "channel": rest[1], "version": v}, nil); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("set: %s@%s -> v%d\n", rest[0], rest[1], v)

	case "delete":
		if len(rest) < 2 {
			fatal("usage: gentctl channel delete <process> <channel>")
		}
		if err := call(*serverFlag+"/channels", http.MethodDelete,
			map[string]any{"name": rest[0], "channel": rest[1]}, nil); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("deleted: %s@%s\n", rest[0], rest[1])

	default:
		fatal("unknown channel subcommand %q", sub)
	}
}

func runPromoteCmd(server string, args []string) {
	fs := flag.NewFlagSet("promote", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	fromFlag := fs.String("from", "", "source channel")
	toFlag := fs.String("to", "", "target channel")
	processFlag := fs.String("process", "", "limit to this process and its dependency subtree (optional)")
	fs.Parse(args)

	if *fromFlag == "" || *toFlag == "" {
		fatal("--from and --to are required")
	}

	body := map[string]any{"from": *fromFlag, "to": *toFlag}
	if *processFlag != "" {
		body["process"] = *processFlag
	}

	var resp struct {
		From     string           `json:"from"`
		To       string           `json:"to"`
		Promoted []map[string]any `json:"promoted"`
	}
	if err := call(*serverFlag+"/channels/promote", http.MethodPost, body, &resp); err != nil {
		fatal("%v", err)
	}
	for _, p := range resp.Promoted {
		fmt.Printf("promoted: %v@v%v -> %s\n", p["name"], p["version"], resp.To)
	}
}

func runStatusCmd(server string, args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	channelFlag := fs.String("channel", "latest", "channel to inspect")
	fs.Parse(args)

	var resp []struct {
		Name      string `json:"name"`
		Version   int    `json:"version"`
		StaleRefs []struct {
			TaskID         string `json:"task_id"`
			ChildName      string `json:"child_name"`
			BakedVersion   int    `json:"baked_version"`
			ChannelVersion int    `json:"channel_version"`
		} `json:"stale_refs"`
	}
	if err := call(*serverFlag+"/channels/status", http.MethodPost,
		map[string]any{"channel": *channelFlag}, &resp); err != nil {
		fatal("%v", err)
	}

	allClean := true
	for _, item := range resp {
		if len(item.StaleRefs) == 0 {
			continue
		}
		allClean = false
		fmt.Printf("STALE  %s@v%d\n", item.Name, item.Version)
		for _, ref := range item.StaleRefs {
			fmt.Printf("  task %q: %s baked@v%d, channel@v%d\n",
				ref.TaskID, ref.ChildName, ref.BakedVersion, ref.ChannelVersion)
		}
	}
	if allClean {
		fmt.Printf("channel %q is coherent\n", *channelFlag)
	}
}

// runRunCmd starts a new process instance. The process name is the first
// argument; flags follow it (e.g. `gentctl run greeter --set name=Sam`). Input is
// assembled from --input (a JSON/YAML literal, @file, or - for stdin) and any
// number of --set key=value overrides; see buildInput.
func runRunCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: gentctl run <process> [--channel C | --version N] [--input <json|@file|->] [--set k=v ...]")
	}
	process := args[0]

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	channelFlag := fs.String("channel", "", "resolve the version via this channel")
	versionFlag := fs.Int("version", 0, "pin an explicit process version")
	inputFlag := fs.String("input", "", "input as a JSON/YAML literal, @file, or - for stdin")
	var sets multiFlag
	fs.Var(&sets, "set", "set an input field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	fs.Parse(args[1:])

	input, hasInput, err := buildInput(*inputFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{"process": process}
	switch {
	case *versionFlag > 0:
		body["version"] = *versionFlag
	case *channelFlag != "":
		body["channel"] = *channelFlag
	}
	if hasInput {
		body["input"] = input
	}

	var resp struct {
		ID      string `json:"id"`
		Process string `json:"process"`
		Version int    `json:"version"`
		Status  string `json:"status"`
	}
	if err := call(*serverFlag+"/instances", http.MethodPost, body, &resp); err != nil {
		// Surface an input-schema mismatch as a clear, dedicated message instead of
		// the generic "server: ..." wrapper.
		if detail, ok := inputValidationError(err); ok {
			fatal("input is not valid for %s:\n  %s", process, detail)
		}
		fatal("%v", err)
	}
	fmt.Printf("started: %s  %s@v%d  (%s)\n", resp.ID, resp.Process, resp.Version, resp.Status)
}

// runGetCmd prints a single instance's details, including its full context. The
// instance id is the first argument; pass --json for the raw response.
func runGetCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: gentctl get <instance-id> [--json]")
	}
	id := args[0]

	fs := flag.NewFlagSet("get", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	jsonFlag := fs.Bool("json", false, "print the raw JSON response")
	fs.Parse(args[1:])

	u := *serverFlag + "/instances/" + url.PathEscape(id)
	if *jsonFlag {
		var raw json.RawMessage
		if err := callGet(u, &raw); err != nil {
			fatal("%v", err)
		}
		var buf bytes.Buffer
		json.Indent(&buf, raw, "", "  ")
		os.Stdout.Write(buf.Bytes())
		os.Stdout.Write([]byte("\n"))
		return
	}

	var inst struct {
		ID         string         `json:"id"`
		Process    string         `json:"process"`
		Version    int            `json:"version"`
		Status     string         `json:"status"`
		WaitState  string         `json:"wait_state"`
		RetryCount int            `json:"retry_count"`
		Error      string         `json:"error"`
		CreatedAt  string         `json:"created_at"`
		UpdatedAt  string         `json:"updated_at"`
		Context    map[string]any `json:"context"`
	}
	if err := callGet(u, &inst); err != nil {
		fatal("%v", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID:\t%s\n", inst.ID)
	fmt.Fprintf(w, "Process:\t%s@v%d\n", inst.Process, inst.Version)
	fmt.Fprintf(w, "Status:\t%s\n", inst.Status)
	if inst.WaitState != "" {
		fmt.Fprintf(w, "Wait:\t%s\n", inst.WaitState)
	}
	if inst.RetryCount > 0 {
		fmt.Fprintf(w, "Retries:\t%d\n", inst.RetryCount)
	}
	fmt.Fprintf(w, "Created:\t%s\n", longTime(inst.CreatedAt))
	fmt.Fprintf(w, "Updated:\t%s\n", longTime(inst.UpdatedAt))
	if inst.Error != "" {
		fmt.Fprintf(w, "Error:\t%s\n", inst.Error)
	}
	w.Flush()

	if len(inst.Context) > 0 {
		fmt.Println("\nContext:")
		b, _ := json.MarshalIndent(inst.Context, "", "  ")
		os.Stdout.Write(b)
		os.Stdout.Write([]byte("\n"))
	}
}

func runInstancesCmd(server string, args []string) {
	fs := flag.NewFlagSet("instances", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	statusFlag := fs.String("status", "", "filter by status (running, completed, failing, failed, cancelling, cancelled)")
	sortFlag := fs.String("sort", "created", "sort key: created (newest first) or updated (most recently active)")
	limitFlag := fs.Int("limit", 20, "max instances to show (server caps a page at 100; use --all for more)")
	allFlag := fs.Bool("all", false, "list every instance (follow all pages)")
	fs.Parse(args)

	q := url.Values{}
	if *statusFlag != "" {
		q.Set("status", *statusFlag)
	}
	q.Set("sort", *sortFlag)
	q.Set("order", "desc")
	if !*allFlag {
		q.Set("limit", strconv.Itoa(*limitFlag))
	}
	u := *serverFlag + "/instances?" + q.Encode()

	type instanceRow struct {
		ID        string `json:"id"`
		Process   string `json:"process"`
		Version   int    `json:"version"`
		Status    string `json:"status"`
		Error     string `json:"error"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}

	var rows []instanceRow
	var err error
	if *allFlag {
		rows, err = listAll[instanceRow](u)
	} else {
		var p page[instanceRow]
		if err = callGet(u, &p); err == nil {
			rows = p.Items
		}
	}
	if err != nil {
		fatal("%v", err)
	}
	if len(rows) == 0 {
		fmt.Println("no instances")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tPROCESS\tUPDATED\tCREATED\tERROR")
	for _, r := range rows {
		errMsg := r.Error
		if len(errMsg) > 50 {
			errMsg = errMsg[:47] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s@v%d\t%s\t%s\t%s\n",
			r.ID, r.Status, r.Process, r.Version,
			shortTime(r.UpdatedAt), shortTime(r.CreatedAt), errMsg)
	}
	w.Flush()
}

func runLogsCmd(server string, args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	levelFlag := fs.String("level", "", "filter by level (debug, info, warn, error); empty = all")
	sinceFlag := fs.Int64("since", 0, "only logs at/after this unix-millis timestamp")
	limitFlag := fs.Int("limit", 200, "max entries to return")
	treeFlag := fs.Bool("tree", false, "include the whole process subtree (root instance id)")
	modeFlag := fs.String("mode", "detail", "output: basic (no data body), detail (+ data), or json (one JSON object per line, untruncated)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fatal("usage: gentctl logs [--level L] [--since MS] [--limit N] [--tree] [--mode basic|detail|json] <instance-id>")
	}
	id := fs.Arg(0)
	mode, err := logview.ParseMode(*modeFlag)
	if err != nil {
		fatal("%v", err)
	}

	q := url.Values{}
	if *levelFlag != "" {
		q.Set("level", *levelFlag)
	}
	if *sinceFlag > 0 {
		q.Set("since", strconv.FormatInt(*sinceFlag, 10))
	}
	if *limitFlag > 0 {
		q.Set("limit", strconv.Itoa(*limitFlag))
	}
	if *treeFlag {
		q.Set("tree", "true")
	}
	u := *serverFlag + "/instances/" + url.PathEscape(id) + "/logs"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}

	// json mode dumps each entry as the server's JSON, one per line (JSONL):
	// everything, untruncated, pipe-friendly (jq).
	if mode == logview.ModeJSON {
		var raw struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := callGet(u, &raw); err != nil {
			fatal("%v", err)
		}
		for _, it := range raw.Items {
			os.Stdout.Write(it)
			os.Stdout.Write([]byte("\n"))
		}
		return
	}

	type logRow struct {
		Time     string         `json:"time"`
		Instance string         `json:"instance"`
		Level    string         `json:"level"`
		Event    string         `json:"event"`
		Task     string         `json:"task"`
		Message  string         `json:"message"`
		Code     string         `json:"code"`
		Data     string         `json:"data"`
		Meta     map[string]any `json:"meta"`
	}
	// A single page, bounded by --limit (the server caps it at 1000). Unlike
	// instances/channels we don't follow next_cursor here: --limit is a deliberate
	// cap on how much trail to print.
	var resp page[logRow]
	if err := callGet(u, &resp); err != nil {
		fatal("%v", err)
	}
	if len(resp.Items) == 0 {
		return
	}

	// Render via the shared logview column layout — the same one the server console
	// uses, so a row reads identically in either place. The CLI adds a header (it has
	// the whole page) and shows the ID column only with --tree (a single-instance view
	// repeats one id). The data body is shown only in detail mode.
	fmt.Println(logview.Header(*treeFlag))
	for _, l := range resp.Items {
		t, _ := parseTime(l.Time)
		rec := logview.Record{Event: l.Event, Task: l.Task, Msg: l.Message, Code: l.Code, Data: l.Data, Meta: l.Meta}
		idTag := ""
		if *treeFlag {
			idTag = shortID(l.Instance)
		}
		fmt.Println(logview.RenderEvent(t, l.Level, idTag, l.Event, l.Task, rec.Detail(mode), *treeFlag))
	}
}

// shortID returns a compact, distinguishing tag for an instance id in tree-log
// display. It uses the id's tail, not its head: instance ids are UUIDv7s whose
// leading bits are a millisecond timestamp, so a parent and a child spawned in the
// same millisecond share a long prefix — the random tail is what tells them apart.
func shortID(id string) string {
	if len(id) > 6 {
		return id[len(id)-6:]
	}
	return id
}

// ── input assembly (gentctl run) ────────────────────────────────────────────────

// buildInput assembles the process input from --input and any --set overrides. The
// --input value (a JSON/YAML literal, @file, or - for stdin) is the base; each
// --set key=value is then applied on top (requiring the base to be an object).
// Returns (value, present, error): present is false when neither flag was given,
// so the input is omitted entirely for processes that take none.
func buildInput(inputFlag string, sets []string) (any, bool, error) {
	var base any
	present := false
	if inputFlag != "" {
		data, err := readInputSource(inputFlag)
		if err != nil {
			return nil, false, err
		}
		v, err := parseRelaxed(data)
		if err != nil {
			return nil, false, fmt.Errorf("parse --input: %w", err)
		}
		base = v
		present = true
	}
	if len(sets) > 0 {
		m, ok := base.(map[string]any)
		if base == nil {
			m, ok = map[string]any{}, true
		}
		if !ok {
			return nil, false, fmt.Errorf("--set needs the input to be an object, but --input is %T", base)
		}
		for _, s := range sets {
			if err := applySet(m, s); err != nil {
				return nil, false, err
			}
		}
		base, present = m, true
	}
	return base, present, nil
}

// readInputSource resolves an --input value: "-" reads stdin, "@path" reads a file,
// anything else is the literal string.
func readInputSource(val string) ([]byte, error) {
	switch {
	case val == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(val, "@"):
		return os.ReadFile(val[1:])
	default:
		return []byte(val), nil
	}
}

// parseRelaxed parses data as YAML — a superset of JSON, so strict JSON works while
// also allowing the shell-friendly relaxed forms (unquoted keys, single quotes,
// trailing commas), e.g. {name: Sam, count: 3}. The result is round-tripped through
// JSON so the value contains only JSON-native types.
func parseRelaxed(data []byte) (any, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	jsonBytes, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(jsonBytes, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// applySet applies one "key=value" (or "a.b.c=value") override onto m, inferring
// the value's type and creating nested objects for dotted keys.
func applySet(m map[string]any, kv string) error {
	eq := strings.IndexByte(kv, '=')
	if eq < 0 {
		return fmt.Errorf("--set %q must be key=value", kv)
	}
	key, val := kv[:eq], kv[eq+1:]
	if key == "" {
		return fmt.Errorf("--set %q has an empty key", kv)
	}
	return setPath(m, strings.Split(key, "."), inferScalar(val))
}

// setPath walks/creates the nested objects named by path and sets the final key.
func setPath(m map[string]any, path []string, val any) error {
	for i := 0; i < len(path)-1; i++ {
		child, ok := m[path[i]]
		if !ok {
			next := map[string]any{}
			m[path[i]], m = next, next
			continue
		}
		next, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("--set: %q is already set to a non-object", strings.Join(path[:i+1], "."))
		}
		m = next
	}
	m[path[len(path)-1]] = val
	return nil
}

// inferScalar maps a --set value string to a JSON-native scalar: true/false/null,
// then integer, then float, else the string unchanged. Use --input for values that
// must stay strings (e.g. "007") or for arrays / deep structures.
func inferScalar(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// inputValidationError extracts the detail of a server-side input-schema rejection
// ("input validation: <detail>") so run can present it as its own message.
func inputValidationError(err error) (string, bool) {
	const marker = "input validation: "
	s := err.Error()
	if i := strings.Index(s, marker); i >= 0 {
		return s[i+len(marker):], true
	}
	return "", false
}

// ── time formatting ─────────────────────────────────────────────────────────────

// parseTime parses an RFC3339(/Nano) timestamp and converts it to local time.
func parseTime(rfc string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, rfc); err == nil {
			return t.Local(), true
		}
	}
	return time.Time{}, false
}

// shortTime renders a timestamp compactly for list columns: a relative age for
// recent times ("just now", "5m ago", "3h ago", "2d ago"), or a short absolute
// "YY-MM-DD HH:MM" beyond a week. Unparseable input is returned unchanged.
func shortTime(rfc string) string {
	t, ok := parseTime(rfc)
	if !ok {
		return rfc
	}
	return relAge(t)
}

func relAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0, d >= 7*24*time.Hour:
		return t.Format("06-01-02 15:04")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// longTime renders a full local timestamp with its relative age, for detail views:
// "2006-01-02 15:04:05  (5m ago)".
func longTime(rfc string) string {
	t, ok := parseTime(rfc)
	if !ok {
		return rfc
	}
	return fmt.Sprintf("%s  (%s)", t.Format("2006-01-02 15:04:05"), relAge(t))
}


func runCancelCmd(server string, args []string) {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fatal("usage: gentctl cancel <instance-id>")
	}
	id := fs.Arg(0)

	if err := call(*serverFlag+"/instances/"+url.PathEscape(id)+"/cancel", http.MethodPost, nil, nil); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("cancelled: %s\n", id)
}

func runRetryCmd(server string, args []string) {
	fs := flag.NewFlagSet("retry", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	forceFlag := fs.Bool("force", false, "override only_once retry protection")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fatal("usage: gentctl retry [--force] <instance-id>")
	}
	id := fs.Arg(0)

	u := *serverFlag + "/instances/" + url.PathEscape(id) + "/retry"
	if *forceFlag {
		u += "?force=true"
	}
	if err := call(u, http.MethodPost, nil, nil); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("retried: %s\n", id)
}

// callGet sends a GET request with no body and decodes the response into out.
func callGet(url string, out any) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &errResp); err != nil {
			return fmt.Errorf("server error (status %d)", resp.StatusCode)
		}
		return fmt.Errorf("server: %s", errResp.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// page is the {items, page:{...}} envelope every list endpoint now returns.
type page[T any] struct {
	Items []T `json:"items"`
	Page  struct {
		After string `json:"after"`
	} `json:"page"`
}

// listAll fetches every page of a list endpoint, following page.after until it is
// absent (set only while more rows remain), and returns the concatenated items.
// base is the request URL without an after cursor (it may already carry other
// query params).
func listAll[T any](base string) ([]T, error) {
	var all []T
	after := ""
	for {
		u := base
		if after != "" {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + "after=" + url.QueryEscape(after)
		}
		var p page[T]
		if err := callGet(u, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Items...)
		if p.Page.After == "" {
			return all, nil
		}
		after = p.Page.After
	}
}

// call sends body as JSON to url and decodes the response into out.
func call(url, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &errResp); err != nil {
			return fmt.Errorf("server error (status %d)", resp.StatusCode)
		}
		return fmt.Errorf("server: %s", errResp.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// loadDefs reads all files and returns a slice of raw definition objects.
func loadDefs(files []string) ([]any, error) {
	var all []any
	for _, path := range files {
		docs, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		all = append(all, docs...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no process definitions found in provided files")
	}
	return all, nil
}

func readFile(path string) ([]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".json" {
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
		if arr, ok := doc.([]any); ok {
			return arr, nil
		}
		return []any{doc}, nil
	}

	var docs []any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc any
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parse YAML: %w", err)
		}
		if doc == nil {
			continue
		}
		jsonBytes, err := json.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("convert YAML to JSON: %w", err)
		}
		var jsonDoc any
		json.Unmarshal(jsonBytes, &jsonDoc)
		docs = append(docs, jsonDoc)
	}
	return docs, nil
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type gentConfig struct {
	Server string `yaml:"server,omitempty"`
}

func configFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gent", "config.yaml"), nil
}

func loadConfig() gentConfig {
	path, err := configFilePath()
	if err != nil {
		return gentConfig{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return gentConfig{}
	}
	var cfg gentConfig
	yaml.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg gentConfig) error {
	path, err := configFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func runConfigCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: gentctl config get <key>")
		fmt.Fprintln(os.Stderr, "       gentctl config set <key> <value>")
		os.Exit(1)
	}
	sub, key := args[0], args[1]
	switch sub {
	case "get":
		cfg := loadConfig()
		switch key {
		case "server":
			if cfg.Server == "" {
				fmt.Println("(not set)")
			} else {
				fmt.Println(cfg.Server)
			}
		default:
			fatal("unknown config key %q", key)
		}
	case "set":
		if len(args) < 3 {
			fatal("usage: gentctl config set <key> <value>")
		}
		val := args[2]
		cfg := loadConfig()
		switch key {
		case "server":
			cfg.Server = val
		default:
			fatal("unknown config key %q", key)
		}
		if err := saveConfig(cfg); err != nil {
			fatal("save config: %v", err)
		}
		path, _ := configFilePath()
		fmt.Printf("set server = %s  (%s)\n", val, path)
	default:
		fatal("unknown config subcommand %q", sub)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  gentctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest] [--auto-update-parents]
  gentctl validate -f file.yaml [-f file2.yaml ...]
  gentctl run      <process> [--channel C | --version N] [--input <json|@file|->] [--set k=v ...]
  gentctl instances [--status <status>] [--sort updated|created] [--limit <n>] [--all]
  gentctl get      <instance-id> [--json]
  gentctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--tree] [--mode basic|detail|json] <instance-id>
  gentctl cancel   <instance-id>
  gentctl retry    [--force] <instance-id>
  gentctl channel list   <process>
  gentctl channel set    <process> <channel> <version>
  gentctl channel delete <process> <channel>
  gentctl promote  --from <channel> --to <channel> [--process <name>]
  gentctl status   --channel <channel>
  gentctl config   get <key>
  gentctl config   set <key> <value>

Flags:
  -f        definition file (YAML or JSON, multi-doc --- supported)
  --input   process input: a JSON/YAML literal, @file, or - for stdin
  --set     input field key=value (repeatable; dotted keys nest, values type-inferred)
  --server  gent server URL (overrides $GENT_SERVER and config file)

Config keys:
  server    gent server base URL`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gentctl: "+format+"\n", args...)
	os.Exit(1)
}
