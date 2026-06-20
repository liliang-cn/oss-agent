// Package schemaimport extracts a precise object model from a SQL schema (DDL)
// into the knowledge graph: CREATE TABLE → entity, FOREIGN KEY → relation.
// Deterministic, no LLM — the database schema *is* the ground-truth ontology.
// Parsing is delegated to cortexdb's importflow.ParseDDL (a maintained
// Postgres/MySQL DDL parser) rather than a bespoke regex. Product-agnostic.
package schemaimport

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/liliang-cn/cortexdb/v2/pkg/importflow"

	"github.com/liliang-cn/oss-agent/internal/knowledge"
)

// Stats summarizes an import.
type Stats struct {
	Tables int
	FKs    int
}

// Import parses a schema SQL file and loads its tables + foreign keys into the
// knowledge graph (entities of type "Table", relations of type "REFERENCES").
func Import(ctx context.Context, store *knowledge.Store, path string) (Stats, error) {
	var st Stats
	b, err := os.ReadFile(path)
	if err != nil {
		return st, fmt.Errorf("read %s: %w", path, err)
	}
	tables, err := importflow.ParseDDL(string(b))
	if err != nil {
		return st, fmt.Errorf("parse DDL: %w", err)
	}

	docID := "schema:" + baseName(path)
	known := map[string]string{} // upper table name -> entity id
	var ents []knowledge.GraphEntity
	batch := map[string]string{}

	for _, t := range tables {
		id := "table:" + strings.ToLower(t.Name)
		cols := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, c.Name)
		}
		desc := "DB table " + t.Name
		if len(cols) > 0 {
			desc += " — columns: " + strings.Join(cols, ", ")
		}
		known[strings.ToUpper(t.Name)] = id
		ents = append(ents, knowledge.GraphEntity{
			ID: id, Name: t.Name, Type: "Table", Description: desc, ChunkIDs: []string{id},
		})
		batch[id] = t.Name + "\n" + desc
		st.Tables++
	}
	if st.Tables == 0 {
		return st, fmt.Errorf("no CREATE TABLE statements found in %s", path)
	}

	if err := store.EmbedBatch(ctx, batch, map[string]string{"document_id": docID, "source": "schema"}); err != nil {
		return st, fmt.Errorf("embed: %w", err)
	}
	if err := store.UpsertEntities(ctx, docID, ents); err != nil {
		return st, fmt.Errorf("upsert tables: %w", err)
	}

	var rels []knowledge.GraphRelation
	for _, t := range tables {
		from := known[strings.ToUpper(t.Name)]
		for _, fk := range t.ForeignKeys {
			to, ok := known[strings.ToUpper(fk.RefTable)]
			if !ok || to == from {
				continue
			}
			rels = append(rels, knowledge.GraphRelation{From: from, To: to, Type: "REFERENCES"})
			st.FKs++
		}
	}
	if err := store.UpsertRelations(ctx, docID, rels); err != nil {
		return st, fmt.Errorf("upsert foreign keys: %w", err)
	}
	return st, nil
}

func baseName(path string) string {
	p := strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".sql")
}
