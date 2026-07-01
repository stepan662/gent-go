package expressiontest

import (
	"testing"

	"genroc/internal/expression"
)

// secretContextJSON has secret scalars (config.api_key, self.result.token), a
// whole secret object (box), and non-secret siblings — to probe taint reliably.
const secretContextJSON = `{
	"type": "object",
	"properties": {
		"config": {
			"type": "object",
			"properties": {
				"api_key": { "type": "string", "secret": true },
				"url":     { "type": "string" }
			},
			"required": ["api_key", "url"]
		},
		"self": {
			"type": "object",
			"properties": {
				"result": {
					"type": "object",
					"properties": {
						"token": { "type": "string", "secret": true },
						"name":  { "type": "string" }
					},
					"required": ["token", "name"]
				}
			},
			"required": ["result"]
		},
		"box": {
			"type": "object",
			"secret": true,
			"properties": { "inner": { "type": "string" } },
			"required": ["inner"]
		},
		"input": { "type": "string" }
	},
	"required": ["config", "self", "box", "input"]
}`

func TestReferencesSecret(t *testing.T) {
	c := ctx(t, secretContextJSON)

	// Every one of these reads a secret somewhere — must taint regardless of what
	// the expression then does with it.
	secret := []string{
		`config.api_key`,
		`config.api_key + "x"`,
		`"Bearer " + config.api_key`,
		`config.api_key ?? "default"`,
		`config.url == "x" ? config.api_key : "fallback"`, // secret only in a branch
		`config.url == config.api_key`,                    // secret as a comparison operand
		`config.api_key == "" ? "a" : "b"`,                // secret only in the condition
		`self.result.token`,
		`box.inner`, // reading from inside a secret object
		`box`,       // the secret object itself
	}
	// None of these touch a secret.
	notSecret := []string{
		`config.url`,
		`input`,
		`"Bearer " + config.url`,
		`self.result.name`,
		`self.result`, // object whose sub-field is secret, but the object node itself is not
		`config.url == "x"`,
		`config.url ?? "default"`,
		`"static text"`,
		`42`,
	}

	for _, e := range secret {
		got, err := expression.ReferencesSecret(e, c)
		if err != nil {
			t.Fatalf("ReferencesSecret(%q): %v", e, err)
		}
		if !got {
			t.Errorf("ReferencesSecret(%q) = false, want true (secret leak!)", e)
		}
	}
	for _, e := range notSecret {
		got, err := expression.ReferencesSecret(e, c)
		if err != nil {
			t.Fatalf("ReferencesSecret(%q): %v", e, err)
		}
		if got {
			t.Errorf("ReferencesSecret(%q) = true, want false (over-redaction)", e)
		}
	}
}
