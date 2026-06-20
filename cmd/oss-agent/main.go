// Command oss-agent is the CLI entry point for the oss-agent ops & support agent.
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
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	agentpkg "github.com/liliang-cn/agent-go/v2/pkg/agent"

	"github.com/liliang-cn/oss-agent/web"

	"github.com/liliang-cn/oss-agent/internal/agents"
	"github.com/liliang-cn/oss-agent/internal/config"
	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/graphimport"
	"github.com/liliang-cn/oss-agent/internal/httpapi"
	"github.com/liliang-cn/oss-agent/internal/ingest"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/loganalyze"
	"github.com/liliang-cn/oss-agent/internal/objectmodel"
	"github.com/liliang-cn/oss-agent/internal/safety"
	"github.com/liliang-cn/oss-agent/internal/salvage"
	"github.com/liliang-cn/oss-agent/internal/scaffold"
	"github.com/liliang-cn/oss-agent/internal/schemaimport"
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
	case "refresh":
		runRefresh(strings.Join(os.Args[2:], " "))
	case "import-graph":
		runImportGraph(strings.Join(os.Args[2:], " "))
	case "import-schema":
		runImportSchema(strings.Join(os.Args[2:], " "))
	case "import-model":
		runImportModel(strings.Join(os.Args[2:], " "))
	case "salvage":
		runSalvage(strings.Join(os.Args[2:], " "))
	case "init":
		runInit(strings.Join(os.Args[2:], " "))
	case "search":
		runSearch(strings.Join(os.Args[2:], " "))
	case "search-graph":
		runSearchGraph(strings.Join(os.Args[2:], " "))
	case "chat":
		runChat()
	case "serve":
		runServe(false)
	case "ui":
		runServe(true)
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

	// 3a'. salvage: understand stalled before writing knowledge-graph.json, but
	// left intermediate batches — merge them into an equivalent graph and import.
	if salvage.Available(dir) {
		fmt.Println("[3/3] no knowledge-graph.json — salvaging from intermediate batches")
		merged, ss, e := salvage.Salvage(dir)
		if e == nil {
			fmt.Printf("  salvaged %d nodes, %d edges, %d layers from %s\n", ss.Nodes, ss.Edges, ss.Layers, strings.Join(ss.Sources, ", "))
			st, e2 := graphimport.Import(ctx, store, merged)
			if e2 != nil {
				fail("import salvaged graph: %v", e2)
			}
			fmt.Printf("  imported salvaged graph — %d nodes, %d edges into %s\n", st.Nodes, st.Edges, cfg.KnowledgeDBPath)
			return
		}
		fmt.Fprintf(os.Stderr, "  salvage failed: %v — falling back to text ingest\n", e)
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

// runRefresh cleanly updates a single source in place: it purges that source's
// existing chunks (and code graph nodes/edges) — catching deletions, not just
// changes — then re-imports it. Mirrors ingest-repo's argument (git url or local
// dir). Only the named source is touched; the other sources are untouched.
func runRefresh(arg string) {
	if strings.TrimSpace(arg) == "" {
		fail("usage: oss-agent refresh <git-url|local-dir>")
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

	// resolve dir + name (clone if a URL), same as ingest-repo
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
		}
	}

	graphPath := filepath.Join(dir, ".understand-anything", "knowledge-graph.json")
	if fileExists(graphPath) {
		// code source: purge by exact document_id, then re-import the graph
		docID, e := graphimport.DocIDFor(graphPath)
		if e != nil {
			fail("resolve doc id: %v", e)
		}
		embN, nodeN, e := store.PurgeSource(ctx, docID, false)
		if e != nil {
			fail("purge %s: %v", docID, e)
		}
		fmt.Printf("[purge] %s — removed %d chunks, %d graph nodes\n", docID, embN, nodeN)
		st, e := graphimport.Import(ctx, store, graphPath)
		if e != nil {
			fail("import graph: %v", e)
		}
		fmt.Printf("[reimport] %d nodes, %d edges, %d layers\n", st.Nodes, st.Edges, st.Layers)
		return
	}

	// text source: purge by document_id prefix "<name>/", then re-ingest
	embN, _, e := store.PurgeSource(ctx, name+"/", true)
	if e != nil {
		fail("purge %s: %v", name, e)
	}
	fmt.Printf("[purge] %s/* — removed %d chunks\n", name, embN)
	dom := loadDomain(cfg)
	ex := agents.BuildExtractor(cfg, dom)
	st, e := ingest.Repo(ctx, store, dir, name, dom, ex)
	if e != nil {
		fail("ingest repo: %v", e)
	}
	fmt.Printf("[reingest] %s — %d docs, %d code files, %d error strings\n", name, st.DocFiles, st.CodeFiles, st.ErrorStrings)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// runServe starts the HTTP API + embedded web UI. With an LLM key it serves the
// full agent (/ask, /chat, /analyze-log?diagnose=true); without one it serves the
// search-only endpoints. Address from OSS_HTTP_ADDR (default :7634). When openUI
// is true (the `ui` command) it also opens the browser.
func runServe(openUI bool) {
	cfg := config.Load()
	dom := loadDomain(cfg)

	var svc *agentpkg.Service
	var store *knowledge.Store
	var err error
	if cfg.LLMAPIKey != "" {
		svc, store, err = agents.Build(cfg, dom)
		if err != nil {
			fail("build agent: %v", err)
		}
		defer svc.Close()
	} else {
		fmt.Fprintln(os.Stderr, "warning: OSS_LLM_API_KEY not set — serving search-only (no /ask, /diagnose)")
		store, err = knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
		if err != nil {
			fail("open knowledge: %v", err)
		}
	}
	defer store.Close()

	// Embedded web UI (web/dist) — nil if the build is absent.
	var static fs.FS
	if sub, e := fs.Sub(web.Dist, "dist"); e == nil {
		if _, e2 := fs.Stat(sub, "index.html"); e2 == nil {
			static = sub
		}
	}

	addr := env("OSS_HTTP_ADDR", ":7634")
	convMem := svc != nil && env("OSS_CONV_MEMORY", "on") != "off"
	rlPerMin := envInt("OSS_RATE_LIMIT_PER_MIN", 30) // per-IP LLM-endpoint cap; 0 = unlimited
	srv := httpapi.New(svc, store, dom, convMem, static, rlPerMin)
	fmt.Printf("oss-agent serving %s on %s  (llm=%v, probes=%d, conv_memory=%v, ui=%v)\n", dom.Name, addr, svc != nil, len(dom.Probes), convMem, static != nil)
	fmt.Println("API under /api/* ; web UI at / (if built)")

	if openUI && static != nil {
		go openBrowser("http://localhost" + addr)
	}
	if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
		fail("http server: %v", err)
	}
}

// openBrowser best-effort opens the default browser at url (macOS/Linux/Windows).
func openBrowser(url string) {
	time.Sleep(400 * time.Millisecond)
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}

// env mirrors config's helper for the serve address.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads an integer env var with a default.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
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

// runImportSchema extracts an object model from a SQL schema (DDL) into the graph.
func runImportSchema(path string) {
	if strings.TrimSpace(path) == "" {
		fail("usage: oss-agent import-schema <schema.sql>")
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
	st, err := schemaimport.Import(context.Background(), store, path)
	if err != nil {
		fail("import schema: %v", err)
	}
	fmt.Printf("imported %s — %d tables, %d foreign keys into %s\n", path, st.Tables, st.FKs, cfg.KnowledgeDBPath)
}

// runImportModel extracts an object model from any structured source-of-truth
// (.proto / OpenAPI .yaml|.json / C-struct .h|.c / .sql) into the graph.
func runImportModel(path string) {
	if strings.TrimSpace(path) == "" {
		fail("usage: oss-agent import-model <file.proto|openapi.yaml|*.h|schema.sql>")
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
	st, err := objectmodel.Load(context.Background(), store, path)
	if err != nil {
		fail("import model: %v", err)
	}
	fmt.Printf("imported %s [%s] — %d objects, %d references into %s\n", path, st.Format, st.Objects, st.Refs, cfg.KnowledgeDBPath)
}

// runSalvage rebuilds a knowledge-graph.json from a stalled Understand-Anything
// run's intermediate batches (no re-run needed), then imports it.
func runSalvage(arg string) {
	if strings.TrimSpace(arg) == "" {
		fail("usage: oss-agent salvage <repo-dir>   (a repo whose .understand-anything/intermediate exists)")
	}
	cfg := config.Load()
	if cfg.EmbAPIKey == "" {
		fail("set OSS_EMB_API_KEY (or OSS_LLM_API_KEY) for embeddings")
	}
	merged, ss, err := salvage.Salvage(arg)
	if err != nil {
		fail("salvage: %v", err)
	}
	fmt.Printf("salvaged %d nodes, %d edges, %d layers from %s → %s\n", ss.Nodes, ss.Edges, ss.Layers, strings.Join(ss.Sources, ", "), merged)
	store, err := knowledge.Open(cfg.KnowledgeDBPath, cfg.EmbBaseURL, cfg.EmbAPIKey, cfg.EmbModel, cfg.EmbDim)
	if err != nil {
		fail("open knowledge: %v", err)
	}
	defer store.Close()
	st, err := graphimport.Import(context.Background(), store, merged)
	if err != nil {
		fail("import salvaged graph: %v", err)
	}
	fmt.Printf("imported salvaged graph — %d nodes, %d edges into %s\n", st.Nodes, st.Edges, cfg.KnowledgeDBPath)
}

// runInit drafts a domain.toml for a repo via the LLM and writes it to
// domain.generated.toml. The operator must review it (esp. red_lines) before use.
func runInit(arg string) {
	if strings.TrimSpace(arg) == "" {
		fail("usage: oss-agent init <git-url|local-dir>   (drafts domain.generated.toml via the LLM)")
	}
	cfg := config.Load()
	if cfg.LLMAPIKey == "" {
		fail("set OSS_LLM_API_KEY — init drafts the config with the LLM")
	}
	ctx := context.Background()

	dir, name := arg, filepath.Base(strings.TrimRight(arg, "/"))
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") || strings.HasSuffix(arg, ".git") {
		name = strings.TrimSuffix(filepath.Base(arg), ".git")
		dir = filepath.Join("repos", name)
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			fmt.Printf("cloning %s → %s (shallow)\n", arg, dir)
			cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", arg, dir)
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if e := cmd.Run(); e != nil {
				fail("git clone: %v", e)
			}
		}
	}

	llm, err := agents.LLM(cfg)
	if err != nil {
		fail("init llm: %v", err)
	}
	fmt.Printf("drafting domain config from %s (model=%s)…\n", dir, cfg.LLMModel)
	toml, err := scaffold.Generate(ctx, llm, dir, name)
	if err != nil {
		fail("scaffold: %v", err)
	}
	out := "domain.generated.toml"
	if err := os.WriteFile(out, []byte(toml), 0o644); err != nil {
		fail("write %s: %v", out, err)
	}
	fmt.Printf("\nwrote %s\n", out)
	fmt.Println("→ REVIEW it (especially [[red_lines]] and [[probes]]), then:")
	fmt.Printf("   cp %s domain.toml && oss-agent domain    # verify it loads\n", out)
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
(generic engine; the active domain is a plug-in supplied entirely by a domain.toml)

usage:
  oss-agent init <url|dir>      draft a domain.toml for a repo via the LLM (review before use)
  oss-agent ask <question>      ask the agent (calls probes + knowledge search)
  oss-agent diagnose <symptom>  troubleshoot a cluster symptom
  oss-agent chat                multi-turn conversation (history kept; ReAct tools)
  oss-agent serve               start the HTTP API (OSS_HTTP_ADDR, default :7634)
  oss-agent analyze-log <path>  triage a log file / dir / .tar.gz / .zip, then AI diagnosis
  oss-agent ingest <dir>        ingest *.md docs from a directory
  oss-agent ingest-repo <url>   one-liner: clone → understand → import (auto-salvages a stall)
  oss-agent refresh <url|dir>   purge one source (catches deletions) and re-import it
  oss-agent import-graph <f>    import an Understand-Anything knowledge-graph.json
  oss-agent import-schema <f>   import a SQL schema (CREATE TABLE → entity, FK → relation)
  oss-agent import-model <f>    import an object model: .proto / OpenAPI .yaml|.json / .h struct / .sql
  oss-agent salvage <repo-dir>  rebuild a graph from a stalled understand run's intermediate batches
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
		fail("%v\n  set OSS_DOMAIN_FILE to your product's domain.toml (see examples/example/domain.toml)", err)
	}
	return d
}

func fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
