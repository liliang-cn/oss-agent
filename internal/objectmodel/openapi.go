package objectmodel

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// parseOpenAPI extracts schema objects from an OpenAPI 3 (components.schemas) or
// Swagger 2 (definitions) document. YAML and JSON are both accepted (yaml.v3
// parses JSON). Each schema becomes an Object; every $ref inside it becomes a
// reference to the named schema.
func parseOpenAPI(b []byte) []Object {
	var doc map[string]any
	if yaml.Unmarshal(b, &doc) != nil || doc == nil {
		return nil
	}

	schemas := schemasMap(doc)
	if len(schemas) == 0 {
		return nil
	}

	var objs []Object
	for name, raw := range schemas {
		o := Object{Name: name, Kind: "Schema"}
		sch, _ := raw.(map[string]any)
		if d, ok := sch["description"].(string); ok {
			o.Desc = d
		}
		if props, ok := sch["properties"].(map[string]any); ok {
			for pname := range props {
				o.Fields = append(o.Fields, Field{Name: pname})
			}
		}
		refSeen := map[string]bool{}
		for _, ref := range collectRefs(raw) {
			if ref != "" && ref != name && !refSeen[ref] {
				refSeen[ref] = true
				o.Refs = append(o.Refs, ref)
			}
		}
		objs = append(objs, o)
	}
	return objs
}

// schemasMap finds the schema dictionary in either OpenAPI 3 or Swagger 2 layout.
func schemasMap(doc map[string]any) map[string]any {
	if comp, ok := doc["components"].(map[string]any); ok {
		if s, ok := comp["schemas"].(map[string]any); ok {
			return s
		}
	}
	if s, ok := doc["definitions"].(map[string]any); ok { // Swagger 2
		return s
	}
	return nil
}

// collectRefs walks an arbitrary JSON/YAML value and returns the schema name of
// every "$ref" it finds (last path segment of e.g. #/components/schemas/Foo).
func collectRefs(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				if s, ok := val.(string); ok {
					if i := strings.LastIndex(s, "/"); i >= 0 {
						out = append(out, s[i+1:])
					}
				}
				continue
			}
			out = append(out, collectRefs(val)...)
		}
	case []any:
		for _, e := range t {
			out = append(out, collectRefs(e)...)
		}
	}
	return out
}
