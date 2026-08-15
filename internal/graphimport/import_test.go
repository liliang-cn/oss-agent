package graphimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liliang-cn/oss-agent/internal/domain"
)

func writeGraph(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "knowledge-graph.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func codeVocab(rel ...string) domain.CodeVocabulary {
	return domain.CodeVocabulary{
		EntityTypes:   []string{"file", "function"},
		RelationTypes: rel,
		Resolution:    domain.ResolutionSyntactic,
	}
}

const twoFileGraph = `{
  "name": "demo",
  "nodes": [
    {"id": "file:src/a.go", "type": "file", "name": "a.go", "filePath": "src/a.go", "summary": "A"},
    {"id": "function:src/a.go:Run", "type": "function", "name": "Run", "filePath": "src/a.go", "summary": "runs"}
  ],
  "edges": [
    {"source": "file:src/a.go", "target": "function:src/a.go:Run", "type": "contains", "direction": "forward", "weight": 0.9}
  ]
}`

// Understand-Anything forbids putting the project name in a node id, so every
// repo it analyses produces `file:src/a.go`. Importing two repos into one graph
// would collide silently: the second repo's file would land on the first repo's
// node. The namespace has to be added here because upstream will not add it.
func TestPlanNamespacesIDsByRepo(t *testing.T) {
	p, err := Plan(writeGraph(t, twoFileGraph), "linstor-server", codeVocab("contains"))
	if err != nil {
		t.Fatal(err)
	}

	want := "linstor-server/file:src/a.go"
	var found bool
	for _, e := range p.Entities {
		if e.ID == want {
			found = true
			if e.Metadata["source_id"] != "file:src/a.go" {
				t.Errorf("original id must be kept for traceability, got %q", e.Metadata["source_id"])
			}
			if e.Metadata["repo"] != "linstor-server" {
				t.Errorf("repo = %q", e.Metadata["repo"])
			}
		}
	}
	if !found {
		t.Fatalf("no entity with namespaced id %q; got %v", want, ids(p))
	}

	// Edges must be rewritten with the same namespace or they dangle.
	for _, r := range p.Relations {
		if !strings.HasPrefix(r.From, "linstor-server/") || !strings.HasPrefix(r.To, "linstor-server/") {
			t.Fatalf("edge endpoints not namespaced: %s -> %s", r.From, r.To)
		}
	}
}

func TestPlanKeepsTwoReposApart(t *testing.T) {
	a, err := Plan(writeGraph(t, twoFileGraph), "repo-a", codeVocab("contains"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Plan(writeGraph(t, twoFileGraph), "repo-b", codeVocab("contains"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Entities[0].ID == b.Entities[0].ID {
		t.Fatalf("two repos produced the same node id %q", a.Entities[0].ID)
	}
	if a.DocID == b.DocID {
		t.Fatalf("two repos produced the same document id %q", a.DocID)
	}
}

// A vocabulary that does not declare an edge type must stop the whole import,
// not quietly drop the edges. A graph that is half-written is worse than one
// that refused: nothing tells you which half.
func TestPlanRejectsBatchOnUndeclaredEdgeType(t *testing.T) {
	_, err := Plan(writeGraph(t, twoFileGraph), "demo", codeVocab("calls"))
	if err == nil {
		t.Fatal("expected an undeclared edge type to refuse the import")
	}
	if !strings.Contains(err.Error(), "contains") {
		t.Errorf("error must name the offending type, got %v", err)
	}
}

// The report is the point: it tells you exactly what to add to domain.toml.
func TestRejectionReportIsAHistogram(t *testing.T) {
	body := `{
  "name": "demo",
  "nodes": [
    {"id": "file:a.go", "type": "file", "name": "a.go", "summary": ""},
    {"id": "file:b.go", "type": "file", "name": "b.go", "summary": ""}
  ],
  "edges": [
    {"source": "file:a.go", "target": "file:b.go", "type": "imports"},
    {"source": "file:b.go", "target": "file:a.go", "type": "imports"},
    {"source": "file:a.go", "target": "file:b.go", "type": "implements"}
  ]
}`
	_, err := Plan(writeGraph(t, body), "demo", codeVocab())

	var rej *RejectedError
	if !asRejected(err, &rej) {
		t.Fatalf("expected a RejectedError, got %v", err)
	}
	if rej.EdgeTypes["imports"] != 2 || rej.EdgeTypes["implements"] != 1 {
		t.Errorf("histogram = %v, want imports:2 implements:1", rej.EdgeTypes)
	}
	if !strings.Contains(rej.Error(), "imports (2)") {
		t.Errorf("message should carry the counts, got %q", rej.Error())
	}
}

// Upstream's weight is a constant the prompt tells the model to emit (implements
// is always 0.9), so it says nothing about this edge. Carrying it into the graph
// as a weight would dress a guess up as a measurement.
func TestPlanDropsUpstreamWeightAndRecordsResolution(t *testing.T) {
	p, err := Plan(writeGraph(t, twoFileGraph), "demo", codeVocab("contains"))
	if err != nil {
		t.Fatal(err)
	}
	r := p.Relations[0]
	if r.Weight != 0 {
		t.Errorf("weight = %v, want 0 — upstream's number is a prompt constant", r.Weight)
	}
	if r.Metadata["resolution"] != domain.ResolutionSyntactic {
		t.Errorf("resolution = %q, want syntactic", r.Metadata["resolution"])
	}
	if r.Metadata["direction"] != "forward" {
		t.Errorf("direction should survive, got %q", r.Metadata["direction"])
	}
}

// Node types are gated too, and by the same rule.
func TestPlanRejectsUndeclaredNodeType(t *testing.T) {
	body := `{"name":"demo","nodes":[{"id":"service:x","type":"service","name":"x","summary":""}],"edges":[]}`
	_, err := Plan(writeGraph(t, body), "demo", codeVocab())
	var rej *RejectedError
	if !asRejected(err, &rej) {
		t.Fatalf("expected rejection, got %v", err)
	}
	if rej.NodeTypes["service"] != 1 {
		t.Errorf("node histogram = %v", rej.NodeTypes)
	}
}

// Layers and tour steps are oss-agent's own scaffolding, not upstream code
// types, so they are exempt from the code vocabulary — but they still have to
// be namespaced or two repos' layers collide.
func TestPlanNamespacesLayersAndTour(t *testing.T) {
	body := `{
  "name": "demo",
  "nodes": [{"id": "file:a.go", "type": "file", "name": "a.go", "summary": ""}],
  "edges": [],
  "layers": [{"id": "layer:core", "name": "Core", "description": "d", "nodeIds": ["file:a.go"]}],
  "tour": [{"order": 1, "title": "Start", "description": "d", "nodeIds": ["file:a.go"]}]
}`
	p, err := Plan(writeGraph(t, body), "demo", codeVocab())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.Entities {
		if !strings.HasPrefix(e.ID, "demo/") {
			t.Fatalf("entity %q is not namespaced", e.ID)
		}
	}
	for _, r := range p.Relations {
		if !strings.HasPrefix(r.From, "demo/") || !strings.HasPrefix(r.To, "demo/") {
			t.Fatalf("membership edge not namespaced: %s -> %s", r.From, r.To)
		}
	}
}

// An edge whose endpoint is not a node in this graph would create a phantom
// node on upsert. Upstream warns its own assembly step auto-corrects ids and
// may rewire edges, so this is not hypothetical.
func TestPlanRejectsDanglingEdge(t *testing.T) {
	body := `{
  "name": "demo",
  "nodes": [{"id": "file:a.go", "type": "file", "name": "a.go", "summary": ""}],
  "edges": [{"source": "file:a.go", "target": "file:ghost.go", "type": "imports"}]
}`
	_, err := Plan(writeGraph(t, body), "demo", codeVocab("imports"))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected a dangling edge to be refused, got %v", err)
	}
}

func ids(p *ImportPlan) []string {
	out := make([]string, 0, len(p.Entities))
	for _, e := range p.Entities {
		out = append(out, e.ID)
	}
	return out
}
