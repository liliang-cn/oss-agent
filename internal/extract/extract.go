// Package extract turns a text chunk into an ontology fragment (entities +
// relations) using an LLM, constrained to the entity/relation vocabulary of a
// Domain. The result is written into the knowledge graph by the knowledge store.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	agdomain "github.com/liliang-cn/agent-go/v2/pkg/domain"

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
	llm agdomain.Generator
	dom *domain.Domain
}

// New builds an Extractor. Pass nil llm to disable extraction.
func New(llm agdomain.Generator, dom *domain.Domain) *Extractor {
	return &Extractor{llm: llm, dom: dom}
}

// Extract returns the ontology fragment for one chunk.
func (e *Extractor) Extract(ctx context.Context, chunk string) (*Triples, error) {
	out, err := e.llm.Generate(ctx, e.prompt(chunk), &agdomain.GenerationOptions{
		Temperature: 0,
		MaxTokens:   1500,
	})
	if err != nil {
		return nil, err
	}
	return parse(out)
}

func (e *Extractor) prompt(chunk string) string {
	return fmt.Sprintf(`Extract a knowledge graph from the %s text below.
Use ONLY these entity types: %s
Use ONLY these relation types: %s

Return STRICT JSON only (no prose, no code fence):
{"entities":[{"name":"...","type":"<entity type>","description":"short"}],
 "relations":[{"from":"<entity name>","to":"<entity name>","type":"<relation type>"}]}
Extract only what is clearly present. If nothing relevant, return {"entities":[],"relations":[]}.

TEXT:
%s`, e.dom.Name, strings.Join(e.dom.EntityTypes, ", "), strings.Join(e.dom.RelationTypes, ", "), chunk)
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
