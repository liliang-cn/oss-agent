import { useEffect, useRef, useState } from "react";
import { gsap } from "gsap";
import { MessagesSquare, ScrollText, Search as SearchIcon, Boxes, Circle, Network, Sun, Moon } from "lucide-react";
import { Chat } from "@/components/Chat";
import { LogAnalysis } from "@/components/LogAnalysis";
import { Search } from "@/components/Search";
import { GraphView } from "@/components/GraphView";
import { cn } from "@/lib/utils";

type View = "chat" | "logs" | "search" | "graph";
type Health = { domain: string; title: string; llm: boolean; probes: number; status: string };
type Theme = "dark" | "light";

const NAV: { id: View; label: string; icon: typeof MessagesSquare }[] = [
  { id: "chat", label: "Chat", icon: MessagesSquare },
  { id: "logs", label: "Log analysis", icon: ScrollText },
  { id: "search", label: "Knowledge", icon: SearchIcon },
  { id: "graph", label: "Graph explorer", icon: Network },
];

export default function App() {
  const [view, setView] = useState<View>("chat");
  const [health, setHealth] = useState<Health | null>(null);
  const [theme, setTheme] = useState<Theme>(() => (localStorage.getItem("oss_theme") as Theme) || "dark");
  const paneRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark");
    localStorage.setItem("oss_theme", theme);
  }, [theme]);

  useEffect(() => {
    fetch("/api/healthz")
      .then((r) => r.json())
      .then((h: Health) => {
        setHealth(h);
        if (h?.title) document.title = `oss-agent · ${h.title}`;
      })
      .catch(() => setHealth(null));
  }, []);

  useEffect(() => {
    if (paneRef.current) gsap.fromTo(paneRef.current, { opacity: 0, y: 10 }, { opacity: 1, y: 0, duration: 0.35, ease: "power2.out" });
  }, [view]);

  return (
    <div className="flex h-screen w-screen overflow-hidden">
      <aside className="flex w-60 shrink-0 flex-col border-r border-border bg-card/40 p-4">
        <div className="mb-8 flex items-center gap-2 px-2">
          <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-primary/15 text-primary">
            <Boxes className="h-5 w-5" />
          </div>
          <div>
            <div className="text-sm font-semibold leading-tight">oss-agent</div>
            <div className="text-xs text-muted-foreground">{health?.title || "knowledge platform"}</div>
          </div>
        </div>

        <nav className="space-y-1">
          {NAV.map((n) => (
            <button
              key={n.id}
              onClick={() => setView(n.id)}
              className={cn(
                "flex w-full items-center gap-3 rounded-md px-3 py-2 text-sm transition-colors",
                view === n.id ? "bg-primary/15 text-primary" : "text-muted-foreground hover:bg-secondary/50 hover:text-foreground"
              )}
            >
              <n.icon className="h-4 w-4" />
              {n.label}
            </button>
          ))}
        </nav>

        <button
          onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
          className="mt-auto mb-3 flex items-center gap-2 rounded-md px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-secondary/50 hover:text-foreground"
        >
          {theme === "dark" ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
          {theme === "dark" ? "Light theme" : "Dark theme"}
        </button>

        <div className="rounded-lg border border-border bg-background/30 p-3 text-xs">
          <div className="mb-1 flex items-center gap-1.5">
            <Circle className={cn("h-2.5 w-2.5 fill-current", health ? "text-primary" : "text-muted-foreground")} />
            <span className="font-medium">{health ? "connected" : "offline"}</span>
          </div>
          {health && (
            <div className="space-y-0.5 text-muted-foreground">
              <div>domain: {health.domain}</div>
              <div>llm: {health.llm ? "on" : "search-only"} · probes: {health.probes}</div>
            </div>
          )}
        </div>
      </aside>

      <main className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center justify-between border-b border-border px-6 py-4">
          <h1 className="text-lg font-semibold">{NAV.find((n) => n.id === view)?.label}</h1>
          <span className="text-xs text-muted-foreground">{health?.domain || ""}</span>
        </header>
        <div ref={paneRef} className="min-h-0 flex-1 px-6 py-4">
          {view === "chat" && <Chat />}
          {view === "logs" && <LogAnalysis />}
          {view === "search" && <Search />}
          {view === "graph" && <GraphView theme={theme} />}
        </div>
      </main>
    </div>
  );
}
