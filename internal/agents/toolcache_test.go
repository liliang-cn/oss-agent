package agents

import (
	"context"
	"errors"
	"testing"
	"time"
)

func countingHandler(calls *int) func(context.Context, map[string]interface{}) (interface{}, error) {
	return func(context.Context, map[string]interface{}) (interface{}, error) {
		*calls++
		return map[string]interface{}{"n": *calls}, nil
	}
}

// The regression: one turn of reasoning called the same list tool four times
// with identical arguments, each a round trip and another copy of the result in
// the context window. The persona forbade it and the model did it anyway.
func TestToolCacheCollapsesIdenticalCalls(t *testing.T) {
	calls := 0
	h := newToolCache().wrap("sds_event_list", countingHandler(&calls))
	args := map[string]interface{}{"limit": 20}

	for i := 0; i < 4; i++ {
		if _, err := h(context.Background(), args); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
}

func TestToolCacheSeparatesDifferentArguments(t *testing.T) {
	calls := 0
	h := newToolCache().wrap("sds_event_list", countingHandler(&calls))
	if _, err := h(context.Background(), map[string]interface{}{"limit": 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := h(context.Background(), map[string]interface{}{"limit": 50}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("handler ran %d times, want 2 — different arguments are different calls", calls)
	}
}

// json.Marshal sorts map keys, so the same call written two ways is one call.
func TestToolCacheIgnoresArgumentOrder(t *testing.T) {
	calls := 0
	h := newToolCache().wrap("t", countingHandler(&calls))
	_, _ = h(context.Background(), map[string]interface{}{"a": 1, "b": 2})
	_, _ = h(context.Background(), map[string]interface{}{"b": 2, "a": 1})
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
}

// An operator who acts on the cluster and immediately asks whether it worked
// must see the new state, so the window has to expire.
func TestToolCacheExpires(t *testing.T) {
	calls := 0
	c := newToolCache()
	h := c.wrap("t", countingHandler(&calls))
	if _, err := h(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// Age the entry past the TTL rather than sleeping through it.
	c.mu.Lock()
	for k, e := range c.entries {
		e.at = time.Now().Add(-toolCacheTTL - time.Second)
		c.entries[k] = e
	}
	c.mu.Unlock()

	if _, err := h(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("handler ran %d times, want 2 — a stale entry must be refetched", calls)
	}
}

// A failure is cached like any other result: a loop that retries a failing tool
// four times in one turn learns nothing new and costs four round trips.
func TestToolCacheCachesErrors(t *testing.T) {
	calls := 0
	h := newToolCache().wrap("t", func(context.Context, map[string]interface{}) (interface{}, error) {
		calls++
		return nil, errors.New("upstream down")
	})
	for i := 0; i < 3; i++ {
		if _, err := h(context.Background(), nil); err == nil {
			t.Fatal("expected the error to be returned")
		}
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
}
