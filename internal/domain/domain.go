// Package domain is the platform's product-agnostic seam. oss-agent is a generic
// engine; each project that uses it describes ITS OWN product in a domain.toml
// config — persona, ontology vocabulary, source error patterns, read-only probes,
// and repos. No product is compiled into the platform; a worked example config
// ships under examples/.
package domain

import (
	"fmt"
	"regexp"

	"github.com/BurntSushi/toml"

	"github.com/liliang-cn/oss-agent/internal/probes"
	"github.com/liliang-cn/oss-agent/internal/safety"
)

// Domain bundles the product-specific knowledge that shapes the agent. It is
// loaded from a domain.toml; the engine treats it as opaque config.
type Domain struct {
	Name             string            `toml:"name"`           // human label
	Title            string            `toml:"title"`          // UI title/brand (defaults to Name)
	Persona          string            `toml:"persona"`        // agent system prompt
	EntityTypes      []string          `toml:"entity_types"`   // ontology node types
	RelationTypes    []string          `toml:"relation_types"` // ontology edge types
	ErrorPatternsRaw []string          `toml:"error_patterns"` // regex sources (group 1 = message)
	Probes           []probes.Probe    `toml:"probes"`         // read-only diagnostic commands
	Repos            []string          `toml:"repos"`          // upstream repos to ingest
	RedLines         []safety.RuleSpec `toml:"red_lines"`      // deterministic destructive-command blocks

	// Vocabulary holds vocabularies that are NOT the prose one above.
	Vocabulary Vocabulary `toml:"vocabulary"`

	// ErrorPatterns are compiled from ErrorPatternsRaw at load time.
	ErrorPatterns []*regexp.Regexp `toml:"-"`
}

// Vocabulary partitions the ontology vocabulary by where its edges come from.
//
// The fields above (EntityTypes/RelationTypes) are the PROSE vocabulary: an LLM
// reads documentation and emits Cluster, Node, DEPLOYED_ON. They are pasted
// into the extractor's prompt ("Use ONLY these entity types"), which is exactly
// why a code vocabulary cannot live there — telling a prose extractor it may
// emit `function` and `calls` invites it to invent code structure out of
// documentation.
//
// A code vocabulary is produced by a deterministic extractor instead, and is
// consumed by the importer as an admission filter. Different provenance,
// different consumer, different list.
type Vocabulary struct {
	Code CodeVocabulary `toml:"code"`
}

// Resolution says how far an extractor got toward meaning.
const (
	// ResolutionSyntactic is name matching over a parse tree. It cannot decide
	// which of two same-named methods a call reaches, and for Go it cannot
	// derive `implements` at all, because interface satisfaction is implicit
	// and structural.
	ResolutionSyntactic = "syntactic"
	// ResolutionTyped is backed by a type checker (go/types and friends), so an
	// edge names the symbol it actually resolves to.
	ResolutionTyped = "typed"
)

// CodeVocabulary is the admission list for edges mined from source.
type CodeVocabulary struct {
	EntityTypes   []string `toml:"entity_types"`
	RelationTypes []string `toml:"relation_types"`
	// Resolution defaults to syntactic, because that is what a parse-tree
	// extractor can honestly claim. Declaring `typed` is a promise about the
	// producer, not a wish.
	Resolution string `toml:"resolution"`
}

// The default code vocabulary is Understand-Anything's own type unions
// (packages/core/src/types.ts, v2.8.x), which is what `ingest-repo` imports.
//
// Defaulted rather than required because the importer refuses a whole graph
// containing any undeclared type: a domain.toml that transcribes these by hand
// and misses one turns a working import into a total rejection, and there is
// nothing product-specific about the list to transcribe in the first place. A
// domain that declares its own list still overrides this completely — narrowing
// the admission list is a legitimate way to keep a graph small.
var (
	defaultCodeEntityTypes = []string{
		"file", "function", "class", "module", "concept",
		"config", "document", "service", "table", "endpoint",
		"pipeline", "schema", "resource",
		"domain", "flow", "step",
		"article", "entity", "topic", "claim", "source",
	}
	defaultCodeRelationTypes = []string{
		// structural
		"imports", "exports", "contains", "inherits", "implements",
		// behavioral
		"calls", "subscribes", "publishes", "middleware",
		// data flow
		"reads_from", "writes_to", "transforms", "validates",
		// dependencies
		"depends_on", "tested_by", "configures",
		// semantic
		"related", "similar_to",
		// infrastructure
		"deploys", "serves", "provisions", "triggers",
		// schema/data
		"migrates", "documents", "routes", "defines_schema",
		// domain
		"contains_flow", "flow_step", "cross_domain",
		// knowledge
		"cites", "contradicts", "builds_on", "exemplifies", "categorized_under", "authored_by",
	}
)

// AllowsCodeRelation reports whether the code vocabulary declares this edge type.
//
// Deliberately case-sensitive. The prose vocabulary shouts (CONTAINS,
// DEPENDS_ON) and the code vocabulary whispers (contains, depends_on); a
// case-insensitive compare would merge "a cluster contains a node" with "a file
// contains a function". Those share a word and nothing else, and a graph that
// conflates them answers "what is inside this?" with both at once.
func (d *Domain) AllowsCodeRelation(t string) bool {
	for _, declared := range d.Vocabulary.Code.RelationTypes {
		if declared == t {
			return true
		}
	}
	return false
}

// AllowsCodeEntity reports whether the code vocabulary declares this node type.
func (d *Domain) AllowsCodeEntity(t string) bool {
	for _, declared := range d.Vocabulary.Code.EntityTypes {
		if declared == t {
			return true
		}
	}
	return false
}

// SourceGroup is one named thing to mine out of source files.
//
// Only one group exists now: error_patterns, folded in by SourceGroups. It
// answers "what messages does this code print", which is what an operator
// reading a log needs, and log strings genuinely are not in the AST — a regex
// is the right tool for them. A configurable source_patterns list used to sit
// alongside it, mining types/operations/constants by regex; that was a
// degraded tree-sitter producing flat name lists whose relationships an LLM
// then had to guess back. Code structure now arrives as a real graph via
// `ingest-repo` (Understand-Anything → graphimport), so the regex route to it
// is gone.
type SourceGroup struct {
	// Name identifies the group and suffixes the document id
	// ("pkg/gateway/nfs.go#types").
	Name string `toml:"name"`
	// Summary opens the stored document: "<Summary> <file> (<repo>):".
	// Defaults to "Extracted from".
	Summary string `toml:"summary"`
	// Patterns are regexes whose capture group 1 is the value to keep.
	Patterns []string `toml:"patterns"`
	// MinLength drops captures shorter than this. Defaults to 3; error messages
	// want more, identifiers want less.
	MinLength int `toml:"min_length"`

	// Compiled is filled at load time.
	Compiled []*regexp.Regexp `toml:"-"`
}

// Load reads and validates a domain config from a TOML file.
func Load(path string) (*Domain, error) {
	var d Domain
	if _, err := toml.DecodeFile(path, &d); err != nil {
		return nil, fmt.Errorf("load domain %q: %w", path, err)
	}
	if d.Name == "" {
		return nil, fmt.Errorf("domain %q: missing required field 'name'", path)
	}
	if d.Persona == "" {
		return nil, fmt.Errorf("domain %q: missing required field 'persona'", path)
	}
	if d.Title == "" {
		d.Title = d.Name
	}
	switch d.Vocabulary.Code.Resolution {
	case "":
		d.Vocabulary.Code.Resolution = ResolutionSyntactic
	case ResolutionSyntactic, ResolutionTyped:
	default:
		return nil, fmt.Errorf("domain %q: vocabulary.code resolution %q must be %q or %q",
			path, d.Vocabulary.Code.Resolution, ResolutionSyntactic, ResolutionTyped)
	}
	// Each list defaults independently: a domain narrowing the node types it
	// admits should not silently lose the edge types along with them.
	if len(d.Vocabulary.Code.EntityTypes) == 0 {
		d.Vocabulary.Code.EntityTypes = append([]string(nil), defaultCodeEntityTypes...)
	}
	if len(d.Vocabulary.Code.RelationTypes) == 0 {
		d.Vocabulary.Code.RelationTypes = append([]string(nil), defaultCodeRelationTypes...)
	}
	for _, p := range d.ErrorPatternsRaw {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("domain %q: invalid error_pattern %q: %w", path, p, err)
		}
		d.ErrorPatterns = append(d.ErrorPatterns, re)
	}
	return &d, nil
}

// SourceGroups returns every group to mine from source files — just the
// error_patterns group. See the SourceGroup comment for why it is alone.
func (d *Domain) SourceGroups() []SourceGroup {
	var out []SourceGroup
	if len(d.ErrorPatterns) > 0 {
		out = append(out, SourceGroup{
			Name:      "errors",
			Summary:   "Error and log messages emitted by",
			Compiled:  d.ErrorPatterns,
			MinLength: 8, // a message shorter than this is a fragment, not a line
		})
	}
	return out
}
