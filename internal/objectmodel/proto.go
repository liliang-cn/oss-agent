package objectmodel

import (
	"regexp"
	"strings"
)

// protoScalars are the built-in protobuf scalar types — never object references.
var protoScalars = map[string]bool{
	"double": true, "float": true, "int32": true, "int64": true,
	"uint32": true, "uint64": true, "sint32": true, "sint64": true,
	"fixed32": true, "fixed64": true, "sfixed32": true, "sfixed64": true,
	"bool": true, "string": true, "bytes": true,
}

var (
	reProtoMessage = regexp.MustCompile(`(?m)^\s*message\s+(\w+)\s*\{`)
	// field: [repeated|optional|required] <type> name = N;  (type may be map<k,v>)
	reProtoField = regexp.MustCompile(`(?m)^\s*(?:repeated\s+|optional\s+|required\s+)?([\w.]+(?:<[^>]+>)?)\s+(\w+)\s*=\s*\d+\s*;`)
)

// parseProto extracts protobuf messages and their inter-message field references.
// Nested message/enum blocks are stripped from a message body before fields are
// read, so a parent does not absorb its children's fields.
func parseProto(src string) []Object {
	var objs []Object
	for _, m := range reProtoMessage.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		bodyStart := m[1] // just after the opening brace
		body, _ := braceBody(src, bodyStart-1)
		o := Object{Name: name, Kind: "Message"}

		refSeen := map[string]bool{}
		for _, fm := range reProtoField.FindAllStringSubmatch(stripNestedBlocks(body), -1) {
			typeTok, fname := fm[1], fm[2]
			o.Fields = append(o.Fields, Field{Name: fname, Type: typeTok})
			for _, ref := range protoRefTypes(typeTok) {
				if !refSeen[ref] {
					refSeen[ref] = true
					o.Refs = append(o.Refs, ref)
				}
			}
		}
		objs = append(objs, o)
	}
	return objs
}

// protoRefTypes returns the candidate referenced message names in a field type
// token, dropping scalars and package qualifiers (map<k,v> yields its value type).
func protoRefTypes(tok string) []string {
	var out []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if i := strings.LastIndex(t, "."); i >= 0 {
			t = t[i+1:] // drop package qualifier: google.protobuf.Timestamp → Timestamp
		}
		if t == "" || protoScalars[t] {
			return
		}
		out = append(out, t)
	}
	if strings.HasPrefix(tok, "map<") {
		inner := strings.TrimSuffix(strings.TrimPrefix(tok, "map<"), ">")
		parts := strings.SplitN(inner, ",", 2)
		if len(parts) == 2 {
			add(parts[1]) // value type only
		}
		return out
	}
	add(tok)
	return out
}

// reNestedBlock matches a nested message/enum/oneof block to remove it.
var reNestedBlock = regexp.MustCompile(`(?s)\b(?:message|enum|oneof)\s+\w+\s*\{`)

// stripNestedBlocks removes nested message/enum/oneof bodies from a message body
// so only the message's own fields remain.
func stripNestedBlocks(body string) string {
	for {
		loc := reNestedBlock.FindStringIndex(body)
		if loc == nil {
			return body
		}
		_, end := braceBody(body, loc[1]-1)
		if end <= loc[0] {
			return body
		}
		body = body[:loc[0]] + body[end:]
	}
}

// braceBody returns the content between the brace at openIdx and its match, plus
// the absolute index just past the closing brace.
func braceBody(s string, openIdx int) (string, int) {
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[openIdx+1 : i], i + 1
			}
		}
	}
	return s[min(openIdx+1, len(s)):], len(s)
}
