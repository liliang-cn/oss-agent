// Package extract turns a text chunk into an ontology fragment (entities +
// relations) using an LLM, constrained to the entity/relation vocabulary of a
// Domain. The result is written into the knowledge graph by the knowledge store.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	agdomain "github.com/liliang-cn/agent-go/v3/pkg/domain"

	"github.com/liliang-cn/oss-agent/internal/domain"
)

// Entity is one extracted ontology node.
type Entity struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// Relation is one extracted ontology edge (by entity name).
type Relation struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// Triples is the extracted graph fragment for a chunk.
type Triples struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

// Extractor calls an LLM to extract ontology triples for a Domain.
type Extractor struct {
	llm  agdomain.Generator
	dom  *domain.Domain
	warn sync.Once
}

// New builds an Extractor. Pass nil llm to disable extraction.
func New(llm agdomain.Generator, dom *domain.Domain) *Extractor {
	return &Extractor{llm: llm, dom: dom}
}

// Extract returns the ontology fragment for one chunk.
func (e *Extractor) Extract(ctx context.Context, chunk string) (*Triples, error) {
	e.warnIfUnconstrained()
	out, err := e.llm.Generate(ctx, e.prompt(chunk), &agdomain.GenerationOptions{
		Temperature: 0,
		MaxTokens:   1500,
	})
	if err != nil {
		return nil, err
	}
	return parse(out)
}

// warnIfUnconstrained says once, at the start of an ingest, that the domain
// declares no ontology. Loading such a domain stays legal — most domain.toml
// files in the wild predate the field — but the consequence is invisible
// otherwise: the model picks its own type names, they differ between chunks, and
// no fixed edge whitelist can traverse the graph that comes out.
func (e *Extractor) warnIfUnconstrained() {
	e.warn.Do(func() {
		var missing []string
		if len(e.dom.EntityTypes) == 0 {
			missing = append(missing, "entity_types")
		}
		if len(e.dom.RelationTypes) == 0 {
			missing = append(missing, "relation_types")
		}
		if len(missing) == 0 {
			return
		}
		log.Printf("[ossagent] warning: domain %q declares no %s — the extracted graph will use "+
			"type names the model invents per chunk, and graph expansion will fall back to the "+
			"built-in code vocabulary, so most of it will not be traversable. Declare the "+
			"vocabulary in domain.toml.", e.dom.Name, strings.Join(missing, " or "))
	})
}

// freeVocabularyGuidance replaces the type constraint when the domain declares
// none. The instruction it replaces read "Use ONLY these entity types:" followed
// by nothing, which is not a weaker constraint but a self-contradicting one: it
// forbids everything and then lists no exception. Asking for stable, conventional
// names is the honest version — it cannot make the vocabulary closed, but it at
// least pushes the model to reuse a name across chunks instead of coining a
// synonym each time. UPPER_SNAKE for relations matches what `oss-agent init`
// generates, so a hand-written domain and a scaffolded one agree.
const freeVocabularyGuidance = `This domain declares no fixed vocabulary, so choose type names yourself: singular
CamelCase nouns for entities (StoragePool, ErrorCode) and UPPER_SNAKE verbs for
relations (DEPENDS_ON, CONTAINS). Reuse the same name for the same idea in every
chunk — a synonym coined here is an edge nothing can follow later.`

func (e *Extractor) prompt(chunk string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Extract a knowledge graph from the %s text below.\n", e.dom.Name)
	if len(e.dom.EntityTypes) > 0 {
		fmt.Fprintf(&b, "Use ONLY these entity types: %s\n", strings.Join(e.dom.EntityTypes, ", "))
	}
	if len(e.dom.RelationTypes) > 0 {
		fmt.Fprintf(&b, "Use ONLY these relation types: %s\n", strings.Join(e.dom.RelationTypes, ", "))
	}
	if len(e.dom.EntityTypes) == 0 || len(e.dom.RelationTypes) == 0 {
		fmt.Fprintf(&b, "%s\n", freeVocabularyGuidance)
	}
	fmt.Fprintf(&b, `
Return STRICT JSON only (no prose, no code fence):
{"entities":[{"name":"...","type":"<entity type>","description":"short"}],
 "relations":[{"from":"<entity name>","to":"<entity name>","type":"<relation type>"}]}
Extract only what is clearly present. If nothing relevant, return {"entities":[],"relations":[]}.

TEXT:
%s`, chunk)
	return b.String()
}

// parse tolerates a stray code fence or surrounding prose around the JSON.
func parse(s string) (*Triples, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	var t Triples
	if err := json.Unmarshal([]byte(s), &t); err != nil {
		return nil, fmt.Errorf("parse triples: %w", err)
	}
	return &t, nil
}
