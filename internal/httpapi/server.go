// Package httpapi exposes the agent over HTTP: a thin JSON layer over the same
// agent.Service + knowledge.Store the CLI uses. It is product-agnostic — the
// active domain is whatever was loaded into the service.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/liliang-cn/agent-go/v2/pkg/agent"

	"github.com/liliang-cn/oss-agent/internal/cite"
	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/loganalyze"
)

// Server holds the shared agent + knowledge store behind the HTTP handlers.
// svc may be nil (no LLM key): the LLM-backed endpoints then return 503 while the
// search endpoints keep working.
type Server struct {
	svc        *agent.Service
	store      *knowledge.Store
	dom        *domain.Domain
	convMemory bool       // cross-session conversation memory on /chat
	static     fs.FS      // embedded web UI (may be nil)
	limiter    *ipLimiter // per-IP rate limit on LLM endpoints
	mu         sync.Mutex // serializes LLM agent runs (one ReAct loop at a time)
}

// New builds a Server. Pass svc=nil for a search-only (no-LLM) deployment.
// convMemory enables cross-session semantic recall of past chat turns on /chat.
// static, when non-nil, is the built web UI served at / (API lives under /api).
// rlPerMin caps LLM-endpoint requests per client IP per minute (0 = unlimited).
func New(svc *agent.Service, store *knowledge.Store, dom *domain.Domain, convMemory bool, static fs.FS, rlPerMin int) *Server {
	return &Server{svc: svc, store: store, dom: dom, convMemory: convMemory, static: static, limiter: newIPLimiter(rlPerMin)}
}

// ── per-IP rate limiter (sliding 60s window) ──

type ipLimiter struct {
	mu     sync.Mutex
	perMin int
	hits   map[string][]int64
}

func newIPLimiter(perMin int) *ipLimiter {
	return &ipLimiter{perMin: perMin, hits: make(map[string][]int64)}
}

func (l *ipLimiter) allow(ip string) bool {
	if l.perMin <= 0 {
		return true
	}
	now := time.Now().Unix()
	cutoff := now - 60
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.perMin {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	return true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// limited wraps an LLM-endpoint handler with the per-IP rate limit.
func (s *Server) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(clientIP(r)) {
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded — slow down")
			return
		}
		next(w, r)
	}
}

// Routes mounts the JSON API under /api and the embedded web UI at /.
func (s *Server) Routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("/healthz", s.handleHealth)
	api.HandleFunc("/ask", s.limited(s.handleAsk))
	api.HandleFunc("/ask/stream", s.limited(s.handleAskStream))   // SSE token stream
	api.HandleFunc("/diagnose", s.limited(s.handleAsk))           // same loop, troubleshooting framing
	api.HandleFunc("/chat", s.limited(s.handleChat))              // multi-turn (session history kept)
	api.HandleFunc("/chat/stream", s.limited(s.handleChatStream)) // multi-turn, raw text stream (Vercel AI SDK)
	api.HandleFunc("/search", s.handleSearch)
	api.HandleFunc("/search-graph", s.handleSearchGraph)
	api.HandleFunc("/graph/all", s.handleGraphAll)       // entire graph
	api.HandleFunc("/graph", s.handleGraph)              // subgraph seeded by ?q=
	api.HandleFunc("/graph/expand", s.handleGraphExpand) // one-hop expand ?id=
	api.HandleFunc("/analyze-log", s.limited(s.handleAnalyzeLog))

	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", api))
	if s.static != nil {
		mux.Handle("/", spaHandler(s.static))
	}
	return logRequests(mux)
}

// spaHandler serves static files from fsys, falling back to index.html so the
// single-page app loads at any path.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(fsys, p); err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			http.ServeFileFS(w, r2, fsys, "index.html")
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"domain": s.dom.Name,
		"title":  s.dom.Title,
		"probes": len(s.dom.Probes),
		"llm":    s.svc != nil,
	})
}

type askRequest struct {
	Question string `json:"question"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if s.svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "no LLM configured: set OSS_LLM_API_KEY")
		return
	}
	var req askRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeErr(w, http.StatusBadRequest, "question is required")
		return
	}
	s.mu.Lock()
	answer, err := s.svc.Ask(r.Context(), req.Question)
	s.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "ask failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"answer": answer})
}

type chatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

// handleChat is multi-turn: each call runs under a session_id whose conversation
// history agent-go loads and persists (in its own session store at OSS_DB_PATH),
// so follow-up turns remember earlier ones. A new session_id is minted when the
// client omits it and returned for reuse.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if s.svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "no LLM configured: set OSS_LLM_API_KEY")
		return
	}
	var req chatRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeErr(w, http.StatusBadRequest, "message is required")
		return
	}
	sid := strings.TrimSpace(req.SessionID)
	if sid == "" {
		sid = newSessionID()
	}

	// Cross-session recall: pull semantically relevant turns from any past
	// conversation and prepend them as optional context (best-effort).
	message := req.Message
	recalled := 0
	if s.convMemory {
		if turns, rerr := s.store.RecallTurns(r.Context(), req.Message, 4); rerr == nil && len(turns) > 0 {
			var b strings.Builder
			b.WriteString("Context recalled from earlier conversations (use if relevant, otherwise ignore):\n")
			for _, t := range turns {
				b.WriteString(fmt.Sprintf("- [%s] %s\n", t.Role, truncate(t.Content, 240)))
			}
			b.WriteString("\nCurrent message:\n")
			b.WriteString(req.Message)
			message = b.String()
			recalled = len(turns)
		}
	}

	s.mu.Lock()
	res, err := s.svc.Run(r.Context(), message, agent.WithSessionID(sid))
	s.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "chat failed: "+err.Error())
		return
	}
	answer := res.Text()

	// Persist this turn into the shared conversation memory (best-effort).
	if s.convMemory {
		_ = s.store.SaveTurn(r.Context(), sid, "user", req.Message)
		_ = s.store.SaveTurn(r.Context(), sid, "assistant", answer)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": sid,
		"answer":     answer,
		"recalled":   recalled,
	})
}

// truncate shortens s to at most n runes for compact recall context.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// augmentWithRecall prepends cross-session recalled context to a message (when
// enabled) and returns the augmented message plus the recall count.
func (s *Server) augmentWithRecall(ctx context.Context, msg string) (string, int) {
	if !s.convMemory {
		return msg, 0
	}
	turns, err := s.store.RecallTurns(ctx, msg, 4)
	if err != nil || len(turns) == 0 {
		return msg, 0
	}
	var b strings.Builder
	b.WriteString("Context recalled from earlier conversations (use if relevant, otherwise ignore):\n")
	for _, t := range turns {
		b.WriteString(fmt.Sprintf("- [%s] %s\n", t.Role, truncate(t.Content, 240)))
	}
	b.WriteString("\nCurrent message:\n")
	b.WriteString(msg)
	return b.String(), len(turns)
}

// aiMessage matches the Vercel AI SDK useChat POST body ({messages:[{role,content}]}).
type aiChatBody struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
}

// handleChatStream is /chat for the Vercel AI SDK: it accepts the SDK's
// {messages,...} body (or {message}), runs the agent under session_id, and writes
// the answer as a raw text stream (AI SDK streamProtocol:'text'). Cross-session
// recall + persistence apply. session_id echoes back via the X-Session-Id header.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if s.svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "no LLM configured: set OSS_LLM_API_KEY")
		return
	}
	var body aiChatBody
	if !readJSON(w, r, &body) {
		return
	}
	userMsg := strings.TrimSpace(body.Message)
	if userMsg == "" {
		for i := len(body.Messages) - 1; i >= 0; i-- {
			if body.Messages[i].Role == "user" {
				userMsg = strings.TrimSpace(body.Messages[i].Content)
				break
			}
		}
	}
	if userMsg == "" {
		writeErr(w, http.StatusBadRequest, "no user message")
		return
	}
	sid := strings.TrimSpace(body.SessionID)
	if sid == "" {
		sid = newSessionID()
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	message, recalled := s.augmentWithRecall(r.Context(), userMsg)
	// NDJSON event stream: one JSON object per line. Frame types:
	//   {"t":"tool","name":..,"args":{..}}   agent invoked a tool
	//   {"t":"tool_result","name":..}         that tool returned
	//   {"t":"text","d":".."}                 answer delta
	//   {"t":"error","d":".."} / {"t":"done"}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Session-Id", sid)
	w.Header().Set("X-Recalled", fmt.Sprintf("%d", recalled))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w) // Encode writes a trailing newline → NDJSON
	frame := func(v any) { _ = enc.Encode(v); flusher.Flush() }

	// Real streaming via RunStream: forward tool calls/results live, then token
	// deltas when the model speaks the answer; when it returns the answer via
	// task_complete (no token events), the final EventTypeComplete is flushed in
	// chunks so the client still sees a progressive reveal.
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.svc.RunStream(r.Context(), message)
	if err != nil {
		frame(map[string]any{"t": "error", "d": err.Error()})
		frame(map[string]any{"t": "done"})
		return
	}
	var streamed bool
	var full, final string
	var sources []string
	seenSrc := map[string]bool{}
	for ev := range events {
		switch ev.Type {
		case agent.EventTypePartial:
			if ev.Content != "" {
				streamed = true
				full += ev.Content
				frame(map[string]any{"t": "text", "d": ev.Content})
			}
		case agent.EventTypeToolCall:
			// task_complete is the internal answer-delivery sentinel (its args carry
			// the whole answer); don't surface it as a tool.
			if ev.ToolName == "task_complete" {
				continue
			}
			frame(map[string]any{"t": "tool", "name": ev.ToolName, "args": ev.ToolArgs})
		case agent.EventTypeToolResult:
			if ev.ToolName == "task_complete" {
				continue
			}
			if ev.ToolName == "knowledge_search" {
				cite.CollectSources(ev.ToolResult, seenSrc, &sources)
			}
			frame(map[string]any{"t": "tool_result", "name": ev.ToolName})
		case agent.EventTypeComplete:
			final = ev.Content
		case agent.EventTypeError:
			// Swallow internal, non-fatal infra hiccups (e.g. history compaction)
			// so they don't pollute the answer.
			if strings.Contains(ev.Content, "compaction") {
				continue
			}
			frame(map[string]any{"t": "error", "d": ev.Content})
		}
	}
	// The authoritative answer is the EventTypeComplete payload. When the model
	// streamed only a preamble ("Let me check…") as partials and then delivered the
	// real answer via task_complete, `final` holds it and the streamed text doesn't
	// contain it — clear the preamble and emit the real answer. When the answer was
	// itself streamed, `full` already contains `final`, so we leave it alone.
	if final != "" && !strings.Contains(full, strings.TrimSpace(final)) {
		if streamed {
			frame(map[string]any{"t": "reset"}) // drop the preamble shown so far
		}
		full = final
		for _, ch := range chunkText(final, 18) {
			frame(map[string]any{"t": "text", "d": ch})
		}
	}
	// Deterministic citation footer: the model is unreliable at inline citing, so
	// always list the sources knowledge_search actually retrieved this turn (unless
	// the model already produced a Sources section).
	if foot := cite.Footer(full, sources); foot != "" {
		for _, ch := range chunkText(foot, 24) {
			frame(map[string]any{"t": "text", "d": ch})
		}
		full += foot
	}
	frame(map[string]any{"t": "done"})
	if s.convMemory && full != "" {
		_ = s.store.SaveTurn(context.Background(), sid, "user", userMsg)
		_ = s.store.SaveTurn(context.Background(), sid, "assistant", full)
	}
}

// chunkText splits s into ~n-rune pieces (rune boundaries) for incremental flush.
func chunkText(s string, n int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += n {
		end := i + n
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

// handleAskStream streams the answer token-by-token over Server-Sent Events.
// Each token is one `data:` event; a final `event: done` closes the stream.
// Accepts the question via POST body {"question"} or ?q= for easy EventSource use.
func (s *Server) handleAskStream(w http.ResponseWriter, r *http.Request) {
	if s.svc == nil {
		writeErr(w, http.StatusServiceUnavailable, "no LLM configured: set OSS_LLM_API_KEY")
		return
	}
	question := r.URL.Query().Get("q")
	if question == "" && r.Method == http.MethodPost {
		var req askRequest
		if readJSON(w, r, &req) {
			question = req.Question
		} else {
			return
		}
	}
	if strings.TrimSpace(question) == "" {
		writeErr(w, http.StatusBadRequest, "question is required (POST {\"question\"} or ?q=)")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sendData := func(text string) {
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		fmt.Fprint(w, "\n")
		flusher.Flush()
	}

	// One ReAct loop at a time; the stream holds the lock for its duration.
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.svc.RunStream(r.Context(), question)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	// Forward token deltas as they arrive. If the provider doesn't stream partials,
	// fall back to the final completion content so the answer is never lost.
	var streamed bool
	var final string
	for ev := range events {
		switch ev.Type {
		case agent.EventTypePartial:
			if ev.Content != "" {
				streamed = true
				sendData(ev.Content)
			}
		case agent.EventTypeComplete:
			final = ev.Content
		case agent.EventTypeError:
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", ev.Content)
			flusher.Flush()
		}
	}
	if !streamed && final != "" {
		sendData(final)
	}
	fmt.Fprint(w, "event: done\ndata: \n\n")
	flusher.Flush()
}

type searchRequest struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req searchRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	if req.TopK <= 0 {
		req.TopK = 6
	}
	hits, err := s.store.SearchSemantic(r.Context(), req.Query, req.TopK)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "search failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"hits": hits})
}

func (s *Server) handleSearchGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req searchRequest
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	if req.TopK <= 0 {
		req.TopK = 6
	}
	gr, err := s.store.SearchGraph(r.Context(), req.Query, req.TopK)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "search-graph failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"hits":              gr.Hits,
		"related_via_graph": gr.Neighbors,
	})
}

func (s *Server) handleGraphAll(w http.ResponseWriter, r *http.Request) {
	gv, err := s.store.AllGraph(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "graph failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gv)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, "q is required")
		return
	}
	gv, err := s.store.QueryGraph(r.Context(), q, 6)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "graph failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gv)
}

func (s *Server) handleGraphExpand(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	gv, err := s.store.ExpandNode(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "expand failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gv)
}

// handleAnalyzeLog triages a log. It accepts either a multipart upload (field
// "log" — a file/dir-archive .tar.gz/.zip/.gz or a plain log) or a JSON body
// {"path": "<server-side path>"}. With ?diagnose=true and an LLM configured, it
// also returns a grounded AI root-cause.
func (s *Server) handleAnalyzeLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	path, cleanup, err := s.resolveLogInput(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer cleanup()

	root, rcleanup, err := loganalyze.Resolve(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "resolve: "+err.Error())
		return
	}
	defer rcleanup()

	rep, err := loganalyze.Analyze(root, s.dom.ErrorPatterns)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "analyze: "+err.Error())
		return
	}
	resp := map[string]interface{}{
		"files_scanned": rep.FilesScanned,
		"files_total":   rep.FilesTotal,
		"findings":      rep.Findings,
		"groups":        rep.Groups,
	}

	if r.URL.Query().Get("diagnose") == "true" && s.svc != nil && len(rep.Groups) > 0 {
		prompt := fmt.Sprintf(
			"You are triaging a log/diagnostic bundle. Below is a de-duplicated digest of "+
				"the problems found (most severe first). Use knowledge_search to ground your "+
				"analysis in the source code and docs. Identify the most likely root cause, "+
				"explain the evidence, and give safe, ordered recovery steps.\n\n%s",
			rep.Brief(10))
		s.mu.Lock()
		diag, derr := s.svc.Ask(r.Context(), prompt)
		s.mu.Unlock()
		if derr != nil {
			resp["diagnosis_error"] = derr.Error()
		} else {
			resp["diagnosis"] = diag
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveLogInput returns a filesystem path to analyze plus a cleanup func. It
// handles a multipart "log" upload (written to a temp file keeping its extension
// so archive detection works) or a JSON {"path"} body.
func (s *Server) resolveLogInput(r *http.Request) (string, func(), error) {
	noop := func() {}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(256 << 20); err != nil {
			return "", noop, fmt.Errorf("parse upload: %w", err)
		}
		file, hdr, err := r.FormFile("log")
		if err != nil {
			return "", noop, fmt.Errorf("missing 'log' file field: %w", err)
		}
		defer file.Close()
		tmp, err := os.CreateTemp("", "oss-upload-*"+ext(hdr.Filename))
		if err != nil {
			return "", noop, err
		}
		if _, err := io.Copy(tmp, file); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", noop, err
		}
		tmp.Close()
		return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
	}

	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return "", noop, fmt.Errorf("send a multipart 'log' file or JSON {\"path\":...}: %w", err)
	}
	if strings.TrimSpace(body.Path) == "" {
		return "", noop, fmt.Errorf("path is required (or upload a 'log' file)")
	}
	return body.Path, noop, nil
}

// ext returns the archive-relevant extension of a filename (handles .tar.gz).
func ext(name string) string {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".tar.gz") {
		return ".tar.gz"
	}
	return filepath.Ext(name)
}

// ── helpers ──

func readJSON(w http.ResponseWriter, r *http.Request, dst interface{}) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]interface{}{"ok": false, "error": msg})
}

// newSessionID mints a random conversation id when the client doesn't supply one.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "sess"
	}
	return "sess-" + hex.EncodeToString(b[:])
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(os.Stderr, "[http] %s %s\n", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
