package schematest

import "testing"

func TestNormalize_items(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"type": "array",
		"items": {"$ref": "#/$defs/Item"},
		"$defs": {
			"Item":   {"type": "string"},
			"Unused": {"type": "integer"}
		}
	}`), `{
		"$defs": {"Item": {"type": "string"}},
		"items": {"$ref": "#/$defs/Item"},
		"type": "array"
	}`)
}

func TestNormalize_prefixItems(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"type": "array",
		"prefixItems": [
			{"$ref": "#/$defs/First"},
			{"type": "string"}
		],
		"$defs": {
			"First":  {"type": "integer"},
			"Unused": {"type": "boolean"}
		}
	}`), `{
		"$defs": {"First": {"type": "integer"}},
		"prefixItems": [
			{"$ref": "#/$defs/First"},
			{"type": "string"}
		],
		"type": "array"
	}`)
}

func TestNormalize_oneOf(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"oneOf": [
			{"$ref": "#/$defs/A"},
			{"$ref": "#/$defs/B"}
		],
		"$defs": {
			"A":      {"type": "string"},
			"B":      {"type": "integer"},
			"Unused": {"type": "boolean"}
		}
	}`), `{
		"$defs": {
			"A": {"type": "string"},
			"B": {"type": "integer"}
		},
		"oneOf": [
			{"$ref": "#/$defs/A"},
			{"$ref": "#/$defs/B"}
		]
	}`)
}

func TestNormalize_anyOf(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"anyOf": [{"$ref": "#/$defs/A"}, {"type": "null"}],
		"$defs": {
			"A":      {"type": "string"},
			"Unused": {"type": "boolean"}
		}
	}`), `{
		"$defs": {"A": {"type": "string"}},
		"anyOf": [{"$ref": "#/$defs/A"}, {"type": "null"}]
	}`)
}

func TestNormalize_allOf(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"allOf": [{"$ref": "#/$defs/Base"}, {"type": "object"}],
		"$defs": {
			"Base":   {"type": "object"},
			"Unused": {"type": "boolean"}
		}
	}`), `{
		"$defs": {"Base": {"type": "object"}},
		"allOf": [{"$ref": "#/$defs/Base"}, {"type": "object"}]
	}`)
}

func TestNormalize_not(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"not": {"$ref": "#/$defs/Forbidden"},
		"$defs": {
			"Forbidden": {"type": "string"},
			"Unused":    {"type": "integer"}
		}
	}`), `{
		"$defs": {"Forbidden": {"type": "string"}},
		"not": {"$ref": "#/$defs/Forbidden"}
	}`)
}

func TestNormalize_additionalProperties(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"type": "object",
		"additionalProperties": {"$ref": "#/$defs/Value"},
		"$defs": {
			"Value":  {"type": "string"},
			"Unused": {"type": "integer"}
		}
	}`), `{
		"$defs": {"Value": {"type": "string"}},
		"additionalProperties": {"$ref": "#/$defs/Value"},
		"type": "object"
	}`)
}

func TestNormalize_ifThenElse(t *testing.T) {
	assertJSON(t, normalize(t, `{
		"if":   {"$ref": "#/$defs/Cond"},
		"then": {"$ref": "#/$defs/Yes"},
		"else": {"$ref": "#/$defs/No"},
		"$defs": {
			"Cond":   {"type": "string"},
			"Yes":    {"type": "integer"},
			"No":     {"type": "boolean"},
			"Unused": {"type": "null"}
		}
	}`), `{
		"$defs": {
			"Cond": {"type": "string"},
			"No":   {"type": "boolean"},
			"Yes":  {"type": "integer"}
		},
		"else": {"$ref": "#/$defs/No"},
		"if":   {"$ref": "#/$defs/Cond"},
		"then": {"$ref": "#/$defs/Yes"}
	}`)
}

func TestNormalize_rejectExternalRef(t *testing.T) {
	assertErr(t,
		`{"$ref": "https://example.com/schema"}`,
		`unsupported $ref "https://example.com/schema": must be "#/$defs/<name>" or "#<anchor>"`,
	)
}

func TestNormalize_rejectRelativePointer(t *testing.T) {
	assertErr(t,
		`{"$ref": "#/properties/foo"}`,
		`unsupported $ref "#/properties/foo": must be "#/$defs/<name>" or "#<anchor>"`,
	)
}

func TestNormalize_rejectUnknownAnchor(t *testing.T) {
	assertErr(t,
		`{"$ref": "#no-such-anchor", "$defs": {}}`,
		`unresolved $ref "#no-such-anchor": anchor "no-such-anchor" is not defined in the root resource`,
	)
}

func TestNormalize_rejectShortPathWithoutID(t *testing.T) {
	// "#/$defs/Item" must match a root-level definition exactly.
	// Without a $id boundary, short-name suffix matching is not applied.
	assertErr(t,
		`{
			"properties": {"x": {"$ref": "#/$defs/Item"}},
			"$defs": {
				"Order": {
					"$defs": {
						"Item": {"type": "string"}
					}
				}
			}
		}`,
		`unresolved $ref "#/$defs/Item": no matching definition`,
	)
}

func TestNormalize_shortPathWithIDScope(t *testing.T) {
	// Inside a $id sub-resource "#/$defs/Item" resolves relative to that resource.
	out := normalize(t, `{
		"properties": {"order": {"$ref": "#/$defs/Order"}},
		"$defs": {
			"Order": {
				"$id": "urn:order",
				"type": "object",
				"properties": {"item": {"$ref": "#/$defs/Item"}},
				"$defs": {
					"Item": {"type": "string"}
				}
			}
		}
	}`)
	assertJSON(t, out, `{
		"$defs": {
			"Item":  {"type": "string"},
			"Order": {
				"type": "object",
				"properties": {"item": {"$ref": "#/$defs/Item"}}
			}
		},
		"properties": {"order": {"$ref": "#/$defs/Order"}}
	}`)
}
