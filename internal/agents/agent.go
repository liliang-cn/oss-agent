// Package agents builds the ops agent: an agent-go service wired with a frontier
// LLM, a cortexdb-backed graph memory, the domain's read-only probe tools, a
// GraphRAG knowledge_search tool, and the deterministic red-line safety wall.
// Everything product-specific comes from the *domain.Domain, so the same builder
// serves any storage or infrastructure product.
package agents

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/liliang-cn/agent-go/v2/pkg/agent"
	agdomain "github.com/liliang-cn/agent-go/v2/pkg/domain"
	"github.com/liliang-cn/agent-go/v2/pkg/providers"

	"github.com/liliang-cn/oss-agent/internal/cite"
	"github.com/liliang-cn/oss-agent/internal/config"
	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/extract"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/probes"
	"github.com/liliang-cn/oss-agent/internal/safety"
)

// LLM builds a bare LLM generator from config (used by scaffolding/extraction
// paths that need raw generation without the full agent service).
func LLM(cfg config.Config) (agdomain.Generator, error) {
	return providers.NewOpenAILLMProvider(&agdomain.OpenAIProviderConfig{
		BaseURL:  cfg.LLMBaseURL,
		APIKey:   cfg.LLMAPIKey,
		LLMModel: cfg.LLMModel,
	})
}

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
		WithSystemPrompt(dom.Persona + citationDirective).
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
	registerSuggestAction(svc, filter)
	return svc, store, nil
}

// SuggestActionToolName is the tool the agent calls to propose a state-changing
// action for operator approval (it never executes anything). Stream converts the
// tool's result into an EventSuggestion.
const SuggestActionToolName = "suggest_action"

// registerSuggestAction exposes the structured suggestion channel. The handler
// does NOT execute anything: it runs the proposed action through the deterministic
// red-line wall, then returns the structured proposal (action, params, reason,
// severity, verdict) so the streaming layer can render an approve card while the
// ReAct loop continues from a short confirmation message.
func registerSuggestAction(svc *agent.Service, filter *safety.Filter) {
	params := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "Machine-readable action id to propose, e.g. \"ha.evict\" or \"resource.promote\". Never executed — only proposed for operator approval.",
			},
			"params": map[string]interface{}{
				"type":        "object",
				"description": "Arbitrary key/value parameters for the action (e.g. node, resource, volume).",
			},
			"reason": map[string]interface{}{
				"type":        "string",
				"description": "Why this action is recommended, grounded in the current symptom/evidence.",
			},
			"severity": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"low", "medium", "high"},
				"description": "Operator-facing risk level. Defaults to \"medium\".",
			},
		},
		"required": []string{"action", "reason"},
	}
	svc.AddTool(SuggestActionToolName,
		"Propose a state-changing action for a human operator to approve. This does NOT execute "+
			"anything — use it whenever the safe next step is a mutation (evict, promote, resize, "+
			"restart, …) instead of describing the raw command. The proposal is checked against the "+
			"red-line wall and surfaced to the operator as an approve/reject card.",
		params,
		func(_ context.Context, args map[string]interface{}) (interface{}, error) {
			return suggestionResult(filter, args), nil
		})
}

// suggestionResult is the pure core of the suggest_action handler: it normalizes
// the proposal, runs it through the red-line wall, and returns the JSON-safe result
// the streaming layer converts into an EventSuggestion. It executes nothing.
func suggestionResult(filter *safety.Filter, args map[string]interface{}) map[string]interface{} {
	action, _ := args["action"].(string)
	reason, _ := args["reason"].(string)
	severity, _ := args["severity"].(string)
	if severity == "" {
		severity = "medium"
	}
	var p map[string]interface{}
	if raw, ok := args["params"].(map[string]interface{}); ok {
		p = raw
	}
	verdict := filter.Check(actionCommandString(action, p))
	return map[string]interface{}{
		"ok":      true,
		"message": "suggestion recorded; awaiting operator approval",
		"suggestion": map[string]interface{}{
			"action":   action,
			"params":   p,
			"reason":   reason,
			"severity": severity,
			"verdict":  verdict,
		},
	}
}

// actionCommandString renders a proposal as a single command-like line so the
// deterministic red-line wall (which matches shell command patterns) can rule on
// it: "action k1=v1 k2=v2" with keys sorted for stable matching.
func actionCommandString(action string, params map[string]interface{}) string {
	if len(params) == 0 {
		return action
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(action)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, params[k])
	}
	return b.String()
}

// citationDirective is appended to every domain's persona so answers cite their
// retrieved evidence regardless of what the domain.toml persona says. Each
// knowledge_search hit carries a stable, readable "cite" label derived from its
// source, so the same source keeps the same label across every search in a turn.
const citationDirective = `

Citations (required — do this every time you use knowledge_search):
- Each hit has a "cite" label (e.g. drbd-troubleshooting.adoc) and a full "source".
  When a statement in your answer comes from a hit, append its label in square
  brackets right after the claim, e.g. "... triggers a full resync [drbd-troubleshooting].".
- The same source always has the same label — reuse it, and combine like [a][b].
- End the answer with a "Sources" section: one line per label you cited,
  "- [label] full-source". List each source once; only labels that appeared in
  knowledge_search results — never invent one.`

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
			gr, err := store.SearchGraph(ctx, query, 4)
			if err != nil {
				return map[string]interface{}{"ok": false, "error": err.Error()}, nil
			}
			// Tag each hit [S1], [S2]… so the model can cite it inline. The tag's
			// source is the chunk's document id (file path / doc identifier).
			hits := make([]map[string]interface{}, 0, len(gr.Hits))
			for _, h := range gr.Hits {
				hits = append(hits, map[string]interface{}{
					"cite":     cite.Label(h.DocumentID),
					"source":   h.DocumentID,
					"content":  h.Content,
					"score":    h.Score,
					"entities": h.Entities,
				})
			}
			return map[string]interface{}{
				"ok":                true,
				"hits":              hits,
				"related_via_graph": gr.Neighbors,
				"citation_hint":     "Cite each grounded statement inline with the hit's [cite] label; list the labels you used under a final Sources section.",
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
