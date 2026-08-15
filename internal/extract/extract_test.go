package extract

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/liliang-cn/oss-agent/internal/domain"
)

// TestWarnsOnceOnMissingOntology proves the ingest is not silent about a graph
// nothing will be able to traverse, and that it says so once per run rather than
// once per chunk.
func TestWarnsOnceOnMissingOntology(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetFlags(log.LstdFlags) })

	e := New(nil, &domain.Domain{Name: "storage"})
	e.warnIfUnconstrained()
	e.warnIfUnconstrained()

	out := buf.String()
	if strings.Count(out, "declares no") != 1 {
		t.Errorf("want exactly one warning, got:\n%s", out)
	}
	for _, want := range []string{"storage", "entity_types", "relation_types"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not mention %q:\n%s", want, out)
		}
	}

	buf.Reset()
	full := New(nil, &domain.Domain{Name: "storage", EntityTypes: []string{"Node"}, RelationTypes: []string{"CONTAINS"}})
	full.warnIfUnconstrained()
	if buf.Len() != 0 {
		t.Errorf("warned about a domain that declares an ontology:\n%s", buf.String())
	}
}

// TestPromptConstrainsToDeclaredVocabulary keeps the closed-vocabulary case
// intact: a domain that declares an ontology still gets it verbatim.
func TestPromptConstrainsToDeclaredVocabulary(t *testing.T) {
	e := New(nil, &domain.Domain{
		Name:          "storage",
		EntityTypes:   []string{"Node", "Volume"},
		RelationTypes: []string{"DEPLOYED_ON", "CONTAINS"},
	})

	p := e.prompt("some text")
	for _, want := range []string{
		"Use ONLY these entity types: Node, Volume",
		"Use ONLY these relation types: DEPLOYED_ON, CONTAINS",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, freeVocabularyGuidance) {
		t.Errorf("prompt offers a free vocabulary despite a declared one:\n%s", p)
	}
}

// TestPromptOmitsEmptyConstraint is the fix: with nothing declared, the prompt
// used to read "Use ONLY these entity types:" followed by nothing — a rule that
// forbids every type and permits none, which the model can only ignore.
func TestPromptOmitsEmptyConstraint(t *testing.T) {
	e := New(nil, &domain.Domain{Name: "storage"})

	p := e.prompt("some text")
	if strings.Contains(p, "Use ONLY") {
		t.Errorf("prompt constrains to an empty vocabulary:\n%s", p)
	}
	if !strings.Contains(p, freeVocabularyGuidance) {
		t.Errorf("prompt does not ask for stable type names:\n%s", p)
	}
	if !strings.Contains(p, "some text") {
		t.Errorf("prompt lost the chunk:\n%s", p)
	}
}

// TestPromptConstrainsWhatItCan covers the half-declared domain: the side that
// exists is still enforced, and the guidance covers the side that does not.
func TestPromptConstrainsWhatItCan(t *testing.T) {
	e := New(nil, &domain.Domain{Name: "storage", EntityTypes: []string{"Node"}})

	p := e.prompt("some text")
	if !strings.Contains(p, "Use ONLY these entity types: Node") {
		t.Errorf("prompt dropped the declared entity types:\n%s", p)
	}
	if strings.Contains(p, "Use ONLY these relation types") {
		t.Errorf("prompt constrains relations it has no vocabulary for:\n%s", p)
	}
	if !strings.Contains(p, freeVocabularyGuidance) {
		t.Errorf("prompt does not ask for stable relation names:\n%s", p)
	}
}
