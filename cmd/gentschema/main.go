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

	"gent/internal/gentschema"
	"gent/internal/model"
)

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

	result, err := gentschema.Generate(&def)
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gentschema: "+format+"\n", args...)
	os.Exit(1)
}
