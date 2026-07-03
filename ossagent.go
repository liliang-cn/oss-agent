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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/liliang-cn/agent-go/v2/pkg/agent"
	"github.com/liliang-cn/agent-go/v2/pkg/mcp"

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

	// MCPServers are external MCP servers whose tools are mounted into the ReAct
	// loop. This is a code-only knob (no OSS_* fallback). A server that fails to
	// connect is skipped (the agent degrades to knowledge-only); inspect the
	// outcome with Agent.MCPStatus. Mark a spec ReadOnly to mount only its
	// observational tools — the safe default for cluster-control servers.
	MCPServers []MCPServerSpec
}

// MCPServerSpec configures one external MCP server to mount into the agent.
type MCPServerSpec struct {
	Name              string            // logical name, e.g. "sds"
	Transport         string            // "stdio" | "http" | "sse" (default "stdio")
	Command           string            // stdio: executable
	Args              []string          // stdio args
	URL               string            // http/sse base URL
	Headers           map[string]string // http/sse optional headers
	ReadOnly          bool              // when true, mount only read-only tools
	ReadOnlyToolAllow []string          // explicit allowlist (optional; overrides the name heuristic)
}

// MCPStatus reports the outcome of mounting one MCP server in New.
type MCPStatus = agents.MCPMountStatus

// Agent is a configured ops/support agent. It is safe for sequential use; run one
// Ask/Chat/Stream at a time (the underlying ReAct loop is single-flight). Always Close it.
type Agent struct {
	svc     *agent.Service
	store   *knowledge.Store
	dom     *domain.Domain
	filter  *safety.Filter
	mcp     []*mcp.Client
	mcpStat []MCPStatus
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

	a := &Agent{svc: svc, store: store, dom: dom, filter: filter}

	// Mount any external MCP servers. This never fails New: unreachable servers are
	// recorded in mcpStat and skipped, leaving a knowledge-only agent.
	if len(cfg.MCPServers) > 0 {
		specs := make([]agents.MCPSpec, len(cfg.MCPServers))
		for i, s := range cfg.MCPServers {
			specs[i] = agents.MCPSpec{
				Name:              s.Name,
				Transport:         s.Transport,
				Command:           s.Command,
				Args:              s.Args,
				URL:               s.URL,
				Headers:           s.Headers,
				ReadOnly:          s.ReadOnly,
				ReadOnlyToolAllow: s.ReadOnlyToolAllow,
			}
		}
		a.mcp, a.mcpStat = agents.MountMCP(context.Background(), svc, specs)
	}

	return a, nil
}

// MCPStatus returns the per-server outcome of mounting the configured MCP servers
// (whether each connected, how many tools were registered, and any error). It is
// empty when no MCPServers were configured.
func (a *Agent) MCPStatus() []MCPStatus { return a.mcpStat }

// Close releases the agent's LLM service, knowledge store, and MCP clients.
func (a *Agent) Close() error {
	a.svc.Close()
	for _, c := range a.mcp {
		_ = c.Close()
	}
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

// maxToolRounds caps the ReAct tool-call budget per turn so a model that fails to
// converge (redundantly re-calling the same tool) is forced to answer with what it
// has instead of looping unboundedly and burning API cost.
const maxToolRounds = 8

// debugEnabled turns on verbose per-turn tracing (every tool call with args, every
// tool result, suggestions, resets and the final answer). Enable with OSS_DEBUG=1
// (or true/yes/on). Off by default so production logs stay quiet.
func debugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OSS_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// truncate shortens s to n runes for log lines so a huge tool result / answer does
// not flood the journal.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("…(+%d chars)", len(r)-n)
}

// jsonCompact renders args as a single-line JSON string for logging.
func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// Chat runs a turn within a persistent session: history keyed by sessionID is
// loaded and saved by the agent, so follow-ups remember earlier turns.
func (a *Agent) Chat(ctx context.Context, sessionID, message string) (string, error) {
	res, err := a.svc.Run(ctx, message, agent.WithSessionID(sessionID), agent.WithMaxTurns(maxToolRounds))
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
	EventSuggestion EventKind = "suggestion"  // Suggestion: a structured, non-executed action proposal
)

// Suggestion is a structured, NON-executed action the agent proposes for an
// operator to approve. It is emitted via EventSuggestion when the agent calls the
// built-in suggest_action tool; nothing is run. Verdict is the deterministic
// red-line wall's ruling on the proposed action (Verdict.Blocked ⇒ the action is
// destructive and needs explicit, guarded confirmation).
type Suggestion struct {
	Action   string         `json:"action"`
	Params   map[string]any `json:"params"`
	Reason   string         `json:"reason"`
	Severity string         `json:"severity"` // "low" | "medium" | "high"
	Verdict  Verdict        `json:"verdict"`
}

// Event is one item in a streamed run. For EventSuggestion, Suggestion is set (and
// Tool is "suggest_action"); for tool events Tool/Args are set; otherwise Text.
type Event struct {
	Kind       EventKind
	Text       string
	Tool       string
	Args       map[string]any
	Suggestion *Suggestion // set only for EventSuggestion
}

// Stream runs a question and delivers Events to on as they happen (tool calls,
// answer deltas, …). It returns the full answer (with a deterministic Sources
// footer appended when the model didn't cite) and the sources retrieved this turn.
//
// The model sometimes streams a short preamble, then delivers the real answer via
// its completion tool; when that happens Stream emits an EventReset before the
// authoritative answer so a UI can clear the preamble.
func (a *Agent) Stream(ctx context.Context, question string, on func(Event)) (answer string, sources []string, err error) {
	dbg := debugEnabled()
	if dbg {
		log.Printf("[ossagent] ▶ turn start: question=%q (maxToolRounds=%d, mcp_servers=%d)", truncate(question, 300), maxToolRounds, len(a.mcp))
	}
	events, err := a.svc.RunStreamWithOptions(ctx, question, agent.WithMaxTurns(maxToolRounds))
	if err != nil {
		if dbg {
			log.Printf("[ossagent] ✗ RunStream error: %v", err)
		}
		return "", nil, err
	}
	seen := map[string]bool{}
	var streamed bool
	var full, final string
	var toolCalls, toolResults, partials, suggestions int
	for ev := range events {
		switch ev.Type {
		case agent.EventTypePartial:
			if ev.Content != "" {
				streamed = true
				partials++
				full += ev.Content
				if dbg {
					log.Printf("[ossagent]   · text delta (%d chars): %q", len([]rune(ev.Content)), truncate(ev.Content, 120))
				}
				on(Event{Kind: EventText, Text: ev.Content})
			}
		case agent.EventTypeToolCall:
			if ev.ToolName == "task_complete" { // internal answer sentinel
				if dbg {
					log.Printf("[ossagent]   ✓ task_complete (model signalled done)")
				}
				continue
			}
			if ev.ToolName == agents.SuggestActionToolName {
				if dbg {
					log.Printf("[ossagent]   ⚑ suggest_action call args=%s", jsonCompact(ev.ToolArgs))
				}
				continue // the proposal is surfaced on the tool result as EventSuggestion
			}
			toolCalls++
			if dbg {
				log.Printf("[ossagent]   → tool call #%d: %s args=%s", toolCalls, ev.ToolName, jsonCompact(ev.ToolArgs))
			}
			on(Event{Kind: EventToolCall, Tool: ev.ToolName, Args: ev.ToolArgs})
		case agent.EventTypeToolResult:
			if ev.ToolName == "task_complete" {
				continue
			}
			if ev.ToolName == agents.SuggestActionToolName {
				if s := parseSuggestion(ev.ToolResult); s != nil {
					suggestions++
					if dbg {
						log.Printf("[ossagent]   ⚑ suggestion #%d: action=%s params=%s severity=%s blocked=%v reason=%q",
							suggestions, s.Action, jsonCompact(s.Params), s.Severity, s.Verdict.Blocked, truncate(s.Reason, 160))
					}
					on(Event{Kind: EventSuggestion, Tool: ev.ToolName, Suggestion: s})
				}
				continue
			}
			if ev.ToolName == "knowledge_search" {
				cite.CollectSources(ev.ToolResult, seen, &sources)
			}
			toolResults++
			if dbg {
				log.Printf("[ossagent]   ← tool result #%d: %s → %s", toolResults, ev.ToolName, truncate(fmt.Sprintf("%v", ev.ToolResult), 240))
			}
			on(Event{Kind: EventToolResult, Tool: ev.ToolName})
		case agent.EventTypeComplete:
			final = ev.Content
			if dbg {
				log.Printf("[ossagent]   ■ complete: answer (%d chars): %q", len([]rune(final)), truncate(final, 200))
			}
		case agent.EventTypeError:
			if strings.Contains(ev.Content, "compaction") { // internal, non-fatal
				continue
			}
			if dbg {
				log.Printf("[ossagent]   ⚠ error event: %s", truncate(ev.Content, 200))
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
	if dbg {
		log.Printf("[ossagent] ◀ turn done: tool_calls=%d tool_results=%d suggestions=%d text_deltas=%d sources=%d answer_len=%d",
			toolCalls, toolResults, suggestions, partials, len(sources), len([]rune(full)))
	}
	return full, sources, nil
}

// parseSuggestion reconstructs the structured proposal from the suggest_action
// tool result. It round-trips through JSON so it is robust to whatever concrete
// Go shape the event carries (map, typed value, or a JSON string).
func parseSuggestion(res any) *Suggestion {
	var b []byte
	if s, ok := res.(string); ok {
		b = []byte(s)
	} else {
		var err error
		if b, err = json.Marshal(res); err != nil {
			return nil
		}
	}
	var parsed struct {
		Suggestion *Suggestion `json:"suggestion"`
	}
	if json.Unmarshal(b, &parsed) != nil || parsed.Suggestion == nil {
		return nil
	}
	return parsed.Suggestion
}
