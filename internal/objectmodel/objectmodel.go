// Package objectmodel extracts a precise object model from a project's structured
// source-of-truth — the same idea as schemaimport (SQL DDL), generalized to the
// other shapes a project ships its data model in:
//
//	.proto              protobuf messages        → entities, message refs → relations
//	.yaml/.yml/.json    OpenAPI / Swagger schema → entities, $ref → relations
//	.h/.hpp/.c/.cc/.cpp C/C++ structs            → entities, struct fields → relations
//	.sql                SQL DDL                  → delegated to schemaimport
//
// Like schemaimport this is deterministic (no LLM): the structured source IS the
// ground-truth ontology, so we parse it rather than mine prose. Entities carry the
// kind as their graph type ("Message"/"Schema"/"Struct"); references between
// objects become "REFERENCES" relations. Product-agnostic.
package objectmodel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/schemaimport"
)

// Field is one member of an object (name + the type token used for ref resolution).
type Field struct {
	Name string
	Type string
}

// Object is one node of the object model (a message / schema / struct).
type Object struct {
	Name   string
	Kind   string // "Message", "Schema", "Struct"
	Desc   string
	Fields []Field
	Refs   []string // names of other objects this one references (by field type)
}

// Stats summarizes an import.
type Stats struct {
	Format  string // proto | openapi | cstruct | sql
	Objects int
	Refs    int
}

// Load detects the format from the file extension, parses the object model, and
// loads it into the knowledge graph. SQL is delegated to schemaimport so the same
// `import-model` command handles every structured source.
func Load(ctx context.Context, store *knowledge.Store, path string) (Stats, error) {
	var st Stats
	ext := strings.ToLower(filepath.Ext(path))

	if ext == ".sql" {
		ss, err := schemaimport.Import(ctx, store, path)
		return Stats{Format: "sql", Objects: ss.Tables, Refs: ss.FKs}, err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return st, fmt.Errorf("read %s: %w", path, err)
	}
	src := string(b)

	var objs []Object
	switch ext {
	case ".proto":
		st.Format, objs = "proto", parseProto(src)
	case ".yaml", ".yml", ".json":
		st.Format, objs = "openapi", parseOpenAPI(b)
	case ".h", ".hpp", ".hh", ".c", ".cc", ".cpp", ".cxx":
		st.Format, objs = "cstruct", parseCStruct(src)
	default:
		return st, fmt.Errorf("unsupported object-model source %q (want .proto, .yaml/.json OpenAPI, .h/.c struct, or .sql)", ext)
	}
	if len(objs) == 0 {
		return st, fmt.Errorf("no objects found in %s (wrong format, or nothing matched)", path)
	}

	return loadObjects(ctx, store, path, st.Format, objs)
}

// loadObjects writes the parsed object model into the graph: one entity per
// object, one REFERENCES relation per resolved field reference.
func loadObjects(ctx context.Context, store *knowledge.Store, path, format string, objs []Object) (Stats, error) {
	st := Stats{Format: format}
	docID := "model:" + format + ":" + baseName(path)

	idOf := func(o Object) string {
		return strings.ToLower(o.Kind) + ":" + strings.ToLower(o.Name)
	}
	known := map[string]string{} // lower(name) -> entity id
	for _, o := range objs {
		known[strings.ToLower(o.Name)] = idOf(o)
	}

	var ents []knowledge.GraphEntity
	batch := map[string]string{}
	for _, o := range objs {
		id := idOf(o)
		desc := o.Desc
		if len(o.Fields) > 0 {
			names := make([]string, 0, len(o.Fields))
			for _, f := range o.Fields {
				names = append(names, f.Name)
			}
			if desc != "" {
				desc += " — "
			}
			desc += "fields: " + strings.Join(names, ", ")
		}
		if desc == "" {
			desc = o.Kind + " " + o.Name
		}
		ents = append(ents, knowledge.GraphEntity{
			ID: id, Name: o.Name, Type: o.Kind, Description: desc, ChunkIDs: []string{id},
		})
		batch[id] = o.Name + "\n" + desc
		st.Objects++
	}
	if err := store.EmbedBatch(ctx, batch, map[string]string{"document_id": docID, "source": "objectmodel"}); err != nil {
		return st, fmt.Errorf("embed: %w", err)
	}
	if err := store.UpsertEntities(ctx, docID, ents); err != nil {
		return st, fmt.Errorf("upsert objects: %w", err)
	}

	var rels []knowledge.GraphRelation
	seen := map[string]bool{}
	for _, o := range objs {
		from := idOf(o)
		for _, ref := range o.Refs {
			to, ok := known[strings.ToLower(ref)]
			if !ok || to == from {
				continue
			}
			k := from + "\x00" + to
			if seen[k] {
				continue
			}
			seen[k] = true
			rels = append(rels, knowledge.GraphRelation{From: from, To: to, Type: "REFERENCES"})
			st.Refs++
		}
	}
	if err := store.UpsertRelations(ctx, docID, rels); err != nil {
		return st, fmt.Errorf("upsert references: %w", err)
	}
	return st, nil
}

func baseName(path string) string {
	p := strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.LastIndex(p, "."); i > 0 {
		p = p[:i]
	}
	return p
}
