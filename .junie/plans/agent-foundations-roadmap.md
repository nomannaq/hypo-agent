---
sessionId: session-261120-005918-inmd
---

# Requirements

### Overview & Goals
Build solid foundations for a Grok-style agent: durable execution, persistent memory, skills, tool discovery. Order work so each layer is testable before the next depends on it.

### Where we are
- **Real:** `kernel.Ctx` contract (Step/Sleep/Await/Spawn/Now/Rand) in `internal/kernel/kernel.go`; migration `20261119221930_create_kernel.sql` (runs, journal, timers, signals); auth + server in `cmd/server`; Temporal + pgvector in `compose.yml`; web chat UI (`web/src/pages/agent.tsx`) with a fake `runAgent()`.
- **Stubs (TODO only):** `internal/agent` (reducer/events), `internal/executor`, `internal/model/nebius.go`, `internal/tool/*`, `internal/memory/pgvector.go`, `internal/runtime/*`, `memkernel`, `pgkernel`, `temporalkernel`, `conformance`. `skills/` and `sandbox/` are empty.

### Scope
In: backend runtime, persistence, API/streaming to the existing chat UI.
Out (for now): multi-user billing, marketplace of skills, sub-agent swarms, sidebar features (Scheduled/Graph/Extensions pages).

### Functional Requirements
- A chat message starts a durable run that survives worker restarts and streams events to the UI.
- Tool calls are resolved from a registry, with side effects guarded by approvals and idempotency.
- The agent recalls prior conversations and facts across sessions.
- Skills in `skills/` are discovered and loaded on demand; tools are discoverable by relevance, not all stuffed into the prompt.

# Technical Design

### Key Decisions
- **Kernel first, but start on memkernel.** Develop the loop against `memkernel` (in-process) so the agent can work end to end early; the conformance suite in `internal/kernel/conformance` then holds `pgkernel`/`temporalkernel` to the same behavior. Recommended production engine: Temporal (already in compose); `pgkernel` stays the learning/fallback path and should not block product work.
- **Pure reducer + effectful executor** (as already documented): `internal/agent` is `State × Event -> State`, `internal/executor` wires it to `kernel.Ctx`, model and tools. Every model/tool call is a `Step`, so it is journaled.
- **Events are the source of truth.** Persist the run's event stream (new `run_events` table); the UI is a projection over it via SSE, reconnectable with `Last-Event-ID`.
- **Memory in three tiers:** working context (compaction), episodic (conversation/run summaries), semantic (pgvector facts). Memory writes are Steps; retrieval is a tool + automatic recall at turn start.
- **Skills as files, tools as registry.** A skill is a folder with `SKILL.md` (frontmatter name/description); only name+description are in the prompt, the body loads via a `read_skill` tool (progressive disclosure). Tool discovery: registry holds tool definitions with embeddings; a `search_tools` meta-tool returns the top-k, and only those schemas are added to the next model call.

### Architecture Diagram
```mermaid
graph LR
  UI[Web chat] -->|POST message / SSE| SRV[cmd/server]
  SRV --> RT[runtime scheduler]
  RT --> K[kernel.Ctx]
  K --> EX[executor]
  EX --> AG[agent reducer]
  EX --> M[model gateway]
  EX --> T[tool registry]
  EX --> MEM[memory pgvector]
  T --> SK[skills]
  T --> SB[sandbox]
  K --> PG[(Postgres journal)]
```

### Proposed Changes (by phase)
1. **Agent core:** define `State`, `Event`, `Reduce` in `internal/agent`; golden tests. Implement `memkernel`. Implement `model.Client` (Nebius, retry/backoff, usage metering). Implement `executor.Run` as a simple loop: model -> tool calls -> results -> repeat, each as `kernel.StepT`.
2. **Wire to UI:** `run_events` migration; server endpoints to start a run and stream events (SSE); replace `runAgent()` in `agent.tsx` with the stream; render tool calls/activity with existing `components/agents`.
3. **Durability:** `temporalkernel` (workflow, steps, continue-as-new, workers in `cmd/worker-*`) passing `conformance`; chaos tests (kill worker mid-run). Then `runtime`: scheduler budgets/caps, supervisor for stuck runs, approval via `Await`.
4. **Tools:** `tool.Registry` (definition, JSON schema, handler, effect type read/write/destructive), idempotency ledger (`runID:seq` key), approval policy per effect, first tools (web fetch, search, file read), sandbox runner using `sandbox/images`.
5. **Memory:** `memory.Store` on pgvector (migration for `memories` with embedding, scope, source run), `Compact(history)`, recall at turn start, `remember`/`recall` tools, run summaries on completion.
6. **Skills & discovery:** skill loader scanning `skills/`, prompt index, `read_skill`; tool embeddings + `search_tools`; one or two real skills (e.g. hypothesis-research) as the proof.
7. **Observability & evals:** per-run trace view from journal, token/cost metering, a small replayable eval set of hypotheses.

### Risks
- Nondeterminism: model output, time and randomness must go through Steps/`Now()`/`Rand()`.
- Tool side effects are not exactly-once; require idempotency keys.
- Context growth: needs compaction before long runs, and continue-as-new on Temporal.
- Scope creep into `pgkernel`; time-box it.

# Testing

### Validation Approach
- Golden tests for the reducer (no I/O).
- Conformance suite run against memkernel, pgkernel, temporalkernel.
- Chaos test: kill the worker mid-run, confirm the run resumes without repeating completed steps.

### Key Scenarios
- Message -> streamed reply with a tool call, shown in the UI.
- Run resumes after worker restart; the approval `Await` survives restart.
- A fact stated in session A is recalled in session B.
- Skill index appears in the prompt; `read_skill` loads the body; `search_tools` returns a relevant tool.

### Edge Cases
- Replay with a changed step input fails loudly.
- SSE reconnect with `Last-Event-ID` gets no duplicates or gaps.
- Budget exceeded halts the run cleanly.

# Delivery Steps

### ✓ Step 1: Agent core: reducer, memkernel, model client, executor loop
A run can execute a model -> tool -> result loop in-process, journaled via memkernel, with golden tests.

- Define State/Event/Reduce in `internal/agent` with golden tests.
- Implement `internal/kernel/memkernel` and run `conformance` against it.
- Implement `internal/model` Nebius client with retry and usage metering.
- Implement `internal/executor.Run` using `kernel.StepT` for every model and tool call.

Done: `make test` (reducer goldens, memkernel conformance, executor crash-resume + guards), `make smoke` (live Nebius chat + tool call). `/api/chat` runs through the executor on a per-request memkernel.

###   Step 2: Tool registry and first tools
The executor resolves tool calls from a registry with effect types, idempotency keys and approvals.

- Implement `internal/tool` registry, effects and idempotency ledger.
- Add approval via `kernel.Ctx.Await` in `internal/runtime/approval.go`.
- Add first tools (web fetch, search) and the sandbox runner using `sandbox/images`.

### ✓ Step 3: Stream runs to the chat UI
The web agent page shows real streamed agent output instead of the fake reply.

- ✓ Add `run_events` migration and event persistence (`events.append` activity, `(run_id, seq)` key, insert trigger → `NOTIFY`).
- ✓ Add server endpoints to start a run and stream via SSE with Last-Event-ID resume; runs are owned by the user who started them.
- ✓ Replace the blocking `/api/chat` in `web/src/pages/agent.tsx`; render model and tool steps with `AgentActivity`; reattach after a reload.
- Later: token-by-token text (a non-durable side channel; activities only return final results), and conversations stored server-side instead of sessionStorage.

### * Step 4: Durable execution on Temporal
Runs survive worker crashes and pass the conformance and chaos tests.

- ✓ Implement `temporalkernel` and wire a worker (one `cmd/worker`; steps are named activities, not closures; continue-as-new deferred). See `durability.md`.
- Implement scheduler budgets/caps and supervisor for stuck runs in `internal/runtime`.
- ✓ Add kill-worker chaos test.

Done so far: `/api/chat` runs on Temporal via `cmd/worker`; conformance passes on both kernels; chaos test plus a manual `kill -9` mid-model-call (resumed on a new worker after the heartbeat timeout). Remaining: scheduler and supervisor.

###   Step 5: Persistent memory
The agent recalls facts and summaries across sessions.

- Add `memories` migration with pgvector embeddings.
- Implement `internal/memory` Store, Compact and recall at turn start.
- Add remember/recall tools and run-completion summaries.

###   Step 6: Skills and tool discovery
Skills and tools are discovered on demand instead of loaded all at once.

- Implement skill loader for `skills/` with prompt index and `read_skill` tool.
- Add tool embeddings and `search_tools` meta-tool that injects only top-k schemas.
- Ship one real hypothesis-research skill as proof.