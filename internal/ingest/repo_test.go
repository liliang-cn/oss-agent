package ingest

import "testing"

// A domain whose error_patterns name Go constructs must be able to reach Go
// files. codeExts was C and Java only, so a domain declaring zap/logger/
// fmt.Errorf patterns had them silently matched against nothing.
func TestGoSourcesAreMinedForErrorStrings(t *testing.T) {
	for _, ext := range []string{".go", ".rs", ".py", ".ts", ".js", ".c", ".java"} {
		if !codeExts[ext] {
			t.Errorf("codeExts is missing %s", ext)
		}
	}
	if codeExts[".md"] {
		t.Error("markdown is a doc to ingest whole, not a source to mine for literals")
	}
}
