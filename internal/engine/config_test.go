package engine

import (
	"strings"
	"testing"
)

// TestEvalConfigNamespace verifies the resolved config map is reachable in
// expressions under the "config" namespace.
func TestEvalConfigNamespace(t *testing.T) {
	ctx := map[string]any{"input": nil, "outputs": map[string]any{}}
	config := map[string]any{"flag": true, "url": "http://x", "port": int64(8080)}

	cases := []struct {
		expr string
		want bool
	}{
		{"config.flag == true", true},
		{`config.url == "http://x"`, true},
		{"config.port == 8080", true},
		{"config.missing == null", true},
	}
	for _, tc := range cases {
		got, err := evalBool(tc.expr, ctx, config, nil)
		if err != nil {
			t.Fatalf("evalBool(%q): %v", tc.expr, err)
		}
		if got != tc.want {
			t.Errorf("evalBool(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

// TestEvalNilConfig ensures a nil config does not break expression evaluation:
// referencing config.* yields nil rather than erroring.
func TestEvalNilConfig(t *testing.T) {
	ctx := map[string]any{"input": nil, "outputs": map[string]any{}}
	got, err := evalBool("config.anything == null", ctx, nil, nil)
	if err != nil {
		t.Fatalf("evalBool with nil config: %v", err)
	}
	if !got {
		t.Errorf("config.anything should be nil when config is nil")
	}
}

func TestRedactSecrets(t *testing.T) {
	secrets := []string{"s3cr3t", "topsecret"}
	in := `{"token":"s3cr3t","note":"topsecret value","ok":"public"}`
	got := redactSecrets(in, secrets)
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "topsecret") {
		t.Errorf("redactSecrets left a secret value: %s", got)
	}
	if !strings.Contains(got, "public") {
		t.Errorf("redactSecrets removed a non-secret value: %s", got)
	}
	if redactSecrets("abc", []string{""}) != "abc" {
		t.Error("an empty secret value must be a no-op, not redact everything")
	}
	if redactSecrets("nothing here", nil) != "nothing here" {
		t.Error("no secrets must leave the string unchanged")
	}
}
