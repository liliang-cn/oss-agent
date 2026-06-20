// Package agents builds the ops agent: an agent-go service wired with a frontier
// LLM, a cortexdb-backed graph memory, the domain's read-only probe tools, a
// GraphRAG knowledge_search tool, and the deterministic red-line safety wall.
// Everything product-specific comes from the *domain.Domain, so the same builder
// serves LINBIT, RustFS, or any future storage product.
package agents

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/liliang-cn/agent-go/v2/pkg/agent"
	agdomain "github.com/liliang-cn/agent-go/v2/pkg/domain"
	"github.com/liliang-cn/agent-go/v2/pkg/providers"

	"github.com/liliang-cn/oss-agent/internal/config"
	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/extract"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/probes"
	"github.com/liliang-cn/oss-agent/internal/safety"
)

// BuildExtractor returns an LLM ontology extractor for the domain, or nil if no
// LLM key is configured (ingestion then stores vectors without graph extraction).
func BuildExtractor(cfg config.Config, dom *domain.Domain) *extract.Extractor {
	if cfg.LLMAPIKey == "" {
		return nil
	}
	llm, err := providers.NewOpenAILLMProvider(&agdomain.OpenAIProviderConfig{
		BaseURL:  cfg.LLMBaseURL,
		APIKey:   cfg.LLMAPIKey,
		LLMModel: cfg.LLMModel,
	})
	if err != nil {
		return nil
	}
	return extract.New(llm, dom)
}

// Build constructs the configured agent service and its knowledge store for the
// given domain. Caller must Close both.
func Build(cfg config.Config, dom *domain.Domain) (*agent.Service, *knowledge.Store, error) {
	llm, err := providers.NewOpenAILLMProvider(&agdomain.OpenAIProviderConfig{
		BaseURL:  cfg.LLMBaseURL,
		APIKey:   cfg.LLMAPIKey,
		LLMModel: cfg.LLMModel,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("init llm: %w", err)
	}
	emb, err := providers.NewOpenAIEmbedderProvider(&agdomain.OpenAIProviderConfig{
		BaseURL:        cfg.EmbBaseURL,
		APIKey:         cfg.EmbAPIKey,
		EmbeddingModel: cfg.EmbModel,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("init embedder: %w", err)
	}

	store, err := knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
	if err != nil {
		return nil, nil, fmt.Errorf("open knowledge: %w", err)
	}

	// Note: the knowledge base is our own cortexdb (knowledge_search tool), so we
	// don't enable agent-go's separate graph memory here. The embedder is wired in
	// case skills/RAG use it.
	//
	// PTC (Programmatic Tool Calling) is disabled: it makes the model emit code that
	// calls tools, then splits its reply between a "Return Value" summary and "Logs",
	// which Text() concatenates into a noisy dump. Plain function-calling still does
	// iterative ReAct tool use but yields a single clean text answer in FinalResult —
	// the right shape for an ask/diagnose agent.
	svc, err := agent.New("oss-agent").
		WithSystemPrompt(dom.Persona).
		WithLLM(llm).
		WithEmbedder(emb).
		WithSkills(). // loads ~/.agentgo/skills (understand-* codebase-comprehension skills)
		WithPTC(false).
		WithDBPath(cfg.DBPath).
		Build()
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("build agent: %w", err)
	}

	filter, err := safety.NewFromSpecs(dom.RedLines)
	if err != nil {
		svc.Close()
		store.Close()
		return nil, nil, fmt.Errorf("compile red-lines: %w", err)
	}
	registerProbes(svc, dom.Probes, filter)
	registerKnowledgeSearch(svc, store)
	registerSafetyLint(svc, filter)
	return svc, store, nil
}

// registerKnowledgeSearch exposes the GraphRAG knowledge base as a tool.
func registerKnowledgeSearch(svc *agent.Service, store *knowledge.Store) {
	params := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "What to look up (symptom, error string, command, concept).",
			},
		},
		"required": []string{"query"},
	}
	svc.AddTool("knowledge_search",
		"Search the GraphRAG knowledge base (code, docs, recovery procedures, source error strings). "+
			"Returns the top hits plus related code reached one hop along the knowledge graph "+
			"(calls/contains/depends_on/… edges), so call sites and implementations come together.",
		params,
		func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			query, _ := args["query"].(string)
			gr, err := store.SearchGraph(ctx, query, 6)
			if err != nil {
				return map[string]interface{}{"ok": false, "error": err.Error()}, nil
			}
			return map[string]interface{}{
				"ok":               true,
				"hits":             gr.Hits,
				"related_via_graph": gr.Neighbors,
			}, nil
		})
}

// registerProbes wires each read-only diagnostic command as an agent tool.
func registerProbes(svc *agent.Service, list []probes.Probe, filter *safety.Filter) {
	noParams := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	for _, p := range list {
		pp := p // capture
		svc.AddTool(pp.Name, pp.Description, noParams,
			func(ctx context.Context, _ map[string]interface{}) (interface{}, error) {
				out, runErr := probes.Run(ctx, filter, pp.Argv)
				return map[string]interface{}{
					"command": strings.Join(pp.Argv, " "),
					"output":  out,
					"ok":      runErr == nil,
				}, nil
			})
	}
}

var codeBlock = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\\n(.*?)```")

// registerSafetyLint runs the deterministic red-line wall over any fenced code
// block in the model's answer; destructive one-liners fail the lint and force a
// reframe into a guarded, confirmation-required step.
func registerSafetyLint(svc *agent.Service, filter *safety.Filter) {
	svc.RegisterOutputLint(agent.LintFunc{
		NameValue: "redline_no_destructive_oneliner",
		Fn: func(text string, _ agent.LintContext) (bool, string) {
			for _, m := range codeBlock.FindAllStringSubmatch(text, -1) {
				if v := filter.Check(m[1]); v.Blocked {
					return false, fmt.Sprintf(
						"Your answer hands the operator a destructive command in a code block "+
							"[%s: %s]. Do not present it as a runnable one-liner. Instead explain the "+
							"risk, affected volumes, and required backup, and state it needs explicit "+
							"operator confirmation plus an unlock key.", v.RuleID, v.Reason)
				}
			}
			return true, ""
		},
	})
}
