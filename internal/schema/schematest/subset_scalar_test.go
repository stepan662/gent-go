package schematest

import "testing"

func TestIsSubset_scalar_constraints(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		// numeric bounds
		{
			"sub minimum ≥ super minimum",
			`{"type":"integer","minimum":5}`,
			`{"type":"integer","minimum":3}`,
			true,
		},
		{
			"sub minimum < super minimum",
			`{"type":"integer","minimum":1}`,
			`{"type":"integer","minimum":3}`,
			false,
		},
		{
			"sub has no minimum, super requires one",
			`{"type":"integer"}`,
			`{"type":"integer","minimum":3}`,
			false,
		},
		{
			"sub maximum ≤ super maximum",
			`{"type":"integer","maximum":10}`,
			`{"type":"integer","maximum":20}`,
			true,
		},
		{
			"sub maximum > super maximum",
			`{"type":"integer","maximum":25}`,
			`{"type":"integer","maximum":20}`,
			false,
		},
		{
			"sub has no maximum, super requires one",
			`{"type":"integer"}`,
			`{"type":"integer","maximum":20}`,
			false,
		},
		{
			"sub minimum equal to super minimum",
			`{"type":"number","minimum":5}`,
			`{"type":"number","minimum":5}`,
			true,
		},
		// string length
		{
			"sub minLength ≥ super minLength",
			`{"type":"string","minLength":5}`,
			`{"type":"string","minLength":3}`,
			true,
		},
		{
			"sub minLength < super minLength",
			`{"type":"string","minLength":1}`,
			`{"type":"string","minLength":3}`,
			false,
		},
		{
			"sub maxLength ≤ super maxLength",
			`{"type":"string","maxLength":5}`,
			`{"type":"string","maxLength":10}`,
			true,
		},
		{
			"sub maxLength > super maxLength",
			`{"type":"string","maxLength":15}`,
			`{"type":"string","maxLength":10}`,
			false,
		},
		{
			"sub has no maxLength, super requires one",
			`{"type":"string"}`,
			`{"type":"string","maxLength":10}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_enum(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"sub enum ⊆ super enum",
			`{"type":"string","enum":["a","b"]}`,
			`{"type":"string","enum":["a","b","c"]}`,
			true,
		},
		{
			"sub enum equals super enum",
			`{"type":"string","enum":["a","b"]}`,
			`{"type":"string","enum":["a","b"]}`,
			true,
		},
		{
			"sub enum has value not in super",
			`{"type":"string","enum":["a","b","d"]}`,
			`{"type":"string","enum":["a","b","c"]}`,
			false,
		},
		{
			"sub unconstrained ⊄ super enum",
			`{"type":"string"}`,
			`{"type":"string","enum":["a","b"]}`,
			false,
		},
		{
			"integer enum ⊆ number enum",
			`{"enum":[1,2]}`,
			`{"enum":[1,2,3]}`,
			true,
		},
		{
			"mixed enum with value outside super",
			`{"enum":["a",1]}`,
			`{"enum":["a",1,2]}`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}
