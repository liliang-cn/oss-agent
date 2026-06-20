// Command oss-agent is the CLI entry point for the LINBIT-Ops agent.
//
//	oss-agent ask <question>        one-shot Q&A (may call read-only probes)
//	oss-agent diagnose <symptom>    same loop, framed for troubleshooting
//	oss-agent check <command...>    test a command against the red-line wall
//	oss-agent version
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/liliang-cn/oss-agent/internal/agents"
	"github.com/liliang-cn/oss-agent/internal/config"
	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/graphimport"
	"github.com/liliang-cn/oss-agent/internal/ingest"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/loganalyze"
	"github.com/liliang-cn/oss-agent/internal/safety"
)

const version = "oss-agent 0.1.0 (slice 1: core brain)"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "ask", "diagnose":
		runAsk(strings.Join(os.Args[2:], " "))
	case "ingest":
		runIngest(strings.Join(os.Args[2:], " "))
	case "ingest-repo":
		runIngestRepo(strings.Join(os.Args[2:], " "))
	case "import-graph":
		runImportGraph(strings.Join(os.Args[2:], " "))
	case "search":
		runSearch(strings.Join(os.Args[2:], " "))
	case "search-graph":
		runSearchGraph(strings.Join(os.Args[2:], " "))
	case "chat":
		runChat()
	case "analyze-log", "analyze-sos":
		runAnalyzeLog(strings.Join(os.Args[2:], " "))
	case "domain":
		runDomain()
	case "check":
		runCheck(strings.Join(os.Args[2:], " "))
	case "version", "-v", "--version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func runAsk(q string) {
	if strings.TrimSpace(q) == "" {
		fail("usage: oss-agent ask <question>")
	}
	cfg := config.Load()
	if cfg.LLMAPIKey == "" {
		fail("set OSS_LLM_API_KEY (frontier model API key)")
	}
	svc, store, err := agents.Build(cfg, loadDomain(cfg))
	if err != nil {
		fail("build agent: %v", err)
	}
	defer svc.Close()
	defer store.Close()

	answer, err := svc.Ask(context.Background(), q)
	if err != nil {
		fail("ask: %v", err)
	}
	fmt.Println(answer)
}

// runIngest loads knowledge into the GraphRAG store. With no argument it ingests
// the built-in seed; with a directory it ingests every *.md file found there.
func runIngest(dir string) {
	cfg := config.Load()
	if cfg.EmbAPIKey == "" {
		fail("set OSS_EMB_API_KEY (or OSS_LLM_API_KEY) for embeddings")
	}
	store, err := knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
	if err != nil {
		fail("open knowledge: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	ex := agents.BuildExtractor(cfg, loadDomain(cfg))

	if strings.TrimSpace(dir) == "" {
		fail("usage: oss-agent ingest <dir-with-*.md>   (for a code repo use: ingest-repo <url>)")
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	if len(matches) == 0 {
		fail("no *.md files in %s", dir)
	}
	for _, f := range matches {
		b, err := os.ReadFile(f)
		if err != nil {
			fail("read %s: %v", f, err)
		}
		id := strings.TrimSuffix(filepath.Base(f), ".md")
		if err := store.IngestSemantic(ctx, id, id, string(b), ex); err != nil {
			fail("ingest %s: %v", f, err)
		}
		fmt.Printf("ingested %s\n", f)
	}
	fmt.Printf("done: %d document(s) into %s\n", len(matches), cfg.KnowledgeDBPath)
}

// runIngestRepo is the one-liner: clone → understand → import. It shallow-clones
// (if given a URL), produces an Understand-Anything knowledge-graph.json (running
// the configured OSS_UNDERSTAND_CMD if the graph isn't already there), and imports
// that graph into cortexdb. If no graph can be produced, it falls back to the
// text/code error-string ingest. Importing needs an embedder key.
func runIngestRepo(arg string) {
	if strings.TrimSpace(arg) == "" {
		fail("usage: oss-agent ingest-repo <git-url|local-dir>")
	}
	cfg := config.Load()
	if cfg.EmbAPIKey == "" {
		fail("set OSS_EMB_API_KEY (or OSS_LLM_API_KEY) for embeddings")
	}
	store, err := knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
	if err != nil {
		fail("open knowledge: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// 1. resolve dir (clone if URL)
	dir, name := arg, filepath.Base(strings.TrimRight(arg, "/"))
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") || strings.HasSuffix(arg, ".git") {
		name = strings.TrimSuffix(filepath.Base(arg), ".git")
		dir = filepath.Join("repos", name)
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			fmt.Printf("[1/3] cloning %s → %s (shallow)\n", arg, dir)
			cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", arg, dir)
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if e := cmd.Run(); e != nil {
				fail("git clone: %v", e)
			}
		} else {
			fmt.Printf("[1/3] using existing clone %s\n", dir)
		}
	}

	// 2. understand → knowledge-graph.json (skip if it already exists)
	graphPath := filepath.Join(dir, ".understand-anything", "knowledge-graph.json")
	if !fileExists(graphPath) {
		if uc := os.Getenv("OSS_UNDERSTAND_CMD"); uc != "" {
			fmt.Printf("[2/3] understanding: %s (cwd=%s)\n", uc, dir)
			c := exec.CommandContext(ctx, "bash", "-lc", uc)
			c.Dir = dir
			c.Stdout, c.Stderr = os.Stderr, os.Stderr
			if e := c.Run(); e != nil {
				fmt.Fprintf(os.Stderr, "  understand step failed: %v\n", e)
			}
		} else {
			fmt.Println("[2/3] OSS_UNDERSTAND_CMD not set — skipping AST/LLM graph step")
		}
	} else {
		fmt.Printf("[2/3] found existing graph %s\n", graphPath)
	}

	// 3a. preferred path: import the AST/LLM knowledge graph
	if fileExists(graphPath) {
		st, err := graphimport.Import(ctx, store, graphPath)
		if err != nil {
			fail("import graph: %v", err)
		}
		fmt.Printf("[3/3] imported graph — %d nodes, %d edges into %s\n", st.Nodes, st.Edges, cfg.KnowledgeDBPath)
		return
	}

	// 3b. fallback: text + code error-string ingest
	fmt.Println("[3/3] no knowledge-graph.json — falling back to text/code ingest")
	dom := loadDomain(cfg)
	ex := agents.BuildExtractor(cfg, dom)
	st, err := ingest.Repo(ctx, store, dir, name, dom, ex)
	if err != nil {
		fail("ingest repo: %v", err)
	}
	fmt.Printf("ingested %s — %d docs, %d code files, %d error strings, %d skipped\n",
		name, st.DocFiles, st.CodeFiles, st.ErrorStrings, st.Skipped)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// runChat is a multi-turn REPL: one agent service, reused across turns, so the
// agent-go session accumulates conversation history. Each turn is still a full
// ReAct loop (tools → observe → decide → more tools or answer).
func runChat() {
	cfg := config.Load()
	if cfg.LLMAPIKey == "" {
		fail("set OSS_LLM_API_KEY (frontier model API key)")
	}
	svc, store, err := agents.Build(cfg, loadDomain(cfg))
	if err != nil {
		fail("build agent: %v", err)
	}
	defer svc.Close()
	defer store.Close()

	fmt.Println("oss-agent chat — multi-turn (history kept across turns). Type 'exit' to quit.")
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n› ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		res, err := svc.Chat(context.Background(), line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		ans := "(no answer)"
		if s, ok := res.FinalResult.(string); ok && strings.TrimSpace(s) != "" {
			ans = s
		} else if res.Error != "" {
			ans = "[error] " + res.Error
		} else if res.FinalResult != nil {
			ans = fmt.Sprintf("%v", res.FinalResult)
		}
		fmt.Println("\n" + ans)
	}
}

// runAnalyzeLog is the standalone log-triage feature. It accepts any log file, a
// directory, or an archive (.tar.gz/.tgz/.tar/.zip/.gz), runs a deterministic,
// product-agnostic scan (universal severities + the domain's error_patterns), and
// prints a ranked digest of distinct problems. If an LLM key is configured it then
// asks the agent for a grounded root-cause + safe remediation (knowledge_search +
// the red-line wall), using the distilled findings as evidence.
func runAnalyzeLog(path string) {
	if strings.TrimSpace(path) == "" {
		fail("usage: oss-agent analyze-log <log-file|dir|.tar.gz|.zip>")
	}
	cfg := config.Load()
	dom := loadDomain(cfg)

	root, cleanup, err := loganalyze.Resolve(path)
	if err != nil {
		fail("resolve %s: %v", path, err)
	}
	defer cleanup()

	fmt.Printf("== analyzing %s ==\n", path)
	rep, err := loganalyze.Analyze(root, dom.ErrorPatterns)
	if err != nil {
		fail("analyze: %v", err)
	}
	fmt.Print(rep.Render(15))

	if cfg.LLMAPIKey == "" {
		fmt.Println("\n(set OSS_LLM_API_KEY for an AI root-cause diagnosis grounded in the knowledge base)")
		return
	}
	if len(rep.Groups) == 0 {
		return
	}

	svc, store, err := agents.Build(cfg, dom)
	if err != nil {
		fail("build agent: %v", err)
	}
	defer svc.Close()
	defer store.Close()

	prompt := fmt.Sprintf(
		"You are triaging a log/diagnostic bundle. Below is a de-duplicated digest of "+
			"the problems found (most severe first). Use knowledge_search to ground your "+
			"analysis in the source code and docs. Identify the most likely root cause, "+
			"explain the evidence, and give safe, ordered recovery steps. Never present a "+
			"destructive command as a runnable one-liner without an explicit confirmation gate.\n\n%s",
		rep.Brief(10))

	fmt.Println("\n== AI diagnosis (gpt grounded in code + docs) ==")
	answer, err := svc.Ask(context.Background(), prompt)
	if err != nil {
		fail("diagnose: %v", err)
	}
	fmt.Println(answer)
}

// runImportGraph loads an Understand-Anything knowledge-graph.json into cortexdb
// (node summaries → vectors, nodes/edges → ontology graph).
func runImportGraph(path string) {
	if strings.TrimSpace(path) == "" {
		fail("usage: oss-agent import-graph <knowledge-graph.json>")
	}
	cfg := config.Load()
	if cfg.EmbAPIKey == "" {
		fail("set OSS_EMB_API_KEY (or OSS_LLM_API_KEY) for embeddings")
	}
	store, err := knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
	if err != nil {
		fail("open knowledge: %v", err)
	}
	defer store.Close()
	st, err := graphimport.Import(context.Background(), store, path)
	if err != nil {
		fail("import graph: %v", err)
	}
	fmt.Printf("imported %s — %d nodes, %d edges, %d layers, %d tour-steps into %s\n",
		path, st.Nodes, st.Edges, st.Layers, st.TourSteps, cfg.KnowledgeDBPath)
}

// runSearch queries the knowledge base directly (no LLM) — useful to verify ingest.
func runSearch(query string) {
	if strings.TrimSpace(query) == "" {
		fail("usage: oss-agent search <query>")
	}
	cfg := config.Load()
	store, err := knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
	if err != nil {
		fail("open knowledge: %v", err)
	}
	defer store.Close()
	hits, err := store.SearchSemantic(context.Background(), query, 5)
	if err != nil {
		fail("search: %v", err)
	}
	if len(hits) == 0 {
		fmt.Println("(no hits)")
		return
	}
	for i, h := range hits {
		snippet := strings.Join(strings.Fields(h.Content), " ")
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		fmt.Printf("%d. [%s] (score %.3f)\n   %s\n", i+1, h.DocumentID, h.Score, snippet)
	}
}

// runSearchGraph queries with one-hop graph expansion (the same path knowledge_search
// uses): top hits plus related code reached along calls/contains/… edges.
func runSearchGraph(query string) {
	if strings.TrimSpace(query) == "" {
		fail("usage: oss-agent search-graph <query>")
	}
	cfg := config.Load()
	store, err := knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
	if err != nil {
		fail("open knowledge: %v", err)
	}
	defer store.Close()
	gr, err := store.SearchGraph(context.Background(), query, 5)
	if err != nil {
		fail("search-graph: %v", err)
	}
	fmt.Printf("== hits (%d) ==\n", len(gr.Hits))
	for i, h := range gr.Hits {
		snippet := strings.Join(strings.Fields(h.Content), " ")
		if len(snippet) > 160 {
			snippet = snippet[:160] + "…"
		}
		fmt.Printf("%d. [%s] (%.3f) %s\n", i+1, h.DocumentID, h.Score, snippet)
	}
	fmt.Printf("\n== related via graph (%d) ==\n", len(gr.Neighbors))
	for i, n := range gr.Neighbors {
		sum := strings.Join(strings.Fields(n.Summary), " ")
		if len(sum) > 140 {
			sum = sum[:140] + "…"
		}
		fmt.Printf("%d. (%s ←%s) %s — %s\n", i+1, n.Type, n.Via, n.Name, sum)
	}
}

// runCheck exercises the active domain's red-line wall without any LLM.
func runCheck(cmd string) {
	if strings.TrimSpace(cmd) == "" {
		fail("usage: oss-agent check <command...>")
	}
	dom := loadDomain(config.Load())
	filter, err := safety.NewFromSpecs(dom.RedLines)
	if err != nil {
		fail("compile red-lines: %v", err)
	}
	v := filter.Check(cmd)
	if !v.Blocked {
		fmt.Printf("ALLOWED: %s\n", cmd)
		return
	}
	fmt.Printf("BLOCKED [%s / %s]\n  command: %s\n  reason : %s\n", v.Severity, v.RuleID, v.Command, v.Reason)
	if v.RequiresUnlockKey {
		fmt.Println("  → requires operator unlock key + MFA before execution")
	}
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `oss-agent — AI ops & support agent for storage products
(generic engine; the active domain is a plug-in — LINBIT/DRBD/LINSTOR is the first example)

usage:
  oss-agent ask <question>      ask the agent (calls probes + knowledge search)
  oss-agent diagnose <symptom>  troubleshoot a cluster symptom
  oss-agent chat                multi-turn conversation (history kept; ReAct tools)
  oss-agent analyze-log <path>  triage a log file / dir / .tar.gz / .zip, then AI diagnosis
  oss-agent ingest <dir>        ingest *.md docs from a directory
  oss-agent ingest-repo <url>   one-liner: clone → understand → import (graph)
  oss-agent import-graph <f>    import an Understand-Anything knowledge-graph.json
  oss-agent check <command...>  test a command against the red-line safety wall
  oss-agent version

env:
  OSS_LLM_API_KEY   frontier model API key (required for ask/diagnose)
  OSS_LLM_BASE_URL  default https://api.openai.com/v1
  OSS_LLM_MODEL     default gpt-4o
  OSS_EMB_*         embedder for GraphRAG memory (defaults to LLM creds)
  OSS_DB_PATH       graph-memory db (default ./data/oss-agent.db)
  OSS_UNDERSTAND_CMD  command run in a repo to produce knowledge-graph.json
                      (e.g. claude -p "/understand ." --dangerously-skip-permissions)
`)
}

// runDomain prints the loaded domain config (no key needed) — verifies the toml.
func runDomain() {
	d := loadDomain(config.Load())
	fmt.Printf("domain: %s\n", d.Name)
	fmt.Printf("  entity types : %d\n", len(d.EntityTypes))
	fmt.Printf("  relation types: %d\n", len(d.RelationTypes))
	fmt.Printf("  error patterns: %d\n", len(d.ErrorPatterns))
	fmt.Printf("  probes        : %d\n", len(d.Probes))
	fmt.Printf("  repos         : %d\n", len(d.Repos))
}

// loadDomain loads the active product config (domain.toml). This is the only
// place a "product" enters the platform — there is nothing product-specific in
// the engine itself.
func loadDomain(cfg config.Config) *domain.Domain {
	d, err := domain.Load(cfg.DomainFile)
	if err != nil {
		fail("%v\n  set OSS_DOMAIN_FILE to your product's domain.toml (see examples/linbit/domain.toml)", err)
	}
	return d
}

func fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
