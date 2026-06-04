package schematest

import "testing"

func TestIsSubset_object_required(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"sub requires superset of super's required",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a","b"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			true,
		},
		{
			"sub missing a required field from super",
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a","b"]}`,
			false,
		},
		{
			"sub has exact required match",
			`{"type":"object","required":["x","y"]}`,
			`{"type":"object","required":["x","y"]}`,
			true,
		},
		{
			"sub has no required, super has required",
			`{"type":"object","properties":{"a":{"type":"string"}}}`,
			`{"type":"object","required":["a"]}`,
			false,
		},
		{
			"super has no required, sub has required",
			`{"type":"object","required":["a"]}`,
			`{"type":"object"}`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_object_properties(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"compatible property types",
			`{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`,
			true,
		},
		{
			"incompatible property type (super is narrower)",
			`{"type":"object","properties":{"a":{"type":"number"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]}`,
			false,
		},
		{
			"sub has extra property not in super (no additionalProperties restriction)",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			true,
		},
		{
			"super property not in sub (sub doesn't constrain it — allowed)",
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`,
			true,
		},
		{
			"nested object property compatibility",
			`{"type":"object","properties":{"inner":{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}},"required":["inner"]}`,
			`{"type":"object","properties":{"inner":{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}},"required":["inner"]}`,
			true,
		},
		{
			"nested object property incompatibility",
			`{"type":"object","properties":{"inner":{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}},"required":["inner"]}`,
			`{"type":"object","properties":{"inner":{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}},"required":["inner"]}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_additionalProperties(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"sub has no extra props, super forbids additional",
			`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`,
			true,
		},
		{
			"sub has extra prop, super forbids additional",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`,
			false,
		},
		{
			"sub extra prop compatible with super additionalProperties schema",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"integer"}}`,
			true,
		},
		{
			"sub extra prop incompatible with super additionalProperties schema",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"integer"}}`,
			false,
		},
		{
			"sub extra prop is integer, super additionalProperties allows number",
			`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"]}`,
			`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"number"}}`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}
