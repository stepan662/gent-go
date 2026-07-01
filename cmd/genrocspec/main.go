package main

import (
	"flag"
	"fmt"
	"os"

	"genroc/internal/api"
)

func main() {
	out := flag.String("o", "openapi.json", `output file path (use "-" for stdout)`)
	flag.Parse()

	spec := api.Spec()

	if *out == "-" {
		os.Stdout.Write(spec)
		return
	}

	if err := os.WriteFile(*out, spec, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *out, len(spec))
}
