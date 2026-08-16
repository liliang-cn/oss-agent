package knowledge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Health checks for the retrieval path.
//
// Inventory answers "what is in the store". This answers "does retrieval still
// work", which is a different question and the one that goes wrong silently.
// Every check here exists because the corresponding failure was observed in
// production and produced no error anywhere — the copilot simply answered
// worse, or answered from general knowledge, and nothing said why:
//
//   - A system-wide proxy env var routed the embedder through a host that no
//     longer existed. Chat kept working (its endpoint was in NO_PROXY), so the
//     assistant looked healthy while every knowledge_search failed.
//   - The vector index returned ~50 rows however large a TopK was asked for,
//     because the HNSW ef parameter bounded the candidate list below k. Deep
//     retrieval was impossible and nothing reported a limit.
//   - One imported code graph outnumbered the prose ten to one, so operational
//     questions retrieved code. The answers stayed fluent; only their sources
//     changed.

// CheckStatus is the verdict for one check.
type CheckStatus string

const (
	CheckOK   CheckStatus = "ok"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

// Check is one diagnostic result.
type Check struct {
	Name   string
	Status CheckStatus
	Detail string
	// Hint is what to do about it, present only when something is wrong.
	Hint string
}

// Diagnosis is the full report. Failed is true when any check failed, which is
// what a caller should use for an exit code.
type Diagnosis struct {
	Checks []Check
	Failed bool
}

func (d *Diagnosis) add(c Check) {
	if c.Status == CheckFail {
		d.Failed = true
	}
	d.Checks = append(d.Checks, c)
}

// Doctor runs every check against this store and its configured embedder.
func (s *Store) Doctor(ctx context.Context, embBaseURL string) *Diagnosis {
	d := &Diagnosis{}
	d.add(checkProxy(embBaseURL))
	d.add(s.checkEmbedder(ctx))
	inv, invErr := s.Inventory(ctx)
	d.add(checkInventory(inv, invErr))
	d.add(s.checkRecallDepth(ctx))
	d.add(s.checkSourceBalance(ctx, inv, invErr))
	d.add(s.checkOrphanNodes(ctx))
	return d
}

// checkProxy reports when the embedder endpoint would be sent through an HTTP
// proxy. Being proxied is legitimate; being proxied through a host that does not
// answer is the failure, and it presents as every search erroring at once.
func checkProxy(embBaseURL string) Check {
	const name = "embedder proxy"
	if strings.TrimSpace(embBaseURL) == "" {
		return Check{Name: name, Status: CheckOK, Detail: "no embedder endpoint configured"}
	}
	u, err := url.Parse(embBaseURL)
	if err != nil {
		return Check{Name: name, Status: CheckWarn, Detail: fmt.Sprintf("cannot parse %q", embBaseURL)}
	}
	proxyURL, err := http.ProxyFromEnvironment(&http.Request{URL: u})
	if err != nil {
		return Check{Name: name, Status: CheckWarn, Detail: fmt.Sprintf("proxy lookup failed: %v", err)}
	}
	if proxyURL == nil {
		return Check{Name: name, Status: CheckOK, Detail: "direct (not proxied)"}
	}
	// Reachability is proved by the embedder check below; this one's job is to
	// make the proxy visible, because an unreachable one is invisible otherwise.
	return Check{
		Name:   name,
		Status: CheckWarn,
		Detail: fmt.Sprintf("%s is routed through %s", u.Host, proxyURL.Host),
		Hint: "if embedding calls fail, this proxy is the first suspect — a stale HTTP_PROXY " +
			"takes the whole knowledge base down while chat keeps working. Add the endpoint to " +
			"NO_PROXY (a systemd unit's Environment= overrides its EnvironmentFile=, so it must " +
			"go in a drop-in).",
	}
}

// checkEmbedder embeds one short string. This is the end-to-end proof that the
// endpoint, the credentials and the network path all work.
func (s *Store) checkEmbedder(ctx context.Context) Check {
	const name = "embedder reachable"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()
	_, err := s.db.HybridSearchText(ctx, "healthcheck", 1)
	if err != nil {
		return Check{
			Name: name, Status: CheckFail,
			Detail: err.Error(),
			Hint: "every knowledge_search fails this way, and the agent will answer from general " +
				"knowledge instead of saying so. Check OSS_EMB_BASE_URL/OSS_EMB_API_KEY and the proxy above.",
		}
	}
	return Check{Name: name, Status: CheckOK, Detail: fmt.Sprintf("responded in %s", time.Since(start).Round(time.Millisecond))}
}

func checkInventory(inv *Inventory, err error) Check {
	const name = "store contents"
	if err != nil {
		return Check{Name: name, Status: CheckFail, Detail: err.Error()}
	}
	if inv.Chunks == 0 {
		return Check{
			Name: name, Status: CheckFail, Detail: "no chunks stored",
			Hint: "nothing has been ingested — run `oss-agent ingest <dir>` or `ingest-repo <url>`.",
		}
	}
	return Check{
		Name: name, Status: CheckOK,
		Detail: fmt.Sprintf("%d sources, %d chunks, %d nodes, %d edges, dim %d",
			len(inv.Sources), inv.Chunks, inv.Nodes, inv.Edges, inv.Dim),
	}
}

// recallProbeK is deliberately well past any index's default candidate list.
// The bug this catches returned exactly the index's ef, whatever was asked for.
const recallProbeK = 200

// checkRecallDepth asks for far more candidates than a normal search and reports
// how many come back.
//
// A vector index that caps its result set silently makes deep retrieval
// impossible: over-fetching callers, quota schemes that promote from a tail, and
// anything paginating all quietly see the same ceiling. The symptom is a round
// number of results that does not change when the request grows.
func (s *Store) checkRecallDepth(ctx context.Context) Check {
	const name = "recall depth"
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	small, err := s.db.HybridSearchText(ctx, "the", 10)
	if err != nil {
		return Check{Name: name, Status: CheckWarn, Detail: "probe failed: " + err.Error()}
	}
	large, err := s.db.HybridSearchText(ctx, "the", recallProbeK)
	if err != nil {
		return Check{Name: name, Status: CheckWarn, Detail: "probe failed: " + err.Error()}
	}
	detail := fmt.Sprintf("asked for %d, got %d (asked for 10, got %d)", recallProbeK, len(large), len(small))

	// Fewer rows than asked for is normal on a small corpus; the tell is a
	// result set that stops growing at a suspiciously round number while the
	// store clearly holds more.
	if len(large) == len(small) || len(large) >= recallProbeK {
		return Check{Name: name, Status: CheckOK, Detail: detail}
	}
	if inv, err := s.Inventory(ctx); err == nil && inv.Chunks > len(large)*2 && len(large)%10 == 0 {
		return Check{
			Name: name, Status: CheckWarn,
			Detail: detail + fmt.Sprintf("; the store holds %d chunks", inv.Chunks),
			Hint: "a recall that stops at a round number well below the corpus size usually means the " +
				"vector index is bounded below the requested k (HNSW ef, IVF nprobe). Deep retrieval " +
				"and any over-fetching caller silently hit that ceiling.",
		}
	}
	return Check{Name: name, Status: CheckOK, Detail: detail}
}

// checkSourceBalance reports how the corpus splits between imported code and
// prose, because one code graph can outnumber the runbooks by an order of
// magnitude and then answer operational questions.
func (s *Store) checkSourceBalance(ctx context.Context, inv *Inventory, invErr error) Check {
	const name = "source balance"
	if invErr != nil || inv == nil {
		return Check{Name: name, Status: CheckWarn, Detail: "inventory unavailable"}
	}
	var code, prose int
	for _, src := range inv.Sources {
		if isCodeSource(nil, src.DocumentID) {
			code += src.Chunks
		} else {
			prose += src.Chunks
		}
	}
	if code == 0 {
		return Check{Name: name, Status: CheckOK, Detail: fmt.Sprintf("%d prose chunks, no imported code graph", prose)}
	}
	detail := fmt.Sprintf("%d code chunks vs %d prose chunks", code, prose)
	if prose == 0 {
		return Check{Name: name, Status: CheckWarn, Detail: detail,
			Hint: "a code-only base cannot answer an operational question; ingest the runbooks too."}
	}
	// The quota bounds code at half of any result set, so imbalance is handled
	// at retrieval time. It is still worth seeing: the wider the gap, the more
	// the answer depends on that quota holding.
	if ratio := float64(code) / float64(prose); ratio > 5 {
		return Check{Name: name, Status: CheckWarn,
			Detail: detail + fmt.Sprintf(" (%.0fx)", ratio),
			Hint: "retrieval caps the code share per result set, so this is not fatal, but verify " +
				"operational questions still return runbooks — `oss-agent eval` reports the mix per question.",
		}
	}
	return Check{Name: name, Status: CheckOK, Detail: detail}
}

// checkOrphanNodes counts graph nodes with no edges. A large share means writes
// landed but their edges did not — the state a store reaches when mention edges
// are rejected, or when repeated ingests accumulate entities nothing can reach.
func (s *Store) checkOrphanNodes(ctx context.Context) Check {
	const name = "graph connectivity"
	var total, orphans int
	row := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM graph_nodes`)
	if err := row.Scan(&total); err != nil {
		return Check{Name: name, Status: CheckWarn, Detail: err.Error()}
	}
	if total == 0 {
		return Check{Name: name, Status: CheckOK, Detail: "no graph nodes"}
	}
	row = s.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM graph_nodes n
		WHERE NOT EXISTS (SELECT 1 FROM graph_edges e WHERE e.from_node_id = n.id OR e.to_node_id = n.id)`)
	if err := row.Scan(&orphans); err != nil {
		return Check{Name: name, Status: CheckWarn, Detail: err.Error()}
	}
	share := float64(orphans) / float64(total)
	detail := fmt.Sprintf("%d of %d nodes have no edges (%.0f%%)", orphans, total, share*100)
	if share > 0.15 {
		return Check{Name: name, Status: CheckWarn, Detail: detail,
			Hint: "unreachable nodes are retrieved but never expanded. They accumulate when an " +
				"ingest's edges are rejected, or when a source is re-ingested without being purged " +
				"first — `oss-agent refresh <dir>` purges before re-ingesting.",
		}
	}
	return Check{Name: name, Status: CheckOK, Detail: detail}
}

// Format renders a report for a terminal.
func (d *Diagnosis) Format() string {
	var b strings.Builder
	for _, c := range d.Checks {
		mark := "ok  "
		switch c.Status {
		case CheckWarn:
			mark = "warn"
		case CheckFail:
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "[%s] %-22s %s\n", mark, c.Name, c.Detail)
		if c.Hint != "" {
			for _, line := range wrapHint(c.Hint, 76) {
				fmt.Fprintf(&b, "       %s\n", line)
			}
		}
	}
	return b.String()
}

func wrapHint(s string, width int) []string {
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// ProxyEnvSummary lists the proxy variables in effect, for a caller that wants
// to print them alongside a failure.
func ProxyEnvSummary() string {
	var set []string
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			set = append(set, k+"="+v)
		}
	}
	sort.Strings(set)
	return strings.Join(set, "\n")
}
