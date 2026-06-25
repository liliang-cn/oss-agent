import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import ForceGraph3D from "react-force-graph-3d";
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

// A node as a force engine sees it: our fields + the x/y/z it mutates in.
type FNode = GNode & { x?: number; y?: number; z?: number };
type FLink = { source: string; target: string; type: string };
type Mode = "2d" | "3d";

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
// Stable hashed hue for any type not in the named palette. Lightness drops a bit
// in light mode so hashed colors stay legible on a pale background.
const colorOf = (t: string, dark = true) => {
  if (TYPE_COLOR[t]) return TYPE_COLOR[t];
  let h = 0;
  for (let i = 0; i < t.length; i++) h = (h * 31 + t.charCodeAt(i)) % 360;
  return `hsl(${h}, 62%, ${dark ? 62 : 48}%)`;
};

// Theme-dependent chrome colors for the graph canvas (the type palette is shared).
const PALETTE = (dark: boolean) => ({
  bg: dark ? "#06080f" : "#f8fafc",
  link: dark ? "#334155" : "#cbd5e1",
  nodeText: dark ? "#94a3b8" : "#475569",
  selBorder: dark ? "#e2e8f0" : "#0f172a",
  labelBg: dark ? "#0d1424" : "#ffffff",
  labelBorder: dark ? "#334155" : "#cbd5e1",
  labelText: dark ? "#e2e8f0" : "#0f172a",
});

// Cytoscape stylesheet for the active theme. Node fill comes from per-node data;
// text/edge/selection colors follow the palette.
const cyStyle = (dark: boolean) => {
  const p = PALETTE(dark);
  return [
    {
      selector: "node",
      style: {
        "background-color": "data(color)",
        label: "data(label)",
        "font-size": 5,
        width: "data(size)",
        height: "data(size)",
        color: p.nodeText,
        "text-valign": "top",
        "text-margin-y": -2,
        "min-zoomed-font-size": 10,
      },
    },
    { selector: "node[seed = 1]", style: { "border-width": 3, "border-color": "#14b8a6", "font-size": 9, color: dark ? "#cbd5e1" : "#0f172a" } },
    { selector: "edge", style: { width: 1, "line-color": p.link, "curve-style": "haystack", opacity: 0.55 } },
    { selector: "node:selected", style: { "border-width": 3, "border-color": p.selBorder } },
  ] as any;
};

// ---------- 2D renderer (Cytoscape + fcose): light, handles thousands of nodes ----------
function Graph2D({ nodes, links, selId, onPick, fitSignal, dark }: { nodes: FNode[]; links: FLink[]; selId?: string; onPick: (n: GNode) => void; fitSignal: number; dark: boolean }) {
  const boxRef = useRef<HTMLDivElement>(null);
  const cyRef = useRef<Core | null>(null);
  const [layouting, setLayouting] = useState(false);
  const onPickRef = useRef(onPick);
  onPickRef.current = onPick;

  useEffect(() => {
    if (!boxRef.current || cyRef.current) return;
    const cy = cytoscape({
      container: boxRef.current,
      elements: [],
      style: cyStyle(dark),
      layout: { name: "fcose", animate: false } as any,
      wheelSensitivity: 0.25,
      hideEdgesOnViewport: true,
      textureOnViewport: true,
      pixelRatio: 1,
    });
    cy.on("tap", "node", (evt) => {
      const d = evt.target.data();
      onPickRef.current({ id: d.id, label: d.label, type: d.type, file: d.file, seed: !!d.seed });
    });
    cyRef.current = cy;
    return () => { cy.destroy(); cyRef.current = null; };
  }, []);

  // Re-style on theme change (node fills come from data and are unaffected).
  useEffect(() => {
    cyRef.current?.style(cyStyle(dark));
  }, [dark]);

  // Sync elements whenever the graph changes.
  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    const ids = new Set(nodes.map((n) => n.id));
    const els: ElementDefinition[] = nodes.map((n) => ({
      data: { id: n.id, label: n.label, color: colorOf(n.type, dark), size: n.seed ? 22 : 9, type: n.type, file: n.file || "", seed: n.seed ? 1 : 0 },
    }));
    for (const l of links) {
      if (ids.has(l.source) && ids.has(l.target)) els.push({ data: { id: `${l.source}>${l.target}:${l.type}`, source: l.source, target: l.target, label: l.type } });
    }
    cy.elements().remove();
    cy.add(els);
    const big = cy.nodes().length > 300;
    // fcose 'default' quality gives organic blobs; 'draft' collapses big graphs
    // into stringy spider-arms. The layout blocks the main thread (~seconds on a
    // big graph), so paint a "laying out" overlay first, then defer the run one
    // tick so the message is visible before the freeze. Fewer iterations on big
    // graphs trades a little settling for a much shorter freeze.
    setLayouting(true);
    const t = setTimeout(() => {
      const lay = cy.layout({ name: "fcose", animate: !big, animationDuration: 500, quality: "default", randomize: true, numIter: big ? 1000 : 2500, tile: true, nodeRepulsion: 6000, idealEdgeLength: 60 } as any);
      lay.one("layoutstop", () => { cy.fit(undefined, 30); setLayouting(false); });
      lay.run();
    }, 40);
    return () => clearTimeout(t);
  }, [nodes, links, dark]);

  useEffect(() => {
    const cy = cyRef.current;
    if (!cy) return;
    cy.nodes().unselect();
    if (selId) cy.getElementById(selId).select();
  }, [selId]);

  useEffect(() => {
    if (fitSignal) cyRef.current?.fit(undefined, 30);
  }, [fitSignal]);

  return (
    <>
      <div ref={boxRef} className="absolute inset-0" />
      {layouting && (
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-2 text-muted-foreground">
          <Loader2 className="h-6 w-6 animate-spin text-primary" />
          <p className="text-sm">Laying out {nodes.length.toLocaleString()} nodes — this can take a few seconds…</p>
        </div>
      )}
    </>
  );
}

// ---------- 3D renderer (react-force-graph-3d / WebGL): heavier, tuned for big graphs ----------
function Graph3D({ nodes, links, selId, onPick, dim, fitSignal, dark }: { nodes: FNode[]; links: FLink[]; selId?: string; onPick: (n: GNode) => void; dim: { w: number; h: number }; fitSignal: number; dark: boolean }) {
  const fgRef = useRef<any>(null);
  const p = PALETTE(dark);
  // Hand the engine fresh link copies so it can rewrite source/target → node refs
  // without corrupting our canonical string-id links.
  const data = useMemo(() => ({ nodes, links: links.map((l) => ({ source: l.source, target: l.target, type: l.type })) }), [nodes, links]);
  const big = nodes.length > 1500;

  useEffect(() => {
    if (fitSignal) fgRef.current?.zoomToFit(600, 40);
  }, [fitSignal]);

  return (
    <ForceGraph3D
      ref={fgRef}
      width={dim.w}
      height={dim.h}
      graphData={data}
      backgroundColor={p.bg}
      nodeLabel={(n: any) => `<div style="background:${p.labelBg};color:${p.labelText};padding:3px 7px;border-radius:5px;border:1px solid ${p.labelBorder};font-size:12px">${n.label}${n.type ? ` · ${n.type}` : ""}</div>`}
      nodeColor={(n: any) => (selId && n.id === selId ? p.selBorder : colorOf(n.type, dark))}
      nodeVal={(n: any) => (n.seed ? 6 : 2)}
      nodeOpacity={0.92}
      nodeResolution={6}
      linkColor={() => p.link}
      linkWidth={0.3}
      linkOpacity={dark ? 0.45 : 0.7}
      // Arrows are cone geometry per link — far too heavy on a big graph.
      linkDirectionalArrowLength={big ? 0 : 2}
      linkDirectionalArrowRelPos={1}
      enableNodeDrag={false}
      warmupTicks={big ? 40 : 20}
      cooldownTicks={big ? 60 : 150}
      onNodeClick={(n: any) => {
        onPick({ id: n.id, label: n.label, type: n.type, file: n.file, seed: !!n.seed });
        const dist = 90;
        const r = Math.hypot(n.x || 0, n.y || 0, n.z || 0) || 1;
        const k = 1 + dist / r;
        fgRef.current?.cameraPosition({ x: (n.x || 0) * k, y: (n.y || 0) * k, z: (n.z || 0) * k }, n, 1000);
      }}
    />
  );
}

export function GraphView({ theme = "dark" }: { theme?: "light" | "dark" }) {
  const dark = theme === "dark";
  const [q, setQ] = useState("");
  const [mode, setMode] = useState<Mode>("2d");
  const [loading, setLoading] = useState(false);
  const [shown, setShown] = useState(false);
  const [count, setCount] = useState<{ n: number; e: number } | null>(null);
  const [sel, setSel] = useState<GNode | null>(null);
  const [data, setData] = useState<{ nodes: FNode[]; links: FLink[] }>({ nodes: [], links: [] });
  const [dim, setDim] = useState<{ w: number; h: number }>({ w: 0, h: 0 });
  const [fitSignal, setFitSignal] = useState(0);

  const boxRef = useRef<HTMLDivElement>(null);
  // Canonical, deduped graph (string-id links). Reusing node objects across
  // updates preserves the x/y/z the 3D engine writes in, so expansion doesn't re-scatter.
  const nodeMap = useRef<Map<string, FNode>>(new Map());
  const linkKeys = useRef<Set<string>>(new Set());
  const links = useRef<FLink[]>([]);

  useEffect(() => {
    if (!boxRef.current) return;
    const ro = new ResizeObserver((entries) => {
      const r = entries[0].contentRect;
      setDim({ w: r.width, h: r.height });
    });
    ro.observe(boxRef.current);
    return () => ro.disconnect();
  }, []);

  const merge = useCallback((g: Graph, replace: boolean) => {
    if (replace) {
      nodeMap.current = new Map();
      linkKeys.current = new Set();
      links.current = [];
    }
    for (const n of g.nodes) {
      const ex = nodeMap.current.get(n.id);
      if (ex) { if (n.seed) ex.seed = true; }
      else nodeMap.current.set(n.id, { ...n });
    }
    for (const e of g.edges) {
      const k = `${e.source}>${e.target}:${e.type}`;
      if (!linkKeys.current.has(k) && nodeMap.current.has(e.source) && nodeMap.current.has(e.target)) {
        linkKeys.current.add(k);
        links.current.push({ source: e.source, target: e.target, type: e.type });
      }
    }
    const nodes = [...nodeMap.current.values()];
    setData({ nodes, links: [...links.current] });
    setCount({ n: nodes.length, e: links.current.length });
  }, []);

  const loadAll = useCallback(async () => {
    setShown(true);
    setLoading(true);
    setSel(null);
    try {
      const r = await fetch("/api/graph/all");
      merge((await r.json()) as Graph, true);
    } finally {
      setLoading(false);
    }
  }, [merge]);

  async function run(e: React.FormEvent) {
    e.preventDefault();
    if (!q.trim()) return;
    setShown(true);
    setLoading(true);
    setSel(null);
    try {
      const r = await fetch(`/api/graph?q=${encodeURIComponent(q)}`);
      merge((await r.json()) as Graph, true);
    } finally {
      setLoading(false);
    }
  }

  const expand = useCallback(async (id: string) => {
    const r = await fetch(`/api/graph/expand?id=${encodeURIComponent(id)}`);
    merge((await r.json()) as Graph, false);
  }, [merge]);

  const onPick = useCallback((n: GNode) => {
    setSel(n);
    expand(n.id);
  }, [expand]);

  return (
    <div className="flex h-full flex-col">
      <form onSubmit={run} className="flex flex-wrap gap-2">
        <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Seed the graph — e.g. resource definition controller, send path" className="min-w-[200px] flex-1" />
        <Button type="submit" disabled={loading}>
          {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <SearchIcon className="h-4 w-4" />} Explore
        </Button>
        <Button type="button" variant="outline" disabled={loading} onClick={() => loadAll()}>
          <Boxes className="h-4 w-4" /> All
        </Button>
        <Button type="button" variant="outline" disabled={loading} onClick={() => setFitSignal((s) => s + 1)}>
          Fit
        </Button>
        {/* 2D / 3D toggle */}
        <div className="flex overflow-hidden rounded-md border border-border">
          {(["2d", "3d"] as Mode[]).map((m) => (
            <button
              key={m}
              type="button"
              onClick={() => setMode(m)}
              className={`px-3 text-sm font-medium transition-colors ${mode === m ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-card"}`}
            >
              {m.toUpperCase()}
            </button>
          ))}
        </div>
      </form>

      <div ref={boxRef} className="relative mt-4 flex-1 overflow-hidden rounded-lg border border-border" style={{ background: PALETTE(dark).bg }}>
        {shown && mode === "2d" && <Graph2D nodes={data.nodes} links={data.links} selId={sel?.id} onPick={onPick} fitSignal={fitSignal} dark={dark} />}
        {shown && mode === "3d" && dim.w > 0 && <Graph3D nodes={data.nodes} links={data.links} selId={sel?.id} onPick={onPick} dim={dim} fitSignal={fitSignal} dark={dark} />}

        {!shown && (
          <div className="absolute inset-0 flex flex-col items-center justify-center text-muted-foreground">
            <Network className="mb-3 h-8 w-8 text-primary" />
            <p className="mb-4 text-sm">Render the knowledge graph — click a node to expand · {mode === "3d" ? "drag to orbit" : "drag to pan"} · scroll to zoom</p>
            <Button onClick={() => loadAll()} disabled={loading}>
              {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Boxes className="h-4 w-4" />} Show {mode.toUpperCase()} graph
            </Button>
          </div>
        )}
        {shown && loading && !count && (
          <div className="pointer-events-none absolute inset-0 flex items-center justify-center text-muted-foreground">
            <Loader2 className="h-6 w-6 animate-spin text-primary" />
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
            {count.n} nodes · {count.e} edges · {mode.toUpperCase()}
          </div>
        )}
      </div>
    </div>
  );
}
