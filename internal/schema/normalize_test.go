package schema

import (
	"encoding/json"
	"testing"
)

// normalize parses in as JSON, runs Normalize, and returns the result.
func normalize(t *testing.T, in string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatalf("invalid input schema: %v", err)
	}
	out, err := Normalize(m)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return out
}

// assertErr parses in as JSON, runs Normalize, and checks that the error
// message equals wantMsg exactly.
func assertErr(t *testing.T, in string, wantMsg string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatalf("invalid input schema: %v", err)
	}
	_, err := Normalize(m)
	if err == nil {
		t.Fatalf("expected error %q, got nil", wantMsg)
	}
	if err.Error() != wantMsg {
		t.Errorf("error message mismatch\ngot:  %q\nwant: %q", err.Error(), wantMsg)
	}
}

// assertJSON marshals got and the parsed want to the same indented form and
// compares them. Key order and whitespace in want are irrelevant.
func assertJSON(t *testing.T, got map[string]any, want string) {
	t.Helper()
	gotBytes, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	var wantParsed any
	if err := json.Unmarshal([]byte(want), &wantParsed); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	wantBytes, err := json.MarshalIndent(wantParsed, "", "  ")
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("output mismatch\ngot:\n%s\n\nwant:\n%s", gotBytes, wantBytes)
	}
}
