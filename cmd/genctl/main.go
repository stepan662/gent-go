// genctl is a command-line gateway to a running genroc server, inspired by kubectl.
// It reads process definition files (YAML or JSON, multi-document via ---) and
// forwards them to the server in a single API call.
//
// Usage:
//
//	genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest] [--auto-update-parents]
//	genctl validate -f file.yaml [-f file2.yaml ...]
//	genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
//	genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [-q]
//	genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]
//	genctl instances [--status <status>] [--sort updated|created] [--limit <n>] [--all]
//	genctl get      <instance-id> [--json]
//	genctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--recursive] [--mode basic|detail|json] <instance-id>
//	genctl cancel   <instance-id>
//	genctl retry    [--force] <instance-id>
//	genctl last
//
// get/logs/cancel/retry/signal require an instance id; pass @last for the most recently
// started instance (recorded by run). `genctl last` prints that id.
//
//	genctl channel list   <process>
//	genctl channel set    <process> <channel> <version>
//	genctl channel delete <process> <channel>
//	genctl promote  --from <channel> --to <channel> [--process <name>]
//	genctl status   --channel <channel>
//
// Environment:
//
//	GENROC_SERVER  base URL of the genroc server (default: http://localhost:8448)
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

	"genroc/internal/logview"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cfg := loadConfig()
	server := os.Getenv("GENROC_SERVER")
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
	case "resolve":
		runResolveCmd(server, args)
	case "signal":
		runSignalCmd(server, args)
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
	case "last":
		runLastCmd(args)
	case "config":
		runConfigCmd(args)
	default:
		fmt.Fprintf(os.Stderr, "genctl: unknown command %q\n", cmd)
		usage()
		os.Exit(1)
	}
}

func runApplyCmd(server string, args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	var files multiFlag
	fs.Var(&files, "f", "definition file (YAML or JSON); repeat for multiple files")
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	channelFlag := fs.String("channel", "latest", "channel to apply definitions to")
	autoUpdateFlag := fs.Bool("auto-update-parents", false, "auto-update parent processes on the same channel")
	fs.Parse(args)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: -f is required")
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
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	fs.Parse(args)

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "genctl: -f is required")
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
		fmt.Fprintln(os.Stderr, "Usage: genctl channel <list|set|delete> ...")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("channel", flag.ExitOnError)
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	fs.Parse(args[1:])
	rest := fs.Args()

	sub := args[0]
	switch sub {
	case "list":
		if len(rest) < 1 {
			fatal("usage: genctl channel list <process>")
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
			fatal("usage: genctl channel set <process> <channel> <version>")
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
			fatal("usage: genctl channel delete <process> <channel>")
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
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
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
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
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
// argument; flags follow it (e.g. `genctl run greeter --set name=Sam`). Input is
// assembled from --input (a JSON/YAML literal, or - for stdin) or -f (a file path),
// plus any number of --set key=value overrides; see buildInput.
func runRunCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: genctl run <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]")
	}
	process := args[0]

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	channelFlag := fs.String("channel", "", "resolve the version via this channel")
	versionFlag := fs.Int("version", 0, "pin an explicit process version")
	inputFlag := fs.String("input", "", "input as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read input from a file (path)")
	var sets multiFlag
	fs.Var(&sets, "set", "set an input field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "print only the new instance id, e.g. id=$(genctl run NAME -q)")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(args[1:])

	input, hasInput, err := buildInput(*inputFlag, *fileFlag, sets)
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
	// Record the id so a follow-up command can resolve @last (or a bare-id default)
	// without copy-pasting. Best-effort: an unwritable state dir must not fail run.
	if err := saveLastInstance(resp.ID); err != nil {
		fmt.Fprintf(os.Stderr, "genctl: warning: could not record last instance id: %v\n", err)
	}
	// -q prints just the id so it composes: id=$(genctl run NAME -q).
	if *quietFlag {
		fmt.Println(resp.ID)
		return
	}
	fmt.Printf("started: %s  %s@v%d  (%s)\n", resp.ID, resp.Process, resp.Version, resp.Status)
}

// runResolveCmd submits a result for a task parked on an external action, resuming its
// process. The task's resolve token (from the external-task queue, GET /external-tasks)
// is the first argument; the result payload is assembled from --result (a JSON/YAML
// literal, or - for stdin) or -f (a file path), plus any number of --set key=value
// overrides — exactly like run assembles its input.
func runResolveCmd(server string, args []string) {
	if len(args) == 0 {
		fatal("usage: genctl resolve <token> [--result <json|-> | -f file] [--set k=v ...] [-q]")
	}
	token := args[0]

	fs := flag.NewFlagSet("resolve", flag.ExitOnError)
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	resultFlag := fs.String("result", "", "result as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read result from a file (path)")
	var sets multiFlag
	fs.Var(&sets, "set", "set a result field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "on success print nothing (exit 0); by default prints a confirmation line")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	fs.Parse(args[1:])

	// A missing --result/-f/--set means an empty result: valid for a task with no
	// result_schema, and rejected by the server otherwise (surfaced below).
	result, _, err := buildInput(*resultFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{"token": token, "result": result}

	var resp struct {
		Resolved bool `json:"resolved"`
	}
	if err := call(*serverFlag+"/external-tasks/resolve", http.MethodPost, body, &resp); err != nil {
		// Surface a result-schema mismatch as a clear, dedicated message instead of the
		// generic "server: ..." wrapper (mirrors run's input-validation handling).
		if detail, ok := resultValidationError(err); ok {
			fatal("result is not valid for this task:\n  %s", detail)
		}
		fatal("%v", err)
	}
	if *quietFlag {
		return
	}
	fmt.Printf("resolved: %s\n", token)
}

// runSignalCmd delivers a result to a named external task of an instance, addressed by
// instance id (accepts @last) + --task, rather than by a queue token like resolve. If the
// task is armed now the signal resolves it immediately; otherwise the server buffers it
// FIFO until the task next arms. The result is assembled from --result (a JSON/YAML
// literal, or - for stdin) or -f (a file path), plus any --set key=value overrides.
func runSignalCmd(server string, args []string) {
	fs := flag.NewFlagSet("signal", flag.ExitOnError)
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	taskFlag := fs.String("task", "", "the external task id to signal")
	resultFlag := fs.String("result", "", "result as a JSON/YAML literal, or - for stdin")
	fileFlag := fs.String("f", "", "read result from a file (path)")
	var sets multiFlag
	fs.Var(&sets, "set", "set a result field: key=value (repeatable; dotted keys nest, values are type-inferred)")
	quietFlag := fs.Bool("quiet", false, "on success print nothing (exit 0); by default prints a confirmation line")
	fs.BoolVar(quietFlag, "q", false, "shorthand for --quiet")
	// The instance id is the sole positional (before or after flags); resolves @last.
	id := instanceIDAndFlags(fs, args)

	if *taskFlag == "" {
		fatal("usage: genctl signal <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]")
	}

	result, _, err := buildInput(*resultFlag, *fileFlag, sets)
	if err != nil {
		fatal("%v", err)
	}

	body := map[string]any{"task_id": *taskFlag, "result": result}

	var resp struct {
		Delivered bool `json:"delivered"`
		Buffered  bool `json:"buffered"`
	}
	if err := call(*serverFlag+"/instances/"+url.PathEscape(id)+"/signal", http.MethodPost, body, &resp); err != nil {
		// Surface a result-schema mismatch as a dedicated message (mirrors resolve/run).
		if detail, ok := resultValidationError(err); ok {
			fatal("result is not valid for task %q:\n  %s", *taskFlag, detail)
		}
		fatal("%v", err)
	}
	if *quietFlag {
		return
	}
	state := "delivered"
	if resp.Buffered {
		state = "buffered"
	}
	fmt.Printf("signaled: %s  task=%s  (%s)\n", id, *taskFlag, state)
}

// runGetCmd prints a single instance's details, including its full context. The
// instance id is the first argument; pass --json for the raw response.
func runGetCmd(server string, args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	jsonFlag := fs.Bool("json", false, "print the raw JSON response")
	resolveFlag := fs.Bool("resolve", false, "resolve externalized context values inline instead of {ref, size} references")
	id := instanceIDAndFlags(fs, args)

	u := *serverFlag + "/instances/" + url.PathEscape(id)
	if *resolveFlag {
		u += "?resolve=true"
	}
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
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
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
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	levelFlag := fs.String("level", "", "filter by level (debug, info, warn, error); empty = all")
	sinceFlag := fs.Int64("since", 0, "only logs at/after this unix-millis timestamp")
	limitFlag := fs.Int("limit", 200, "max entries to return")
	recursiveFlag := fs.Bool("recursive", false, "include the whole process subtree (root instance id)")
	resolveFlag := fs.Bool("resolve", false, "inline full externalized payloads instead of a preview + reference")
	modeFlag := fs.String("mode", "detail", "output: basic (no data body), detail (+ data), or json (one JSON object per line, untruncated)")
	id := instanceIDAndFlags(fs, args)
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
	if *recursiveFlag {
		q.Set("recursive", "true")
	}
	if *resolveFlag {
		q.Set("resolve", "true")
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

	type logDataRef struct {
		Ref  string `json:"ref"`
		Size int64  `json:"size"`
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
		DataRef  *logDataRef    `json:"data_ref"`
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
	// the whole page) and shows the ID column only with --recursive (a single-instance view
	// repeats one id). The data body is shown only in detail mode.
	fmt.Println(logview.Header(*recursiveFlag))
	for _, l := range resp.Items {
		t, _ := parseTime(l.Time)
		// An externalized payload comes back as a bare {ref, size} reference with no
		// inline body — show the reference itself in the body's place (rendered raw via
		// the leading "{"). Pass --resolve to fetch and inline the full value instead.
		data := l.Data
		if data == "" && l.DataRef != nil {
			if b, err := json.Marshal(l.DataRef); err == nil {
				data = string(b)
			}
		}
		rec := logview.Record{Event: l.Event, Task: l.Task, Msg: l.Message, Code: l.Code, Data: data, Meta: l.Meta}
		idTag := ""
		if *recursiveFlag {
			idTag = shortID(l.Instance)
		}
		fmt.Println(logview.RenderEvent(t, l.Level, idTag, l.Event, l.Task, rec.Detail(mode), *recursiveFlag))
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

// ── input assembly (genctl run) ────────────────────────────────────────────────

// buildInput assembles an input/result value from a base source and any --set
// overrides. The base comes from exactly one of: literal (a JSON/YAML literal passed to
// --input/--result, or "-" for stdin) or file (a path passed to -f). Each --set
// key=value is then applied on top (requiring the base to be an object). Returns
// (value, present, error): present is false when no source and no --set was given, so
// the value is omitted entirely for processes/tasks that take none.
func buildInput(literal, file string, sets []string) (any, bool, error) {
	base, present, err := readBase(literal, file)
	if err != nil {
		return nil, false, err
	}
	if len(sets) > 0 {
		m, ok := base.(map[string]any)
		if base == nil {
			m, ok = map[string]any{}, true
		}
		if !ok {
			return nil, false, fmt.Errorf("--set needs the base to be an object, but it is %T", base)
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

// readBase resolves the base value from the mutually-exclusive literal (a JSON/YAML
// literal, or "-" for stdin) and file (a path — bare, so the shell tab-completes it)
// sources. Returns present=false when neither is set.
func readBase(literal, file string) (any, bool, error) {
	if literal != "" && file != "" {
		return nil, false, fmt.Errorf("provide the value inline or with -f, not both")
	}
	var data []byte
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, false, err
		}
		data = b
	case literal == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, false, err
		}
		data = b
	case literal != "":
		data = []byte(literal)
	default:
		return nil, false, nil
	}
	v, err := parseRelaxed(data)
	if err != nil {
		return nil, false, fmt.Errorf("parse value: %w", err)
	}
	return v, true, nil
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
	return serverErrorDetail(err, "input validation: ")
}

// resultValidationError extracts the detail of a server-side external-task result
// rejection ("result validation: <detail>") so resolve can present it as its own message.
func resultValidationError(err error) (string, bool) {
	return serverErrorDetail(err, "result validation: ")
}

// serverErrorDetail returns the part of err's message after marker, if present.
func serverErrorDetail(err error, marker string) (string, bool) {
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
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	id := instanceIDAndFlags(fs, args)

	if err := call(*serverFlag+"/instances/"+url.PathEscape(id)+"/cancel", http.MethodPost, nil, nil); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("cancelled: %s\n", id)
}

func runRetryCmd(server string, args []string) {
	fs := flag.NewFlagSet("retry", flag.ExitOnError)
	serverFlag := fs.String("server", server, "genroc server base URL ($GENROC_SERVER)")
	forceFlag := fs.Bool("force", false, "override only_once retry protection")
	id := instanceIDAndFlags(fs, args)

	u := *serverFlag + "/instances/" + url.PathEscape(id) + "/retry"
	if *forceFlag {
		u += "?force=true"
	}
	if err := call(u, http.MethodPost, nil, nil); err != nil {
		fatal("%v", err)
	}
	fmt.Printf("retried: %s\n", id)
}

// runLastCmd prints the most recently started instance id (recorded by `run`), so
// it can be spliced into other commands, e.g. `genctl logs $(genctl last)`.
func runLastCmd(args []string) {
	fmt.Println(resolveInstanceID("@last"))
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

type genrocConfig struct {
	Server string `yaml:"server,omitempty"`
}

func configFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "genroc", "config.yaml"), nil
}

func loadConfig() genrocConfig {
	path, err := configFilePath()
	if err != nil {
		return genrocConfig{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return genrocConfig{}
	}
	var cfg genrocConfig
	yaml.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg genrocConfig) error {
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

// ── last-instance state (genctl run → @last) ───────────────────────────────────

// lastInstanceFilePath is where `run` records the most recently started instance
// id, kept beside the config so a follow-up command can resolve `@last` (or a bare
// default) without the caller copy-pasting it.
func lastInstanceFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "genroc", "last"), nil
}

// saveLastInstance records id as the most recently started instance. Best-effort:
// the caller treats a failure as non-fatal so `run` still succeeds.
func saveLastInstance(id string) error {
	path, err := lastInstanceFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(id+"\n"), 0600)
}

// loadLastInstance returns the most recently started instance id, or "" if none
// has been recorded yet.
func loadLastInstance() string {
	path, err := lastInstanceFilePath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resolveInstanceID maps an instance-id argument to a concrete id. The id must be
// given explicitly: the literal "@last" resolves to the most recently started
// instance (recorded by `run`), and any other non-empty value is returned
// unchanged. An empty argument is an error — a bare command never implies @last —
// as is "@last" when nothing has been started yet.
func resolveInstanceID(arg string) string {
	if arg == "" {
		fatal("an instance id is required — pass one explicitly, or @last for the most recently started instance")
	}
	if arg != "@last" {
		return arg
	}
	id := loadLastInstance()
	if id == "" {
		fatal("@last: no instance recorded yet — run `genctl run <process>` first")
	}
	return id
}

// instanceIDAndFlags parses an instance subcommand's args, where the instance id
// may sit before or after the flags. A leading non-flag token is taken as the id
// (so `get <id> --json` keeps working); otherwise a trailing positional is used (so
// `cancel --server X <id>` works too). The id must be given explicitly — a concrete
// id or "@last"; a missing one is an error (see resolveInstanceID).
func instanceIDAndFlags(fs *flag.FlagSet, args []string) string {
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	fs.Parse(args)
	if id == "" {
		id = fs.Arg(0)
	}
	return resolveInstanceID(id)
}

func runConfigCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: genctl config get <key>")
		fmt.Fprintln(os.Stderr, "       genctl config set <key> <value>")
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
			fatal("usage: genctl config set <key> <value>")
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
  genctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest] [--auto-update-parents]
  genctl validate -f file.yaml [-f file2.yaml ...]
  genctl run      <process> [--channel C | --version N] [--input <json|-> | -f file] [--set k=v ...] [-q]
  genctl resolve  <token> [--result <json|-> | -f file] [--set k=v ...] [-q]
  genctl signal   <instance-id> --task <task-id> [--result <json|-> | -f file] [--set k=v ...] [-q]
  genctl instances [--status <status>] [--sort updated|created] [--limit <n>] [--all]
  genctl get      <instance-id> [--json]
  genctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--recursive] [--mode basic|detail|json] <instance-id>
  genctl cancel   <instance-id>
  genctl retry    [--force] <instance-id>
  genctl last
  genctl channel list   <process>
  genctl channel set    <process> <channel> <version>
  genctl channel delete <process> <channel>
  genctl promote  --from <channel> --to <channel> [--process <name>]
  genctl status   --channel <channel>
  genctl config   get <key>
  genctl config   set <key> <value>

Flags:
  -f        apply: definition file(s), YAML or JSON, multi-doc --- (repeatable);
            run/resolve/signal: read the input/result from a file (path — tab-completes)
  --input   process input: a JSON/YAML literal, or - for stdin
  --result  external-task result (resolve/signal): a JSON/YAML literal, or - for stdin
  --task    the external task id to signal
  --set     input/result field key=value (repeatable; dotted keys nest, values type-inferred)
  --server  genroc server URL (overrides $GENROC_SERVER and config file)
  -q        with run, print only the new instance id (id=$(genctl run NAME -q));
            with resolve/signal, suppress the confirmation line

Instance id:
  get/logs/cancel/retry/signal require an instance id; pass @last for the most recently
  started instance (recorded by run), or run "genctl last" to print it.

External tasks:
  resolve takes a task's resolve token (the "<instance-id>.<nonce>" from the
  external-task queue, GET /external-tasks); signal addresses a task by instance id
  + --task and buffers the result if the task is not armed yet.

Config keys:
  server    genroc server base URL`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genctl: "+format+"\n", args...)
	os.Exit(1)
}
