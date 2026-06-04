package expression

import (
	"encoding/json"
	"fmt"
	"math"
)

// ErrUnsupported is returned when the expression uses a construct outside
// the supported subset.
type ErrUnsupported struct{ Detail string }

func (e ErrUnsupported) Error() string {
	return "unsupported expression: " + e.Detail
}

// binOp pairs the type-inference and runtime-evaluation behaviour of a binary
// operator. Defining both here means adding a new operator touches one place.
type binOp struct {
	infer func(left, right map[string]any) (map[string]any, error)
	eval  func(left, right any) (any, error)
}

// unOp pairs the type-inference and runtime-evaluation behaviour of a unary
// operator.
type unOp struct {
	infer func(operand map[string]any) (map[string]any, error)
	eval  func(operand any) (any, error)
}

// binaryOps is the authoritative list of supported binary operators.
// Adding an entry here makes it available to both InferType and Eval.
// && and || carry nil eval because short-circuit requires lazy operand
// evaluation; the walkers handle them directly.
var binaryOps = map[string]binOp{
	"==": {infer: alwaysBoolean, eval: func(l, r any) (any, error) { return equalValues(l, r), nil }},
	"!=": {infer: alwaysBoolean, eval: func(l, r any) (any, error) { return !equalValues(l, r), nil }},
	"<":  {infer: inferOrderingCmp, eval: numCmp(func(a, b float64) bool { return a < b })},
	">":  {infer: inferOrderingCmp, eval: numCmp(func(a, b float64) bool { return a > b })},
	"<=": {infer: inferOrderingCmp, eval: numCmp(func(a, b float64) bool { return a <= b })},
	">=": {infer: inferOrderingCmp, eval: numCmp(func(a, b float64) bool { return a >= b })},
	"&&": {infer: inferLogical, eval: nil},
	"||": {infer: inferLogical, eval: nil},
	"+":  {infer: inferAdd, eval: evalAdd},
	"-":  {infer: inferArith, eval: numBinOp("-", func(a, b float64) float64 { return a - b })},
	"*":  {infer: inferArith, eval: numBinOp("*", func(a, b float64) float64 { return a * b })},
	"/":  {infer: inferDiv, eval: evalDiv},
	"%":  {infer: inferMod, eval: evalMod},
	"??": {infer: inferNullCoalesce, eval: nil},
}

// unaryOps is the authoritative list of supported unary operators.
var unaryOps = map[string]unOp{
	"!": {infer: inferNot, eval: func(v any) (any, error) { return evalNot(v) }},
	"-": {infer: numericPassthrough, eval: func(v any) (any, error) { return negateNum(v) }},
	"+": {infer: numericPassthrough, eval: func(v any) (any, error) { return requireNum(v) }},
}

// ---- infer helpers ----

func alwaysBoolean(_, _ map[string]any) (map[string]any, error) {
	return typeSchema("boolean"), nil
}

// inferOrderingCmp rejects nullable, ambiguous, and non-numeric operands.
// expr-lang only supports numeric ordering; string and boolean comparisons fail
// at runtime.
func inferOrderingCmp(left, right map[string]any) (map[string]any, error) {
	if hasNullType(left) || hasNullType(right) {
		return nil, fmt.Errorf("comparison requires non-nullable operands")
	}
	lt, ok := concreteTypeOf(left)
	if !ok {
		return nil, fmt.Errorf("comparison requires an unambiguous operand")
	}
	rt, ok := concreteTypeOf(right)
	if !ok {
		return nil, fmt.Errorf("comparison requires an unambiguous operand")
	}
	if !isNumeric(lt) || !isNumeric(rt) {
		return nil, fmt.Errorf("comparison requires numeric operands, got %q and %q", lt, rt)
	}
	return typeSchema("boolean"), nil
}

// inferLogical rejects nullable, ambiguous, and non-boolean operands.
func inferLogical(left, right map[string]any) (map[string]any, error) {
	if hasNullType(left) || hasNullType(right) {
		return nil, fmt.Errorf("logical operator requires non-nullable boolean operands")
	}
	lt, ok := concreteTypeOf(left)
	if !ok {
		return nil, fmt.Errorf("logical operator requires an unambiguous operand")
	}
	rt, ok := concreteTypeOf(right)
	if !ok {
		return nil, fmt.Errorf("logical operator requires an unambiguous operand")
	}
	if lt != "boolean" || rt != "boolean" {
		return nil, fmt.Errorf("logical operator requires boolean operands, got %q and %q", lt, rt)
	}
	return typeSchema("boolean"), nil
}

func inferNot(operand map[string]any) (map[string]any, error) {
	if hasNullType(operand) {
		return nil, fmt.Errorf("! requires a non-nullable boolean operand")
	}
	t, ok := concreteTypeOf(operand)
	if !ok {
		return nil, fmt.Errorf("! requires an unambiguous operand")
	}
	if t != "boolean" {
		return nil, fmt.Errorf("! requires a boolean operand, got %q", t)
	}
	return typeSchema("boolean"), nil
}

func evalNot(v any) (any, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("! requires a boolean operand, got %T", v)
	}
	return !b, nil
}

func inferAdd(left, right map[string]any) (map[string]any, error) {
	if hasNullType(left) || hasNullType(right) {
		return nil, fmt.Errorf("operator requires non-nullable operands")
	}
	lt, ltOK := concreteTypeOf(left)
	rt, rtOK := concreteTypeOf(right)
	if !ltOK || !rtOK {
		return nil, fmt.Errorf("operator requires an unambiguous operand")
	}
	if lt == "string" && rt == "string" {
		return typeSchema("string"), nil
	}
	return inferArith(left, right)
}

func inferArith(left, right map[string]any) (map[string]any, error) {
	if hasNullType(left) || hasNullType(right) {
		return nil, fmt.Errorf("operator requires non-nullable operands")
	}
	lt, ltOK := concreteTypeOf(left)
	rt, rtOK := concreteTypeOf(right)
	if !ltOK || !rtOK {
		return nil, fmt.Errorf("operator requires an unambiguous numeric operand")
	}
	if !isNumeric(lt) || !isNumeric(rt) {
		return nil, fmt.Errorf("operator requires numeric operands, got %q and %q", lt, rt)
	}
	if lt == "integer" && rt == "integer" {
		return typeSchema("integer"), nil
	}
	return typeSchema("number"), nil
}

// inferMod requires both operands to be integer — expr-lang rejects float % int
// at runtime, unlike the other arithmetic operators.
func inferMod(left, right map[string]any) (map[string]any, error) {
	if hasNullType(left) || hasNullType(right) {
		return nil, fmt.Errorf("%% requires non-nullable operands")
	}
	lt, ltOK := concreteTypeOf(left)
	rt, rtOK := concreteTypeOf(right)
	if !ltOK || !rtOK {
		return nil, fmt.Errorf("%% requires an unambiguous integer operand")
	}
	if lt != "integer" || rt != "integer" {
		return nil, fmt.Errorf("%% requires integer operands, got %q and %q", lt, rt)
	}
	return typeSchema("integer"), nil
}

// inferDiv always returns number because division of integers can produce a fraction.
func inferDiv(left, right map[string]any) (map[string]any, error) {
	if hasNullType(left) || hasNullType(right) {
		return nil, fmt.Errorf("/ requires non-nullable operands")
	}
	lt, ltOK := concreteTypeOf(left)
	rt, rtOK := concreteTypeOf(right)
	if !ltOK || !rtOK {
		return nil, fmt.Errorf("/ requires an unambiguous numeric operand")
	}
	if !isNumeric(lt) || !isNumeric(rt) {
		return nil, fmt.Errorf("/ requires numeric operands, got %q and %q", lt, rt)
	}
	return typeSchema("number"), nil
}

func numericPassthrough(operand map[string]any) (map[string]any, error) {
	if hasNullType(operand) {
		return nil, fmt.Errorf("unary operator requires a non-nullable numeric operand")
	}
	t, ok := concreteTypeOf(operand)
	if !ok {
		return nil, fmt.Errorf("unary operator requires an unambiguous numeric operand")
	}
	if !isNumeric(t) {
		return nil, fmt.Errorf("unary operator requires a numeric operand, got %q", t)
	}
	return operand, nil
}

// inferNullCoalesce infers the result type of a ?? b.
// If left is always null, the result is right's type.
// If left is never nullable, the result is left's type (right is unreachable).
// If left is nullable, the result is the union of stripNull(left) and right.
func inferNullCoalesce(left, right map[string]any) (map[string]any, error) {
	if isNullType(left) {
		return right, nil
	}
	nonNullLeft := stripNull(left)
	if schemasEqual(left, nonNullLeft) {
		return left, nil
	}
	if schemasEqual(nonNullLeft, right) {
		return nonNullLeft, nil
	}
	lct, lOK := concreteTypeOf(nonNullLeft)
	rct, rOK := concreteTypeOf(right)
	if lOK && rOK && isNumeric(lct) && isNumeric(rct) {
		if lct == rct {
			return typeSchema(lct), nil
		}
		return typeSchema("number"), nil
	}
	return map[string]any{"oneOf": []any{nonNullLeft, right}}, nil
}

// ---- eval helpers ----

func equalValues(l, r any) bool {
	if lf, rf, ok := bothNumeric(l, r); ok {
		return lf == rf
	}
	if ls, rs, ok := bothString(l, r); ok {
		return ls == rs
	}
	if lb, rb, ok := bothBool(l, r); ok {
		return lb == rb
	}
	return l == r
}

func numCmp(fn func(float64, float64) bool) func(any, any) (any, error) {
	return func(l, r any) (any, error) {
		lf, rf, ok := bothNumeric(l, r)
		if !ok {
			return nil, fmt.Errorf("comparison requires numeric operands")
		}
		return fn(lf, rf), nil
	}
}

func evalAdd(l, r any) (any, error) {
	if ls, rs, ok := bothString(l, r); ok {
		return ls + rs, nil
	}
	return numBinOp("+", func(a, b float64) float64 { return a + b })(l, r)
}

// numBinOp returns an eval func for a numeric binary operator that preserves
// integer type when both operands are integers and the result is whole.
func numBinOp(op string, fn func(float64, float64) float64) func(any, any) (any, error) {
	return func(l, r any) (any, error) {
		lf, rf, ok := bothNumeric(l, r)
		if !ok {
			return nil, fmt.Errorf("%s requires numeric operands", op)
		}
		result := fn(lf, rf)
		if isIntLike(l) && isIntLike(r) && result == math.Trunc(result) {
			return int(result), nil
		}
		return result, nil
	}
}

func evalDiv(l, r any) (any, error) {
	lf, rf, ok := bothNumeric(l, r)
	if !ok {
		return nil, fmt.Errorf("/ requires numeric operands")
	}
	if rf == 0 {
		return nil, fmt.Errorf("division by zero")
	}
	return lf / rf, nil
}

func evalMod(l, r any) (any, error) {
	if !isIntLike(l) || !isIntLike(r) {
		return nil, fmt.Errorf("%% requires integer operands, got %T and %T", l, r)
	}
	lf, rf, _ := bothNumeric(l, r)
	if rf == 0 {
		return nil, fmt.Errorf("modulo by zero")
	}
	return int(math.Mod(lf, rf)), nil
}

func negateNum(v any) (any, error) {
	f, ok := toFloat64(v)
	if !ok {
		return nil, fmt.Errorf("unary - requires a numeric operand")
	}
	if isIntLike(v) {
		return -int(f), nil
	}
	return -f, nil
}

func requireNum(v any) (any, error) {
	if _, ok := toFloat64(v); !ok {
		return nil, fmt.Errorf("unary + requires a numeric operand")
	}
	return v, nil
}

// ---- shared type / numeric utilities ----

func typeSchema(t string) map[string]any {
	return map[string]any{"type": t}
}

func isNumeric(t string) bool {
	return t == "integer" || t == "number"
}

// nullableSchema returns {type: [X, "null"]} when one of a/b is a null schema
// and the other carries a single string type. Falls back to (nil, false) for
// complex cases so the caller can use oneOf instead.
// withNull makes schema nullable. Simple types produce {type:[X,"null"]};
// already-nullable or {} schemas are returned as-is; complex schemas are
// wrapped in {oneOf:[schema,{type:"null"}]}.
func withNull(s map[string]any) map[string]any {
	if len(s) == 0 {
		return s // {} = any, already includes null
	}
	if result, ok := tryNullable(s, typeSchema("null")); ok {
		return result
	}
	return map[string]any{"oneOf": []any{s, typeSchema("null")}}
}

func nullableSchema(a, b map[string]any) (map[string]any, bool) {
	if s, ok := tryNullable(a, b); ok {
		return s, true
	}
	return tryNullable(b, a)
}

// tryNullable checks if other is {type:"null"} and self can be made nullable.
// Returns (nullable schema, true) on success.
// All keys of self are preserved — only the "type" field is widened to include "null".
// Schemas with "properties" are intentionally excluded: merging null into their
// type array would mislead property lookups (the properties would still appear
// present even when the value is null). Those schemas are wrapped in oneOf by
// withNull instead.
func tryNullable(self, other map[string]any) (map[string]any, bool) {
	if !isNullType(other) {
		return nil, false
	}
	// self is already a nullable type array → return as-is.
	if types, ok := self["type"].([]any); ok && typeArrayContainsNull(types) {
		return self, true
	}
	// Schemas with properties need oneOf wrapping so that property access after
	// null-check narrowing works correctly (handled by withNull's fallback).
	if _, hasProps := self["properties"]; hasProps {
		return nil, false
	}
	// self has a simple non-null type → widen to [type, "null"], preserving all other keys.
	if t, ok := self["type"].(string); ok && t != "null" {
		result := make(map[string]any, len(self))
		for k, v := range self {
			result[k] = v
		}
		result["type"] = []any{t, "null"}
		return result, true
	}
	return nil, false
}

func isNullType(s map[string]any) bool {
	t, _ := s["type"].(string)
	return t == "null"
}

// concreteTypeOf extracts a single effective type string from a schema:
//   - {"type":"T"} → ("T", true)
//   - anyOf/oneOf where all non-null variants share the same type → (type, true)
//   - anyOf/oneOf where all non-null variants are numeric (mixed int/number) → ("number", true)
//   - anything else (ambiguous, no type, nullable) → ("", false)
//
// Nullable schemas (those caught by hasNullType) are not unwrapped here;
// callers must check hasNullType first.
func concreteTypeOf(s map[string]any) (string, bool) {
	if t, ok := s["type"].(string); ok {
		return t, true
	}
	for _, kw := range []string{"anyOf", "oneOf"} {
		variants, ok := s[kw].([]any)
		if !ok {
			continue
		}
		var types []string
		for _, v := range variants {
			vs, ok := v.(map[string]any)
			if !ok {
				return "", false
			}
			if isNullType(vs) {
				return "", false // nullable — hasNullType must have missed it; reject
			}
			t, ok := vs["type"].(string)
			if !ok {
				return "", false
			}
			types = append(types, t)
		}
		if len(types) == 0 {
			return "", false
		}
		// All variants the same concrete type.
		if allEqual(types) {
			return types[0], true
		}
		// All variants numeric (integer/number mix) → widen to number.
		if allSatisfy(types, isNumeric) {
			return "number", true
		}
		return "", false
	}
	return "", false
}

func allEqual(ss []string) bool {
	for _, s := range ss[1:] {
		if s != ss[0] {
			return false
		}
	}
	return true
}

func allSatisfy(ss []string, fn func(string) bool) bool {
	for _, s := range ss {
		if !fn(s) {
			return false
		}
	}
	return true
}

// unwrapSingleVariant simplifies a oneOf/anyOf schema that has exactly one
// variant (with no null variants) into that variant directly, so
// {"oneOf":[{"type":"number"}]} is treated the same as {"type":"number"}.
// Schemas with a null variant are left unchanged so hasNullType can catch them.
func unwrapSingleVariant(s map[string]any) map[string]any {
	for _, kw := range []string{"anyOf", "oneOf"} {
		variants, ok := s[kw].([]any)
		if !ok {
			continue
		}
		var nonNull []map[string]any
		for _, v := range variants {
			vs, ok := v.(map[string]any)
			if !ok {
				return s
			}
			if isNullType(vs) {
				return s // preserve nullable schemas for hasNullType to handle
			}
			nonNull = append(nonNull, vs)
		}
		if len(nonNull) == 1 {
			return nonNull[0]
		}
	}
	return s
}

// hasNullType reports whether null is a possible type for the schema:
// {"type":"null"}, {"type":["X","null"]}, or anyOf/oneOf with a null variant.
func hasNullType(s map[string]any) bool {
	switch t := s["type"].(type) {
	case string:
		return t == "null"
	case []any:
		return typeArrayContainsNull(t)
	}
	for _, kw := range []string{"anyOf", "oneOf"} {
		if variants, ok := s[kw].([]any); ok {
			for _, v := range variants {
				if vs, ok := v.(map[string]any); ok && isNullType(vs) {
					return true
				}
			}
		}
	}
	return false
}

func typeArrayContainsNull(types []any) bool {
	for _, t := range types {
		if t == "null" {
			return true
		}
	}
	return false
}

func schemasEqual(a, b map[string]any) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(aj) == string(bj)
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}

func isIntLike(v any) bool {
	switch v.(type) {
	case int, int64:
		return true
	}
	return false
}

func bothNumeric(a, b any) (float64, float64, bool) {
	af, aok := toFloat64(a)
	bf, bok := toFloat64(b)
	return af, bf, aok && bok
}

func bothString(a, b any) (string, string, bool) {
	as, aok := a.(string)
	bs, bok := b.(string)
	return as, bs, aok && bok
}

func bothBool(a, b any) (bool, bool, bool) {
	ab, aok := a.(bool)
	bb, bok := b.(bool)
	return ab, bb, aok && bok
}

func mustBool(v any) bool {
	b, _ := v.(bool)
	return b
}
