package expression

import (
	"encoding/json"
	"fmt"
	"math"

	"genroc/internal/schema"
)

// ErrUnsupported is returned when the expression uses a construct outside
// the supported subset.
type ErrUnsupported struct{ Detail string }

func (e ErrUnsupported) Error() string {
	return "unsupported expression: " + e.Detail
}

// binOp pairs the type-inference and runtime-evaluation behaviour of a binary operator.
type binOp struct {
	infer func(left, right *schema.SchemaNode) (*schema.SchemaNode, error)
	eval  func(left, right any) (any, error)
}

// unOp pairs the type-inference and runtime-evaluation behaviour of a unary operator.
type unOp struct {
	infer func(operand *schema.SchemaNode) (*schema.SchemaNode, error)
	eval  func(operand any) (any, error)
}

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

var unaryOps = map[string]unOp{
	"!": {infer: inferNot, eval: func(v any) (any, error) { return evalNot(v) }},
	"-": {infer: numericPassthrough, eval: func(v any) (any, error) { return negateNum(v) }},
	"+": {infer: numericPassthrough, eval: func(v any) (any, error) { return requireNum(v) }},
}

// ---- infer helpers ----

func alwaysBoolean(_, _ *schema.SchemaNode) (*schema.SchemaNode, error) {
	return typeSchema("boolean"), nil
}

func inferOrderingCmp(left, right *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(left) || schema.HasNullType(right) {
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

func inferLogical(left, right *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(left) || schema.HasNullType(right) {
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

func inferNot(operand *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(operand) {
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

func inferAdd(left, right *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(left) || schema.HasNullType(right) {
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

func inferArith(left, right *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(left) || schema.HasNullType(right) {
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

func inferMod(left, right *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(left) || schema.HasNullType(right) {
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

func inferDiv(left, right *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(left) || schema.HasNullType(right) {
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

func numericPassthrough(operand *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.HasNullType(operand) {
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

func inferNullCoalesce(left, right *schema.SchemaNode) (*schema.SchemaNode, error) {
	if schema.IsNullType(left) {
		return right, nil
	}
	nonNullLeft := schema.StripNull(left)
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
	return &schema.SchemaNode{OneOf: []*schema.SchemaNode{nonNullLeft, right}}, nil
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

// ---- schema helpers ----

func typeSchema(t string) *schema.SchemaNode {
	return &schema.SchemaNode{Type: schema.SchemaType{t}}
}

func isNumeric(t string) bool {
	return t == "integer" || t == "number"
}

// withNull makes s nullable. Simple types produce {type:[T,"null"]};
// complex schemas are wrapped in {oneOf:[s,{type:"null"}]}.
func withNull(s *schema.SchemaNode) *schema.SchemaNode {
	if s == nil {
		return s
	}
	if result, ok := tryNullable(s, typeSchema("null")); ok {
		return result
	}
	return &schema.SchemaNode{OneOf: []*schema.SchemaNode{s, typeSchema("null")}}
}

func nullableSchema(a, b *schema.SchemaNode) (*schema.SchemaNode, bool) {
	if s, ok := tryNullable(a, b); ok {
		return s, true
	}
	return tryNullable(b, a)
}

// tryNullable checks if other is {type:"null"} and self can be made nullable.
// Schemas with properties are excluded (they need oneOf wrapping by withNull).
func tryNullable(self, other *schema.SchemaNode) (*schema.SchemaNode, bool) {
	if !schema.IsNullType(other) {
		return nil, false
	}
	if schema.HasNullType(self) {
		return self, true
	}
	if self.Properties != nil {
		return nil, false
	}
	if len(self.Type) == 1 && self.Type[0] != "null" {
		n := *self
		n.Type = schema.SchemaType{self.Type[0], "null"}
		return &n, true
	}
	return nil, false
}

// concreteTypeOf extracts a single effective type string from a schema.
func concreteTypeOf(s *schema.SchemaNode) (string, bool) {
	if s == nil {
		return "", false
	}
	if len(s.Type) == 1 {
		return s.Type[0], true
	}
	for _, variants := range [][]*schema.SchemaNode{s.AnyOf, s.OneOf} {
		if variants == nil {
			continue
		}
		var types []string
		for _, v := range variants {
			if v == nil {
				return "", false
			}
			if schema.IsNullType(v) {
				return "", false
			}
			if len(v.Type) != 1 {
				return "", false
			}
			types = append(types, v.Type[0])
		}
		if len(types) == 0 {
			return "", false
		}
		if allEqual(types) {
			return types[0], true
		}
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
// non-null variant into that variant directly.
func unwrapSingleVariant(s *schema.SchemaNode) *schema.SchemaNode {
	if s == nil {
		return s
	}
	for _, variants := range [][]*schema.SchemaNode{s.AnyOf, s.OneOf} {
		if variants == nil {
			continue
		}
		var nonNull []*schema.SchemaNode
		for _, v := range variants {
			if v == nil {
				return s
			}
			if schema.IsNullType(v) {
				return s
			}
			nonNull = append(nonNull, v)
		}
		if len(nonNull) == 1 {
			return nonNull[0]
		}
	}
	return s
}

func schemasEqual(a, b *schema.SchemaNode) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(aj) == string(bj)
}

// ---- runtime type / numeric utilities ----

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
