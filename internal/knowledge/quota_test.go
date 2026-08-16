package knowledge

import (
	"testing"

	"github.com/liliang-cn/cortexdb/v2/pkg/core"
)

// Scores mirror what the live store produces: code and prose land in the same
// narrow band (~0.016 measured), which is why count alone decided the ranking.
func codeChunk(id string, score float64) core.ScoredEmbedding {
	return core.ScoredEmbedding{
		Embedding: core.Embedding{
			ID:       id,
			Metadata: map[string]string{"document_id": "ua:sds:knowledge-graph", "source": "understand"},
		},
		Score: score,
	}
}

func proseChunk(id string, score float64) core.ScoredEmbedding {
	return core.ScoredEmbedding{
		Embedding: core.Embedding{
			ID:       id,
			Metadata: map[string]string{"document_id": "ops-" + id},
		},
		Score: score,
	}
}

func idsOf(in []core.ScoredEmbedding) []string {
	out := make([]string, 0, len(in))
	for _, r := range in {
		out = append(out, r.ID)
	}
	return out
}

func proseOf(in []core.ScoredEmbedding) []core.ScoredEmbedding {
	out := make([]core.ScoredEmbedding, 0, len(in))
	for _, r := range in {
		if !isCodeSource(r.Metadata, r.Metadata["document_id"]) {
			out = append(out, r)
		}
	}
	return out
}

func codeOf(in []core.ScoredEmbedding) []core.ScoredEmbedding {
	out := make([]core.ScoredEmbedding, 0, len(in))
	for _, r := range in {
		if isCodeSource(r.Metadata, r.Metadata["document_id"]) {
			out = append(out, r)
		}
	}
	return out
}

func countCode(in []core.ScoredEmbedding) int {
	n := 0
	for _, r := range in {
		if isCodeSource(r.Metadata, r.Metadata["document_id"]) {
			n++
		}
	}
	return n
}

// The regression this exists for: importing one repository's code graph put
// ~1000 code nodes against a few hundred prose chunks, and pure relevance order
// handed every top-5 slot to code. Asked how to recover from a node losing
// power, the store answered with the struct that models quorum.
func TestSourceQuotaKeepsProseWhenCodeDominates(t *testing.T) {
	// Relevance order as retrieval produced it: code sweeps the head, the
	// runbooks that actually answer the question sit in the tail.
	pool := []core.ScoredEmbedding{
		codeChunk("c1", 0.026), codeChunk("c2", 0.024), codeChunk("c3", 0.022),
		codeChunk("c4", 0.021), codeChunk("c5", 0.020), codeChunk("c6", 0.019),
		proseChunk("runbook-a", 0.018), proseChunk("runbook-b", 0.017), proseChunk("runbook-c", 0.016),
	}
	got := mergeByQuota(proseOf(pool), codeOf(pool), 6)
	if len(got) != 6 {
		t.Fatalf("kept %d results, want 6: %v", len(got), idsOf(got))
	}
	if n := countCode(got); n != 3 {
		t.Fatalf("code share = %d/6, want 3 — the quota did not bind: %v", n, idsOf(got))
	}
	// The prose that was promoted must be the most relevant prose, not any prose.
	want := []string{"c1", "c2", "c3", "runbook-a", "runbook-b", "runbook-c"}
	for i, id := range idsOf(got) {
		if id != want[i] {
			t.Fatalf("result order = %v, want %v", idsOf(got), want)
		}
	}
}

// A question about the code base is still answered from the code base: with no
// prose to promote, the quota releases rather than shrinking the answer.
func TestSourceQuotaReleasesWhenOnlyCodeMatches(t *testing.T) {
	pool := []core.ScoredEmbedding{
		codeChunk("c1", 0.026), codeChunk("c2", 0.024), codeChunk("c3", 0.022),
		codeChunk("c4", 0.021), codeChunk("c5", 0.020), codeChunk("c6", 0.019),
	}
	got := mergeByQuota(nil, pool, 5)
	if len(got) != 5 {
		t.Fatalf("kept %d results, want 5 — an all-code pool must still fill the slots: %v", len(got), idsOf(got))
	}
	if n := countCode(got); n != 5 {
		t.Fatalf("code share = %d/5, want 5", n)
	}
}

// Prose is not capped: the quota bounds code, it does not balance the two.
func TestSourceQuotaDoesNotCapProse(t *testing.T) {
	pool := []core.ScoredEmbedding{
		proseChunk("a", 0.026), proseChunk("b", 0.024), proseChunk("c", 0.022),
		proseChunk("d", 0.021), codeChunk("c1", 0.020),
	}
	got := mergeByQuota(proseOf(pool), codeOf(pool), 4)
	if n := countCode(got); n != 0 {
		t.Fatalf("code share = %d/4, want 0 — prose outranked it: %v", n, idsOf(got))
	}
}

// Graphs imported before the importer wrote `source` metadata are still
// recognised by their document-id prefix.
func TestSourceQuotaRecognisesLegacyCodeDocIDs(t *testing.T) {
	legacy := core.ScoredEmbedding{Embedding: core.Embedding{
		ID:       "legacy",
		Metadata: map[string]string{"document_id": "ua:old-repo:knowledge-graph"},
	}, Score: 0.02}
	if !isCodeSource(legacy.Metadata, legacy.Metadata["document_id"]) {
		t.Fatal("a ua: document id must be recognised as a code source without the source metadata")
	}
}

func TestSourceQuotaPassesThroughSmallResults(t *testing.T) {
	pool := []core.ScoredEmbedding{codeChunk("c1", 0.02)}
	if got := mergeByQuota(nil, pool, 6); len(got) != 1 {
		t.Fatalf("a single candidate must pass through, got %d", len(got))
	}
}
