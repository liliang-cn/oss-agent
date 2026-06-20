// Package salvage rescues an Understand-Anything run that produced intermediate
// output but never wrote a final knowledge-graph.json (the headless /understand
// step stalls on large repos). It reconstructs an equivalent graph by merging the
// pieces the run already wrote to <repo>/.understand-anything/intermediate/:
//
//	assembled-graph.json   — the post-merge, post-review node/edge set (preferred)
//	batch-*.json           — per-file-analyzer node/edge batches (supplement / fallback)
//	layers-normalized.json — architecture layers (optional)
//
// The merged result is written as knowledge-graph.salvaged.json (shape-compatible
// with graphimport), so the normal import path can consume it unchanged.
package salvage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// node/edge/layer mirror the subset of the Understand-Anything schema we carry
// through. Field tags match both the intermediate files and graphimport's reader.
type node struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Name       string   `json:"name"`
	FilePath   string   `json:"filePath,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Complexity string   `json:"complexity,omitempty"`
}

type edge struct {
	Source    string  `json:"source"`
	Target    string  `json:"target"`
	Type      string  `json:"type"`
	Direction string  `json:"direction,omitempty"`
	Weight    float64 `json:"weight,omitempty"`
}

type layer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	NodeIDs     []string `json:"nodeIds"`
}

type graph struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Nodes  []node  `json:"nodes"`
	Edges  []edge  `json:"edges"`
	Layers []layer `json:"layers,omitempty"`
}

// Stats summarizes what was recovered and from where.
type Stats struct {
	Nodes   int
	Edges   int
	Layers  int
	Sources []string // intermediate files that contributed
}

// Available reports whether a repo has intermediate output to salvage from.
func Available(repoDir string) bool {
	d := filepath.Join(repoDir, ".understand-anything", "intermediate")
	info, err := os.Stat(d)
	return err == nil && info.IsDir()
}

// Salvage merges the intermediate output under repoDir into a single graph file
// and returns its path. The graph's name is the repo folder name, so its import
// document id ("ua:<name>") matches what a clean run would have produced.
func Salvage(repoDir string) (string, Stats, error) {
	var st Stats
	inter := filepath.Join(repoDir, ".understand-anything", "intermediate")
	if !Available(repoDir) {
		return "", st, fmt.Errorf("no intermediate output under %s", inter)
	}

	g := graph{Name: filepath.Base(strings.TrimRight(repoDir, "/")), Kind: "code"}
	nodesByID := map[string]node{}
	edgeSeen := map[string]bool{}

	mergeNodesEdges := func(file string) {
		b, err := os.ReadFile(file)
		if err != nil {
			return
		}
		var part struct {
			Nodes []node `json:"nodes"`
			Edges []edge `json:"edges"`
		}
		if json.Unmarshal(b, &part) != nil {
			return
		}
		contributed := false
		for _, n := range part.Nodes {
			if n.ID == "" {
				continue
			}
			// Keep the richer record if we see the same id twice (assembled wins
			// because it is merged first and has summaries/tags).
			if _, ok := nodesByID[n.ID]; !ok {
				nodesByID[n.ID] = n
				contributed = true
			}
		}
		for _, e := range part.Edges {
			if e.Source == "" || e.Target == "" {
				continue
			}
			k := e.Source + "\x00" + e.Target + "\x00" + e.Type
			if !edgeSeen[k] {
				edgeSeen[k] = true
				g.Edges = append(g.Edges, e)
				contributed = true
			}
		}
		if contributed {
			st.Sources = append(st.Sources, filepath.Base(file))
		}
	}

	// 1. assembled-graph.json first (merged + reviewed = the best source).
	mergeNodesEdges(filepath.Join(inter, "assembled-graph.json"))

	// 2. every batch-*.json (covers a stall before assembly; fills gaps after).
	batches, _ := filepath.Glob(filepath.Join(inter, "batch-*.json"))
	sort.Strings(batches)
	for _, f := range batches {
		mergeNodesEdges(f)
	}

	if len(nodesByID) == 0 {
		return "", st, fmt.Errorf("no nodes recovered from %s (no assembled-graph.json or batch-*.json)", inter)
	}

	// stable node order for a deterministic output file
	ids := make([]string, 0, len(nodesByID))
	for id := range nodesByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g.Nodes = append(g.Nodes, nodesByID[id])
	}
	st.Nodes = len(g.Nodes)
	st.Edges = len(g.Edges)

	// 3. layers (optional). Only keep memberships pointing at recovered nodes.
	if b, err := os.ReadFile(filepath.Join(inter, "layers-normalized.json")); err == nil {
		var ls []layer
		if json.Unmarshal(b, &ls) == nil {
			for _, l := range ls {
				if l.ID == "" {
					continue
				}
				kept := l.NodeIDs[:0]
				for _, nid := range l.NodeIDs {
					if _, ok := nodesByID[nid]; ok {
						kept = append(kept, nid)
					}
				}
				l.NodeIDs = kept
				g.Layers = append(g.Layers, l)
			}
			if len(g.Layers) > 0 {
				st.Layers = len(g.Layers)
				st.Sources = append(st.Sources, "layers-normalized.json")
			}
		}
	}

	out := filepath.Join(repoDir, ".understand-anything", "knowledge-graph.salvaged.json")
	b, err := json.MarshalIndent(g, "", " ")
	if err != nil {
		return "", st, fmt.Errorf("marshal merged graph: %w", err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return "", st, fmt.Errorf("write %s: %w", out, err)
	}
	return out, st, nil
}
