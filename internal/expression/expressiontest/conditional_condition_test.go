package expressiontest

import "testing"

// nullableErrorSchema — error is a nullable object with string fields task/message/code,
// mirroring the error context shape produced by the genroc engine.
var nullableErrorSchema = mustSchema(`{
	"properties": {
		"error": {
			"oneOf": [
				{
					"type": "object",
					"properties": {
						"task":    {"type": "string"},
						"message": {"type": "string"},
						"code":    {"type": "string"}
					},
					"required": ["task", "message", "code"]
				},
				{"type": "null"}
			]
		}
	},
	"required": ["error"]
}`)

// Unknown identifiers used only in the conditional condition must be rejected.
// Prior to the condition type-check fix, inferConditional never called inferNode
// on n.Cond, so names like "non_existant" were silently ignored.

func TestConditionalCondition_RejectsUnknownIdentifier(t *testing.T) {
	inferErr(t, "non_existant != null ? 0 : 1", integerXSchema, `"non_existant"`)
}

// Guard on "error" propagates through member access — accessing error.code in the
// safe branch of error != null infers as non-null string.
func TestConditionalCondition_NullGuardPropagatesThrough_MemberAccess(t *testing.T) {
	assertSchema(t,
		infer(t, "error != null ? error.code : 'hi'", nullableErrorSchema),
		`{"type": "string"}`,
	)
}
