package knowledge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// TestExpandEdgeTypesUsesDeclaredRelationTypes pins the fix: what a domain.toml
// declares is what expansion filters on. Before this, expansion always filtered
// on codeEdgeTypes — a calls/inherits/implements vocabulary no ops corpus emits.
func TestExpandEdgeTypesUsesDeclaredRelationTypes(t *testing.T) {
	s := &Store{}
	WithRelationTypes([]string{"DEPLOYED_ON", "MANAGES"})(s)

	got := s.expandEdgeTypes()
	for _, want := range []string{"DEPLOYED_ON", "MANAGES"} {
		if !hasString(got, want) {
			t.Fatalf("expandEdgeTypes() = %v, missing declared type %q", got, want)
		}
	}
	// The declared vocabulary replaces the fallback rather than joining it: a
	// domain that says what its edges are has said all of it.
	if hasString(got, "inherits") {
		t.Errorf("expandEdgeTypes() = %v, still carries the code fallback", got)
	}
}

// TestExpandEdgeTypesToleratesCasing covers the defence in depth: the filter
// downstream is exact string equality and the edge type in the graph is whatever
// the extracting LLM emitted, so both casings of a declared type are passed.
func TestExpandEdgeTypesToleratesCasing(t *testing.T) {
	s := &Store{}
	WithRelationTypes([]string{"deployed_on"})(s)

	got := s.expandEdgeTypes()
	for _, want := range []string{"deployed_on", "DEPLOYED_ON"} {
		if !hasString(got, want) {
			t.Fatalf("expandEdgeTypes() = %v, missing %q", got, want)
		}
	}
	if len(got) != 2 {
		t.Errorf("expandEdgeTypes() = %v, want exactly the two casings", got)
	}
}

// TestExpandEdgeTypesFallsBackWithoutOntology keeps the no-ontology domain
// behaving exactly as it did before the change.
func TestExpandEdgeTypesFallsBackWithoutOntology(t *testing.T) {
	for name, types := range map[string][]string{
		"nil":   nil,
		"empty": {},
		"blank": {"", "  "},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Store{}
			WithRelationTypes(types)(s)
			got := s.expandEdgeTypes()
			if len(got) != len(codeEdgeTypes) {
				t.Fatalf("expandEdgeTypes() = %v, want the code fallback %v", got, codeEdgeTypes)
			}
			for i, want := range codeEdgeTypes {
				if got[i] != want {
					t.Fatalf("expandEdgeTypes()[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestExploreEdgeTypesUnionsDomainVocabulary checks the explorer widens rather
// than replaces: browsing must still reach the imported code and schema edges.
func TestExploreEdgeTypesUnionsDomainVocabulary(t *testing.T) {
	s := &Store{}
	WithRelationTypes([]string{"PROMOTES"})(s)

	got := s.exploreEdgeTypes()
	for _, want := range []string{"PROMOTES", "REFERENCES", "calls"} {
		if !hasString(got, want) {
			t.Fatalf("exploreEdgeTypes() = %v, missing %q", got, want)
		}
	}
}

// TestDeclaredRelationTypesTraverseRealEdges is the end-to-end version of the
// bug: a graph built from a domain's own relations, walked with that domain's
// own vocabulary. It writes the edge the way ingest does and expands it the way
// SearchGraph does, so it fails against the hardcoded whitelist for the reason
// production did — 0 of 415 edges matched.
func TestDeclaredRelationTypesTraverseRealEdges(t *testing.T) {
	ctx := context.Background()
	db, err := cortexdb.Open(cortexdb.DefaultConfig(filepath.Join(t.TempDir(), "k.db")))
	if err != nil {
		t.Fatalf("open cortexdb: %v", err)
	}
	defer db.Close()
	s := &Store{db: db, tb: db.GraphRAGTools()}
	WithRelationTypes([]string{"deployed_on"})(s)

	const doc = "runbook.md"
	if _, err := s.tb.UpsertEntities(ctx, cortexdb.ToolUpsertEntitiesRequest{
		DocumentID: doc,
		Entities: []cortexdb.ToolEntityInput{
			{Name: "Resource", Type: "Resource"},
			{Name: "Node", Type: "Node"},
		},
	}); err != nil {
		t.Fatalf("upsert entities: %v", err)
	}
	// The declaration is lower_snake and the extractor emitted UPPER_SNAKE —
	// the drift that made a case-sensitive filter match nothing.
	if _, err := s.tb.UpsertRelations(ctx, cortexdb.ToolUpsertRelationsRequest{
		DocumentID: doc,
		Relations:  []cortexdb.ToolRelationInput{{From: "Resource", To: "Node", Type: "DEPLOYED_ON"}},
	}); err != nil {
		t.Fatalf("upsert relations: %v", err)
	}

	seed := cortexdb.EntityNodeID("Resource")
	neighbors := func(edgeTypes []string) []string {
		t.Helper()
		exp, err := s.tb.ExpandGraph(ctx, cortexdb.ToolExpandGraphRequest{
			NodeIDs: []string{seed}, MaxHops: 1, EdgeTypes: edgeTypes, Limit: graphNeighborsPerSeed,
		})
		if err != nil {
			t.Fatalf("expand graph: %v", err)
		}
		var out []string
		for _, n := range exp.Nodes {
			if n != nil && n.ID != seed {
				out = append(out, n.ID)
			}
		}
		return out
	}

	if got := neighbors(codeEdgeTypes); len(got) != 0 {
		t.Fatalf("code fallback reached %v; the test no longer reproduces the bug", got)
	}
	if got := neighbors(s.expandEdgeTypes()); len(got) != 1 {
		t.Fatalf("declared vocabulary reached %v, want the one DEPLOYED_ON neighbour", got)
	}
}

func hasString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
