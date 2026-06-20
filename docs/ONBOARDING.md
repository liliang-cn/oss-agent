# Onboarding a new project → a deployed agent

oss-agent is product-agnostic: the engine never changes. Bringing a new OSS
project online is a repeatable pipeline whose only product-specific input is a
`domain.toml`. This is the standardized process.

```
write domain.toml  →  build knowledge base  →  serve  →  deploy
   (the only            (ingest-repo ×N,        (single      (make deploy +
    judgment)            import-schema)          binary)       Caddy/systemd)
```

The engine has no compiled-in product knowledge. LINBIT (`examples/linbit/domain.toml`)
is just the first customer; copy it as a template.

---

## 0. Prerequisites

- Go 1.25+ (build). Node only if you change the front-end (`make web`).
- An **embedder** and an **LLM**, both OpenAI-compatible. We use Alibaba DashScope:
  - LLM: `qwen3.7-plus`
  - Embedder: `text-embedding-v4` (1024-dim)
  - (any OpenAI-compatible endpoint works; the query embedder MUST match the one
    used to build the index.)
- For code-graph ingestion: the [Understand-Anything](https://github.com/Egonex-AI/Understand-Anything)
  `/understand` skill reachable via `OSS_UNDERSTAND_CMD` (e.g. `claude -p "/understand ." --dangerously-skip-permissions`).

Environment used throughout (export once):

```bash
export OSS_DOMAIN_FILE=examples/linbit/domain.toml
export OSS_LLM_API_KEY=<key>   OSS_LLM_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1  OSS_LLM_MODEL=qwen3.7-plus
export OSS_EMB_API_KEY=<key>   OSS_EMB_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1  OSS_EMB_MODEL=text-embedding-v4  OSS_EMB_DIM=1024
export OSS_UNDERSTAND_CMD='claude -p "/understand ." --dangerously-skip-permissions'
```

---

## 1. Write `domain.toml` (the only product-specific input)

Copy `examples/linbit/domain.toml` and edit. Fields:

| field | meaning |
|---|---|
| `name` / `title` | engine label / UI brand |
| `persona` | the agent's system prompt (who it is, how to answer, safety posture) |
| `entity_types` / `relation_types` | the domain ontology vocabulary (used by extraction + graph) |
| `error_patterns` | regexes (group 1 = message) for log triage |
| `[[probes]]` | read-only diagnostic commands the agent may run |
| `repos` | upstream repos to ingest |
| `[[red_lines]]` | destructive-command blocks (the deterministic safety wall) |

`oss-agent domain` prints the loaded config to verify the TOML.

---

## 2. Build the knowledge base

Two layers get built into `data/knowledge.db`: a **graph** (code structure +
object model) and **semantic vectors** (for retrieval).

**a) Code repos → graph** (one command per repo, fully automatic):

```bash
oss-agent ingest-repo https://github.com/<org>/<repo>      # clone → /understand → import-graph
# (re-running understand on huge repos can stall; if so, merge intermediate/batch-*.json — see worklog)
```

**b) Docs / KB / blog → semantic vectors** (point at a dir of .md/.adoc):

```bash
oss-agent ingest-repo repos/<docs-dir>     # no knowledge-graph.json → text/semantic ingest
```

**c) Object model → graph** (deterministic, no LLM). Use whatever structured
source the project ships — this is the precise ontology, don't mine it from prose:

```bash
oss-agent import-schema path/to/schema.sql     # CREATE TABLE → entity, FOREIGN KEY → relation
```

> Adapter set: SQL schema (`import-schema`) today. Protobuf / OpenAPI / C-struct
> are the same idea — parse the structured source, not the docs. (planned)

**d) Verify:**

```bash
oss-agent search "<concept>"          # vector + keyword
oss-agent search-graph "<concept>"    # + one-hop graph expansion
oss-agent ask "<question>"            # full agent (probes + knowledge_search + red-line wall)
```

**Updating a source later** (catches deletions, no full rebuild):

```bash
oss-agent refresh <repo-or-dir>
```

---

## 3. Run locally

```bash
make run            # = go build + ./oss-agent serve   (API under /api/*, UI at /)
# or:  oss-agent ui   (also opens the browser)
```

Open `http://localhost:7634`. Without an LLM key it serves search-only.

---

## 4. Deploy (single binary + systemd + Caddy)

The binary embeds the web UI, and with cloud LLM/embedder there are **no runtime
deps** (no ollama). First-time provisioning is below; updates are `make deploy`.

### 4.1 First-time provision (on the server, once)

```bash
ssh HOST 'mkdir -p /opt/oss-agent/data'
scp domain.toml HOST:/opt/oss-agent/domain.toml
scp data/knowledge.db HOST:/opt/oss-agent/data/knowledge.db   # ship the prebuilt index

# env (root-only)
ssh HOST 'cat > /opt/oss-agent/oss-agent.env <<ENV
OSS_DOMAIN_FILE=/opt/oss-agent/domain.toml
OSS_KNOWLEDGE_DB_PATH=/opt/oss-agent/data/knowledge.db
OSS_DB_PATH=/opt/oss-agent/data/oss-agent.db
OSS_HTTP_ADDR=127.0.0.1:47634
OSS_LLM_API_KEY=...      OSS_LLM_BASE_URL=...  OSS_LLM_MODEL=qwen3.7-plus
OSS_EMB_API_KEY=...      OSS_EMB_BASE_URL=...  OSS_EMB_MODEL=text-embedding-v4  OSS_EMB_DIM=1024
OSS_RATE_LIMIT_PER_MIN=30
ENV
chmod 600 /opt/oss-agent/oss-agent.env'
```

systemd unit `/etc/systemd/system/oss-agent.service`:

```ini
[Unit]
Description=oss-agent
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
WorkingDirectory=/opt/oss-agent
EnvironmentFile=/opt/oss-agent/oss-agent.env
ExecStart=/opt/oss-agent/oss-agent serve
Restart=on-failure
RestartSec=3
[Install]
WantedBy=multi-user.target
```

```bash
ssh HOST 'systemctl daemon-reload && systemctl enable --now oss-agent'
```

### 4.2 Caddy: TLS + basic auth + reverse proxy

The public API has no built-in auth; gate the whole origin at the proxy (a token
baked into the public UI JS would be readable by anyone, so origin-level basic
auth is the right control). Generate a hash and add a site block:

```bash
ssh HOST 'caddy hash-password --plaintext "<password>"'   # → $2a$14$...
```

```caddy
oss.example.com {
    basic_auth {
        <user> <bcrypt-hash>
    }
    reverse_proxy 127.0.0.1:47634 {
        flush_interval -1          # required for streaming chat / SSE
    }
}
```

```bash
ssh HOST 'caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile && systemctl reload caddy'
```

Point DNS `oss.example.com A → <server ip>`; Caddy auto-provisions the cert.

### 4.3 Rate limiting

Built into the app: per-IP sliding-window cap on the LLM endpoints
(`/ask`, `/ask/stream`, `/diagnose`, `/chat`, `/chat/stream`, `/analyze-log`),
returns 429 when exceeded. Configure with `OSS_RATE_LIMIT_PER_MIN` (default 30,
`0` = unlimited). Read-only/static endpoints are not limited.

### 4.4 Updates

```bash
make deploy HOST=<host>     # cross-compile linux/amd64 → scp → restart service
make push-db HOST=<host>    # ship a freshly rebuilt knowledge.db
```

---

## Environment reference

| var | default | purpose |
|---|---|---|
| `OSS_DOMAIN_FILE` | `./domain.toml` | active product config |
| `OSS_LLM_API_KEY` / `_BASE_URL` / `_MODEL` | — / OpenAI / `gpt-4o` | reasoning LLM |
| `OSS_EMB_API_KEY` / `_BASE_URL` / `_MODEL` / `_DIM` | (LLM creds) / `text-embedding-3-small` / 1536 | embedder (must match the index) |
| `OSS_KNOWLEDGE_DB_PATH` | `./data/knowledge.db` | cortexdb knowledge base |
| `OSS_DB_PATH` | `./data/oss-agent.db` | agent-go session store |
| `OSS_HTTP_ADDR` | `:7634` | serve listen address |
| `OSS_CONV_MEMORY` | `on` | cross-session chat memory |
| `OSS_RATE_LIMIT_PER_MIN` | `30` | per-IP LLM-endpoint cap (0 = off) |
| `OSS_UNDERSTAND_CMD` | — | command to produce knowledge-graph.json |

---

## What's standardized vs per-project

- **Fully automatic, any project**: code→graph (`ingest-repo`), docs→vectors,
  `serve`, the agent itself (domain.toml-driven), deploy (`make deploy`).
- **Finite adapters, pick one**: object-model extraction from a structured source
  (`import-schema`; proto/OpenAPI planned).
- **Irreducible human input**: authoring `domain.toml` (persona + red-lines) and
  choosing the schema source. Everything else is the pipeline above.
