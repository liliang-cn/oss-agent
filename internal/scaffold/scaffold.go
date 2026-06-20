// Package scaffold drafts a domain.toml for a new project from its repository,
// using an LLM. domain.toml is the single product-specific input the engine needs;
// authoring it by hand is the main human bottleneck in onboarding. This turns
// "point at a repo" into "review a draft": the LLM proposes persona, ontology
// vocabulary, error-string patterns, read-only probes, and candidate red-lines;
// a human still MUST review the red-lines (the safety wall) before trusting it.
package scaffold

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	agdomain "github.com/liliang-cn/agent-go/v2/pkg/domain"
)

// draft is the JSON shape we ask the LLM to fill.
type draft struct {
	Name          string   `json:"name"`
	Title         string   `json:"title"`
	Persona       string   `json:"persona"`
	EntityTypes   []string `json:"entity_types"`
	RelationTypes []string `json:"relation_types"`
	ErrorPatterns []string `json:"error_patterns"`
	Probes        []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Argv        []string `json:"argv"`
	} `json:"probes"`
	RedLines []struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		Pattern  string `json:"pattern"`
		Reason   string `json:"reason"`
	} `json:"red_lines"`
}

// Generate inspects repoDir, asks the LLM to draft a domain config, and returns a
// rendered domain.toml string. repoName seeds the default name/title.
func Generate(ctx context.Context, llm agdomain.Generator, repoDir, repoName string) (string, error) {
	sig := gatherSignals(repoDir, repoName)
	out, err := llm.Generate(ctx, prompt(sig), &agdomain.GenerationOptions{
		Temperature: 0.2,
		MaxTokens:   3000,
	})
	if err != nil {
		return "", fmt.Errorf("llm generate: %w", err)
	}
	d, err := parseDraft(out)
	if err != nil {
		return "", err
	}
	if d.Name == "" {
		d.Name = repoName
	}
	return render(d, repoName), nil
}

// signals is the compact repo summary fed to the LLM.
type signals struct {
	Name      string
	Readme    string
	TopLevel  []string
	Languages []string
}

func gatherSignals(dir, name string) signals {
	s := signals{Name: name}
	// README (first ~6KB of the first match)
	for _, cand := range []string{"README.md", "README.adoc", "README.rst", "README.txt", "README"} {
		if b, err := os.ReadFile(filepath.Join(dir, cand)); err == nil {
			s.Readme = truncate(string(b), 6000)
			break
		}
	}
	// top-level entries + language histogram
	entries, _ := os.ReadDir(dir)
	langCount := map[string]int{}
	for _, e := range entries {
		nm := e.Name()
		if strings.HasPrefix(nm, ".") {
			continue
		}
		if e.IsDir() {
			s.TopLevel = append(s.TopLevel, nm+"/")
		} else {
			s.TopLevel = append(s.TopLevel, nm)
		}
	}
	// shallow language scan (one level into top dirs)
	countExt := func(p string) {
		if ext := strings.ToLower(filepath.Ext(p)); ext != "" {
			langCount[ext]++
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			sub, _ := os.ReadDir(filepath.Join(dir, e.Name()))
			for _, f := range sub {
				if !f.IsDir() {
					countExt(f.Name())
				}
			}
		} else {
			countExt(e.Name())
		}
	}
	type kv struct {
		ext string
		n   int
	}
	var langs []kv
	for k, v := range langCount {
		langs = append(langs, kv{k, v})
	}
	sort.Slice(langs, func(i, j int) bool { return langs[i].n > langs[j].n })
	for i, l := range langs {
		if i >= 8 {
			break
		}
		s.Languages = append(s.Languages, fmt.Sprintf("%s(%d)", l.ext, l.n))
	}
	if len(s.TopLevel) > 40 {
		s.TopLevel = s.TopLevel[:40]
	}
	sort.Strings(s.TopLevel)
	return s
}

func prompt(s signals) string {
	return fmt.Sprintf(`You are configuring an AI ops/support agent for an open-source software product.
Produce a domain configuration as STRICT JSON only (no prose, no code fence).

The product repository is %q.
Top-level entries: %s
Dominant file types: %s

README (may be truncated):
"""
%s
"""

Fill this JSON exactly:
{
 "name": "human label for the product, e.g. \"Redis (in-memory store)\"",
 "title": "short UI brand, e.g. \"Redis knowledge\"",
 "persona": "the agent's system prompt: who it is (an expert SRE copilot for THIS product), that it reasons step by step (ReAct) and grounds answers via knowledge_search and read-only probes, and a non-negotiable safety section saying a deterministic wall blocks data-destroying commands and that such fixes require explicit operator confirmation. Tailor it to this product.",
 "entity_types": ["8-12 domain object types this product reasons about (its nouns)"],
 "relation_types": ["6-10 UPPER_SNAKE relation types between those objects"],
 "error_patterns": ["3-6 Go regexes that capture log/error message text emitted by THIS product's source; group 1 = the message. Match the languages above (e.g. fprintf/log./panic/LOG()). Use double-backslash for regex escapes in JSON."],
 "probes": [{"name":"snake_name","description":"what this read-only command shows","argv":["the","read-only","cli","command"]}],
 "red_lines": [{"id":"kebab-id","severity":"CRITICAL|HIGH","pattern":"Go regex matching a DESTRUCTIVE command for this product","reason":"why it is dangerous"}]
}
Make probes and red_lines specific to THIS product's real CLI. If unsure of exact commands, give the most likely real ones. Output JSON only.`,
		s.Name, strings.Join(s.TopLevel, ", "), strings.Join(s.Languages, ", "), s.Readme)
}

func parseDraft(out string) (*draft, error) {
	out = strings.TrimSpace(out)
	if i := strings.Index(out, "{"); i > 0 {
		out = out[i:]
	}
	if j := strings.LastIndex(out, "}"); j >= 0 {
		out = out[:j+1]
	}
	var d draft
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return nil, fmt.Errorf("parse draft JSON: %w", err)
	}
	return &d, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
