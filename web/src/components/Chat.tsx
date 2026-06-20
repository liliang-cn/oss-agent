import { useEffect, useRef } from "react";
import { useChat } from "@ai-sdk/react";
import { gsap } from "gsap";
import { SendHorizonal, Sparkles, User, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/primitives";
import { Markdown } from "@/components/Markdown";

// Persist a conversation id so cross-session memory + in-session history line up.
function sessionId() {
  let id = localStorage.getItem("oss_session");
  if (!id) {
    id = "web-" + Math.random().toString(36).slice(2) + Date.now().toString(36);
    localStorage.setItem("oss_session", id);
  }
  return id;
}

export function Chat() {
  const sid = useRef(sessionId());
  const { messages, input, handleInputChange, handleSubmit, isLoading } = useChat({
    api: "/api/chat/stream",
    streamProtocol: "text",
    body: { session_id: sid.current },
  });
  const listRef = useRef<HTMLDivElement>(null);
  const endRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    endRef.current?.scrollIntoView({ behavior: "smooth" });
    const last = listRef.current?.lastElementChild;
    if (last) gsap.from(last, { y: 16, opacity: 0, duration: 0.4, ease: "power2.out" });
  }, [messages.length]);

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      (e.currentTarget as HTMLTextAreaElement).form?.requestSubmit();
    }
  }

  return (
    <div className="flex h-full flex-col">
      <div ref={listRef} className="flex-1 space-y-5 overflow-y-auto px-1 py-2">
        {messages.length === 0 && (
          <div className="mt-20 text-center text-muted-foreground">
            <Sparkles className="mx-auto mb-3 h-8 w-8 text-primary" />
            <p className="text-lg font-medium text-foreground">Ask the knowledge agent</p>
            <p className="mt-1 text-sm">Grounded in your project's code, docs, KB &amp; internal skills.</p>
          </div>
        )}
        {messages.map((m) => (
          <div key={m.id} className={`flex gap-3 ${m.role === "user" ? "justify-end" : "justify-start"}`}>
            {m.role !== "user" && (
              <div className="mt-1 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-primary/15 text-primary">
                <Sparkles className="h-4 w-4" />
              </div>
            )}
            <div
              className={`max-w-[78%] rounded-2xl px-4 py-2.5 text-sm leading-relaxed ${
                m.role === "user"
                  ? "whitespace-pre-wrap bg-primary text-primary-foreground rounded-br-sm"
                  : "glass rounded-bl-sm"
              }`}
            >
              {m.role === "user" ? (
                m.content
              ) : m.content ? (
                <Markdown>{m.content}</Markdown>
              ) : isLoading ? (
                <Dots />
              ) : (
                ""
              )}
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

      <form onSubmit={handleSubmit} className="mt-3 flex items-end gap-2">
        <Textarea
          value={input}
          onChange={handleInputChange}
          onKeyDown={onKey}
          placeholder="e.g. How do I safely recover a resource stuck in a degraded state?"
          rows={1}
          className="max-h-40"
        />
        <Button type="submit" size="icon" disabled={isLoading || !input.trim()}>
          {isLoading ? <Loader2 className="h-4 w-4 animate-spin" /> : <SendHorizonal className="h-4 w-4" />}
        </Button>
      </form>
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
