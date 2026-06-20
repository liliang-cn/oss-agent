// Package graphimport loads an Understand-Anything knowledge-graph.json into the
// cortexdb knowledge base, faithfully: node summaries become semantic vectors;
// nodes/edges become the ontology graph (with filePath/tags/complexity on nodes
// and weight/direction on edges); and the architecture layers + guided tour are
// imported as their own nodes with membership edges.
package graphimport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/liliang-cn/oss-agent/internal/knowledge"
)

// matches Understand-Anything packages/core/src/types.ts
type node struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	FilePath   string   `json:"filePath"`
	Summary    string   `json:"summary"`
	Tags       []string `json:"tags"`
	Complexity string   `json:"complexity"`
}

type edge struct {
	Source    string  `json:"source"`
	Target    string  `json:"target"`
	Type      string  `json:"type"`
	Direction string  `json:"direction"`
	Weight    float64 `json:"weight"`
}

type layer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	NodeIDs     []string `json:"nodeIds"`
}

type tourStep struct {
	Order       int      `json:"order"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	NodeIDs     []string `json:"nodeIds"`
}

type graph struct {
	Name   string     `json:"name"`
	Kind   string     `json:"kind"`
	Nodes  []node     `json:"nodes"`
	Edges  []edge     `json:"edges"`
	Layers []layer    `json:"layers"`
	Tour   []tourStep `json:"tour"`
}

// Stats summarizes an import.
type Stats struct {
	Nodes    int
	Edges    int
	Layers   int
	TourSteps int
}

const batchSize = 64

// Import reads a knowledge-graph.json and loads it (faithfully) into the store.
func Import(ctx context.Context, store *knowledge.Store, path string) (Stats, error) {
	var st Stats
	b, err := os.ReadFile(path)
	if err != nil {
		return st, fmt.Errorf("read %s: %w", path, err)
	}
	var g graph
	if err := json.Unmarshal(b, &g); err != nil {
		return st, fmt.Errorf("parse graph: %w", err)
	}
	name := g.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	docID := "ua:" + name

	var ents []knowledge.GraphEntity
	var rels []knowledge.GraphRelation
	batch := make(map[string]string, batchSize)
	sharedMeta := map[string]string{"document_id": docID, "source": "understand"}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := store.EmbedBatch(ctx, batch, sharedMeta); err != nil {
			return err
		}
		batch = make(map[string]string, batchSize)
		return nil
	}
	add := func(id, text string) error {
		batch[id] = text
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	}

	// 1. code nodes → vectors + entities (with full metadata)
	for _, n := range g.Nodes {
		if n.ID == "" {
			continue
		}
		text := n.Name
		if n.Summary != "" {
			text += "\n" + n.Summary
		}
		if err := add(n.ID, text); err != nil {
			return st, fmt.Errorf("embed: %w", err)
		}
		meta := map[string]string{}
		if n.FilePath != "" {
			meta["file_path"] = n.FilePath
		}
		if len(n.Tags) > 0 {
			meta["tags"] = strings.Join(n.Tags, ",")
		}
		if n.Complexity != "" {
			meta["complexity"] = n.Complexity
		}
		ents = append(ents, knowledge.GraphEntity{
			ID: n.ID, Name: n.Name, Type: n.Type, Description: n.Summary,
			Metadata: meta, ChunkIDs: []string{n.ID},
		})
		st.Nodes++
	}

	// 2. architecture layers → entities + membership edges
	for _, l := range g.Layers {
		if l.ID == "" {
			continue
		}
		if err := add(l.ID, l.Name+"\n"+l.Description); err != nil {
			return st, fmt.Errorf("embed layer: %w", err)
		}
		ents = append(ents, knowledge.GraphEntity{
			ID: l.ID, Name: l.Name, Type: "layer", Description: l.Description, ChunkIDs: []string{l.ID},
		})
		for _, nid := range l.NodeIDs {
			rels = append(rels, knowledge.GraphRelation{From: l.ID, To: nid, Type: "layer_contains"})
		}
		st.Layers++
	}

	// 3. guided tour → entities + coverage edges
	for _, t := range g.Tour {
		tid := fmt.Sprintf("tour:%d", t.Order)
		if err := add(tid, t.Title+"\n"+t.Description); err != nil {
			return st, fmt.Errorf("embed tour: %w", err)
		}
		ents = append(ents, knowledge.GraphEntity{
			ID: tid, Name: t.Title, Type: "tour_step", Description: t.Description,
			Metadata: map[string]string{"order": strconv.Itoa(t.Order)}, ChunkIDs: []string{tid},
		})
		for _, nid := range t.NodeIDs {
			rels = append(rels, knowledge.GraphRelation{From: tid, To: nid, Type: "tour_covers"})
		}
		st.TourSteps++
	}

	if err := flush(); err != nil {
		return st, fmt.Errorf("embed batch: %w", err)
	}
	if err := store.UpsertEntities(ctx, docID, ents); err != nil {
		return st, fmt.Errorf("upsert entities: %w", err)
	}

	// 4. code edges (with weight + direction)
	for _, e := range g.Edges {
		if e.Source == "" || e.Target == "" {
			continue
		}
		var meta map[string]string
		if e.Direction != "" {
			meta = map[string]string{"direction": e.Direction}
		}
		rels = append(rels, knowledge.GraphRelation{From: e.Source, To: e.Target, Type: e.Type, Weight: e.Weight, Metadata: meta})
		st.Edges++
	}
	if err := store.UpsertRelations(ctx, docID, rels); err != nil {
		return st, fmt.Errorf("upsert relations: %w", err)
	}
	return st, nil
}
