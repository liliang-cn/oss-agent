// Package knowledge is the GraphRAG knowledge base over a project's source and docs.
// It wraps cortexdb: documents are chunked, embedded, and linked into a graph;
// retrieval is vector + graph expansion.
package knowledge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
	"github.com/liliang-cn/cortexdb/v2/pkg/core"

	"github.com/liliang-cn/oss-agent/internal/extract"
)

// searchReranker is the second-stage reranker applied to the over-fetched
// candidate set: it boosts chunks whose text contains the query terms, pushing
// lexically on-topic results above purely vector-similar ones.
var searchReranker = core.NewKeywordMatchReranker(0.3)

// relevanceFloor keeps a retrieved chunk only if its reranked score is at least
// this fraction of the top chunk's score — a query-relative relevance gate.
const relevanceFloor = 0.6

// pruneByRelevance drops candidates below relevanceFloor × the best score and caps
// the result at n, preserving the reranked order. Empty/zero-score inputs pass through.
func pruneByRelevance(in []core.ScoredEmbedding, ratio float64, n int) []core.ScoredEmbedding {
	if len(in) == 0 {
		return in
	}
	best := in[0].Score
	for _, r := range in {
		if r.Score > best {
			best = r.Score
		}
	}
	floor := best * ratio
	out := make([]core.ScoredEmbedding, 0, n)
	for _, r := range in {
		if r.Score >= floor {
			out = append(out, r)
			if len(out) >= n {
				break
			}
		}
	}
	return out
}

// extractConcurrency bounds parallel LLM ontology-extraction calls during
// ingest. The gateway permits ~10 concurrent requests; sequential extraction is
// otherwise the dominant ingest cost.
const extractConcurrency = 10

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
		Metadata:   map[string]string{"source": "text-knowledge"},
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
	// Two-stage retrieval: hybrid (vector + text) recall over a wide candidate pool,
	// reranked, then pruned to the chunks within relevanceFloor of the top reranked
	// score and capped at topK. The floor is *query-relative*: raw scores vary ~3x
	// across queries (and decay smoothly), so an absolute MinScore either keeps
	// everything or empties the result — a relative floor drops the off-topic tail
	// without that risk, raising the precision of the agent's context.
	candidates, err := s.db.HybridSearchTextWithOptions(ctx, query, cortexdb.TextSearchOptions{
		TopK:     topK * 4, // over-fetch so the floor has a tail to trim
		Reranker: searchReranker,
	})
	if err != nil {
		return nil, err
	}
	results := pruneByRelevance(candidates, relevanceFloor, topK)
	res := &GraphResult{}
	// The graph stores nodes under cortexdb-normalized "entity:" ids, while the
	// embedding/chunk id is the raw node id (e.g. "function:pkg/main.c:foo").
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
			eid := cortexdb.EntityNodeID(r.ID)
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

// PurgeSource removes every embedding belonging to a source, plus (for code
// sources) the derived graph entity nodes and — via FK cascade — their edges.
// The source is identified by its embedding metadata document_id: exact match, or
// a prefix match (text sources store one document_id per file under "<name>/...").
// Catches deletions too: it enumerates what's actually in the DB, not the source.
func (s *Store) PurgeSource(ctx context.Context, docMatch string, prefix bool) (embCount, nodeCount int, err error) {
	sqldb := s.db.SQL()
	q := `SELECT id FROM embeddings WHERE json_extract(metadata,'$.document_id') = ?`
	arg := docMatch
	if prefix {
		q = `SELECT id FROM embeddings WHERE json_extract(metadata,'$.document_id') LIKE ?`
		arg = docMatch + "%"
	}
	rows, err := sqldb.QueryContext(ctx, q, arg)
	if err != nil {
		return 0, 0, fmt.Errorf("query source ids: %w", err)
	}
	var embIDs []string
	nodeSet := make(map[string]struct{})
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return 0, 0, scanErr
		}
		embIDs = append(embIDs, id)
		nodeSet[cortexdb.EntityNodeID(id)] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(embIDs) == 0 {
		return 0, 0, nil
	}
	if err := s.db.Vector().DeleteBatch(ctx, embIDs); err != nil {
		return 0, 0, fmt.Errorf("delete embeddings: %w", err)
	}
	nodeIDs := make([]string, 0, len(nodeSet))
	for id := range nodeSet {
		nodeIDs = append(nodeIDs, id)
	}
	res, derr := s.db.Graph().DeleteNodesBatch(ctx, nodeIDs)
	if derr != nil {
		return len(embIDs), 0, fmt.Errorf("delete graph nodes: %w", derr)
	}
	return len(embIDs), res.SuccessCount, nil
}

// ── graph view (for the knowledge-graph explorer UI) ──

// GraphViewNode / GraphViewEdge / GraphView are a renderable subgraph.
type GraphViewNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
	File  string `json:"file,omitempty"`
	Seed  bool   `json:"seed,omitempty"`
}
type GraphViewEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}
type GraphView struct {
	Nodes []GraphViewNode `json:"nodes"`
	Edges []GraphViewEdge `json:"edges"`
}

const graphViewMaxNodes = 60

// graphExploreEdgeTypes widens expansion for the explorer to include the
// uppercase DOMAIN relations (from LLM ontology extraction) alongside the code
// edges, so concept subgraphs (ResourceGroup ─CONTAINS→ Resource) are traversed.
var graphExploreEdgeTypes = append(append([]string{}, codeEdgeTypes...),
	"CONTAINS", "MANAGES", "DEPLOYED_ON", "DEPENDS_ON", "RESOLVES",
	"DEFINED_IN", "TRIGGERED_BY", "MUTUALLY_EXCLUSIVE", "TRIGGERED_IN_STATE",
	"REFERENCES") // REFERENCES = SQL foreign keys (object model from schema import)

// AllGraph returns the entire knowledge graph (all entity nodes + edges) for the
// full-graph explorer view. Reads directly via SQL since there's no query seed.
func (s *Store) AllGraph(ctx context.Context) (*GraphView, error) {
	gv := &GraphView{}
	sqldb := s.db.SQL()
	rows, err := sqldb.QueryContext(ctx, `SELECT id, content, node_type, properties FROM graph_nodes`)
	if err != nil {
		return nil, fmt.Errorf("query nodes: %w", err)
	}
	have := make(map[string]struct{})
	for rows.Next() {
		var id, content, ntype, props string
		if err := rows.Scan(&id, &content, &ntype, &props); err != nil {
			rows.Close()
			return nil, err
		}
		n := GraphViewNode{ID: id, Label: content, Type: ntype}
		if props != "" {
			var p map[string]interface{}
			if json.Unmarshal([]byte(props), &p) == nil {
				if v, ok := p["name"].(string); ok && v != "" {
					n.Label = v
				}
				if v, ok := p["file_path"].(string); ok {
					n.File = v
				}
			}
		}
		gv.Nodes = append(gv.Nodes, n)
		have[id] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	erows, err := sqldb.QueryContext(ctx, `SELECT from_node_id, to_node_id, edge_type FROM graph_edges`)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var from, to, etype string
		if err := erows.Scan(&from, &to, &etype); err != nil {
			return nil, err
		}
		if _, ok := have[from]; !ok {
			continue
		}
		if _, ok := have[to]; !ok {
			continue
		}
		gv.Edges = append(gv.Edges, GraphViewEdge{Source: from, Target: to, Type: etype})
	}
	return gv, erows.Err()
}

// QueryGraph returns a small subgraph seeded by a search query: the matched code
// entities plus their one-hop code-edge neighbors (nodes + edges), for the explorer.
func (s *Store) QueryGraph(ctx context.Context, query string, topK int) (*GraphView, error) {
	if topK <= 0 {
		topK = 6
	}
	results, err := s.db.HybridSearchText(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	seedSet := make(map[string]struct{})
	for _, r := range results { // vector hits → code entities
		if r.ID != "" {
			seedSet[cortexdb.EntityNodeID(r.ID)] = struct{}{}
		}
	}
	// Also seed by graph-node name: domain-concept entities (from LLM ontology
	// extraction) have no embeddings, so vector search misses them — match by label.
	if rows, e := s.db.SQL().QueryContext(ctx,
		`SELECT id FROM graph_nodes WHERE lower(content) LIKE ?
		 ORDER BY CASE WHEN node_type IN
		   ('Table','Resource','ResourceGroup','Volume','StoragePool','Node','Cluster','ErrorCode','State','CLI_Command','ConfigParameter','KernelModule')
		   THEN 0 ELSE 1 END
		 LIMIT 20`,
		"%"+strings.ToLower(query)+"%"); e == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				seedSet[id] = struct{}{}
			}
		}
		rows.Close()
	}
	if len(seedSet) == 0 {
		return &GraphView{}, nil
	}
	seeds := make([]string, 0, len(seedSet))
	for id := range seedSet {
		seeds = append(seeds, id)
	}
	exp, err := s.tb.ExpandGraph(ctx, cortexdb.ToolExpandGraphRequest{
		NodeIDs: seeds, MaxHops: 1, EdgeTypes: graphExploreEdgeTypes, Limit: 8,
	})
	if err != nil {
		return nil, err
	}
	return graphViewFrom(exp, seedSet), nil
}

// ExpandNode returns the one-hop neighborhood of a single graph node (click-to-expand).
func (s *Store) ExpandNode(ctx context.Context, id string) (*GraphView, error) {
	exp, err := s.tb.ExpandGraph(ctx, cortexdb.ToolExpandGraphRequest{
		NodeIDs: []string{id}, MaxHops: 1, EdgeTypes: graphExploreEdgeTypes, Limit: 14,
	})
	if err != nil {
		return nil, err
	}
	return graphViewFrom(exp, map[string]struct{}{id: {}}), nil
}

func graphViewFrom(exp *cortexdb.ToolExpandGraphResponse, seedSet map[string]struct{}) *GraphView {
	gv := &GraphView{}
	have := make(map[string]struct{})
	for _, n := range exp.Nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if len(gv.Nodes) >= graphViewMaxNodes {
			break
		}
		node := GraphViewNode{ID: n.ID, Label: n.Content, Type: n.NodeType}
		if p := n.Properties; p != nil {
			if v, ok := p["name"].(string); ok && v != "" {
				node.Label = v
			}
			if v, ok := p["file_path"].(string); ok {
				node.File = v
			}
		}
		if _, ok := seedSet[n.ID]; ok {
			node.Seed = true
		}
		gv.Nodes = append(gv.Nodes, node)
		have[n.ID] = struct{}{}
	}
	for _, e := range exp.Edges {
		if _, ok := have[e.FromNodeID]; !ok {
			continue
		}
		if _, ok := have[e.ToNodeID]; !ok {
			continue
		}
		gv.Edges = append(gv.Edges, GraphViewEdge{Source: e.FromNodeID, Target: e.ToNodeID, Type: e.EdgeType})
	}
	return gv
}

// ── cross-session conversation memory (cortexdb Memory API) ──

// convNamespace buckets all chat turns into one global, cross-session pool so a
// new question can semantically recall relevant turns from any past conversation.
const convNamespace = "conversations"

// ConvTurn is a recalled past conversation turn.
type ConvTurn struct {
	Role      string
	Content   string
	SessionID string
	Score     float64
}

// SaveTurn records one chat turn into the shared conversation memory (embedded for
// semantic recall). Best-effort: callers typically ignore the error.
func (s *Store) SaveTurn(ctx context.Context, sessionID, role, content string) error {
	_, err := s.db.SaveMemory(ctx, cortexdb.MemorySaveRequest{
		MemoryID:  "conv:" + role + ":" + randomID(),
		Scope:     "global",
		Namespace: convNamespace,
		Role:      role,
		Content:   content,
		Metadata:  map[string]any{"session_id": sessionID},
	})
	return err
}

// randomID returns a short random hex id (for memory record keys).
func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "x"
	}
	return hex.EncodeToString(b[:])
}

// RecallTurns semantically recalls relevant past turns across ALL sessions.
func (s *Store) RecallTurns(ctx context.Context, query string, topK int) ([]ConvTurn, error) {
	if topK <= 0 {
		topK = 4
	}
	resp, err := s.db.SearchMemory(ctx, cortexdb.MemorySearchRequest{
		Scope:     "global",
		Namespace: convNamespace,
		Query:     query,
		TopK:      topK,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ConvTurn, 0, len(resp.Results))
	for _, h := range resp.Results {
		sid, _ := h.Memory.Metadata["session_id"].(string)
		out = append(out, ConvTurn{Role: h.Memory.Role, Content: h.Memory.Content, SessionID: sid, Score: h.Score})
	}
	return out, nil
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
	chunks := chunk(content, 1200)
	if len(chunks) == 0 {
		return nil
	}
	// 1. embed + store vectors in one batched call (the embedder sub-batches
	//    to a provider-safe size internally).
	texts := make(map[string]string, len(chunks))
	for i, ch := range chunks {
		texts[fmt.Sprintf("%s#%d", docID, i)] = ch
	}
	if err := s.db.InsertTextBatch(ctx, texts, map[string]string{"document_id": docID, "title": title}); err != nil {
		return fmt.Errorf("embed chunks: %w", err)
	}
	if ex == nil {
		return nil
	}

	// 2. LLM ontology extraction — run concurrently (the gateway permits ~N
	//    parallel requests), since one call per chunk is otherwise the bottleneck.
	type frag struct {
		chunkID string
		tr      *extract.Triples
	}
	sem := make(chan struct{}, extractConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	frags := make([]frag, 0, len(chunks))
	for i, ch := range chunks {
		chunkID := fmt.Sprintf("%s#%d", docID, i)
		wg.Add(1)
		sem <- struct{}{}
		go func(id, text string) {
			defer wg.Done()
			defer func() { <-sem }()
			tr, err := ex.Extract(ctx, text)
			if err != nil || tr == nil {
				return // extraction is best-effort; never fail ingest on it
			}
			mu.Lock()
			frags = append(frags, frag{id, tr})
			mu.Unlock()
		}(chunkID, ch)
	}
	wg.Wait()

	// 3. upsert the extracted ontology (serialized — single SQLite writer).
	for _, f := range frags {
		var ents []cortexdb.ToolEntityInput
		for _, e := range f.tr.Entities {
			if e.Name != "" {
				ents = append(ents, cortexdb.ToolEntityInput{Name: e.Name, Type: e.Type, Description: e.Description, ChunkIDs: []string{f.chunkID}})
			}
		}
		if len(ents) > 0 {
			_, _ = s.tb.UpsertEntities(ctx, cortexdb.ToolUpsertEntitiesRequest{DocumentID: docID, Entities: ents})
		}
		var rels []cortexdb.ToolRelationInput
		for _, r := range f.tr.Relations {
			if r.From != "" && r.To != "" {
				rels = append(rels, cortexdb.ToolRelationInput{From: r.From, To: r.To, Type: r.Type, ChunkIDs: []string{f.chunkID}})
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
