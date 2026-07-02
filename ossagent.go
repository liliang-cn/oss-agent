// Package ossagent is the library API for building a product-agnostic AI ops &
// support agent over an open-source project's code, docs, and runbooks.
//
// The engine knows nothing about any specific product: a "domain" (persona,
// ontology vocabulary, log error patterns, read-only probes, destructive-command
// red-lines, repos) is supplied as a domain.toml or in code. The same agent then
// answers and diagnoses questions grounded in a GraphRAG knowledge base, streams
// its tool use, cites sources, triages logs, and blocks destructive commands.
//
// Minimal use:
//
//	a, err := ossagent.New(ossagent.Config{DomainFile: "domain.toml"})
//	if err != nil { log.Fatal(err) }
//	defer a.Close()
//	answer, err := a.Ask(ctx, "How do I recover a resource stuck in a degraded state?")
//
// Any Config field left zero falls back to the matching OSS_* environment variable
// and then a built-in default, so a process configured purely through the
// environment can call ossagent.New(ossagent.Config{}).
package ossagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/liliang-cn/agent-go/v2/pkg/agent"

	"github.com/liliang-cn/oss-agent/internal/agents"
	"github.com/liliang-cn/oss-agent/internal/cite"
	"github.com/liliang-cn/oss-agent/internal/config"
	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/loganalyze"
	"github.com/liliang-cn/oss-agent/internal/safety"
)

// Re-exported types so callers never import internal packages.
type (
	// Domain is a product configuration: persona, ontology, error patterns,
	// probes, red-lines, repos. Load one from TOML with LoadDomain.
	Domain = domain.Domain
	// Hit is a retrieved knowledge chunk.
	Hit = knowledge.Hit
	// Verdict is the red-line wall's ruling on a command.
	Verdict = safety.Verdict
	// LogReport is the result of triaging a log file/dir/archive.
	LogReport = loganalyze.Report
)

// Config configures an Agent. Every zero-valued field falls back to its OSS_*
// environment variable and then a default (see the package docs).
type Config struct {
	LLMBaseURL string // OSS_LLM_BASE_URL (default https://api.openai.com/v1)
	LLMAPIKey  string // OSS_LLM_API_KEY (required for Ask/Diagnose/Chat/Stream)
	LLMModel   string // OSS_LLM_MODEL (default gpt-4o)

	EmbBaseURL string // OSS_EMB_BASE_URL (defaults to LLMBaseURL)
	EmbAPIKey  string // OSS_EMB_API_KEY (defaults to LLMAPIKey)
	EmbModel   string // OSS_EMB_MODEL (default text-embedding-3-small)
	EmbDim     int    // OSS_EMB_DIM (default 1536) — must match the index

	KnowledgeDBPath string // OSS_KNOWLEDGE_DB_PATH (default ./data/knowledge.db)
	SessionDBPath   string // OSS_DB_PATH (default ./data/oss-agent.db)

	// Domain source. Set Domain for an in-memory config, or DomainFile for a path
	// to a domain.toml (OSS_DOMAIN_FILE if both are empty). Domain wins if set.
	DomainFile string
	Domain     *Domain
}

// Agent is a configured ops/support agent. It is safe for sequential use; run one
// Ask/Chat/Stream at a time (the underlying ReAct loop is single-flight). Always Close it.
type Agent struct {
	svc    *agent.Service
	store  *knowledge.Store
	dom    *domain.Domain
	filter *safety.Filter
}

// LoadDomain reads a domain.toml from disk.
func LoadDomain(path string) (*Domain, error) { return domain.Load(path) }

// New builds an Agent from cfg (env/defaults fill any zero fields).
func New(cfg Config) (*Agent, error) {
	c := config.Load() // env + defaults
	if cfg.LLMBaseURL != "" {
		c.LLMBaseURL = cfg.LLMBaseURL
	}
	if cfg.LLMAPIKey != "" {
		c.LLMAPIKey = cfg.LLMAPIKey
	}
	if cfg.LLMModel != "" {
		c.LLMModel = cfg.LLMModel
	}
	if cfg.EmbBaseURL != "" {
		c.EmbBaseURL = cfg.EmbBaseURL
	}
	if cfg.EmbAPIKey != "" {
		c.EmbAPIKey = cfg.EmbAPIKey
	}
	if cfg.EmbModel != "" {
		c.EmbModel = cfg.EmbModel
	}
	if cfg.EmbDim != 0 {
		c.EmbDim = cfg.EmbDim
	}
	if cfg.KnowledgeDBPath != "" {
		c.KnowledgeDBPath = cfg.KnowledgeDBPath
	}
	if cfg.SessionDBPath != "" {
		c.DBPath = cfg.SessionDBPath
	}

	dom := cfg.Domain
	if dom == nil {
		if cfg.DomainFile != "" {
			c.DomainFile = cfg.DomainFile
		}
		var err error
		if dom, err = domain.Load(c.DomainFile); err != nil {
			return nil, fmt.Errorf("load domain: %w", err)
		}
	}

	svc, store, err := agents.Build(c, dom)
	if err != nil {
		return nil, err
	}
	filter, err := safety.NewFromSpecs(dom.RedLines)
	if err != nil {
		svc.Close()
		store.Close()
		return nil, fmt.Errorf("compile red-lines: %w", err)
	}
	return &Agent{svc: svc, store: store, dom: dom, filter: filter}, nil
}

// Close releases the agent's LLM service and knowledge store.
func (a *Agent) Close() error {
	a.svc.Close()
	return a.store.Close()
}

// Domain returns the loaded product configuration.
func (a *Agent) Domain() *Domain { return a.dom }

// Ask runs one ReAct turn (probes + knowledge_search + red-line wall) and returns
// the grounded answer.
func (a *Agent) Ask(ctx context.Context, question string) (string, error) {
	return a.svc.Ask(ctx, question)
}

// Diagnose is Ask framed for troubleshooting a symptom.
func (a *Agent) Diagnose(ctx context.Context, symptom string) (string, error) {
	return a.svc.Ask(ctx, "Troubleshoot this symptom and give the safest recovery steps:\n"+symptom)
}

// Chat runs a turn within a persistent session: history keyed by sessionID is
// loaded and saved by the agent, so follow-ups remember earlier turns.
func (a *Agent) Chat(ctx context.Context, sessionID, message string) (string, error) {
	res, err := a.svc.Run(ctx, message, agent.WithSessionID(sessionID))
	if err != nil {
		return "", err
	}
	return res.Text(), nil
}

// Search returns the top reranked knowledge chunks for a query (no LLM call).
func (a *Agent) Search(ctx context.Context, query string, topK int) ([]Hit, error) {
	gr, err := a.store.SearchGraph(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	return gr.Hits, nil
}

// CheckCommand runs a command line through the deterministic red-line wall.
func (a *Agent) CheckCommand(command string) Verdict { return a.filter.Check(command) }

// AnalyzeLog triages a log file, directory, or archive (.tar.gz/.zip/…) into a
// ranked set of distinct problems using the domain's error patterns.
func (a *Agent) AnalyzeLog(path string) (*LogReport, error) {
	root, cleanup, err := loganalyze.Resolve(path)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return loganalyze.Analyze(root, a.dom.ErrorPatterns)
}

// EventKind classifies a streaming Event.
type EventKind string

const (
	EventText       EventKind = "text"        // Text is an answer delta
	EventToolCall   EventKind = "tool_call"   // Tool + Args: a tool the agent invoked
	EventToolResult EventKind = "tool_result" // Tool: that tool returned
	EventReset      EventKind = "reset"       // discard answer text emitted so far (a preamble)
	EventError      EventKind = "error"       // Text is a non-fatal error message
)

// Event is one item in a streamed run.
type Event struct {
	Kind EventKind
	Text string
	Tool string
	Args map[string]any
}

// Stream runs a question and delivers Events to on as they happen (tool calls,
// answer deltas, …). It returns the full answer (with a deterministic Sources
// footer appended when the model didn't cite) and the sources retrieved this turn.
//
// The model sometimes streams a short preamble, then delivers the real answer via
// its completion tool; when that happens Stream emits an EventReset before the
// authoritative answer so a UI can clear the preamble.
func (a *Agent) Stream(ctx context.Context, question string, on func(Event)) (answer string, sources []string, err error) {
	events, err := a.svc.RunStream(ctx, question)
	if err != nil {
		return "", nil, err
	}
	seen := map[string]bool{}
	var streamed bool
	var full, final string
	for ev := range events {
		switch ev.Type {
		case agent.EventTypePartial:
			if ev.Content != "" {
				streamed = true
				full += ev.Content
				on(Event{Kind: EventText, Text: ev.Content})
			}
		case agent.EventTypeToolCall:
			if ev.ToolName == "task_complete" { // internal answer sentinel
				continue
			}
			on(Event{Kind: EventToolCall, Tool: ev.ToolName, Args: ev.ToolArgs})
		case agent.EventTypeToolResult:
			if ev.ToolName == "task_complete" {
				continue
			}
			if ev.ToolName == "knowledge_search" {
				cite.CollectSources(ev.ToolResult, seen, &sources)
			}
			on(Event{Kind: EventToolResult, Tool: ev.ToolName})
		case agent.EventTypeComplete:
			final = ev.Content
		case agent.EventTypeError:
			if strings.Contains(ev.Content, "compaction") { // internal, non-fatal
				continue
			}
			on(Event{Kind: EventError, Text: ev.Content})
		}
	}
	// Emit the authoritative Complete answer if it wasn't already streamed.
	if final != "" && !strings.Contains(full, strings.TrimSpace(final)) {
		if streamed {
			on(Event{Kind: EventReset})
		}
		full = final
		on(Event{Kind: EventText, Text: final})
	}
	if foot := cite.Footer(full, sources); foot != "" {
		full += foot
		on(Event{Kind: EventText, Text: foot})
	}
	return full, sources, nil
}
