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

	// ErrorPatterns are compiled from ErrorPatternsRaw at load time.
	ErrorPatterns []*regexp.Regexp `toml:"-"`
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
	return &d, nil
}
