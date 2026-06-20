// Package knowledge is the GraphRAG knowledge base over DRBD/LINSTOR material.
// It wraps cortexdb: documents are chunked, embedded, and linked into a graph;
// retrieval is vector + graph expansion.
package knowledge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"

	"github.com/liliang-cn/oss-agent/internal/extract"
)

// Store is the GraphRAG knowledge base.
type Store struct {
	db *cortexdb.DB
	tb *cortexdb.GraphRAGToolbox
}

// Open opens (creating if needed) the knowledge DB at path, using the given
// OpenAI-compatible embedder config.
func Open(dbPath, embBaseURL, embAPIKey, embModel string, embDim int) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	emb := NewOpenAIEmbedder(embBaseURL, embAPIKey, embModel, embDim)
	db, err := cortexdb.Open(cortexdb.DefaultConfig(dbPath), cortexdb.WithEmbedder(emb))
	if err != nil {
		return nil, fmt.Errorf("open cortexdb: %w", err)
	}
	return &Store{db: db, tb: db.GraphRAGTools()}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// IngestDoc ingests one document into the GraphRAG store.
func (s *Store) IngestDoc(ctx context.Context, id, title, content string) error {
	_, err := s.tb.IngestDocument(ctx, cortexdb.ToolIngestDocumentRequest{
		DocumentID: id,
		Title:      title,
		Content:    content,
		ChunkSize:  800,
		Metadata:   map[string]string{"source": "linbit-knowledge"},
	})
	return err
}


// Hit is a retrieved knowledge chunk.
type Hit struct {
	DocumentID string
	Content    string
	Score      float64
	Entities   []string
}

// Search runs vector + graph retrieval and returns the top chunks.
func (s *Store) Search(ctx context.Context, query string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 6
	}
	resp, err := s.tb.SearchText(ctx, cortexdb.ToolSearchTextRequest{
		Query:               query,
		TopK:                topK,
		MaxEntitiesPerChunk: 8,
		// graph expansion is on by default (DisableGraph=false)
	})
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(resp.Chunks))
	for _, c := range resp.Chunks {
		hits = append(hits, Hit{DocumentID: c.DocumentID, Content: c.Content, Score: c.Score, Entities: c.Entities})
	}
	return hits, nil
}

// Neighbor is a graph node reached by expanding along code-semantic edges from a
// retrieved hit (one hop). It explains *why* it's related via the edge type.
type Neighbor struct {
	ID       string
	Name     string
	Type     string
	Via      string // edge type that connected it to a seed (e.g. "calls", "contains")
	Summary  string
	FilePath string
}

// GraphResult is hybrid retrieval augmented with one-hop graph expansion.
type GraphResult struct {
	Hits      []Hit
	Neighbors []Neighbor
}

// codeEdgeTypes are the behavioral/structural edges worth following during
// expansion. We skip imports/exports (too many, low signal) and the navigational
// layer_contains/tour_covers/mentions edges.
var codeEdgeTypes = []string{"calls", "contains", "depends_on", "inherits", "implements", "triggers", "deploys", "related"}

const (
	graphNeighborsPerSeed = 6  // cap neighbors pulled per seed hit
	graphNeighborsTotal   = 12 // overall cap on neighbors returned
)

// SearchGraph runs hybrid retrieval, then expands one hop along code-semantic
// edges from the retrieved nodes, returning the hits plus their graph neighbors
// (deduped, seed-excluded, capped). Doc/blog chunks that aren't graph nodes
// simply contribute no neighbors.
func (s *Store) SearchGraph(ctx context.Context, query string, topK int) (*GraphResult, error) {
	if topK <= 0 {
		topK = 6
	}
	results, err := s.db.HybridSearchText(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	res := &GraphResult{}
	// The graph stores nodes under cortexdb-normalized "entity:" ids, while the
	// embedding/chunk id is the raw node id (e.g. "function:drbd/drbd_main.c:foo").
	// Map each seed chunk id to its entity id so expansion can match.
	seeds := make([]string, 0, len(results))
	seedSet := make(map[string]struct{}, len(results))
	for _, r := range results {
		docID := r.Metadata["document_id"]
		if docID == "" {
			docID = r.ID
		}
		res.Hits = append(res.Hits, Hit{DocumentID: docID, Content: r.Content, Score: r.Score})
		if r.ID != "" {
			eid := graphEntityID(r.ID)
			if _, dup := seedSet[eid]; !dup {
				seeds = append(seeds, eid)
				seedSet[eid] = struct{}{}
			}
		}
	}
	if len(seeds) == 0 {
		return res, nil
	}

	exp, err := s.tb.ExpandGraph(ctx, cortexdb.ToolExpandGraphRequest{
		NodeIDs:   seeds,
		MaxHops:   1,
		EdgeTypes: codeEdgeTypes,
		Limit:     graphNeighborsPerSeed,
	})
	if err != nil {
		// expansion is best-effort: fall back to plain hits rather than failing.
		return res, nil
	}

	// edge type that connects a seed to each neighbor (for the "via" explanation)
	via := make(map[string]string)
	for _, e := range exp.Edges {
		if _, ok := seedSet[e.FromNodeID]; ok {
			via[e.ToNodeID] = e.EdgeType
		}
		if _, ok := seedSet[e.ToNodeID]; ok {
			if _, seen := via[e.FromNodeID]; !seen {
				via[e.FromNodeID] = e.EdgeType
			}
		}
	}

	seen := make(map[string]struct{})
	for _, n := range exp.Nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if _, isSeed := seedSet[n.ID]; isSeed {
			continue
		}
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		nb := Neighbor{ID: n.ID, Type: n.NodeType, Via: via[n.ID], Name: n.Content}
		if p := n.Properties; p != nil {
			if v, ok := p["name"].(string); ok && v != "" {
				nb.Name = v
			}
			if v, ok := p["description"].(string); ok {
				nb.Summary = v
			}
			if v, ok := p["file_path"].(string); ok {
				nb.FilePath = v
			}
		}
		res.Neighbors = append(res.Neighbors, nb)
		if len(res.Neighbors) >= graphNeighborsTotal {
			break
		}
	}
	return res, nil
}

// graphEntityID mirrors cortexdb's graphEntityNodeID: lowercase, keep letters and
// digits, map space/-/_ to '_', drop everything else, prefix "entity:". This lets
// us turn a raw chunk/node id into the normalized id under which the graph stores
// the corresponding entity node.
func graphEntityID(raw string) string {
	if strings.HasPrefix(raw, "entity:") {
		return raw
	}
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('_')
		}
	}
	id := strings.Trim(b.String(), "_")
	if id == "" {
		id = "entity"
	}
	return "entity:" + id
}

// chunk splits text into ~size-byte passages on line boundaries.
func chunk(text string, size int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= size {
		return []string{text}
	}
	var out []string
	var b strings.Builder
	for _, ln := range strings.Split(text, "\n") {
		if b.Len()+len(ln)+1 > size && b.Len() > 0 {
			out = append(out, strings.TrimSpace(b.String()))
			b.Reset()
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// IngestSemantic chunks content, stores each chunk as a SEMANTIC vector (via the
// configured embedder), and — when ex != nil — extracts an ontology fragment per
// chunk (entities + relations) into the knowledge graph. Requires an embedder.
func (s *Store) IngestSemantic(ctx context.Context, docID, title, content string, ex *extract.Extractor) error {
	for i, ch := range chunk(content, 1200) {
		chunkID := fmt.Sprintf("%s#%d", docID, i)
		meta := map[string]string{"document_id": docID, "title": title, "chunk": fmt.Sprintf("%d", i)}
		if err := s.db.InsertText(ctx, chunkID, ch, meta); err != nil {
			return fmt.Errorf("embed chunk %s: %w", chunkID, err)
		}
		if ex == nil {
			continue
		}
		tr, err := ex.Extract(ctx, ch)
		if err != nil || tr == nil { // extraction is best-effort; never fail ingest on it
			continue
		}
		var ents []cortexdb.ToolEntityInput
		for _, e := range tr.Entities {
			if e.Name != "" {
				ents = append(ents, cortexdb.ToolEntityInput{Name: e.Name, Type: e.Type, Description: e.Description, ChunkIDs: []string{chunkID}})
			}
		}
		if len(ents) > 0 {
			_, _ = s.tb.UpsertEntities(ctx, cortexdb.ToolUpsertEntitiesRequest{DocumentID: docID, Entities: ents})
		}
		var rels []cortexdb.ToolRelationInput
		for _, r := range tr.Relations {
			if r.From != "" && r.To != "" {
				rels = append(rels, cortexdb.ToolRelationInput{From: r.From, To: r.To, Type: r.Type, ChunkIDs: []string{chunkID}})
			}
		}
		if len(rels) > 0 {
			_, _ = s.tb.UpsertRelations(ctx, cortexdb.ToolUpsertRelationsRequest{DocumentID: docID, Relations: rels})
		}
	}
	return nil
}

// SearchSemantic runs hybrid (vector + keyword) retrieval. With an embedder it is
// semantic + lexical fused; without one cortexdb falls back to FTS5/BM25.
func (s *Store) SearchSemantic(ctx context.Context, query string, topK int) ([]Hit, error) {
	if topK <= 0 {
		topK = 6
	}
	results, err := s.db.HybridSearchText(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(results))
	for _, r := range results {
		docID := r.Metadata["document_id"]
		if docID == "" {
			docID = r.ID
		}
		hits = append(hits, Hit{DocumentID: docID, Content: r.Content, Score: r.Score})
	}
	return hits, nil
}

// ── External graph import (e.g. Understand-Anything knowledge-graph.json) ──

// GraphEntity is one node to import. ID is the stable key (the source graph's
// node id) so edges resolve; Name is the human label (becomes graph content).
type GraphEntity struct {
	ID          string
	Name        string
	Type        string
	Description string
	Metadata    map[string]string
	ChunkIDs    []string
}

// GraphRelation is one edge to import (From/To are entity IDs = node ids).
type GraphRelation struct {
	From     string
	To       string
	Type     string
	Weight   float64
	Metadata map[string]string
}

// EmbedText stores one text under id with a semantic vector (no chunking).
func (s *Store) EmbedText(ctx context.Context, id, text string, meta map[string]string) error {
	return s.db.InsertText(ctx, id, text, meta)
}

// EmbedBatch stores a batch of id→text in a single embedder call (one HTTP
// request for the whole batch), sharing meta across the batch.
func (s *Store) EmbedBatch(ctx context.Context, texts map[string]string, meta map[string]string) error {
	return s.db.InsertTextBatch(ctx, texts, meta)
}

// UpsertEntities writes graph entity nodes (keyed by Name).
func (s *Store) UpsertEntities(ctx context.Context, docID string, ents []GraphEntity) error {
	if len(ents) == 0 {
		return nil
	}
	in := make([]cortexdb.ToolEntityInput, 0, len(ents))
	for _, e := range ents {
		in = append(in, cortexdb.ToolEntityInput{ID: e.ID, Name: e.Name, Type: e.Type, Description: e.Description, Metadata: e.Metadata, ChunkIDs: e.ChunkIDs})
	}
	_, err := s.tb.UpsertEntities(ctx, cortexdb.ToolUpsertEntitiesRequest{DocumentID: docID, Entities: in})
	return err
}

// UpsertRelations writes graph edges (From/To are entity Names).
func (s *Store) UpsertRelations(ctx context.Context, docID string, rels []GraphRelation) error {
	if len(rels) == 0 {
		return nil
	}
	in := make([]cortexdb.ToolRelationInput, 0, len(rels))
	for _, r := range rels {
		in = append(in, cortexdb.ToolRelationInput{From: r.From, To: r.To, Type: r.Type, Weight: r.Weight, Metadata: r.Metadata})
	}
	_, err := s.tb.UpsertRelations(ctx, cortexdb.ToolUpsertRelationsRequest{DocumentID: docID, Relations: in})
	return err
}
