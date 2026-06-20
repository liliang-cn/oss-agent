// Package httpapi exposes the agent over HTTP: a thin JSON layer over the same
// agent.Service + knowledge.Store the CLI uses. It is product-agnostic — the
// active domain is whatever was loaded into the service.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/liliang-cn/agent-go/v2/pkg/agent"

	"github.com/liliang-cn/oss-agent/internal/domain"
	"github.com/liliang-cn/oss-agent/internal/knowledge"
	"github.com/liliang-cn/oss-agent/internal/loganalyze"
)

// Server holds the shared agent + knowledge store behind the HTTP handlers.
// svc may be nil (no LLM key): the LLM-backed endpoints then return 503 while the
// search endpoints keep working.
type Server struct {
	svc   *agent.Service
	store *knowledge.Store
	dom   *domain.Domain
	mu    sync.Mutex // serializes LLM agent runs (one ReAct loop at a time)
}

// New builds a Server. Pass svc=nil for a search-only (no-LLM) deployment.
func New(svc *agent.Service, store *knowledge.Store, dom *domain.Domain) *Server {
	return &Server{svc: svc, store: store, dom: dom}
}

// Routes returns the HTTP handler with all endpoints mounted.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/ask", s.handleAsk)
	mux.HandleFunc("/ask/stream", s.handleAskStream) // SSE token stream
	mux.HandleFunc("/diagnose", s.handleAsk)         // same loop, troubleshooting framing
	mux.HandleFunc("/chat", s.handleChat)            // multi-turn (session history kept)
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/search-graph", s.handleSearchGraph)
	mux.HandleFunc("/analyze-log", s.handleAnalyzeLog)
	return logRequests(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"domain":  s.dom.Name,
		"probes":  len(s.dom.Probes),
		"llm":     s.svc != nil,
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
	s.mu.Lock()
	res, err := s.svc.Run(r.Context(), req.Message, agent.WithSessionID(sid))
	s.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "chat failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": sid,
		"answer":     res.Text(),
	})
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
