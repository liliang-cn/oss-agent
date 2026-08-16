package domain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// error_patterns must keep working untouched: it is folded in as one group so a
// domain that declares only the old field behaves exactly as before.
func TestSourceGroupsFoldsInErrorPatterns(t *testing.T) {
	d := &Domain{ErrorPatternsRaw: []string{`pr_err\("([^"]+)"`}}
	if err := compileForTest(d, ""); err != nil {
		t.Fatal(err)
	}
	groups := d.SourceGroups()
	if len(groups) != 1 || groups[0].Name != "errors" {
		t.Fatalf("groups = %+v, want one named errors", groups)
	}
	// A message shorter than a log line is a fragment; the old behaviour.
	if groups[0].MinLength != 8 {
		t.Errorf("errors MinLength = %d, want 8", groups[0].MinLength)
	}
}

// source_patterns is retired: mining types/operations/constants by regex was a
// degraded tree-sitter, and code structure now arrives as a real graph via
// ingest-repo (Understand-Anything → graphimport). A domain.toml still carrying
// the table loads fine — TOML decoding ignores unknown keys — and only its
// error_patterns are mined.
func TestSourcePatternsTableIsIgnored(t *testing.T) {
	d := &Domain{ErrorPatternsRaw: []string{`pr_err\("([^"]+)"`}}
	if err := compileForTest(d, "\n[[source_patterns]]\nname = \"types\"\npatterns = ['type\\s+([A-Z]\\w+)\\s+struct']\n"); err != nil {
		t.Fatalf("a domain.toml with a leftover source_patterns table must still load: %v", err)
	}
	groups := d.SourceGroups()
	if len(groups) != 1 || groups[0].Name != "errors" {
		t.Fatalf("groups = %+v, want the errors group alone", groups)
	}
}

// compileForTest runs the same compilation Load does, without a file. extra is
// appended to the TOML body verbatim.
func compileForTest(d *Domain, extra string) error {
	d.Name, d.Persona = "t", "p"
	tmp := filepath.Join(os.TempDir(), "oss-agent-domain-test.toml")
	body := "name = \"t\"\npersona = \"p\"\n"
	for _, p := range d.ErrorPatternsRaw {
		body += "error_patterns = ['" + p + "']\n"
	}
	body += extra
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	loaded, err := Load(tmp)
	if err != nil {
		return err
	}
	*d = *loaded
	return nil
}

// The code vocabulary is separate from the prose vocabulary because they have
// different consumers. The prose types go into the LLM extractor's prompt
// ("Use ONLY these entity types"); the code types must not, or the extractor is
// told it may invent `function` and `calls` nodes out of documentation.
func TestCodeVocabularyIsSeparateFromProse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "domain.toml")
	if err := os.WriteFile(path, []byte(`
name = "x"
persona = "y"
entity_types = ["Cluster", "Node"]
relation_types = ["MANAGES"]

[vocabulary.code]
entity_types = ["file", "function"]
relation_types = ["imports", "calls"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.EntityTypes) != 2 || d.EntityTypes[0] != "Cluster" {
		t.Errorf("prose entity types = %v, want the domain concepts only", d.EntityTypes)
	}
	if len(d.Vocabulary.Code.RelationTypes) != 2 {
		t.Errorf("code relation types = %v", d.Vocabulary.Code.RelationTypes)
	}
	// The prose vocabulary must not have absorbed the code vocabulary.
	for _, tp := range d.RelationTypes {
		if tp == "imports" || tp == "calls" {
			t.Fatalf("code type %q leaked into the prose vocabulary %v", tp, d.RelationTypes)
		}
	}
}

// Resolution records how the code edges were derived. A syntactic extractor
// cannot resolve Go's implicit interfaces or distinguish two same-named
// methods, so an importer must be able to say which it is rather than let a
// consumer assume every edge is type-checked.
func TestCodeVocabularyResolutionDefaultsToSyntactic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "domain.toml")
	if err := os.WriteFile(path, []byte(`
name = "x"
persona = "y"

[vocabulary.code]
relation_types = ["calls"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Vocabulary.Code.Resolution; got != "syntactic" {
		t.Errorf("resolution = %q, want syntactic by default", got)
	}
}

func TestCodeVocabularyRejectsUnknownResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "domain.toml")
	if err := os.WriteFile(path, []byte(`
name = "x"
persona = "y"

[vocabulary.code]
resolution = "vibes"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "resolution") {
		t.Fatalf("expected an unknown resolution to be rejected, got %v", err)
	}
}

// AllowsRelation is what the importer gates on. It is case-sensitive on
// purpose: the prose vocabulary shouts (MANAGES, DEPENDS_ON) and the code
// vocabulary whispers (contains, depends_on), and folding their cases together
// would silently merge "a cluster contains a node" with "a file contains a
// function" — two different relations that happen to share a word.
func TestAllowsRelationIsCaseSensitive(t *testing.T) {
	d := &Domain{}
	d.Vocabulary.Code.RelationTypes = []string{"contains", "calls"}

	if !d.AllowsCodeRelation("contains") {
		t.Error("declared type must be allowed")
	}
	if d.AllowsCodeRelation("CONTAINS") {
		t.Error("CONTAINS is a domain relation, not the code one; it must not match")
	}
	if d.AllowsCodeRelation("implements") {
		t.Error("undeclared type must be refused")
	}
}

// A domain that says nothing about code still admits an Understand-Anything
// graph. Requiring the list would be a transcription task with a cliff at the
// end: the importer refuses a whole graph over one undeclared type.
func TestCodeVocabularyDefaultsToUpstreamUnions(t *testing.T) {
	d := &Domain{}
	if err := compileForTest(d, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"file", "function", "class"} {
		if !d.AllowsCodeEntity(want) {
			t.Errorf("default code vocabulary is missing node type %q", want)
		}
	}
	for _, want := range []string{"calls", "imports", "implements", "tested_by"} {
		if !d.AllowsCodeRelation(want) {
			t.Errorf("default code vocabulary is missing edge type %q", want)
		}
	}
	if d.Vocabulary.Code.Resolution != ResolutionSyntactic {
		t.Errorf("resolution = %q, want the honest default %q", d.Vocabulary.Code.Resolution, ResolutionSyntactic)
	}
}

// An explicit list replaces the default rather than extending it: narrowing the
// admission list is how a domain keeps its graph small, and a merge would make
// that impossible.
func TestCodeVocabularyExplicitListWins(t *testing.T) {
	d := &Domain{}
	extra := "\n[vocabulary.code]\nentity_types = ['file']\nrelation_types = ['contains']\n"
	if err := compileForTest(d, extra); err != nil {
		t.Fatal(err)
	}
	if !d.AllowsCodeEntity("file") || d.AllowsCodeEntity("function") {
		t.Errorf("entity types = %v, want exactly [file]", d.Vocabulary.Code.EntityTypes)
	}
	if !d.AllowsCodeRelation("contains") || d.AllowsCodeRelation("calls") {
		t.Errorf("relation types = %v, want exactly [contains]", d.Vocabulary.Code.RelationTypes)
	}
}
