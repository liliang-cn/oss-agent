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

// One audited ingest was 72% duplicate files from .claude/worktrees, plus
// generated .pb.go restating its .proto and test assertions posing as
// operator-facing log lines. None of that is knowledge; all of it competes
// with knowledge at retrieval time.
func TestNonKnowledgeFilesAreSkipped(t *testing.T) {
	for _, dir := range []string{".claude", ".understand-anything", ".git", "vendor"} {
		if !skipDirs[dir] {
			t.Errorf("skipDirs is missing %s", dir)
		}
	}
	for _, name := range []string{"api.pb.go", "api.pb.gw.go", "store_test.go", "bundle.min.js"} {
		if !skipFile(name) {
			t.Errorf("skipFile(%s) = false, want true", name)
		}
	}
	for _, name := range []string{"store.go", "testing.go", "contest.go", "README.md"} {
		if skipFile(name) {
			t.Errorf("skipFile(%s) = true, want false", name)
		}
	}
}
