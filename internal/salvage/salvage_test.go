package salvage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSalvageRealData runs the merge against a real Understand-Anything run if one
// is present in the working tree (skipped in CI / clean checkouts).
func TestSalvageRealData(t *testing.T) {
	repo := "../../repos/drbd-utils"
	if !Available(repo) {
		t.Skip("no real intermediate output at " + repo)
	}
	_, st, err := Salvage(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes < 100 || st.Edges < 100 {
		t.Errorf("suspiciously small salvage: %d nodes, %d edges", st.Nodes, st.Edges)
	}
	t.Logf("real-data salvage: %d nodes, %d edges, %d layers from %v", st.Nodes, st.Edges, st.Layers, st.Sources)
	_ = os.Remove(filepath.Join(repo, ".understand-anything", "knowledge-graph.salvaged.json"))
}

func TestSalvageMerge(t *testing.T) {
	repo := t.TempDir()
	inter := filepath.Join(repo, ".understand-anything", "intermediate")
	if err := os.MkdirAll(inter, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(inter, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// assembled has A→B; batch adds C and a duplicate A and a new edge B→C.
	write("assembled-graph.json", `{"nodes":[
		{"id":"a","name":"A","summary":"node a"},
		{"id":"b","name":"B"}],
		"edges":[{"source":"a","target":"b","type":"calls"}]}`)
	write("batch-1.json", `{"nodes":[
		{"id":"a","name":"A-dup"},
		{"id":"c","name":"C"}],
		"edges":[{"source":"b","target":"c","type":"calls"},
		         {"source":"a","target":"b","type":"calls"}]}`)
	write("layers-normalized.json", `[
		{"id":"layer:core","name":"Core","description":"d","nodeIds":["a","b","zzz"]}]`)

	out, st, err := Salvage(repo)
	if err != nil {
		t.Fatal(err)
	}
	if st.Nodes != 3 {
		t.Errorf("want 3 unique nodes, got %d", st.Nodes)
	}
	if st.Edges != 2 {
		t.Errorf("want 2 unique edges (dedup a→b), got %d", st.Edges)
	}
	if st.Layers != 1 {
		t.Errorf("want 1 layer, got %d", st.Layers)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("merged file not written: %v", err)
	}
}
