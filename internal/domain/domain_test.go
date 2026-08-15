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
	if err := compileForTest(d); err != nil {
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

// A domain can now ask for anything its language declares — the types a system
// defines are its domain model, and mining them puts the ontology's own
// vocabulary in front of the extractor.
func TestSourcePatternsAreNamedAndSeparate(t *testing.T) {
	d := &Domain{
		ErrorPatternsRaw: []string{`pr_err\("([^"]+)"`},
		SourcePatterns: []SourceGroup{
			{Name: "types", Patterns: []string{`type\s+([A-Z]\w+)\s+struct`}},
		},
	}
	if err := compileForTest(d); err != nil {
		t.Fatal(err)
	}
	groups := d.SourceGroups()
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want errors + types", len(groups))
	}
	types := groups[1]
	if types.Name != "types" || len(types.Compiled) != 1 {
		t.Errorf("types group = %+v", types)
	}
	if types.Summary == "" {
		t.Error("a group with no summary must still title its document")
	}
	if types.MinLength != 3 {
		t.Errorf("MinLength = %d, want the default 3 — identifiers are short", types.MinLength)
	}
}

func TestSourcePatternsRejectAnUnusableGroup(t *testing.T) {
	if err := compileForTest(&Domain{SourcePatterns: []SourceGroup{{Patterns: []string{"x"}}}}); err == nil {
		t.Error("a group with no name must be rejected: the name is its document id suffix")
	}
	if err := compileForTest(&Domain{SourcePatterns: []SourceGroup{{Name: "t", Patterns: []string{"("}}}}); err == nil {
		t.Error("an invalid regex must be rejected at load, not silently matched against nothing")
	}
}

// compileForTest runs the same compilation Load does, without a file.
func compileForTest(d *Domain) error {
	d.Name, d.Persona = "t", "p"
	tmp := filepath.Join(os.TempDir(), "oss-agent-domain-test.toml")
	body := "name = \"t\"\npersona = \"p\"\n"
	for _, p := range d.ErrorPatternsRaw {
		body += "error_patterns = ['" + p + "']\n"
	}
	for _, g := range d.SourcePatterns {
		body += "\n[[source_patterns]]\nname = \"" + g.Name + "\"\npatterns = ['" + strings.Join(g.Patterns, "','") + "']\n"
	}
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
