// Package graphimport loads an Understand-Anything knowledge-graph.json into the
// cortexdb knowledge base: node summaries become semantic vectors; nodes/edges
// become the ontology graph; and the architecture layers + guided tour are
// imported as their own nodes with membership edges.
//
// It is not a faithful copy. Three things upstream produces are deliberately not
// carried across, each because taking it at face value would put a claim in the
// graph that nothing behind it supports:
//
//   - Node ids are namespaced by repo. Upstream's own agent instructions forbid
//     including the project name, so every repo yields `file:src/index.ts`.
//   - Edge weight is dropped. It is a constant the extraction prompt dictates
//     (implements is always 0.9), so it measures nothing.
//   - Edge and node types are checked against the domain's code vocabulary, and
//     a file containing any undeclared type is refused whole.
//
// Every edge also carries the resolution it was derived at, because upstream's
// `calls` and `implements` are name matching over a parse tree — for Go, which
// satisfies interfaces implicitly and structurally, `implements` is a guess.
package graphimport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liliang-cn/oss-agent/internal/domain"
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
	Nodes     int
	Edges     int
	Layers    int
	TourSteps int
}

const batchSize = 64

// DocIDFor returns the document_id that Import uses for a knowledge-graph.json
// (so callers can purge the prior import before re-importing). It is
// "ua:<repo>:<name>", where name is the graph's name field or the file basename.
//
// The repo is part of the id for the same reason it is part of every node id:
// two repos analysed by the same tool produce the same graph name often enough
// (both "src", both "main") that sharing a document id would make re-importing
// one silently purge the other.
func DocIDFor(path, repo string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var g graph
	if err := json.Unmarshal(b, &g); err != nil {
		return "", fmt.Errorf("parse graph: %w", err)
	}
	name := g.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return "ua:" + repo + ":" + name, nil
}

// Import reads a knowledge-graph.json, validates it against the domain's code
// vocabulary, and loads it into the store. Nothing is written unless the whole
// file is admissible — see RejectedError for why refusing beats skipping.
func Import(ctx context.Context, store *knowledge.Store, path, repo string, vocab domain.CodeVocabulary) (Stats, error) {
	var st Stats
	p, err := Plan(path, repo, vocab)
	if err != nil {
		return st, err
	}

	sharedMeta := map[string]string{"document_id": p.DocID, "source": "understand", "repo": repo}
	batch := make(map[string]string, batchSize)
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
	for id, text := range p.Embed {
		batch[id] = text
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return st, fmt.Errorf("embed: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return st, fmt.Errorf("embed batch: %w", err)
	}

	if err := store.UpsertEntities(ctx, p.DocID, p.Entities); err != nil {
		return st, fmt.Errorf("upsert entities: %w", err)
	}
	if err := store.UpsertRelations(ctx, p.DocID, p.Relations); err != nil {
		return st, fmt.Errorf("upsert relations: %w", err)
	}

	for _, e := range p.Entities {
		switch e.Type {
		case "layer":
			st.Layers++
		case "tour_step":
			st.TourSteps++
		default:
			st.Nodes++
		}
	}
	st.Edges = len(p.Relations)
	return st, nil
}
