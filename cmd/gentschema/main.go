// gentschema reads a process definition JSON file and writes a single JSON file
// containing normalised JSON Schemas for the process input and every task output.
//
// Usage:
//
//	gentschema -i definition.json [-o out.json]
//
// If -i is omitted, the definition is read from stdin.
// If -o is omitted, the result is written to stdout.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"gent/internal/model"
)

// TaskSchemas holds the schemas associated with a single task step.
type TaskSchemas struct {
	Input   map[string]any `json:"input,omitempty"`
	Output  map[string]any `json:"output,omitempty"`
	Context map[string]any `json:"context,omitempty"`
}

// SchemaFile is the top-level output written to the JSON file.
type SchemaFile struct {
	Process      string                 `json:"process"`
	Version      int                    `json:"version"`
	ProcessInput map[string]any         `json:"process_input,omitempty"`
	Tasks        map[string]TaskSchemas `json:"tasks,omitempty"`
}

func main() {
	in := flag.String("i", "", `input definition file (omit or "-" to read from stdin)`)
	out := flag.String("o", "-", `output file path (omit or "-" for stdout)`)
	flag.Parse()

	var src io.Reader = os.Stdin
	if *in != "" && *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			fatal("open %s: %v", *in, err)
		}
		defer f.Close()
		src = f
	}

	var def model.ProcessDefinition
	if err := json.NewDecoder(src).Decode(&def); err != nil {
		fatal("decode definition: %v", err)
	}

	result, err := Generate(&def)
	if err != nil {
		fatal("generate schemas: %v", err)
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal("marshal output: %v", err)
	}
	data = append(data, '\n')

	if *out == "-" {
		os.Stdout.Write(data)
		return
	}

	if err := os.WriteFile(*out, data, 0644); err != nil {
		fatal("write %s: %v", *out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *out, len(data))
}

// Generate normalises all schemas in def and builds the SchemaFile output.
func Generate(def *model.ProcessDefinition) (SchemaFile, error) {
	if err := def.Normalize(); err != nil {
		return SchemaFile{}, err
	}
	result := SchemaFile{Process: def.Name, Version: def.Version}
	if len(def.InputSchema) > 0 {
		result.ProcessInput = def.InputSchema
	}
	tasks := make(map[string]TaskSchemas)
	collectTaskSchemas(def.Steps, tasks)
	buildContexts(def.Steps, nil, tasks, def.InputSchema)
	if len(tasks) > 0 {
		result.Tasks = tasks
	}
	return result, nil
}

// collectTaskSchemas walks steps recursively and collects input/output schemas
// of every task step into out, keyed by the step ID.
func collectTaskSchemas(steps []*model.Step, out map[string]TaskSchemas) {
	for _, s := range steps {
		if s.Type == model.StepTypeTask && len(s.OutputSchema) > 0 {
			out[s.ID] = TaskSchemas{Output: s.OutputSchema}
		}
		collectTaskSchemas(s.Then, out)
		collectTaskSchemas(s.Else, out)
	}
}

// buildContexts walks the step tree in execution order, sets Context on each
// task entry in tasks, and returns the IDs of all tasks that could have run
// by the end of steps (used when merging conditional branches).
//
// accumulated holds the IDs of all tasks that could have run before the
// current position, regardless of whether they have an output schema.
func buildContexts(steps []*model.Step, accumulated []string, tasks map[string]TaskSchemas, processInput map[string]any) []string {
	for _, s := range steps {
		switch s.Type {
		case model.StepTypeTask:
			if ts, ok := tasks[s.ID]; ok {
				ts.Context = contextSchema(accumulated, tasks, processInput)
				tasks[s.ID] = ts
			}
			accumulated = append(accumulated, s.ID)
		case model.StepTypeConditional:
			thenAcc := buildContexts(s.Then, sliceCopy(accumulated), tasks, processInput)
			elseAcc := buildContexts(s.Else, sliceCopy(accumulated), tasks, processInput)
			accumulated = sliceUnion(thenAcc, elseAcc)
		}
	}
	return accumulated
}

// contextSchema builds a JSON Schema describing the context available to a task:
// the process input (if any) and the outputs of all preceding tasks (if any).
func contextSchema(preceding []string, tasks map[string]TaskSchemas, processInput map[string]any) map[string]any {
	props := make(map[string]any)
	if len(processInput) > 0 {
		props["input"] = processInput
	}
	outputProps := make(map[string]any)
	for _, id := range preceding {
		if ts, ok := tasks[id]; ok && len(ts.Output) > 0 {
			outputProps[id] = ts.Output
		}
	}
	outputs := map[string]any{"type": "object"}
	if len(outputProps) > 0 {
		outputs["properties"] = outputProps
	}
	props["outputs"] = outputs
	return map[string]any{"type": "object", "properties": props}
}

func sliceCopy(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func sliceUnion(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	result := append([]string{}, a...)
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			result = append(result, v)
			seen[v] = true
		}
	}
	return result
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gentschema: "+format+"\n", args...)
	os.Exit(1)
}
