package objectmodel

import "testing"

func names(objs []Object) map[string]Object {
	m := map[string]Object{}
	for _, o := range objs {
		m[o.Name] = o
	}
	return m
}

func TestParseProto(t *testing.T) {
	src := `
syntax = "proto3";
package x;

message Address {
  string city = 1;
}

message Person {
  string name = 1;
  int32 age = 2;
  Address home = 3;
  repeated Address others = 4;
  map<string, Address> by_label = 5;
  enum Kind { A = 0; B = 1; }
  Kind kind = 6;
}
`
	objs := parseProto(src)
	m := names(objs)
	if len(m) != 2 {
		t.Fatalf("want 2 messages, got %d (%v)", len(m), keys(m))
	}
	p := m["Person"]
	if got := refSet(p.Refs); !got["Address"] {
		t.Errorf("Person should reference Address; refs=%v", p.Refs)
	}
	// Nested enum Kind may appear in Refs but is harmless: it is not a parsed
	// object, so load-time resolution drops it (only known names become edges).
	if len(p.Fields) < 5 {
		t.Errorf("Person should have its own fields, not the enum's; got %d", len(p.Fields))
	}
}

func TestParseOpenAPI(t *testing.T) {
	doc := []byte(`
openapi: 3.0.0
components:
  schemas:
    Order:
      description: a customer order
      properties:
        id: { type: string }
        customer:
          $ref: '#/components/schemas/Customer'
        lines:
          type: array
          items:
            $ref: '#/components/schemas/LineItem'
    Customer:
      properties:
        name: { type: string }
    LineItem:
      properties:
        sku: { type: string }
`)
	objs := parseOpenAPI(doc)
	m := names(objs)
	if len(m) != 3 {
		t.Fatalf("want 3 schemas, got %d (%v)", len(m), keys(m))
	}
	o := m["Order"]
	rs := refSet(o.Refs)
	if !rs["Customer"] || !rs["LineItem"] {
		t.Errorf("Order should reference Customer and LineItem; refs=%v", o.Refs)
	}
	if o.Desc != "a customer order" {
		t.Errorf("description not captured: %q", o.Desc)
	}
}

func TestParseCStruct(t *testing.T) {
	src := `
struct point { int x; int y; };

typedef struct {
  char name[32];
  struct point origin;
  region area;
} shape;

typedef struct { int w; int h; } region;
`
	objs := parseCStruct(src)
	m := names(objs)
	if _, ok := m["point"]; !ok {
		t.Fatalf("missing struct point; got %v", keys(m))
	}
	sh, ok := m["shape"]
	if !ok {
		t.Fatalf("missing typedef shape; got %v", keys(m))
	}
	rs := refSet(sh.Refs)
	if !rs["point"] {
		t.Errorf("shape should reference point (struct point origin); refs=%v", sh.Refs)
	}
	if !rs["region"] {
		t.Errorf("shape should reference region (typedef field); refs=%v", sh.Refs)
	}
}

func refSet(in []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range in {
		m[s] = true
	}
	return m
}

func keys(m map[string]Object) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
