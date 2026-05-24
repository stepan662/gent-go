package main_test

import (
	"encoding/json"
	"testing"

	"gent/internal/gentschema"
	"gent/internal/model"
)

func runGenerate(t *testing.T, defJSON string) gentschema.SchemaFile {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	out, err := gentschema.Generate(&def)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return out
}

func runGenerateErr(t *testing.T, defJSON string) error {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(defJSON), &def); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	_, err := gentschema.Generate(&def)
	return err
}

func schemaKeys(out gentschema.SchemaFile) []string {
	keys := make([]string, 0, len(out.Tasks))
	for k := range out.Tasks {
		keys = append(keys, k)
	}
	return keys
}

func defKeys(out gentschema.SchemaFile) []string {
	keys := make([]string, 0, len(out.Defs))
	for k := range out.Defs {
		keys = append(keys, k)
	}
	return keys
}

func assertJSON(t *testing.T, got any, wantJSON string) {
	t.Helper()
	ga, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	var wantParsed any
	if err := json.Unmarshal([]byte(wantJSON), &wantParsed); err != nil {
		t.Fatalf("wantJSON is not valid JSON: %v\n%s", err, wantJSON)
	}
	gb, _ := json.MarshalIndent(wantParsed, "", "  ")
	if string(ga) != string(gb) {
		t.Errorf("schema mismatch:\n got:  %s\n want: %s", ga, gb)
	}
}
