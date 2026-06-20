import { useCallback, useEffect, useRef, useState } from "react";
import cytoscape, { type Core, type ElementDefinition } from "cytoscape";
// @ts-expect-error no types shipped
import fcose from "cytoscape-fcose";
import { Network, Loader2, Search as SearchIcon, Boxes } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/primitives";

cytoscape.use(fcose);

type GNode = { id: string; label: string; type: string; file?: string; seed?: boolean };
type GEdge = { source: string; target: string; type: string };
type Graph = { nodes: GNode[]; edges: GEdge[] };

const TYPE_COLOR: Record<string, string> = {
  // code structure
  function: "#2dd4bf",
  file: "#60a5fa",
  class: "#c084fc",
  config: "#fbbf24",
  service: "#4ade80",
  endpoint: "#f472b6",
  layer: "#fb923c",
  tour_step: "#f87171",
  // domain concepts (LLM ontology) — warm/distinct palette
  Resource: "#f43f5e",
  ResourceGroup: "#e11d48",
  Volume: "#fb7185",
  StoragePool: "#f59e0b",
  Node: "#84cc16",
  Cluster: "#22c55e",
  State: "#a855f7",
  CLI_Command: "#0ea5e9",
  ConfigParameter: "#eab308",
  ErrorCode: "#ef4444",
  KernelModule: "#14b8a6",
  Table: "#f43f5e", // object model from SQL schema
};
const colorOf = (t: string) => TYPE_COLOR[t] || "#94a3b8";

function toElements(g: Graph): ElementDefinition[] {
  const ids = new Set(g.nodes.map((n) => n.id));
  const els: ElementDefinition[] = g.nodes.map((n) => ({
    data: { id: n.id, label: n.label, color: colorOf(n.type), size: n.seed ? 22 : 9, type: n.type, file: n.file || "", seed: n.seed ? 1 : 0 },
  }));
  for (const e of g.edges) {
    if (ids.has(e.source) && ids.has(e.target)) {
      els.push({ data: { id: `${e.source}>${e.target}:${e.type}`, source: e.source, target: e.target, label: e.type } });
    }
  }
  return els;
}

export function GraphView() {
  const [q, setQ] = useState("");
  const [loading, setLoading] = useState(false);
  const [count, setCount] = useState<{ n: number; e: number } | null>(null);
  const [sel, setSel] = useState<GNode | null>(null);
  const boxRef = useRef<HTMLDivElement>(null);
  const cyRef = useRef<Core | null>(null);

  // init cytoscape once
  useEffect(() => {
    if (!boxRef.current || cyRef.current) return;
    const cy = cytoscape({
      container: boxRef.current,
      elements: [],
      style: [
        {
          selector: "node",
          style: {
            "background-color": "data(color)",
            label: "data(label)",
            "font-size": 5,
            width: "data(size)",
            height: "data(size)",
            color: "#94a3b8",
            "text-valign": "top",
            "text-margin-y": -2,
            "min-zoomed-font-size": 10,
          },
        },
        { selector: "node[seed = 1]", style: { "border-width": 3, "border-color": "#2dd4bf", "font-size": 9, color: "#cbd5e1" } },
        {
          selector: "edge",
          style: {
            width: 1,
            "line-color": "#475569",
            "curve-style": "bezier",
            "target-arrow-shape": "triangle",
            "target-arrow-color": "#475569",
            "arrow-scale": 0.6,
            opacity: 0.55,
          },
        },
        { selector: "node:selected", style: { "border-width": 3, "border-color": "#e2e8f0" } },
      ],
      layout: { name: "fcose", animate: false } as any,
      wheelSensitivity: 0.25,
      hideEdgesOnViewport: true,
      textureOnViewport: true,
      pixelRatio: 1,
    });
    cy.on("tap", "node", (evt) => {
      const d = evt.target.data();
      setSel({ id: d.id, label: d.label, type: d.type, file: d.file, seed: !!d.seed });
      expand(d.id);
    });
    cyRef.current = cy;
    loadAll();
    return () => { cy.destroy(); cyRef.current = null; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  function relayout(animate = true) {
    const cy = cyRef.current;
    if (!cy) return;
    const big = cy.nodes().length > 300;
    // Match the static graph.html aesthetic: fcose 'default' quality + tuned
    // repulsion/edge-length give organic blobs (not the stringy 'draft' layout).
    const lay = cy.layout({ name: "fcose", animate: animate && !big, animationDuration: 500, quality: "default", randomize: true, nodeRepulsion: 6000, idealEdgeLength: 60 } as any);
    lay.one("layoutstop", () => cy.fit(undefined, 30));
    lay.run();
    setCount({ n: cy.nodes().length, e: cy.edges().length });
  }

  const loadAll = useCallback(async () => {
    if (!cyRef.current) return;
    setLoading(true);
    setSel(null);
    try {
      const r = await fetch("/api/graph/all");
      const data: Graph = await r.json();
      const cy = cyRef.current;
      cy.elements().remove();
      cy.add(toElements(data));
      relayout(false);
    } finally {
      setLoading(false);
    }
  }, []);

  async function run(e: React.FormEvent) {
    e.preventDefault();
    if (!q.trim() || !cyRef.current) return;
    setLoading(true);
    setSel(null);
    try {
      const r = await fetch(`/api/graph?q=${encodeURIComponent(q)}`);
      const data: Graph = await r.json();
      const cy = cyRef.current;
      cy.elements().remove();
      cy.add(toElements(data));
      relayout();
    } finally {
      setLoading(false);
    }
  }

  const expand = useCallback(async (id: string) => {
    const r = await fetch(`/api/graph/expand?id=${encodeURIComponent(id)}`);
    const data: Graph = await r.json();
    const cy = cyRef.current;
    if (!cy) return;
    const add = toElements(data).filter((el) => cy.getElementById(el.data.id as string).empty());
    if (add.length) {
      cy.add(add);
      relayout();
    }
  }, []);

  return (
    <div className="flex h-full flex-col">
      <form onSubmit={run} className="flex gap-2">
        <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Seed the graph — e.g. resource definition controller, drbd_send" />
        <Button type="submit" disabled={loading}>
          {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <SearchIcon className="h-4 w-4" />} Explore
        </Button>
        <Button type="button" variant="outline" disabled={loading} onClick={() => loadAll()}>
          <Boxes className="h-4 w-4" /> All
        </Button>
      </form>

      <div className="relative mt-4 flex-1 overflow-hidden rounded-lg border border-border bg-card/30">
        <div ref={boxRef} className="absolute inset-0" />
        {!count && (
          <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center text-muted-foreground">
            <Network className="mb-3 h-8 w-8 text-primary" />
            <p className="text-sm">Search to render a subgraph · click a node to expand · scroll to zoom · drag to pan</p>
          </div>
        )}
        {sel && (
          <div className="glass absolute bottom-3 left-3 max-w-sm rounded-lg p-3 text-sm">
            <div className="font-mono font-medium text-primary">{sel.label}</div>
            <div className="mt-0.5 text-xs text-muted-foreground">{sel.type}{sel.file ? ` · ${sel.file}` : ""}</div>
          </div>
        )}
        {count && (
          <div className="absolute right-3 top-3 rounded-md border border-border bg-card/70 px-2 py-1 text-xs text-muted-foreground">
            {count.n} nodes · {count.e} edges
          </div>
        )}
      </div>
    </div>
  );
}
