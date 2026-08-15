// Package domain is the platform's product-agnostic seam. oss-agent is a generic
// engine; each project that uses it describes ITS OWN product in a domain.toml
// config — persona, ontology vocabulary, source error patterns, read-only probes,
// and repos. No product is compiled into the platform; a worked example config
// ships under examples/.
package domain

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/liliang-cn/oss-agent/internal/probes"
	"github.com/liliang-cn/oss-agent/internal/safety"
)

// Domain bundles the product-specific knowledge that shapes the agent. It is
// loaded from a domain.toml; the engine treats it as opaque config.
type Domain struct {
	Name             string            `toml:"name"`            // human label
	Title            string            `toml:"title"`           // UI title/brand (defaults to Name)
	Persona          string            `toml:"persona"`         // agent system prompt
	EntityTypes      []string          `toml:"entity_types"`    // ontology node types
	RelationTypes    []string          `toml:"relation_types"`  // ontology edge types
	ErrorPatternsRaw []string          `toml:"error_patterns"`  // regex sources (group 1 = message)
	SourcePatterns   []SourceGroup     `toml:"source_patterns"` // named groups mined from source files
	Probes           []probes.Probe    `toml:"probes"`          // read-only diagnostic commands
	Repos            []string          `toml:"repos"`           // upstream repos to ingest
	RedLines         []safety.RuleSpec `toml:"red_lines"`       // deterministic destructive-command blocks

	// ErrorPatterns are compiled from ErrorPatternsRaw at load time.
	ErrorPatterns []*regexp.Regexp `toml:"-"`
}

// SourceGroup is one named thing to mine out of source files.
//
// error_patterns answers "what messages does this code print", which is what an
// operator reading a log needs. It is not the only question a codebase answers.
// The types a system declares ARE its domain model — a Go project's
// "type HAResource struct" says what an HA resource is more precisely than any
// prose about it — and mining them puts the vocabulary the ontology asks for in
// front of the extractor instead of hoping the docs happened to spell it out.
//
// Each group becomes its own document per source file, titled by Summary, so a
// question about types retrieves types rather than competing with log strings
// inside one undifferentiated blob.
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
	for _, p := range d.ErrorPatternsRaw {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("domain %q: invalid error_pattern %q: %w", path, p, err)
		}
		d.ErrorPatterns = append(d.ErrorPatterns, re)
	}
	for i := range d.SourcePatterns {
		g := &d.SourcePatterns[i]
		if strings.TrimSpace(g.Name) == "" {
			return nil, fmt.Errorf("domain %q: source_patterns[%d] needs a name", path, i)
		}
		if g.Summary == "" {
			g.Summary = "Extracted from"
		}
		if g.MinLength <= 0 {
			g.MinLength = 3
		}
		for _, raw := range g.Patterns {
			re, err := regexp.Compile(raw)
			if err != nil {
				return nil, fmt.Errorf("domain %q: source_patterns[%q]: invalid pattern %q: %w", path, g.Name, raw, err)
			}
			g.Compiled = append(g.Compiled, re)
		}
	}
	return &d, nil
}

// SourceGroups returns every group to mine, with error_patterns folded in as one
// so a domain that only declares the old field keeps working unchanged.
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
	return append(out, d.SourcePatterns...)
}
