package graphimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
)

// ImportPlan is everything a graph JSON would write, computed before anything is
// written.
//
// Splitting this out of Import is not tidiness: the hard part of importing is
// the contract — which ids, which edge types, what a rejection means — and the
// contract is only testable if deciding it does not require a database and an
// embedding endpoint.
type ImportPlan struct {
	DocID     string
	Repo      string
	Embed     map[string]string
	Entities  []knowledge.GraphEntity
	Relations []knowledge.GraphRelation
}

// RejectedError refuses a whole import and says exactly which types caused it.
//
// Refusing the batch rather than skipping the offending edges is deliberate.
// A partial graph is the worst outcome available: the edges that survived look
// like the whole truth, and nothing in the store records what was dropped. The
// histogram exists so the fix is mechanical — every key is a line to add to
// domain.toml.
type RejectedError struct {
	Repo      string
	EdgeTypes map[string]int
	NodeTypes map[string]int
}

func (e *RejectedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "import of %q refused: the code vocabulary does not declare ", e.Repo)
	if len(e.NodeTypes) > 0 {
		fmt.Fprintf(&b, "node types %s", histogram(e.NodeTypes))
	}
	if len(e.NodeTypes) > 0 && len(e.EdgeTypes) > 0 {
		b.WriteString(" and ")
	}
	if len(e.EdgeTypes) > 0 {
		fmt.Fprintf(&b, "relation types %s", histogram(e.EdgeTypes))
	}
	b.WriteString(". Declare them under [vocabulary.code] in domain.toml, or drop them upstream.")
	return b.String()
}

func histogram(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s (%d)", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func asRejected(err error, target **RejectedError) bool { return errors.As(err, target) }

// namespace prefixes an upstream node id with the repo it came from.
//
// Understand-Anything's id convention is `<type>:<relative-path>[:<symbol>]`,
// and its own agent instructions forbid including the project name. That is
// right for a single-repo graph and wrong for a shared store: two repos both
// produce `file:src/index.ts`, and the second import lands on the first repo's
// node. Upstream will not fix this — the namespace is ours to add.
func namespace(repo, id string) string { return repo + "/" + id }

// Plan reads a knowledge-graph.json and decides what it would write, refusing
// the whole file if anything in it falls outside the declared code vocabulary.
func Plan(path, repo string, vocab domain.CodeVocabulary) (*ImportPlan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var g graph
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("parse graph: %w", err)
	}
	if strings.TrimSpace(repo) == "" {
		return nil, errors.New("import needs a repo name to namespace node ids with")
	}

	name := g.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), ".json")
	}

	resolution := vocab.Resolution
	if resolution == "" {
		resolution = domain.ResolutionSyntactic
	}

	rej := &RejectedError{Repo: repo, EdgeTypes: map[string]int{}, NodeTypes: map[string]int{}}
	allowed := map[string]bool{}
	for _, t := range vocab.EntityTypes {
		allowed[t] = true
	}
	allowedRel := map[string]bool{}
	for _, t := range vocab.RelationTypes {
		allowedRel[t] = true
	}

	p := &ImportPlan{
		DocID: "ua:" + repo + ":" + name,
		Repo:  repo,
		Embed: map[string]string{},
	}

	// 1. code nodes
	known := map[string]bool{}
	for _, n := range g.Nodes {
		if n.ID == "" {
			continue
		}
		if !allowed[n.Type] {
			rej.NodeTypes[n.Type]++
			continue
		}
		id := namespace(repo, n.ID)
		known[id] = true

		text := n.Name
		if n.Summary != "" {
			text += "\n" + n.Summary
		}
		p.Embed[id] = text

		meta := map[string]string{
			"repo":       repo,
			"source_id":  n.ID,
			"resolution": resolution,
		}
		if n.FilePath != "" {
			meta["file_path"] = n.FilePath
		}
		if len(n.Tags) > 0 {
			meta["tags"] = strings.Join(n.Tags, ",")
		}
		if n.Complexity != "" {
			meta["complexity"] = n.Complexity
		}
		p.Entities = append(p.Entities, knowledge.GraphEntity{
			ID: id, Name: n.Name, Type: n.Type, Description: n.Summary,
			Metadata: meta, ChunkIDs: []string{id},
		})
	}

	// 2. layers and 3. tour steps are oss-agent's own scaffolding rather than
	// upstream code types, so the code vocabulary does not gate them — but they
	// share the id space and must be namespaced with everything else.
	for _, l := range g.Layers {
		if l.ID == "" {
			continue
		}
		id := namespace(repo, l.ID)
		known[id] = true
		p.Embed[id] = l.Name + "\n" + l.Description
		p.Entities = append(p.Entities, knowledge.GraphEntity{
			ID: id, Name: l.Name, Type: "layer", Description: l.Description,
			Metadata: map[string]string{"repo": repo, "source_id": l.ID},
			ChunkIDs: []string{id},
		})
		for _, nid := range l.NodeIDs {
			p.Relations = append(p.Relations, knowledge.GraphRelation{
				From: id, To: namespace(repo, nid), Type: "layer_contains",
				Metadata: map[string]string{"repo": repo},
			})
		}
	}
	for _, t := range g.Tour {
		id := namespace(repo, fmt.Sprintf("tour:%d", t.Order))
		known[id] = true
		p.Embed[id] = t.Title + "\n" + t.Description
		p.Entities = append(p.Entities, knowledge.GraphEntity{
			ID: id, Name: t.Title, Type: "tour_step", Description: t.Description,
			Metadata: map[string]string{"repo": repo, "order": strconv.Itoa(t.Order)},
			ChunkIDs: []string{id},
		})
		for _, nid := range t.NodeIDs {
			p.Relations = append(p.Relations, knowledge.GraphRelation{
				From: id, To: namespace(repo, nid), Type: "tour_covers",
				Metadata: map[string]string{"repo": repo},
			})
		}
	}

	// 4. code edges
	var dangling []string
	for _, e := range g.Edges {
		if e.Source == "" || e.Target == "" {
			continue
		}
		if !allowedRel[e.Type] {
			rej.EdgeTypes[e.Type]++
			continue
		}
		from, to := namespace(repo, e.Source), namespace(repo, e.Target)
		if !known[from] {
			dangling = append(dangling, e.Source)
			continue
		}
		if !known[to] {
			dangling = append(dangling, e.Target)
			continue
		}
		meta := map[string]string{"repo": repo, "resolution": resolution}
		if e.Direction != "" {
			meta["direction"] = e.Direction
		}
		// Upstream's weight is a constant its prompt tells the model to emit
		// (implements is always 0.9), so it measures nothing about this edge.
		// Leaving it at zero keeps a guess from reading as a score.
		p.Relations = append(p.Relations, knowledge.GraphRelation{
			From: from, To: to, Type: e.Type, Metadata: meta,
		})
	}

	if len(rej.NodeTypes) > 0 || len(rej.EdgeTypes) > 0 {
		return nil, rej
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		return nil, fmt.Errorf("import of %q refused: %d edge endpoint(s) name nodes this graph does not define, e.g. %q",
			repo, len(dangling), dangling[0])
	}
	return p, nil
}
