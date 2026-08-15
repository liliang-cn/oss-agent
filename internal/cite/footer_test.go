package cite

import "testing"

// The footer must not claim grounding the answer did not use. Retrieval always
// returns its best chunk, relevant or not — listing all of them turned "here is
// what I looked at" into "here is what this is based on".
func TestFooterListsOnlyWhatTheAnswerCites(t *testing.T) {
	sources := []string{"corpus/user-guide.md", "corpus/README.md"}
	answer := "Writeback puts the SSD in the durability path [user-guide]."

	got := Footer(answer, sources)
	if !contains(got, "corpus/user-guide.md") {
		t.Errorf("cited source missing from footer:\n%s", got)
	}
	if contains(got, "corpus/README.md") {
		t.Errorf("uncited source was listed as a source:\n%s", got)
	}
}

// An answer from the model's own memory cites nothing, and must not be dressed
// up with a Sources section built from whatever retrieval happened to return.
func TestFooterIsEmptyWhenTheAnswerCitesNothing(t *testing.T) {
	if got := Footer("al-extents defaults to 1237.", []string{"corpus/mcp.md"}); got != "" {
		t.Errorf("Footer = %q, want empty for an uncited answer", got)
	}
}

func TestFooterStillDefersToTheAnswersOwnSources(t *testing.T) {
	answer := "Something [user-guide].\n\n**Sources**\n- [user-guide] corpus/user-guide.md"
	if got := Footer(answer, []string{"corpus/user-guide.md"}); got != "" {
		t.Errorf("Footer = %q, want empty when the answer lists its own", got)
	}
}

func TestFooterHandlesNoSources(t *testing.T) {
	if got := Footer("anything", nil); got != "" {
		t.Errorf("Footer = %q, want empty", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
