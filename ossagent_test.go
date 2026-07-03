package ossagent

import (
	"encoding/json"
	"testing"
)

// TestParseSuggestion verifies the Stream conversion that turns a suggest_action
// tool result into the structured EventSuggestion payload, including the red-line
// verdict. The input map mirrors exactly what the suggest_action handler returns.
func TestParseSuggestion(t *testing.T) {
	result := map[string]interface{}{
		"ok":      true,
		"message": "suggestion recorded; awaiting operator approval",
		"suggestion": map[string]interface{}{
			"action":   "ha.evict",
			"params":   map[string]interface{}{"node": "orange1"},
			"reason":   "node is unresponsive",
			"severity": "high",
			"verdict": Verdict{
				Blocked:  true,
				Command:  "ha.evict node=orange1",
				RuleID:   "no-evict",
				Severity: "HIGH",
				Reason:   "evicting a node forces a failover",
			},
		},
	}

	s := parseSuggestion(result)
	if s == nil {
		t.Fatal("parseSuggestion returned nil for a valid result")
	}
	if s.Action != "ha.evict" {
		t.Fatalf("action = %q", s.Action)
	}
	if s.Params["node"] != "orange1" {
		t.Fatalf("params lost: %v", s.Params)
	}
	if s.Severity != "high" {
		t.Fatalf("severity = %q", s.Severity)
	}
	if !s.Verdict.Blocked || s.Verdict.RuleID != "no-evict" {
		t.Fatalf("verdict not carried: %+v", s.Verdict)
	}
}

// TestParseSuggestionFromJSONString verifies the conversion is robust to the
// result arriving as a JSON string rather than a Go map.
func TestParseSuggestionFromJSONString(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"suggestion": map[string]interface{}{
			"action":   "resource.status",
			"reason":   "health check",
			"severity": "low",
			"verdict":  Verdict{Blocked: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := parseSuggestion(string(raw))
	if s == nil || s.Action != "resource.status" || s.Verdict.Blocked {
		t.Fatalf("unexpected suggestion: %+v", s)
	}
}

// TestParseSuggestionRejectsNonSuggestion verifies non-suggestion tool results are
// ignored (no spurious EventSuggestion).
func TestParseSuggestionRejectsNonSuggestion(t *testing.T) {
	if s := parseSuggestion(map[string]interface{}{"ok": true, "hits": []any{}}); s != nil {
		t.Fatalf("expected nil for a non-suggestion result, got %+v", s)
	}
	if s := parseSuggestion("not json at all"); s != nil {
		t.Fatalf("expected nil for garbage, got %+v", s)
	}
	if s := parseSuggestion(nil); s != nil {
		t.Fatalf("expected nil for nil, got %+v", s)
	}
}

// TestEventSuggestionShape is a compile-and-shape check on the public event API.
func TestEventSuggestionShape(t *testing.T) {
	if EventSuggestion != "suggestion" {
		t.Fatalf("EventSuggestion kind = %q", EventSuggestion)
	}
	ev := Event{Kind: EventSuggestion, Tool: "suggest_action", Suggestion: &Suggestion{Action: "x"}}
	if ev.Suggestion == nil || ev.Suggestion.Action != "x" {
		t.Fatal("Event.Suggestion field not wired")
	}
}
