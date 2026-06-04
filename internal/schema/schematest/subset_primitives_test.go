package schematest

import "testing"

func TestIsSubset_primitives(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{"string ⊆ string", `{"type":"string"}`, `{"type":"string"}`, true},
		{"integer ⊆ integer", `{"type":"integer"}`, `{"type":"integer"}`, true},
		{"number ⊆ number", `{"type":"number"}`, `{"type":"number"}`, true},
		{"boolean ⊆ boolean", `{"type":"boolean"}`, `{"type":"boolean"}`, true},
		{"null ⊆ null", `{"type":"null"}`, `{"type":"null"}`, true},
		{"integer ⊆ number (widening)", `{"type":"integer"}`, `{"type":"number"}`, true},
		{"number ⊄ integer", `{"type":"number"}`, `{"type":"integer"}`, false},
		{"string ⊄ integer", `{"type":"string"}`, `{"type":"integer"}`, false},
		{"string ⊄ boolean", `{"type":"string"}`, `{"type":"boolean"}`, false},
		{"integer ⊄ boolean", `{"type":"integer"}`, `{"type":"boolean"}`, false},
		{"boolean ⊄ string", `{"type":"boolean"}`, `{"type":"string"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_nullable(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"string ⊆ string|null",
			`{"type":"string"}`,
			`{"type":["string","null"]}`,
			true,
		},
		{
			"string|null ⊄ string",
			`{"type":["string","null"]}`,
			`{"type":"string"}`,
			false,
		},
		{
			"string|null ⊆ string|null",
			`{"type":["string","null"]}`,
			`{"type":["string","null"]}`,
			true,
		},
		{
			"integer ⊆ number|null (widening into nullable)",
			`{"type":"integer"}`,
			`{"type":["number","null"]}`,
			true,
		},
		{
			"null ⊆ string|null",
			`{"type":"null"}`,
			`{"type":["string","null"]}`,
			true,
		},
		{
			"null ⊄ string",
			`{"type":"null"}`,
			`{"type":"string"}`,
			false,
		},
		{
			"oneOf [string,null] ⊆ string|null",
			`{"oneOf":[{"type":"string"},{"type":"null"}]}`,
			`{"type":["string","null"]}`,
			true,
		},
		{
			"oneOf [string,null] ⊄ string",
			`{"oneOf":[{"type":"string"},{"type":"null"}]}`,
			`{"type":"string"}`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_empty(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{"string ⊆ {} (any)", `{"type":"string"}`, `{}`, true},
		{"object ⊆ {} (any)", `{"type":"object"}`, `{}`, true},
		{"{} ⊄ string", `{}`, `{"type":"string"}`, false},
		{"{} ⊆ {}", `{}`, `{}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}
