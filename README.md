# oss-agent

A **product-agnostic platform** for building AI ops & support agents over an
open-source project. The engine knows nothing about any specific product — a
"domain" is supplied entirely by a `domain.toml`. LINBIT (DRBD/LINSTOR) is the
first example domain; the same engine serves any storage/infra project.

## What it does

- **Builds a knowledge graph** of a project's source, docs, and internal know-how
  by importing [Understand-Anything](https://github.com/Egonex-AI/Understand-Anything)
  `knowledge-graph.json` (AST + LLM ontology) and/or semantically ingesting prose
  docs/skills — all stored in [cortexdb](https://github.com/liliang-cn/cortexdb)
  (vectors + graph).
- **Answers & diagnoses** via an [agent-go](https://github.com/liliang-cn/agent-go)
  `Agent` (ReAct loop): it calls read-only **probes** and a GraphRAG
  **knowledge_search** tool, grounds answers in the real code/docs, and is guarded
  by a deterministic **red-line wall** that blocks destructive one-liners.
- **Triages logs** (`analyze-log`): any file/dir/archive → de-duplicated, ranked
  findings (generic severities + the domain's `error_patterns`) → optional
  AI root-cause grounded in the knowledge graph.

## Architecture

```
domain.toml ──┐                         ┌── probes (read-only diagnostics)
              ├── engine (product-free) ─┼── knowledge_search (GraphRAG / cortexdb)
repos/docs ───┘                         └── red-line safety wall (deterministic)
                          │
        agent-go Agent (ReAct) ── LLM (OpenAI-compatible) + embedder (e.g. ollama)
```

- `internal/domain`  — loads `domain.toml` (persona, entity/relation types,
  error_patterns, probes, repos, red_lines). The only place a "product" enters.
- `internal/knowledge` — cortexdb wrapper (embed, semantic search, graph upsert).
- `internal/graphimport` — imports Understand-Anything `knowledge-graph.json`.
- `internal/ingest` — semantic text ingest for prose docs/skills.
- `internal/loganalyze` — product-agnostic log triage engine.
- `internal/agents` — wires the agent-go Agent (tools + lint). PTC off for clean
  single-answer Q&A.
- `internal/safety` — the red-line filter (regex, severity → unlock-key gate).
- `internal/extract` — ingest-time LLM ontology extractor (raw LLM, no agent loop).

## Commands

```
oss-agent ask <question>       one-shot Q&A (ReAct: probes + knowledge_search)
oss-agent diagnose <symptom>   same loop, framed for troubleshooting
oss-agent chat                 multi-turn (history kept across turns)
oss-agent analyze-log <path>   triage a log file / dir / .tar.gz / .zip, + AI diagnosis
oss-agent ingest <dir>         ingest *.md docs
oss-agent ingest-repo <url>    clone → understand → import (graph), or text fallback
oss-agent import-graph <f>     import an Understand-Anything knowledge-graph.json
oss-agent search <query>       query the knowledge base directly (no LLM)
oss-agent check <command...>   test a command against the red-line wall
oss-agent domain               print the loaded domain config
```

## Configuration (env)

```
OSS_DOMAIN_FILE     path to the active domain.toml (e.g. examples/linbit/domain.toml)
OSS_LLM_API_KEY     LLM key (OpenAI-compatible)   OSS_LLM_BASE_URL / OSS_LLM_MODEL
OSS_EMB_API_KEY     embedder key                  OSS_EMB_BASE_URL / OSS_EMB_MODEL / OSS_EMB_DIM
OSS_KNOWLEDGE_DB_PATH  cortexdb path (default ./data/knowledge.db)
OSS_UNDERSTAND_CMD  command run in a repo to produce knowledge-graph.json
```

The embedder used to query must match the one used to build the store (e.g. ollama
`embeddinggemma`, 768-dim).

## Building a new domain

1. Write a `domain.toml` (see `examples/linbit/domain.toml`): persona,
   entity/relation types, `error_patterns`, `probes`, `repos`, `red_lines`.
2. Point `OSS_DOMAIN_FILE` at it.
3. Ingest the project's repos (`ingest-repo`) and docs (`ingest-repo` text path).
4. `ask` / `analyze-log` away. No engine code changes.
