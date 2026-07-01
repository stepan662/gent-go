package schematest

import (
	"encoding/json"
	"testing"

	"genroc/internal/schema"
	"github.com/xeipuuv/gojsonschema"
)

func normalize(t *testing.T, in string) *schema.SchemaNode {
	t.Helper()
	var n schema.SchemaNode
	if err := json.Unmarshal([]byte(in), &n); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	out, err := schema.Normalize(&n)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return out
}

func assertParseErr(t *testing.T, in string, wantMsg string) {
	t.Helper()
	var n schema.SchemaNode
	err := json.Unmarshal([]byte(in), &n)
	if err == nil {
		t.Fatalf("expected parse error %q, got nil", wantMsg)
	}
	if wantMsg != "" && err.Error() != wantMsg {
		t.Errorf("error message mismatch\ngot:  %q\nwant: %q", err.Error(), wantMsg)
	}
}

func assertErr(t *testing.T, in string, wantMsg string) {
	t.Helper()
	var n schema.SchemaNode
	if err := json.Unmarshal([]byte(in), &n); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	_, err := schema.Normalize(&n)
	if err == nil {
		t.Fatalf("expected error %q, got nil", wantMsg)
	}
	if err.Error() != wantMsg {
		t.Errorf("error message mismatch\ngot:  %q\nwant: %q", err.Error(), wantMsg)
	}
}

func assertJSON(t *testing.T, got *schema.SchemaNode, want string) {
	t.Helper()
	// Round-trip got through map[string]any so key order matches want (both sort alphabetically).
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	var gotParsed, wantParsed any
	if err := json.Unmarshal(gotRaw, &gotParsed); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantParsed); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	gotBytes, _ := json.MarshalIndent(gotParsed, "", "  ")
	wantBytes, _ := json.MarshalIndent(wantParsed, "", "  ")
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("output mismatch\ngot:\n%s\n\nwant:\n%s", gotBytes, wantBytes)
	}
}

func assertSemanticEquivalence(t *testing.T, src string, valid []any, invalid []any) {
	t.Helper()

	var original schema.SchemaNode
	var toNorm schema.SchemaNode
	if err := json.Unmarshal([]byte(src), &original); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	if err := json.Unmarshal([]byte(src), &toNorm); err != nil {
		t.Fatalf("parse schema copy: %v", err)
	}

	normalized, err := schema.Normalize(&toNorm)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	origSchema, err := gojsonschema.NewSchema(gojsonschema.NewGoLoader(&original))
	if err != nil {
		t.Fatalf("original schema is not a valid JSON Schema: %v", err)
	}
	normSchema, err := gojsonschema.NewSchema(gojsonschema.NewGoLoader(normalized))
	if err != nil {
		t.Fatalf("normalized schema is not a valid JSON Schema: %v", err)
	}

	check := func(data any, wantValid bool) {
		t.Helper()
		dl := gojsonschema.NewGoLoader(data)

		origRes, err := origSchema.Validate(dl)
		if err != nil {
			t.Fatalf("validate against original: %v", err)
		}
		normRes, err := normSchema.Validate(dl)
		if err != nil {
			t.Fatalf("validate against normalized: %v", err)
		}

		if origRes.Valid() != wantValid {
			t.Errorf("original schema: expected valid=%v for %#v (errors: %v)", wantValid, data, origRes.Errors())
		}
		if normRes.Valid() != origRes.Valid() {
			t.Errorf("normalization changed validity for %#v: original=%v normalized=%v\n  original errors:   %v\n  normalized errors: %v",
				data, origRes.Valid(), normRes.Valid(), origRes.Errors(), normRes.Errors())
		}
	}

	for _, d := range valid {
		check(d, true)
	}
	for _, d := range invalid {
		check(d, false)
	}
}
