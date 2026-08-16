package knowledge

import "testing"

// Chunking overlaps on purpose so a sentence split across a boundary stays
// retrievable whole. Written back out verbatim that would duplicate a paragraph
// at every seam, which is not what the document said.
func TestJoinChunksRemovesTheChunkerOverlap(t *testing.T) {
	tail := "the surviving node was ever Primary, it marks a disconnected peer Outdated."
	got := joinChunks([]string{
		"A trap that contaminated two earlier attempts: if " + tail,
		tail + " An outdated node cannot obtain quorum.",
	})
	want := "A trap that contaminated two earlier attempts: if " + tail + " An outdated node cannot obtain quorum."
	if got != want {
		t.Fatalf("joined text kept the overlap:\n got: %q\nwant: %q", got, want)
	}
}

func TestJoinChunksKeepsUnrelatedChunksWhole(t *testing.T) {
	got := joinChunks([]string{"first paragraph.", "second paragraph."})
	if got != "first paragraph.second paragraph." {
		t.Fatalf("unexpected join: %q", got)
	}
}

// Document ids carry slashes and colons; neither may become a directory,
// because a flat directory is what re-ingests.
func TestExportFilenameFlattensIDs(t *testing.T) {
	cases := map[string]string{
		"corpus/user-guide.md":   "corpus_user-guide.md",
		"ua:sds-repo:graph":      "ua_sds-repo_graph.md",
		"ops-hp-e1000e-nic-hang": "ops-hp-e1000e-nic-hang.md",
		"already.md":             "already.md",
	}
	for in, want := range cases {
		if got := exportFilename(in); got != want {
			t.Errorf("exportFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
