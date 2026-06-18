package schematest

import (
	"testing"

	"gent/internal/schema"
)

func anyOf(vs ...*schema.SchemaNode) *schema.SchemaNode { return &schema.SchemaNode{AnyOf: vs} }
func allOf(vs ...*schema.SchemaNode) *schema.SchemaNode { return &schema.SchemaNode{AllOf: vs} }

// objP builds {type:object, properties:{name: v}, required:[name]}.
func objP(name string, v *schema.SchemaNode) *schema.SchemaNode {
	return obj([]string{name}, map[string]*schema.SchemaNode{name: v})
}

func TestCanonicalize_NestedObjectsInCompositions(t *testing.T) {
	tests := []struct {
		name string
		a, b *schema.SchemaNode
	}{
		{
			"oneOf of objects: variant order + inner union canonicalized",
			oneOf(objP("x", oneOf(prim("integer"), prim("integer"))), objP("y", prim("string"))),
			oneOf(objP("y", prim("string")), objP("x", prim("integer"))),
		},
		{
			"anyOf of objects: variant order + inner union canonicalized",
			anyOf(objP("x", oneOf(prim("integer"), prim("integer"))), objP("y", prim("string"))),
			anyOf(objP("y", prim("string")), objP("x", prim("integer"))),
		},
		{
			"allOf of objects: variant order + inner union canonicalized",
			allOf(objP("x", oneOf(prim("string"), prim("string"))), objP("y", prim("integer"))),
			allOf(objP("y", prim("integer")), objP("x", prim("string"))),
		},
		{
			"allOf singleton collapses to its member",
			allOf(objP("x", prim("integer"))),
			objP("x", prim("integer")),
		},
		{
			"object property that is a oneOf of objects",
			objP("val", oneOf(objP("a", prim("integer")), objP("b", oneOf(prim("string"), prim("string"))))),
			objP("val", oneOf(objP("b", prim("string")), objP("a", prim("integer")))),
		},
		{
			"deeply nested object property with an inner union",
			objP("inner", objP("z", oneOf(prim("integer"), prim("integer")))),
			objP("inner", objP("z", prim("integer"))),
		},
		{
			"object inside oneOf with a nullable property",
			oneOf(objP("p", oneOf(prim("string"), prim("null"))), prim("null")),
			oneOf(prim("null"), objP("p", prim("string", "null"))),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ja := canonJSON(t, tt.a)
			jb := canonJSON(t, tt.b)
			if ja != jb {
				t.Errorf("canonical forms differ:\n  a: %s\n  b: %s", ja, jb)
			}
		})
	}
}
