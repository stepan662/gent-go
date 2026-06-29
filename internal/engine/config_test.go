package engine

import (
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

