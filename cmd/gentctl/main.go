// gentctl is a command-line gateway to a running gent server, inspired by kubectl.
// It reads process definition files (YAML or JSON, multi-document via ---) and
// forwards them to the server in a single API call.
//
// Usage:
//
//	gentctl apply    -f file.yaml [-f file2.yaml ...]
//	gentctl validate -f file.yaml [-f file2.yaml ...]
//
// Environment:
//
//	GENT_SERVER  base URL of the gent server (default: http://localhost:8080)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	server := os.Getenv("GENT_SERVER")
	if server == "" {
		server = "http://localhost:8080"
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
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

	switch cmd {
	case "apply":
		runApply(*serverFlag, defs)
	case "validate":
		runValidate(*serverFlag, defs)
	default:
		fmt.Fprintf(os.Stderr, "gentctl: unknown command %q\n", cmd)
		usage()
		os.Exit(1)
	}
}

func runApply(server string, defs []any) {
	var resp []struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
		Saved   bool   `json:"saved"`
	}
	if err := call(server+"/definitions/batch", http.MethodPut, defs, &resp); err != nil {
		fatal("%v", err)
	}
	for _, r := range resp {
		fmt.Printf("saved: %s@v%d\n", r.Name, r.Version)
	}
}

func runValidate(server string, defs []any) {
	var resp json.RawMessage
	if err := call(server+"/definitions/validate", http.MethodPost, defs, &resp); err != nil {
		fatal("%v", err)
	}
	var buf bytes.Buffer
	json.Indent(&buf, resp, "", "  ")
	os.Stdout.Write(buf.Bytes())
	os.Stdout.Write([]byte("\n"))
}

// call sends body as JSON to url and decodes the response into out.
// The HTTP API returns the data directly on success (2xx) and {"error":"..."} on failure (4xx).
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
// Each file may contain one or more YAML documents separated by ---.
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
		// Accept either a single object or an array.
		if arr, ok := doc.([]any); ok {
			return arr, nil
		}
		return []any{doc}, nil
	}

	// YAML: support multi-document streams.
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
		// yaml.v3 decodes into map[string]any compatible with JSON after round-trip.
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

// multiFlag is a flag.Value that accumulates repeated -f values.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  gentctl apply    -f file.yaml [-f file2.yaml ...]
  gentctl validate -f file.yaml [-f file2.yaml ...]

Flags:
  -f        definition file (YAML or JSON, multi-doc --- supported)
  --server  gent server URL (default: $GENT_SERVER or http://localhost:8080)`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gentctl: "+format+"\n", args...)
	os.Exit(1)
}
