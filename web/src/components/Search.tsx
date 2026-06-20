import { useRef, useState } from "react";
import { gsap } from "gsap";
import { Search as SearchIcon, GitBranch, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, Input, Badge } from "@/components/ui/primitives";

type Hit = { DocumentID: string; Content: string; Score: number };
type Neighbor = { Name: string; Type: string; Via: string; Summary: string; FilePath: string };

export function Search() {
  const [q, setQ] = useState("");
  const [hits, setHits] = useState<Hit[]>([]);
  const [neighbors, setNeighbors] = useState<Neighbor[]>([]);
  const [loading, setLoading] = useState(false);
  const [done, setDone] = useState(false);
  const gridRef = useRef<HTMLDivElement>(null);

  async function run(e: React.FormEvent) {
    e.preventDefault();
    if (!q.trim()) return;
    setLoading(true);
    setDone(false);
    try {
      const r = await fetch("/api/search-graph", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query: q, top_k: 6 }),
      });
      const data = await r.json();
      setHits(data.hits || []);
      setNeighbors(data.related_via_graph || []);
      setDone(true);
      requestAnimationFrame(() => {
        if (gridRef.current)
          gsap.from(gridRef.current.querySelectorAll("[data-card]"), {
            y: 18,
            opacity: 0,
            duration: 0.4,
            stagger: 0.05,
            ease: "power2.out",
          });
      });
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="flex h-full flex-col">
      <form onSubmit={run} className="flex gap-2">
        <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search code, docs & KB — e.g. drbd_send sending path" />
        <Button type="submit" disabled={loading}>
          {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <SearchIcon className="h-4 w-4" />} Search
        </Button>
      </form>

      <div ref={gridRef} className="mt-4 flex-1 space-y-5 overflow-y-auto pr-1">
        {done && hits.length === 0 && <p className="text-sm text-muted-foreground">No hits.</p>}

        {hits.length > 0 && (
          <div className="space-y-2">
            <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Hits</h3>
            {hits.map((h, i) => (
              <Card key={i} data-card className="p-3">
                <div className="mb-1 flex items-center gap-2">
                  <Badge variant="muted">{h.DocumentID}</Badge>
                  <span className="text-xs text-muted-foreground">score {h.Score.toFixed(3)}</span>
                </div>
                <p className="line-clamp-3 text-sm text-foreground/90">{h.Content}</p>
              </Card>
            ))}
          </div>
        )}

        {neighbors.length > 0 && (
          <div className="space-y-2">
            <h3 className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
              <GitBranch className="h-3.5 w-3.5" /> Related via graph
            </h3>
            {neighbors.map((n, i) => (
              <Card key={i} data-card className="p-3">
                <div className="mb-1 flex flex-wrap items-center gap-2">
                  <span className="font-mono text-sm text-primary">{n.Name}</span>
                  <Badge variant="muted">{n.Type}</Badge>
                  {n.Via && <Badge>←{n.Via}</Badge>}
                  {n.FilePath && <span className="text-xs text-muted-foreground">{n.FilePath}</span>}
                </div>
                {n.Summary && <p className="line-clamp-2 text-sm text-foreground/80">{n.Summary}</p>}
              </Card>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
