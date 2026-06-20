// Package schemaimport extracts a precise object model from a SQL schema (DDL):
// CREATE TABLE → entity, FOREIGN KEY … REFERENCES → relation. Deterministic, no
// LLM — the database schema *is* the ground-truth ontology. Product-agnostic:
// point it at any project's schema.
package schemaimport

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/liliang-cn/oss-agent/internal/knowledge"
)

var (
	reTable = regexp.MustCompile(`(?is)CREATE TABLE\s+` + "`?" + `(\w+)` + "`?" + `\s*\((.*?)\)\s*;`)
	reFK    = regexp.MustCompile(`(?is)ALTER TABLE\s+` + "`?" + `(\w+)` + "`?" + `\s+ADD CONSTRAINT\s+\w+\s+FOREIGN KEY\s*\([^)]*\)\s*REFERENCES\s+` + "`?" + `(\w+)`)
	reSkip  = regexp.MustCompile(`(?i)^\s*(CONSTRAINT|PRIMARY|FOREIGN|UNIQUE|CHECK|KEY)\b`)
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
	sql := string(b)

	docID := "schema:" + baseName(path)
	known := map[string]string{} // upper table name -> entity id
	var ents []knowledge.GraphEntity
	batch := map[string]string{}

	for _, m := range reTable.FindAllStringSubmatch(sql, -1) {
		name := m[1]
		id := "table:" + strings.ToLower(name)
		cols := columnsOf(m[2])
		desc := "DB table " + name
		if len(cols) > 0 {
			desc += " — columns: " + strings.Join(cols, ", ")
		}
		known[strings.ToUpper(name)] = id
		ents = append(ents, knowledge.GraphEntity{
			ID: id, Name: name, Type: "Table", Description: desc, ChunkIDs: []string{id},
		})
		batch[id] = name + "\n" + desc
		st.Tables++
	}
	if st.Tables == 0 {
		return st, fmt.Errorf("no CREATE TABLE statements found in %s", path)
	}

	// embed entity text (so the schema is searchable) then upsert nodes
	if err := store.EmbedBatch(ctx, batch, map[string]string{"document_id": docID, "source": "schema"}); err != nil {
		return st, fmt.Errorf("embed: %w", err)
	}
	if err := store.UpsertEntities(ctx, docID, ents); err != nil {
		return st, fmt.Errorf("upsert tables: %w", err)
	}

	var rels []knowledge.GraphRelation
	for _, m := range reFK.FindAllStringSubmatch(sql, -1) {
		from, ok1 := known[strings.ToUpper(m[1])]
		to, ok2 := known[strings.ToUpper(m[2])]
		if !ok1 || !ok2 || from == to {
			continue
		}
		rels = append(rels, knowledge.GraphRelation{From: from, To: to, Type: "REFERENCES"})
		st.FKs++
	}
	if err := store.UpsertRelations(ctx, docID, rels); err != nil {
		return st, fmt.Errorf("upsert foreign keys: %w", err)
	}
	return st, nil
}

// columnsOf pulls column names from a CREATE TABLE body (skipping constraints).
func columnsOf(body string) []string {
	var cols []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" || reSkip.MatchString(line) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := strings.Trim(fields[0], "`\"")
		if name != "" && len(cols) < 24 {
			cols = append(cols, name)
		}
	}
	return cols
}

func baseName(path string) string {
	p := strings.ReplaceAll(path, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".sql")
}
