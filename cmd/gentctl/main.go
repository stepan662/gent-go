// gentctl is a command-line gateway to a running gent server, inspired by kubectl.
// It reads process definition files (YAML or JSON, multi-document via ---) and
// forwards them to the server in a single API call.
//
// Usage:
//
//	gentctl apply    -f file.yaml [-f file2.yaml ...] [--channel latest] [--auto-update-parents]
//	gentctl validate -f file.yaml [-f file2.yaml ...]
//	gentctl channel list   <process>
//	gentctl channel set    <process> <channel> <version>
//	gentctl channel delete <process> <channel>
//	gentctl promote  --from <channel> --to <channel> [--process <name>]
//	gentctl status   --channel <channel>
//	gentctl instances [--status <status>]
//	gentctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--tree] <instance-id>
//	gentctl cancel   <instance-id>
//	gentctl retry    [--force] <instance-id>
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
		var resp []struct {
			Channel string `json:"channel"`
			Version int    `json:"version"`
		}
		listURL := *serverFlag + "/channels?name=" + url.QueryEscape(rest[0])
		if err := callGet(listURL, &resp); err != nil {
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
			StepID         string `json:"step_id"`
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
			fmt.Printf("  step %q: %s baked@v%d, channel@v%d\n",
				ref.StepID, ref.ChildName, ref.BakedVersion, ref.ChannelVersion)
		}
	}
	if allClean {
		fmt.Printf("channel %q is coherent\n", *channelFlag)
	}
}

func runInstancesCmd(server string, args []string) {
	fs := flag.NewFlagSet("instances", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	statusFlag := fs.String("status", "", "filter by status (running, completed, failing, failed, cancelling, cancelled)")
	fs.Parse(args)

	u := *serverFlag + "/instances"
	if *statusFlag != "" {
		u += "?status=" + url.QueryEscape(*statusFlag)
	}
	var resp []struct {
		ID      string `json:"id"`
		Process string `json:"process"`
		Version int    `json:"version"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	if err := callGet(u, &resp); err != nil {
		fatal("%v", err)
	}
	for _, inst := range resp {
		line := fmt.Sprintf("%s  %-10s  %s@v%d", inst.ID, inst.Status, inst.Process, inst.Version)
		if inst.Error != "" {
			errMsg := inst.Error
			if len(errMsg) > 60 {
				errMsg = errMsg[:57] + "..."
			}
			line += "  " + errMsg
		}
		fmt.Println(line)
	}
}

func runLogsCmd(server string, args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	serverFlag := fs.String("server", server, "gent server base URL ($GENT_SERVER)")
	levelFlag := fs.String("level", "", "filter by level (debug, info, warn, error)")
	sinceFlag := fs.Int64("since", 0, "only logs at/after this unix-millis timestamp")
	limitFlag := fs.Int("limit", 200, "max entries to return")
	treeFlag := fs.Bool("tree", false, "include the whole process subtree (root instance id)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fatal("usage: gentctl logs [--level L] [--since MS] [--limit N] [--tree] <instance-id>")
	}
	id := fs.Arg(0)

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

	var resp []struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		Event   string `json:"event"`
		Step    string `json:"step"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := callGet(u, &resp); err != nil {
		fatal("%v", err)
	}
	for _, l := range resp {
		line := fmt.Sprintf("%s  %-5s  %-24s", l.Time, strings.ToUpper(l.Level), l.Event)
		if l.Step != "" {
			line += "  step=" + l.Step
		}
		if l.Code != "" {
			line += "  code=" + l.Code
		}
		if l.Message != "" {
			line += "  " + l.Message
		}
		fmt.Println(line)
	}
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
  gentctl channel list   <process>
  gentctl channel set    <process> <channel> <version>
  gentctl channel delete <process> <channel>
  gentctl promote  --from <channel> --to <channel> [--process <name>]
  gentctl status   --channel <channel>
  gentctl instances [--status <status>]
  gentctl logs     [--level <level>] [--since <ms>] [--limit <n>] [--tree] <instance-id>
  gentctl cancel   <instance-id>
  gentctl retry    [--force] <instance-id>
  gentctl config   get <key>
  gentctl config   set <key> <value>

Flags:
  -f        definition file (YAML or JSON, multi-doc --- supported)
  --server  gent server URL (overrides $GENT_SERVER and config file)

Config keys:
  server    gent server base URL`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gentctl: "+format+"\n", args...)
	os.Exit(1)
}
