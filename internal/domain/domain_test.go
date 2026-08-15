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
