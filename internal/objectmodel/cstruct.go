package objectmodel

import (
	"regexp"
	"strings"
)

var (
	// named struct:        struct Name { ... };
	reCStructNamed = regexp.MustCompile(`(?m)\bstruct\s+(\w+)\s*\{`)
	// typedef struct:      typedef struct [Tag] { ... } Name;
	reCTypedefHead = regexp.MustCompile(`(?m)\btypedef\s+struct\s+(?:\w+\s*)?\{`)
	// braceBody consumes the closing brace, so the tail begins after "}": match
	// the trailing typedef name there, e.g. " shape;" or " __attribute__(()) s;".
	reCTypedefName = regexp.MustCompile(`^[^;{]*?\b([A-Za-z_]\w*)\s*;`)
	// a field line: <type tokens> name[...];  — captures the type portion + name
	reCField = regexp.MustCompile(`(?m)^\s*((?:struct\s+\w+|[A-Za-z_]\w*(?:\s*\*+)?)(?:\s+[A-Za-z_]\w*)*)\s+(\*?\w+)\s*(?:\[[^\]]*\])?\s*;`)
)

// cKeywords are type-position tokens that never name a referenced struct.
var cKeywords = map[string]bool{
	"struct": true, "const": true, "unsigned": true, "signed": true,
	"void": true, "char": true, "short": true, "int": true, "long": true,
	"float": true, "double": true, "bool": true, "size_t": true,
	"uint8_t": true, "uint16_t": true, "uint32_t": true, "uint64_t": true,
	"int8_t": true, "int16_t": true, "int32_t": true, "int64_t": true,
	"u8": true, "u16": true, "u32": true, "u64": true,
	"s8": true, "s16": true, "s32": true, "s64": true,
}

// parseCStruct extracts C/C++ struct definitions (named and typedef'd) and the
// references between them (a field whose type is another struct).
func parseCStruct(src string) []Object {
	var objs []Object

	// typedef struct { ... } Name;  — name comes after the closing brace.
	for _, loc := range reCTypedefHead.FindAllStringIndex(src, -1) {
		body, end := braceBody(src, loc[1]-1)
		tail := src[end:]
		nm := reCTypedefName.FindStringSubmatch(tail)
		if nm == nil {
			continue
		}
		objs = append(objs, structObject(nm[1], body))
	}

	// struct Name { ... };  (skip the typedef-struct heads already consumed)
	for _, m := range reCStructNamed.FindAllStringSubmatchIndex(src, -1) {
		// a "typedef struct Tag {" also matches here; only skip the anonymous
		// typedef form (no tag). Tagged ones are still worth indexing.
		name := src[m[2]:m[3]]
		body, _ := braceBody(src, m[1]-1)
		objs = append(objs, structObject(name, body))
	}
	return dedupObjects(objs)
}

func structObject(name, body string) Object {
	o := Object{Name: name, Kind: "Struct"}
	refSeen := map[string]bool{}
	for _, fm := range reCField.FindAllStringSubmatch(body, -1) {
		typeExpr, fname := fm[1], strings.TrimPrefix(fm[2], "*")
		o.Fields = append(o.Fields, Field{Name: fname, Type: strings.TrimSpace(typeExpr)})
		if ref := cRefType(typeExpr); ref != "" && !refSeen[ref] {
			refSeen[ref] = true
			o.Refs = append(o.Refs, ref)
		}
	}
	return o
}

// cRefType returns the referenced struct name in a field type expression, or "".
// "struct foo" → foo; a bare typedef name → itself; scalars/keywords → "".
func cRefType(typeExpr string) string {
	toks := strings.Fields(strings.ReplaceAll(typeExpr, "*", " "))
	for i, t := range toks {
		if t == "struct" && i+1 < len(toks) {
			return toks[i+1]
		}
	}
	// bare type: last non-keyword token may be a typedef'd struct name.
	for i := len(toks) - 1; i >= 0; i-- {
		t := toks[i]
		if !cKeywords[t] && t != "" {
			return t
		}
	}
	return ""
}

// dedupObjects keeps the first (richest) definition when a name appears twice
// (e.g. a typedef tag and its name both matched).
func dedupObjects(in []Object) []Object {
	seen := map[string]bool{}
	out := in[:0]
	for _, o := range in {
		key := strings.ToLower(o.Name)
		if o.Name == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, o)
	}
	return out
}
