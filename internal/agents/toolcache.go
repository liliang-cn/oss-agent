package agents

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Collapsing repeated read-only tool calls.
//
// A reasoning loop re-asks. Observed on a deployed copilot answering one
// question: sds_event_list ran four times with identical arguments inside a
// single turn, each a full round trip to the MCP server and a full copy of the
// result back into the context window. The persona forbids it in as many words
// ("never re-call a tool you already called with the same arguments") and the
// model did it anyway, which is the usual outcome of asking a model to enforce
// a mechanical invariant.
//
// Only read-only tools are cached, and only for a few seconds. The window is
// meant to cover one turn's reasoning and nothing more: an operator who acts on
// the cluster and immediately asks whether it worked must see the new state, so
// the cache expires long before the next question can arrive.

// toolCacheTTL bounds staleness. Long enough to span a turn's repeated calls,
// short enough that a follow-up question re-reads the cluster.
const toolCacheTTL = 15 * time.Second

type cachedResult struct {
	value interface{}
	err   error
	at    time.Time
}

// toolCache collapses identical read-only calls made within toolCacheTTL.
type toolCache struct {
	mu      sync.Mutex
	entries map[string]cachedResult
}

func newToolCache() *toolCache {
	return &toolCache{entries: make(map[string]cachedResult)}
}

// wrap returns handler with same-argument calls collapsed. Callers pass only
// read-only tools; a mutating tool that returned a cached result would report
// an effect that never happened the second time.
func (c *toolCache) wrap(name string, handler func(context.Context, map[string]interface{}) (interface{}, error)) func(context.Context, map[string]interface{}) (interface{}, error) {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		key, ok := cacheKey(name, args)
		if !ok {
			// Arguments that will not serialise cannot be compared, so this
			// call is simply not cacheable.
			return handler(ctx, args)
		}

		c.mu.Lock()
		entry, hit := c.entries[key]
		if hit && time.Since(entry.at) < toolCacheTTL {
			c.mu.Unlock()
			return entry.value, entry.err
		}
		c.mu.Unlock()

		value, err := handler(ctx, args)

		c.mu.Lock()
		c.entries[key] = cachedResult{value: value, err: err, at: time.Now()}
		c.evictExpiredLocked()
		c.mu.Unlock()
		return value, err
	}
}

// evictExpiredLocked keeps the map from growing without bound over a long-lived
// process. Cheap: the map holds at most one entry per distinct call made in the
// last few seconds.
func (c *toolCache) evictExpiredLocked() {
	if len(c.entries) < 64 {
		return
	}
	cutoff := time.Now().Add(-toolCacheTTL)
	for k, e := range c.entries {
		if e.at.Before(cutoff) {
			delete(c.entries, k)
		}
	}
}

// cacheKey identifies a call by tool name and arguments. json.Marshal sorts map
// keys, so two calls that differ only in argument order share a key.
func cacheKey(name string, args map[string]interface{}) (string, bool) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	return name + "\x00" + string(encoded), true
}
