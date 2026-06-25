import { useEffect, useRef, useState } from "react";
import { gsap } from "gsap";
import { SendHorizonal, Sparkles, User, Loader2, Check, Search, Network, Terminal } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/primitives";
import { Markdown } from "@/components/Markdown";

type Tool = { name: string; args?: Record<string, unknown>; done: boolean; count: number };
type Msg = { role: "user" | "assistant"; content: string; tools: Tool[] };

const argsKey = (args?: Record<string, unknown>) => JSON.stringify(args || {});

// Persist a conversation id so cross-session memory + in-session history line up.
function sessionId() {
  let id = localStorage.getItem("oss_session");
  if (!id) {
    id = "web-" + Math.random().toString(36).slice(2) + Date.now().toString(36);
    localStorage.setItem("oss_session", id);
  }
  return id;
}

// Pick an icon + a short human label for a tool by name.
function toolMeta(name: string) {
  const n = name.toLowerCase();
  if (n.includes("graph")) return { Icon: Network, label: name };
  if (n.includes("search") || n.includes("knowledge")) return { Icon: Search, label: name };
  return { Icon: Terminal, label: name };
}

// First short string value in the tool args, shown inline as a preview.
function argPreview(args?: Record<string, unknown>): string {
  if (!args) return "";
  for (const v of Object.values(args)) {
    if (typeof v === "string" && v.trim()) return v.length > 60 ? v.slice(0, 60) + "…" : v;
  }
  return "";
}

function ToolRow({ t }: { t: Tool }) {
  const { Icon, label } = toolMeta(t.name);
  const preview = argPreview(t.args);
  return (
    <div className="flex items-center gap-2 rounded-md border border-border bg-card/50 px-2.5 py-1.5 text-xs">
      <Icon className="h-3.5 w-3.5 shrink-0 text-primary" />
      <span className="font-medium text-foreground">{label}</span>
      {t.count > 1 && <span className="shrink-0 rounded bg-secondary px-1.5 text-[10px] text-muted-foreground">×{t.count}</span>}
      {preview && <span className="truncate text-muted-foreground">{preview}</span>}
      <span className="ml-auto shrink-0">
        {t.done ? <Check className="h-3.5 w-3.5 text-primary" /> : <Loader2 className="h-3.5 w-3.5 animate-spin text-muted-foreground" />}
      </span>
    </div>
  );
}

export function Chat() {
  const sid = useRef(sessionId());
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const listRef = useRef<HTMLDivElement>(null);
  const endRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: "smooth" });
    const last = listRef.current?.lastElementChild;
    if (last) gsap.from(last, { y: 16, opacity: 0, duration: 0.4, ease: "power2.out" });
  }, [messages.length]);

  // Apply a stream frame to the in-flight (last) assistant message.
  function patchLast(fn: (a: Msg) => Msg) {
    setMessages((m) => {
      if (!m.length) return m;
      const copy = m.slice();
      copy[copy.length - 1] = fn(copy[copy.length - 1]);
      return copy;
    });
  }

  async function send() {
    const text = input.trim();
    if (!text || busy) return;
    setInput("");
    setBusy(true);
    setMessages((m) => [...m, { role: "user", content: text, tools: [] }, { role: "assistant", content: "", tools: [] }]);

    try {
      const res = await fetch("/api/chat/stream", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ session_id: sid.current, message: text }),
      });
      if (!res.body) throw new Error("no stream");
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl: number;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (!line) continue;
          let f: any;
          try { f = JSON.parse(line); } catch { continue; }
          if (f.t === "text") {
            patchLast((a) => ({ ...a, content: a.content + (f.d || "") }));
          } else if (f.t === "reset") {
            // Server replaced a streamed preamble with the authoritative answer.
            patchLast((a) => ({ ...a, content: "" }));
          } else if (f.t === "tool") {
            // Collapse identical repeated calls (same tool + args) into one row with a count.
            patchLast((a) => {
              const k = f.name + argsKey(f.args);
              const tools = a.tools.slice();
              const ex = tools.findIndex((t) => t.name + argsKey(t.args) === k);
              if (ex >= 0) tools[ex] = { ...tools[ex], count: tools[ex].count + 1, done: false };
              else tools.push({ name: f.name, args: f.args, done: false, count: 1 });
              return { ...a, tools };
            });
          } else if (f.t === "tool_result") {
            patchLast((a) => {
              const tools = a.tools.slice();
              for (let i = tools.length - 1; i >= 0; i--) {
                if (tools[i].name === f.name && !tools[i].done) { tools[i] = { ...tools[i], done: true }; break; }
              }
              return { ...a, tools };
            });
          } else if (f.t === "error") {
            patchLast((a) => ({ ...a, content: a.content + `\n\n\`[error] ${f.d}\`` }));
          }
        }
      }
    } catch (e: any) {
      patchLast((a) => ({ ...a, content: a.content + `\n\n\`[error] ${e?.message || e}\`` }));
    } finally {
      // Any tool still spinning is finished once the stream closes.
      patchLast((a) => ({ ...a, tools: a.tools.map((t) => ({ ...t, done: true })) }));
      setBusy(false);
    }
  }

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  }

  return (
    <div className="flex h-full flex-col">
      <div ref={listRef} className="flex-1 space-y-5 overflow-y-auto px-1 py-2">
        {messages.length === 0 && (
          <div className="mt-20 text-center text-muted-foreground">
            <Sparkles className="mx-auto mb-3 h-8 w-8 text-primary" />
            <p className="text-lg font-medium text-foreground">Ask the knowledge agent</p>
            <p className="mt-1 text-sm">Grounded in your project's code, docs, KB &amp; internal skills — watch it call tools live.</p>
          </div>
        )}
        {messages.map((m, i) => (
          <div key={i} className={`flex gap-3 ${m.role === "user" ? "justify-end" : "justify-start"}`}>
            {m.role !== "user" && (
              <div className="mt-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-primary/15 text-primary">
                <Sparkles className="h-4 w-4" />
              </div>
            )}
            <div className={`max-w-[78%] ${m.role === "user" ? "" : "w-full"}`}>
              {m.role === "assistant" && m.tools.length > 0 && (
                <div className="mb-2 space-y-1">
                  {m.tools.map((t, j) => <ToolRow key={j} t={t} />)}
                </div>
              )}
              <div
                className={`rounded-2xl px-4 py-2.5 text-sm leading-relaxed ${
                  m.role === "user"
                    ? "whitespace-pre-wrap bg-primary text-primary-foreground rounded-br-sm"
                    : "glass rounded-bl-sm"
                } ${m.role === "assistant" && !m.content && m.tools.length > 0 ? "hidden" : ""}`}
              >
                {m.role === "user" ? (
                  m.content
                ) : m.content ? (
                  <Markdown>{m.content}</Markdown>
                ) : busy && i === messages.length - 1 ? (
                  <Dots />
                ) : (
                  ""
                )}
              </div>
            </div>
            {m.role === "user" && (
              <div className="mt-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-secondary">
                <User className="h-4 w-4" />
              </div>
            )}
          </div>
        ))}
        <div ref={endRef} />
      </div>

      <div className="mt-3 flex items-end gap-2">
        <Textarea
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={onKey}
          placeholder="e.g. How do I safely recover a resource stuck in a degraded state?"
          rows={1}
          className="max-h-40"
        />
        <Button type="button" size="icon" disabled={busy || !input.trim()} onClick={send}>
          {busy ? <Loader2 className="h-4 w-4 animate-spin" /> : <SendHorizonal className="h-4 w-4" />}
        </Button>
      </div>
    </div>
  );
}

function Dots() {
  return (
    <span className="inline-flex gap-1">
      <span className="typing-dot h-1.5 w-1.5 rounded-full bg-current" />
      <span className="typing-dot h-1.5 w-1.5 rounded-full bg-current" style={{ animationDelay: "0.2s" }} />
      <span className="typing-dot h-1.5 w-1.5 rounded-full bg-current" style={{ animationDelay: "0.4s" }} />
    </span>
  );
}
