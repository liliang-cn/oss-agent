package knowledge

import (
	"reflect"
	"testing"
)

// A live SDS knowledge base carried a "StorageClass" node type the domain never
// declared — invented by the extracting model, stored without complaint, and
// only found by running a GROUP BY by hand. Extraction is done by a language
// model, so this is not preventable; it only has to be visible.
func TestDiffVocabularyFindsInventedTypes(t *testing.T) {
	declared := []string{"Node", "DRBDResource", "Gateway"}
	found := map[string]int{"Node": 25, "DRBDResource": 20, "StorageClass": 3}

	undeclared, unused := diffVocabulary(declared, found)

	if want := map[string]int{"StorageClass": 3}; !reflect.DeepEqual(undeclared, want) {
		t.Errorf("undeclared = %v, want %v", undeclared, want)
	}
	if want := []string{"Gateway"}; !reflect.DeepEqual(unused, want) {
		t.Errorf("unused = %v, want %v", unused, want)
	}
}

// "Contains" is the declared "contains" with a capital letter, not a type
// somebody invented. Reporting it as drift would bury the real thing under
// noise the extractor produces on every run.
func TestDiffVocabularyIgnoresCasing(t *testing.T) {
	undeclared, unused := diffVocabulary(
		[]string{"contains", "depends_on"},
		map[string]int{"Contains": 4, "DEPENDS_ON": 2},
	)
	if len(undeclared) != 0 {
		t.Errorf("undeclared = %v, want none", undeclared)
	}
	if len(unused) != 0 {
		t.Errorf("unused = %v, want none — both were extracted, just cased differently", unused)
	}
}

func TestDiffVocabularyOnAnEmptyGraph(t *testing.T) {
	undeclared, unused := diffVocabulary([]string{"Node", "Gateway"}, map[string]int{})
	if len(undeclared) != 0 {
		t.Errorf("undeclared = %v, want none", undeclared)
	}
	if want := []string{"Gateway", "Node"}; !reflect.DeepEqual(unused, want) {
		t.Errorf("unused = %v, want %v", unused, want)
	}
}

// The fingerprint identifies a vocabulary, not a spelling of it. Reordering
// domain.toml must not read as a change of meaning, or every reorder looks like
// the graph needs re-extracting.
func TestVocabularyFingerprintIgnoresOrder(t *testing.T) {
	a := &Store{entityTypes: []string{"Node", "Gateway"}, relationTypes: []string{"contains", "backs"}}
	b := &Store{entityTypes: []string{"Gateway", "Node"}, relationTypes: []string{"backs", "contains"}}
	if a.vocabularyFingerprint() != b.vocabularyFingerprint() {
		t.Error("reordering the same vocabulary changed its fingerprint")
	}
}

// Changing the vocabulary must change the fingerprint, which is the whole point:
// a graph extracted under the old one may hold edges the new one never walks.
func TestVocabularyFingerprintTracksContent(t *testing.T) {
	a := &Store{entityTypes: []string{"Node"}, relationTypes: []string{"contains"}}
	b := &Store{entityTypes: []string{"Node"}, relationTypes: []string{"CONTAINS_THING"}}
	if a.vocabularyFingerprint() == b.vocabularyFingerprint() {
		t.Error("a different vocabulary produced the same fingerprint")
	}
}

// Entity and relation vocabularies must not be able to swap without notice.
func TestVocabularyFingerprintSeparatesEntitiesFromRelations(t *testing.T) {
	a := &Store{entityTypes: []string{"x"}, relationTypes: []string{"y"}}
	b := &Store{entityTypes: []string{"y"}, relationTypes: []string{"x"}}
	if a.vocabularyFingerprint() == b.vocabularyFingerprint() {
		t.Error("swapping entity and relation vocabularies produced the same fingerprint")
	}
}

// A domain that declares no vocabulary must not be reported as having drifted
// from one.
func TestDriftIsSilentWithoutADeclaredVocabulary(t *testing.T) {
	d := OntologyDrift{}
	if d.Registered {
		t.Error("an unregistered ontology must not claim to be registered")
	}
	if !d.Clean() {
		t.Error("no declared vocabulary means nothing to drift from")
	}
}

func TestDriftCleanOnlyWhenNothingWasInvented(t *testing.T) {
	dirty := OntologyDrift{Registered: true, UndeclaredEdgeTypes: map[string]int{"DEPLOYED_ON": 12}}
	if dirty.Clean() {
		t.Error("an undeclared edge type is drift: it is stored and never traversed")
	}
	// Declaring a type the corpus never mentions is untidy, not broken.
	tidy := OntologyDrift{Registered: true, UnusedNodeTypes: []string{"WANTunnel"}}
	if !tidy.Clean() {
		t.Error("a declared-but-unused type is not drift")
	}
}

func TestWithOntologyTrimsAndDedupes(t *testing.T) {
	s := &Store{}
	WithOntology(" SDS ", []string{"Node", " Node ", "", "Gateway"}, []string{"contains", "contains"})(s)

	if s.ontologyName != "SDS" {
		t.Errorf("name = %q, want %q", s.ontologyName, "SDS")
	}
	if want := []string{"Node", "Gateway"}; !reflect.DeepEqual(s.entityTypes, want) {
		t.Errorf("entityTypes = %v, want %v", s.entityTypes, want)
	}
	if want := []string{"contains"}; !reflect.DeepEqual(s.relationTypes, want) {
		t.Errorf("relationTypes = %v, want %v", s.relationTypes, want)
	}
}
