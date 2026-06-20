// Package safety is the platform's deterministic "red-line" interception
// mechanism. It is INDEPENDENT of any LLM: whatever a model proposes, a command
// matching a red-line rule is blocked here, in plain Go. The MECHANISM is generic
// and platform-level; the RULES (which commands are dangerous) are product-specific
// and supplied by the active Domain (domain.toml). No rules declared = nothing blocked.
package safety

import (
	"fmt"
	"regexp"
	"strings"
)

// Severity classifies how dangerous a matched command is.
type Severity string

const (
	SeverityCritical Severity = "CRITICAL" // data-destroying; requires unlock key + MFA
	SeverityHigh     Severity = "HIGH"     // forceful/override; requires explicit confirm
)

// RuleSpec is one red-line, as declared in a domain.toml ([[red_lines]]).
type RuleSpec struct {
	ID       string   `toml:"id"`
	Severity Severity `toml:"severity"`
	Pattern  string   `toml:"pattern"` // regex source matched against a normalized command
	Reason   string   `toml:"reason"`
}

type rule struct {
	spec RuleSpec
	re   *regexp.Regexp
}

// Verdict is the result of checking a command line.
type Verdict struct {
	Blocked           bool
	Command           string
	RuleID            string
	Severity          Severity
	Reason            string
	RequiresUnlockKey bool // true for CRITICAL: client must require unlock key / MFA
}

// Filter holds compiled red-line rules. Safe for concurrent use.
type Filter struct{ rules []rule }

// NewFromSpecs compiles a Filter from domain-declared rule specs.
func NewFromSpecs(specs []RuleSpec) (*Filter, error) {
	f := &Filter{}
	for _, s := range specs {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, fmt.Errorf("red-line %q: invalid pattern %q: %w", s.ID, s.Pattern, err)
		}
		f.rules = append(f.rules, rule{spec: s, re: re})
	}
	return f, nil
}

// splitter breaks a shell line into individual commands across ; && || | newlines.
var splitter = regexp.MustCompile(`(?:&&|\|\||[;|\n])`)

// Check evaluates a (possibly multi-command) shell line and returns the first
// red-line verdict found. Matching is case-insensitive, whitespace-normalized.
func (f *Filter) Check(line string) Verdict {
	for _, raw := range splitter.Split(line, -1) {
		cmd := normalize(raw)
		if cmd == "" {
			continue
		}
		for _, r := range f.rules {
			if r.re.MatchString(cmd) {
				return Verdict{
					Blocked:           true,
					Command:           strings.TrimSpace(raw),
					RuleID:            r.spec.ID,
					Severity:          r.spec.Severity,
					Reason:            r.spec.Reason,
					RequiresUnlockKey: r.spec.Severity == SeverityCritical,
				}
			}
		}
	}
	return Verdict{Blocked: false}
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
