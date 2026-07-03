// This file mounts an external MCP server's tools into the ops agent's ReAct
// loop. Only read-only tools are exposed when a spec is marked ReadOnly, so a
// consumer can safely wire a cluster-control MCP server (e.g. sds-mcp) and let
// the agent observe state without ever mutating it. Actual mutations are surfaced
// as structured, operator-approved suggestions (see registerSuggestAction).
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/liliang-cn/agent-go/v2/pkg/agent"
	"github.com/liliang-cn/agent-go/v2/pkg/mcp"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPSpec describes one external MCP server to mount. It mirrors the public
// ossagent.MCPServerSpec so internal packages never import the root facade.
type MCPSpec struct {
	Name              string
	Transport         string // "stdio" | "http" | "sse"
	Command           string
	Args              []string
	URL               string
	Headers           map[string]string
	ReadOnly          bool
	ReadOnlyToolAllow []string
}

// MCPMountStatus reports the outcome of mounting one MCP server.
type MCPMountStatus struct {
	Name      string // logical server name
	Connected bool   // true if Connect succeeded
	Tools     int    // number of tools actually registered into the agent
	Skipped   int    // tools filtered out by the read-only gate
	Err       string // connect/list error, empty on success
}

// mcpConnectTimeout bounds each server's initialize handshake so a dead server
// never blocks agent construction.
const mcpConnectTimeout = 30 * time.Second

// MountMCP connects each spec's MCP server and registers its (read-only) tools
// into svc. It never returns an error: a server that fails to connect is recorded
// in the returned status slice and skipped, so the agent degrades to knowledge-only.
// Callers must Close every returned client.
func MountMCP(ctx context.Context, svc *agent.Service, specs []MCPSpec) ([]*mcp.Client, []MCPMountStatus) {
	var clients []*mcp.Client
	var statuses []MCPMountStatus

	for _, spec := range specs {
		st := MCPMountStatus{Name: spec.Name}

		client, err := mcp.NewClient(serverConfig(spec), nil)
		if err != nil {
			st.Err = fmt.Sprintf("new client: %v", err)
			statuses = append(statuses, st)
			continue
		}

		cctx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
		err = client.Connect(cctx)
		cancel()
		if err != nil {
			st.Err = fmt.Sprintf("connect: %v", err)
			_ = client.Close()
			statuses = append(statuses, st)
			continue
		}
		st.Connected = true
		clients = append(clients, client)

		for name, tool := range client.GetTools() {
			if spec.ReadOnly && !readOnlyAdmit(name, spec.ReadOnlyToolAllow) {
				st.Skipped++
				continue
			}
			registerMCPTool(svc, client, spec.Name, name, tool, spec.ReadOnly)
			st.Tools++
		}
		statuses = append(statuses, st)
	}

	return clients, statuses
}

// serverConfig translates a spec into an agent-go mcp.ServerConfig.
func serverConfig(spec MCPSpec) *mcp.ServerConfig {
	cfg := &mcp.ServerConfig{
		Name:           spec.Name,
		URL:            spec.URL,
		Headers:        spec.Headers,
		DefaultTimeout: mcpConnectTimeout,
	}
	switch strings.ToLower(strings.TrimSpace(spec.Transport)) {
	case "http":
		cfg.Type = mcp.ServerTypeHTTP
	case "sse":
		cfg.Type = mcp.ServerTypeSSE
	default: // "stdio" or empty
		cfg.Type = mcp.ServerTypeStdio
		if spec.Command != "" {
			cfg.Command = []string{spec.Command}
		}
		cfg.Args = spec.Args
	}
	return cfg
}

// registerMCPTool exposes one MCP tool as an agent tool. The handler forwards the
// call to the MCP server via CallTool; results and errors are returned as plain
// JSON-safe maps so the existing Stream event path surfaces them unchanged.
func registerMCPTool(svc *agent.Service, client *mcp.Client, server, name string, tool *sdkmcp.Tool, readOnly bool) {
	desc := tool.Description
	if desc == "" {
		desc = fmt.Sprintf("Tool %q exposed by MCP server %q.", name, server)
	}
	call := name // capture per-iteration
	c := client
	handler := func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		res, err := c.CallTool(ctx, call, args)
		if err != nil {
			return map[string]interface{}{"ok": false, "error": err.Error()}, nil
		}
		if res != nil && !res.Success {
			return map[string]interface{}{"ok": false, "error": res.Error}, nil
		}
		var data interface{}
		if res != nil {
			data = res.Data
		}
		return map[string]interface{}{"ok": true, "data": data}, nil
	}
	// Flag read-only tools explicitly. agent-go's name heuristic doesn't treat
	// e.g. "*_status" as read-only, so without this a duplicate status poll would
	// be re-executed instead of collapsed. A ReadOnly tool is also concurrency-safe.
	if readOnly {
		svc.AddToolWithMetadata(name, desc, toParams(tool.InputSchema), handler,
			agent.ToolMetadata{ReadOnly: true, ConcurrencySafe: true})
		return
	}
	svc.AddTool(name, desc, toParams(tool.InputSchema), handler)
}

// readOnlyAdmit decides whether a tool may be mounted under ReadOnly.
//
//   - If an explicit allowlist is provided, ONLY names in it are admitted.
//   - Otherwise a name-heuristic ADMITS read-only-looking tools and REJECTS
//     anything mutating; a name matching neither is rejected (fail closed).
//
// Rejection wins over admission so a name carrying both (e.g. "list_and_delete")
// is refused.
func readOnlyAdmit(name string, allow []string) bool {
	if len(allow) > 0 {
		for _, a := range allow {
			if strings.EqualFold(strings.TrimSpace(a), name) {
				return true
			}
		}
		return false
	}
	n := strings.ToLower(name)
	for _, t := range readOnlyRejectTokens {
		if strings.Contains(n, t) {
			return false
		}
	}
	for _, t := range readOnlyAdmitTokens {
		if strings.Contains(n, t) {
			return true
		}
	}
	return false
}

// readOnlyAdmitTokens name a tool as observational.
var readOnlyAdmitTokens = []string{
	"list", "get", "status", "health", "describe", "show",
}

// readOnlyRejectTokens name a tool as mutating; presence forces rejection.
var readOnlyRejectTokens = []string{
	"create", "delete", "remove", "set", "evict", "promote", "demote",
	"start", "stop", "mount", "unmount", "resize", "add", "register",
	"unregister", "restore", "enable", "disable", "adopt", "schedule",
}

// toParams normalizes an MCP tool's JSON-schema input into the map[string]any
// shape agent-go's AddTool expects. It round-trips through JSON so it works
// whether the SDK hands us a map, a typed schema, or json.RawMessage.
func toParams(schema interface{}) map[string]interface{} {
	empty := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	if schema == nil {
		return empty
	}
	if m, ok := schema.(map[string]interface{}); ok && len(m) > 0 {
		return m
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return empty
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return empty
	}
	return m
}
