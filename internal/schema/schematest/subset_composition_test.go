package schematest

import "testing"

func TestIsSubset_composition_sub(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"anyOf [integer,number] ⊆ number (all variants fit)",
			`{"anyOf":[{"type":"integer"},{"type":"number"}]}`,
			`{"type":"number"}`,
			true,
		},
		{
			"anyOf [integer,string] ⊄ number (string doesn't fit)",
			`{"anyOf":[{"type":"integer"},{"type":"string"}]}`,
			`{"type":"number"}`,
			false,
		},
		{
			"oneOf [integer,string] ⊆ anyOf [integer,string,boolean]",
			`{"oneOf":[{"type":"integer"},{"type":"string"}]}`,
			`{"anyOf":[{"type":"integer"},{"type":"string"},{"type":"boolean"}]}`,
			true,
		},
		{
			"oneOf [integer,string] ⊄ oneOf [integer,boolean]",
			`{"oneOf":[{"type":"integer"},{"type":"string"}]}`,
			`{"oneOf":[{"type":"integer"},{"type":"boolean"}]}`,
			false,
		},
		{
			"allOf [integer, minimum:5] ⊆ number (integer constraint covers type)",
			`{"allOf":[{"type":"integer"},{"minimum":5}]}`,
			`{"type":"number"}`,
			true,
		},
		{
			"allOf [string, minLength:3] ⊄ integer",
			`{"allOf":[{"type":"string"},{"minLength":3}]}`,
			`{"type":"integer"}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_composition_super(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"string ⊆ anyOf [string,integer]",
			`{"type":"string"}`,
			`{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
			true,
		},
		{
			"boolean ⊄ anyOf [string,integer]",
			`{"type":"boolean"}`,
			`{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
			false,
		},
		{
			"integer ⊆ anyOf [string,number] (widening)",
			`{"type":"integer"}`,
			`{"anyOf":[{"type":"string"},{"type":"number"}]}`,
			true,
		},
		{
			"string ⊆ oneOf [string,integer]",
			`{"type":"string"}`,
			`{"oneOf":[{"type":"string"},{"type":"integer"}]}`,
			true,
		},
		{
			"integer ⊆ allOf [number, minimum:0]",
			`{"type":"integer","minimum":5}`,
			`{"allOf":[{"type":"number"},{"minimum":0}]}`,
			true,
		},
		{
			"integer ⊄ allOf [number, minimum:10] when sub minimum is less",
			`{"type":"integer","minimum":5}`,
			`{"allOf":[{"type":"number"},{"minimum":10}]}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}
