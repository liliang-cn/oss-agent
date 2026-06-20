import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";

// Markdown renderer styled for chat answers (code blocks, lists, tables, links).
export function Markdown({ children }: { children: string }) {
  return (
    <div className="text-sm leading-relaxed [&>*:first-child]:mt-0 [&>*:last-child]:mb-0">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          p: ({ children }) => <p className="my-2">{children}</p>,
          h1: ({ children }) => <h1 className="mb-2 mt-4 text-lg font-semibold">{children}</h1>,
          h2: ({ children }) => <h2 className="mb-2 mt-4 text-base font-semibold">{children}</h2>,
          h3: ({ children }) => <h3 className="mb-1 mt-3 text-sm font-semibold">{children}</h3>,
          ul: ({ children }) => <ul className="my-2 list-disc space-y-1 pl-5">{children}</ul>,
          ol: ({ children }) => <ol className="my-2 list-decimal space-y-1 pl-5">{children}</ol>,
          li: ({ children }) => <li className="marker:text-muted-foreground">{children}</li>,
          a: ({ children, href }) => (
            <a href={href} target="_blank" rel="noreferrer" className="text-primary underline underline-offset-2">
              {children}
            </a>
          ),
          strong: ({ children }) => <strong className="font-semibold text-foreground">{children}</strong>,
          blockquote: ({ children }) => (
            <blockquote className="my-2 border-l-2 border-primary/50 pl-3 text-muted-foreground">{children}</blockquote>
          ),
          hr: () => <hr className="my-3 border-border" />,
          table: ({ children }) => (
            <div className="my-2 overflow-x-auto">
              <table className="w-full border-collapse text-xs">{children}</table>
            </div>
          ),
          th: ({ children }) => <th className="border border-border bg-secondary/50 px-2 py-1 text-left font-medium">{children}</th>,
          td: ({ children }) => <td className="border border-border px-2 py-1">{children}</td>,
          code: ({ className, children }) => {
            const inline = !className;
            if (inline) {
              return <code className="rounded bg-secondary px-1.5 py-0.5 font-mono text-[0.85em] text-primary">{children}</code>;
            }
            return (
              <pre className="my-2 overflow-x-auto rounded-lg border border-border bg-background/60 p-3">
                <code className="font-mono text-xs leading-relaxed">{children}</code>
              </pre>
            );
          },
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
