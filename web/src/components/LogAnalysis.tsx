import { useRef, useState } from "react";
import { gsap } from "gsap";
import { UploadCloud, FileWarning, Loader2, Stethoscope } from "lucide-react";
import { Card, Badge } from "@/components/ui/primitives";

type Finding = { Severity: string; Count: number; Files: string[]; Sample: { Message: string; File: string; Line: number; Context: string[] } };
type Report = { files_scanned: number; files_total: number; findings: number; groups: Finding[]; diagnosis?: string };

export function LogAnalysis() {
  const [busy, setBusy] = useState(false);
  const [rep, setRep] = useState<Report | null>(null);
  const [name, setName] = useState("");
  const [drag, setDrag] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  async function upload(file: File) {
    setBusy(true);
    setRep(null);
    setName(file.name);
    try {
      const fd = new FormData();
      fd.append("log", file);
      const r = await fetch("/api/analyze-log?diagnose=true", { method: "POST", body: fd });
      const data: Report = await r.json();
      setRep(data);
      requestAnimationFrame(() => {
        if (listRef.current)
          gsap.from(listRef.current.querySelectorAll("[data-finding]"), {
            x: -16, opacity: 0, duration: 0.35, stagger: 0.04, ease: "power2.out",
          });
      });
    } finally {
      setBusy(false);
    }
  }

  const sevVariant = (s: string): "danger" | "warn" => (s === "FATAL" || s === "ERROR" ? "danger" : "warn");

  return (
    <div className="flex h-full flex-col">
      <div
        onDragOver={(e) => { e.preventDefault(); setDrag(true); }}
        onDragLeave={() => setDrag(false)}
        onDrop={(e) => { e.preventDefault(); setDrag(false); if (e.dataTransfer.files[0]) upload(e.dataTransfer.files[0]); }}
        onClick={() => inputRef.current?.click()}
        className={`flex cursor-pointer flex-col items-center justify-center rounded-lg border-2 border-dashed py-10 transition-colors ${
          drag ? "border-primary bg-primary/5" : "border-border hover:border-primary/50"
        }`}
      >
        <UploadCloud className={`h-8 w-8 ${drag ? "text-primary" : "text-muted-foreground"}`} />
        <p className="mt-2 text-sm font-medium">Drop a log, sosreport, or archive</p>
        <p className="text-xs text-muted-foreground">.log · .tar.gz · .zip · .gz — or click to browse</p>
        <input
          ref={inputRef}
          type="file"
          className="hidden"
          onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])}
        />
      </div>

      {busy && (
        <div className="mt-6 flex items-center justify-center gap-2 text-sm text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" /> Analyzing {name}…
        </div>
      )}

      {rep && (
        <div className="mt-4 flex-1 space-y-4 overflow-y-auto pr-1">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <Badge variant="muted">{rep.files_scanned}/{rep.files_total} files</Badge>
            <Badge variant="muted">{rep.findings.toLocaleString()} findings</Badge>
            <Badge>{rep.groups.length} distinct problems</Badge>
          </div>

          {rep.diagnosis && (
            <Card className="border-primary/30 p-4">
              <div className="mb-2 flex items-center gap-2 text-primary">
                <Stethoscope className="h-4 w-4" />
                <span className="text-sm font-semibold">AI diagnosis</span>
              </div>
              <p className="whitespace-pre-wrap text-sm leading-relaxed text-foreground/90">{rep.diagnosis}</p>
            </Card>
          )}

          <div ref={listRef} className="space-y-2">
            {rep.groups.slice(0, 15).map((g, i) => (
              <Card key={i} data-finding className="p-3">
                <div className="mb-1 flex items-center gap-2">
                  <Badge variant={sevVariant(g.Severity)}>{g.Severity} ×{g.Count}</Badge>
                  <FileWarning className="h-3.5 w-3.5 text-muted-foreground" />
                  <span className="truncate text-xs text-muted-foreground">{g.Sample.File}:{g.Sample.Line}</span>
                </div>
                <p className="font-mono text-xs text-foreground/90">{g.Sample.Message}</p>
                {g.Sample.Context?.length > 0 && (
                  <pre className="mt-1 max-h-24 overflow-auto rounded bg-background/50 p-2 text-[11px] text-muted-foreground">
                    {g.Sample.Context.join("\n")}
                  </pre>
                )}
              </Card>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
