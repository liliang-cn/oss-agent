package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	cortexdb "github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// The domain's ontology used to exist in exactly two places, neither of which
// could check anything:
//
//   - as prose pasted into the extraction prompt, telling the model what to emit;
//   - as a list of edge type strings handed to graph expansion at query time.
//
// So nothing recorded which vocabulary a graph had been extracted under, and
// nothing rejected a type outside it. Both cost real time. A graph extracted
// under a SCREAMING_SNAKE relation set kept expanding to nothing long after the
// vocabulary changed, and the only way to find out was to read the traversal
// filter's source. And a live SDS knowledge base carried a "StorageClass" node
// type the domain never declared — invented by the extracting model, stored
// without complaint, invisible until someone ran a GROUP BY by hand.
//
// cortexdb has had somewhere to put this the whole time: a versioned,
// activatable ontology schema, with object types and link types that carry
// which object type sits on each end. Registering the vocabulary there makes it
// a record rather than a convention — one that says what a graph was built
// under, and that drift can be measured against.

// ontologySchemaID is the stable id the domain's schema is stored under. One
// knowledge base holds one domain, so this does not need to vary; re-registering
// replaces it and cortexdb carries the version.
const ontologySchemaID = "oss-agent-domain"

// WithOntology registers the domain's declared vocabulary as a cortexdb
// ontology schema when the store opens, and makes DriftReport meaningful.
//
// Registration is best-effort: a knowledge base that cannot store the schema
// still ingests and still answers, because refusing to serve over bookkeeping
// would trade a real capability for a record-keeping one.
func WithOntology(name string, entityTypes, relationTypes []string) Option {
	return func(s *Store) {
		s.ontologyName = strings.TrimSpace(name)
		s.entityTypes = trimAll(entityTypes)
		s.relationTypes = trimAll(relationTypes)
	}
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return dedupe(out)
}

// entityType is the open object type both ends of every link point at.
//
// It exists to state "unconstrained" in a form cortexdb will accept. A
// domain.toml declares relations as bare names — "backs", "exports" — with no
// statement of what may sit on either side, and cortexdb requires every link
// side to name an object type. Inventing a domain and range per relation would
// be asserting a constraint the domain never made and then rejecting
// extractions on the strength of the guess. Pointing both ends at a supertype
// every entity implements says the true thing instead: any declared entity may
// sit on either side. When a domain gains a way to express ends, they replace
// this and cardinality checking becomes meaningful.
//
// It is an object type rather than an interface because cortexdb refuses a
// schema whose interface name collides with an object type name, and link sides
// name object types.
const entityInterface = "Entity"

// registerOntology stores the declared vocabulary and activates it.
func (s *Store) registerOntology(ctx context.Context) error {
	if len(s.entityTypes) == 0 && len(s.relationTypes) == 0 {
		return nil
	}

	// Every object type carries the two properties a graph node actually has:
	// its store id and the text it was extracted from. cortexdb validates that
	// primary_key names a DECLARED property, so an object type that names "id"
	// without declaring it is rejected and the whole schema fails to register —
	// silently, since registration is best-effort.
	props := []cortexdb.OntologyProperty{
		{
			APIName:     "id",
			DisplayName: "Node id",
			Description: "Identity of the extracted graph node in the knowledge store.",
			DataType:    cortexdb.OntologyDataType{Kind: cortexdb.OntologyDataString},
			Required:    true,
		},
		{
			APIName:     "content",
			DisplayName: "Content",
			Description: "The text the node was extracted from.",
			DataType:    cortexdb.OntologyDataType{Kind: cortexdb.OntologyDataString},
			Searchable:  true,
		},
	}

	objects := make([]cortexdb.OntologyObjectType, 0, len(s.entityTypes)+1)
	objects = append(objects, cortexdb.OntologyObjectType{
		APIName:     entityInterface,
		DisplayName: "Entity",
		Description: "Any extracted graph node. Both ends of every link point here, because the domain declares relation names without ends.",
		Properties:  props,
		PrimaryKey:  "id",
	})
	for _, t := range s.entityTypes {
		if strings.EqualFold(t, entityInterface) {
			continue
		}
		objects = append(objects, cortexdb.OntologyObjectType{
			APIName:     t,
			DisplayName: t,
			Properties:  props,
			PrimaryKey:  "id",
		})
	}

	links := make([]cortexdb.OntologyLinkType, 0, len(s.relationTypes))
	for _, t := range s.relationTypes {
		links = append(links, cortexdb.OntologyLinkType{
			APIName: t,
			A: cortexdb.OntologyLinkSide{
				APIName:           t,
				ObjectTypeAPIName: entityInterface,
				Cardinality:       cortexdb.OntologyCardinalityMany,
			},
			B: cortexdb.OntologyLinkSide{
				APIName:           t + "_of",
				ObjectTypeAPIName: entityInterface,
				Cardinality:       cortexdb.OntologyCardinalityMany,
			},
		})
	}

	name := s.ontologyName
	if name == "" {
		name = "oss-agent domain"
	}
	_, err := s.db.SaveOntologySchema(ctx, cortexdb.OntologySaveRequest{
		Activate: true,
		Schema: cortexdb.OntologySchema{
			SchemaID:    ontologySchemaID,
			Name:        name,
			Description: "Entity and relation vocabulary declared by the active domain.toml.",
			Metadata: map[string]string{
				// The fingerprint is what makes a graph traceable to a
				// vocabulary: compare it against the stored schema and a
				// domain.toml that changed since the last ingest is visible
				// instead of being something you deduce from bad answers.
				"vocabulary_fingerprint": s.vocabularyFingerprint(),
				"entity_type_count":      fmt.Sprint(len(s.entityTypes)),
				"relation_type_count":    fmt.Sprint(len(s.relationTypes)),
			},
			ObjectTypes: objects,
			LinkTypes:   links,
		},
	})
	if err != nil {
		return fmt.Errorf("register ontology schema: %w", err)
	}
	return nil
}

// vocabularyFingerprint is a stable digest of the declared vocabulary. Order
// does not change it, so reordering domain.toml is not mistaken for a change of
// meaning.
func (s *Store) vocabularyFingerprint() string {
	ents := append([]string(nil), s.entityTypes...)
	rels := append([]string(nil), s.relationTypes...)
	sort.Strings(ents)
	sort.Strings(rels)
	sum := sha256.Sum256([]byte(strings.Join(ents, "\x00") + "\x1e" + strings.Join(rels, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// OntologyDrift is what the graph holds that the domain never declared, and
// what it declared that never appeared.
type OntologyDrift struct {
	// Registered is false when no vocabulary was declared, in which case the
	// other fields say nothing.
	Registered bool `json:"registered"`
	// Fingerprint identifies the vocabulary this was measured against.
	Fingerprint string `json:"fingerprint,omitempty"`
	// StoredFingerprint is what the schema in the database was registered with.
	// A difference means the graph was extracted under a different vocabulary
	// than the one now loaded, and stale edges may be unreachable.
	StoredFingerprint string `json:"stored_fingerprint,omitempty"`

	// UndeclaredNodeTypes and UndeclaredEdgeTypes were invented by the
	// extracting model. Undeclared edges are the expensive ones: expansion
	// filters on the declared vocabulary, so they are stored and never walked.
	UndeclaredNodeTypes map[string]int `json:"undeclared_node_types,omitempty"`
	UndeclaredEdgeTypes map[string]int `json:"undeclared_edge_types,omitempty"`

	// UnusedNodeTypes and UnusedEdgeTypes were declared and never extracted.
	// Harmless, but they are prompt budget spent on nothing, and a type the
	// model never reaches for is usually one the corpus does not contain.
	UnusedNodeTypes []string `json:"unused_node_types,omitempty"`
	UnusedEdgeTypes []string `json:"unused_edge_types,omitempty"`
}

// Clean reports whether the graph stayed inside the declared vocabulary.
func (d OntologyDrift) Clean() bool {
	return len(d.UndeclaredNodeTypes) == 0 && len(d.UndeclaredEdgeTypes) == 0
}

// DriftReport compares the types actually in the graph against the vocabulary
// the domain declared.
//
// Extraction is done by a language model, so drift is not a failure mode to be
// prevented — it is the expected behaviour of the component, and the only
// question is whether anyone finds out. This is how they find out.
func (s *Store) DriftReport(ctx context.Context) (*OntologyDrift, error) {
	d := &OntologyDrift{}
	if len(s.entityTypes) == 0 && len(s.relationTypes) == 0 {
		return d, nil
	}
	d.Registered = true
	d.Fingerprint = s.vocabularyFingerprint()

	if got, err := s.db.GetOntologySchema(ctx, cortexdb.OntologyGetRequest{SchemaID: ontologySchemaID}); err == nil && got != nil {
		d.StoredFingerprint = got.Schema.Metadata["vocabulary_fingerprint"]
	}

	nodeCounts, err := s.typeCounts(ctx, "SELECT node_type, COUNT(*) FROM graph_nodes GROUP BY node_type")
	if err != nil {
		return nil, err
	}
	edgeCounts, err := s.typeCounts(ctx, "SELECT edge_type, COUNT(*) FROM graph_edges GROUP BY edge_type")
	if err != nil {
		return nil, err
	}

	d.UndeclaredNodeTypes, d.UnusedNodeTypes = diffVocabulary(s.entityTypes, nodeCounts)
	d.UndeclaredEdgeTypes, d.UnusedEdgeTypes = diffVocabulary(s.relationTypes, edgeCounts)
	return d, nil
}

// typeCounts runs a "type, count" query over the graph tables.
func (s *Store) typeCounts(ctx context.Context, query string) (map[string]int, error) {
	rows, err := s.db.SQL().QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("count types: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int)
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		if t = strings.TrimSpace(t); t != "" {
			out[t] = n
		}
	}
	return out, rows.Err()
}

// diffVocabulary splits what is in the graph against what was declared.
//
// Comparison is case-insensitive for the same reason WithRelationTypes widens
// case before handing types to traversal: the type in the graph is whatever the
// extracting model emitted, and "Contains" is the declared "contains" with a
// capital letter, not a type somebody invented.
func diffVocabulary(declared []string, found map[string]int) (undeclared map[string]int, unused []string) {
	known := make(map[string]string, len(declared))
	for _, t := range declared {
		known[strings.ToLower(t)] = t
	}
	seen := make(map[string]bool, len(declared))

	for t, n := range found {
		if canonical, ok := known[strings.ToLower(t)]; ok {
			seen[canonical] = true
			continue
		}
		if undeclared == nil {
			undeclared = make(map[string]int)
		}
		undeclared[t] = n
	}
	for _, t := range declared {
		if !seen[t] {
			unused = append(unused, t)
		}
	}
	sort.Strings(unused)
	return undeclared, unused
}
