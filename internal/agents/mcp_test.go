package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	agdomain "github.com/liliang-cn/agent-go/v3/pkg/domain"
	"github.com/liliang-cn/agent-go/v3/pkg/providers"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/liliang-cn/oss-agent/internal/safety"
)

func TestReadOnlyAdmit(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		allow []string
		want  bool
	}{
		// heuristic: admit observational names
		{"list admitted", "sds_node_list", nil, true},
		{"get admitted", "sds_gateway_get", nil, true},
		{"status admitted", "sds_ha_status", nil, true},
		{"health admitted", "sds_node_health_check", nil, true},
		{"describe admitted", "describe_pool", nil, true},
		{"show admitted", "show_config", nil, true},
		// heuristic: reject mutating names
		{"create rejected", "sds_pool_create", nil, false},
		{"delete rejected", "sds_gateway_delete", nil, false},
		{"evict rejected", "sds_ha_evict", nil, false},
		{"resize rejected", "sds_resource_resize_volume", nil, false},
		{"adopt rejected", "sds_resource_adopt", nil, false},
		{"add rejected", "sds_pool_add_disk", nil, false},
		// fail closed: matches neither
		{"unknown rejected", "frobnicate", nil, false},
		// reject wins over admit when a name carries both
		{"list_and_delete rejected", "list_and_delete", nil, false},
		// explicit allowlist overrides the heuristic (both ways)
		{"allowlist admits mutating name", "sds_ha_evict", []string{"sds_ha_evict"}, true},
		{"allowlist excludes read name", "sds_node_list", []string{"sds_ha_status"}, false},
		{"allowlist case-insensitive", "sds_ha_status", []string{"SDS_HA_STATUS"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := readOnlyAdmit(tc.tool, tc.allow); got != tc.want {
				t.Fatalf("readOnlyAdmit(%q, %v) = %v, want %v", tc.tool, tc.allow, got, tc.want)
			}
		})
	}
}

func TestToParams(t *testing.T) {
	// nil schema yields an empty object schema
	if m := toParams(nil); m["type"] != "object" {
		t.Fatalf("nil schema: got %v", m)
	}
	// a schema map is passed through
	in := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}}}
	if m := toParams(in); m["properties"] == nil {
		t.Fatalf("map schema lost properties: %v", m)
	}
	// a typed schema round-trips through JSON
	if m := toParams(struct {
		Type string `json:"type"`
	}{Type: "object"}); m["type"] != "object" {
		t.Fatalf("typed schema: got %v", m)
	}
}

func TestServerConfigTransports(t *testing.T) {
	stdio := serverConfig(MCPSpec{Name: "a", Transport: "", Command: "sds-mcp", Args: []string{"--x"}})
	if stdio.Type != "stdio" || len(stdio.Command) != 1 || stdio.Command[0] != "sds-mcp" || len(stdio.Args) != 1 {
		t.Fatalf("stdio config wrong: %+v", stdio)
	}
	httpCfg := serverConfig(MCPSpec{Name: "b", Transport: "http", URL: "http://x"})
	if httpCfg.Type != "http" || httpCfg.URL != "http://x" {
		t.Fatalf("http config wrong: %+v", httpCfg)
	}
	sse := serverConfig(MCPSpec{Name: "c", Transport: "SSE", URL: "http://y"})
	if sse.Type != "sse" {
		t.Fatalf("sse config wrong: %+v", sse)
	}
}

// TestMountMCPReadOnly stands up a real MCP server over HTTP with one read tool
// and one write-named tool, mounts it ReadOnly, and asserts only the read tool is
// registered while the write tool is refused. It also proves the mounted tool is
// callable through the connected client.
func TestMountMCPReadOnly(t *testing.T) {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test-sds", Version: "0.0.1"}, nil)
	objectSchema := map[string]interface{}{"type": "object"}
	srv.AddTool(&sdkmcp.Tool{Name: "widget_list", Description: "list widgets", InputSchema: objectSchema},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "w1,w2"}}}, nil
		})
	srv.AddTool(&sdkmcp.Tool{Name: "widget_delete", Description: "delete a widget", InputSchema: objectSchema},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "deleted"}}}, nil
		})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	// Registered first, so (LIFO) it runs LAST — after the MCP clients are closed.
	// CloseClientConnections force-drops the long-lived SSE stream so Close never
	// blocks waiting on it.
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})

	svc := newTestService(t)
	defer svc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	clients, statuses := MountMCP(ctx, svc, []MCPSpec{{
		Name:      "sds",
		Transport: "http",
		URL:       ts.URL,
		ReadOnly:  true,
	}})
	// Registered after MountMCP, so (LIFO) it runs FIRST — clients close before the
	// server tears down.
	t.Cleanup(func() {
		for _, c := range clients {
			_ = c.Close()
		}
	})

	if len(statuses) != 1 {
		t.Fatalf("want 1 status, got %d", len(statuses))
	}
	st := statuses[0]
	if !st.Connected {
		t.Fatalf("server did not connect: %q", st.Err)
	}
	if st.Tools != 1 {
		t.Fatalf("want 1 registered tool (widget_list), got %d (skipped=%d)", st.Tools, st.Skipped)
	}
	if st.Skipped != 1 {
		t.Fatalf("want 1 skipped tool (widget_delete), got %d", st.Skipped)
	}
	if len(clients) != 1 {
		t.Fatalf("want 1 client, got %d", len(clients))
	}

	// The read tool is callable through the connected client.
	res, err := clients[0].CallTool(ctx, "widget_list", map[string]interface{}{})
	if err != nil {
		t.Fatalf("CallTool(widget_list): %v", err)
	}
	if !res.Success || res.Data != "w1,w2" {
		t.Fatalf("unexpected tool result: success=%v data=%v", res.Success, res.Data)
	}
}

// TestMountMCPUnreachableDegrades verifies a server that cannot connect is recorded
// and skipped rather than failing the mount (knowledge-only degradation).
func TestMountMCPUnreachableDegrades(t *testing.T) {
	svc := newTestService(t)
	defer svc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clients, statuses := MountMCP(ctx, svc, []MCPSpec{{
		Name:      "dead",
		Transport: "http",
		URL:       "http://127.0.0.1:1/mcp", // nothing listening
		ReadOnly:  true,
	}})
	for _, c := range clients {
		_ = c.Close()
	}
	if len(statuses) != 1 || statuses[0].Connected {
		t.Fatalf("expected a single failed status, got %+v", statuses)
	}
	if statuses[0].Err == "" {
		t.Fatalf("expected a connect error to be recorded")
	}
}

// TestSuggestionResultRedLine verifies the suggest_action core runs the proposed
// action through the red-line wall and never executes anything.
func TestSuggestionResultRedLine(t *testing.T) {
	filter, err := safety.NewFromSpecs([]safety.RuleSpec{{
		ID:       "no-evict",
		Severity: safety.SeverityHigh,
		Pattern:  `\bha\.evict\b`,
		Reason:   "evicting a node forces a failover",
	}})
	if err != nil {
		t.Fatalf("compile red-lines: %v", err)
	}

	// A destructive proposal gets a blocking verdict.
	out := suggestionResult(filter, map[string]interface{}{
		"action": "ha.evict",
		"params": map[string]interface{}{"node": "orange1"},
		"reason": "node is down",
	})
	sug, ok := out["suggestion"].(map[string]interface{})
	if !ok {
		t.Fatalf("no suggestion in result: %v", out)
	}
	if sug["severity"] != "medium" {
		t.Fatalf("default severity not applied: %v", sug["severity"])
	}
	verdict, ok := sug["verdict"].(safety.Verdict)
	if !ok {
		t.Fatalf("verdict wrong type: %T", sug["verdict"])
	}
	if !verdict.Blocked || verdict.RuleID != "no-evict" {
		t.Fatalf("expected blocking verdict, got %+v", verdict)
	}

	// A benign proposal is not blocked.
	out2 := suggestionResult(filter, map[string]interface{}{
		"action":   "resource.status",
		"reason":   "check health",
		"severity": "low",
	})
	sug2 := out2["suggestion"].(map[string]interface{})
	if sug2["severity"] != "low" {
		t.Fatalf("explicit severity lost: %v", sug2["severity"])
	}
	if sug2["verdict"].(safety.Verdict).Blocked {
		t.Fatalf("benign action should not be blocked")
	}
}

// newTestService builds a minimal agent-go service for tool-registration tests.
// The LLM provider is never dialed (Build/AddTool do not connect), so a placeholder
// key/URL is sufficient.
func newTestService(t *testing.T) *agent.Service {
	t.Helper()
	llm, err := providers.NewOpenAILLMProvider(&agdomain.OpenAIProviderConfig{
		BaseURL:  "http://127.0.0.1:1/v1",
		APIKey:   "test",
		LLMModel: "gpt-4o",
	})
	if err != nil {
		t.Fatalf("new llm provider: %v", err)
	}
	svc, err := agent.New("oss-agent-test").WithLLM(llm).Build()
	if err != nil {
		t.Fatalf("build service: %v", err)
	}
	return svc
}

// A read-only tool must not be refused because its SUBJECT contains a mutating
// word. These three were all rejected before: sds-mcp marks them read-only and
// the SDS Copilot still could not read promoter state, list snapshot schedules,
// or fetch OCF agent metadata.
func TestReadOnlyAdmitJudgesTheOperationNotTheSubject(t *testing.T) {
	for _, name := range []string{
		"sds_ha_promoter_status",     // subject "promoter", operation "status"
		"sds_snapshot_schedule_list", // subject "schedule", operation "list"
		"sds_ocf_agent_metadata",     // metadata describes, it does not change
		"sds_resource_list",
		"sds_node_health_check",
	} {
		if !readOnlyAdmit(name, nil) {
			t.Errorf("readOnlyAdmit(%q) = false, want true", name)
		}
	}
}

// Reading the operation first must not let anything mutating through.
func TestReadOnlyAdmitStillRefusesMutatingTools(t *testing.T) {
	for _, name := range []string{
		"sds_resource_create",
		"sds_resource_delete",
		"sds_ha_evict",
		"sds_resource_set_role",
		"sds_pool_add_disk",      // ends in a noun; the whole-name scan catches "add"
		"sds_gateway_create_nfs", // ends in a noun; the scan catches "create"
		"list_and_delete",        // ends in the mutating verb
		"sds_backup_restore",
	} {
		if readOnlyAdmit(name, nil) {
			t.Errorf("readOnlyAdmit(%q) = true, want false", name)
		}
	}
}

// A name with no operation to read and no known token stays refused.
func TestReadOnlyAdmitFailsClosed(t *testing.T) {
	for _, name := range []string{"frobnicate", "sds_widget_frobnicate", ""} {
		if readOnlyAdmit(name, nil) {
			t.Errorf("readOnlyAdmit(%q) = true, want false", name)
		}
	}
}

// An explicit allowlist overrides the heuristic in both directions.
func TestReadOnlyAllowlistOverridesTheHeuristic(t *testing.T) {
	if !readOnlyAdmit("sds_resource_create", []string{"sds_resource_create"}) {
		t.Error("an allowlisted name must be admitted even though it looks mutating")
	}
	if readOnlyAdmit("sds_resource_list", []string{"sds_node_list"}) {
		t.Error("with an allowlist, a name outside it must be refused")
	}
}

func TestTrailingSegment(t *testing.T) {
	for name, want := range map[string]string{
		"sds_ha_promoter_status": "status",
		"a.b.c":                  "c",
		"nounderscore":           "",
		"trailing_":              "",
	} {
		if got := trailingSegment(name); got != want {
			t.Errorf("trailingSegment(%q) = %q, want %q", name, got, want)
		}
	}
}
