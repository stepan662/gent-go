package expressiontest

import "testing"

// Ambiguous type contract — a union schema with an incompatible variant must be
// rejected by both Eval (runtime) and InferType (static). Both anyOf and oneOf
// are covered; the Eval oracle uses the incompatible value so all three layers fail.

var (
	// integerOrObjectSchema — arithmetic on integer|object: the object variant
	// causes a runtime type error, and InferType must reject it statically.
	integerOrObjectSchema = mustSchema(`{
		"properties": {
			"x": {"anyOf": [{"type": "integer"}, {"type": "object"}]}
		},
		"required": ["x"]
	}`)

	// stringOrIntegerSchema — string ops on string|integer: the integer variant
	// causes a runtime type error, and InferType must reject it statically.
	stringOrIntegerSchema = mustSchema(`{
		"properties": {
			"x": {"anyOf": [{"type": "string"}, {"type": "integer"}]}
		},
		"required": ["x"]
	}`)

	// oneOfIntegerObjectSchema — same semantics as integerOrObjectSchema but
	// using oneOf to verify both union keywords are handled identically.
	oneOfIntegerObjectSchema = mustSchema(`{
		"properties": {
			"x": {"oneOf": [{"type": "integer"}, {"type": "object"}]}
		},
		"required": ["x"]
	}`)
)

// ambiguousObj is the runtime value used for the object-variant tests: it makes
// expr-lang fail, confirming the three-layer contract holds.
var ambiguousObj = map[string]any{"value": 1.0}

func TestAmbiguousTypeContract_Add(t *testing.T) {
	testAmbiguousTypeCase(t, "x + 1", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Sub(t *testing.T) {
	testAmbiguousTypeCase(t, "x - 1", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Mul(t *testing.T) {
	testAmbiguousTypeCase(t, "x * 2", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Div(t *testing.T) {
	testAmbiguousTypeCase(t, "x / 2", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_UnaryNeg(t *testing.T) {
	testAmbiguousTypeCase(t, "-x", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_StrConcat(t *testing.T) {
	testAmbiguousTypeCase(t, `x + "s"`, 42, stringOrIntegerSchema)
}

func TestAmbiguousTypeContract_Lt(t *testing.T) {
	testAmbiguousTypeCase(t, "x < 1", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Gt(t *testing.T) {
	testAmbiguousTypeCase(t, "x > 1", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Le(t *testing.T) {
	testAmbiguousTypeCase(t, "x <= 1", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Ge(t *testing.T) {
	testAmbiguousTypeCase(t, "x >= 1", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_And(t *testing.T) {
	testAmbiguousTypeCase(t, "x && true", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Or(t *testing.T) {
	testAmbiguousTypeCase(t, "x || false", ambiguousObj, integerOrObjectSchema)
}

func TestAmbiguousTypeContract_Not(t *testing.T) {
	testAmbiguousTypeCase(t, "!x", ambiguousObj, integerOrObjectSchema)
}

// oneOf with an incompatible variant must be rejected, not just anyOf.
func TestAmbiguousTypeContract_OneOfIncompatibleVariant(t *testing.T) {
	inferErr(t, "x + 1", oneOfIntegerObjectSchema, "")
}

// A non-null mixed union (string|integer) is ambiguous for arithmetic — one
// compatible variant does not make the whole union acceptable.
func TestAmbiguousTypeContract_NonNullMixedStringInteger(t *testing.T) {
	inferErr(t, "x + 1", stringOrIntegerSchema, "")
}
