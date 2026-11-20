# AGENTS.md

Guide for anyone (human or coding agent) working in this repo. Read it before
changing `internal/`, especially the durability and state-machine rules: they
are the parts where a plausible-looking change silently breaks replay.

## What Hypo Agent is

An agent for **hypothesis research and validation**: a user states a hypothesis
in a chat UI, and the agent researches and tests it with a model plus tools.
The goal is a Grok-style agent built on solid foundations, in this order:

1. **Durable execution**: a run survives worker crashes and restarts, resuming
   from where it stopped without repeating completed work.
2. **Persistent memory**: facts and conversation summaries carry across
   sessions (pgvector).
3. **Skills**: capabilities as files in `skills/`, loaded on demand.
4. **Tool discovery**: tools found by relevance instead of all stuffed into
   the prompt.

Each layer is testable on its own before the next depends on it. The phased
plan lives in `.junie/plans/agent-foundations-roadmap.md`.

**Stack:** Go 1.26 backend, Postgres 17 + pgvector, Temporal (durable engine),
Inertia (gonertia) + React 19 + Tailwind 4 frontend, Nebius Token Factory
(OpenAI-compatible API) as the model provider.

## Status

| Area | State |
|---|---|
| `internal/agent`: reducer, events, state machine | **Done**: golden + exhaustive state-machine tests |
| `internal/kernel`: `Ctx` contract | **Done** |
| `internal/kernel/memkernel` | **Done**: passes the conformance suite |
| `internal/kernel/conformance` | **Done**: 8 tests, mutation-checked |
| `internal/model`: Nebius client | **Done**: tool calling, retry/backoff, `OnUsage` metering, live smoke test |
| `internal/executor`: agent loop | **Done**: guards, crash-resume test |
| `cmd/server`: auth, sessions, `/api/chat` | **Done**: chat runs through the executor on a per-request memkernel |
| Web UI: beUI chat shell, orb, messages, activity | **Done**; replies are not streamed yet |
| `internal/tool`, `internal/runtime`, `internal/memory` | Stubs (roadmap Steps 2–5) |
| `pgkernel`, `temporalkernel`, `cmd/worker-*` | Stubs (Step 4). Worker `main`s `panic("TODO")` |
| `skills/`, `sandbox/images/` | Empty (Steps 2 and 6) |

Roadmap Step 1 (agent core) is complete. Next: Step 2 (tool registry) and
Step 3 (stream runs to the UI over SSE).

## Architecture

```
web (React/Inertia) ──POST /api/chat──▶ cmd/server
                                           │
                                   kernel.Ctx (memkernel today, Temporal later)
                                           │
                                   internal/executor  ◀── the only package that
                                   │     │      │          wires everything
                     internal/agent   model   tools
                     (pure reducer)  (Nebius) (registry, Step 2)
```

Target (from the roadmap): the server starts a durable run on the runtime
scheduler; the run's events are persisted (`run_events`) and streamed to the
UI over SSE; memory and skills plug in as tools.

### Request flow today

1. `web/src/pages/agent.tsx` POSTs the whole conversation to `/api/chat`.
2. `cmd/server/chat.go` validates it, builds an `executor.Input` (history +
   prompt), creates a fresh `memkernel`, and runs the `agent` workflow.
3. `executor.Run` folds events through `agent.Reduce`, calling the model (and
   tools, once there are any) as journaled Steps until the state is `done` or
   `failed`.
4. The server returns the last assistant message as `{"reply": ...}`.

Nothing is persisted yet: a fresh kernel per request means a server crash
loses the turn. That is expected until Step 4.

## Repo map

```
cmd/
  server/          HTTP + Inertia app: auth, sessions, settings, /api/chat, `seed` subcommand
  migrate/         goose CLI (migrations embedded from db/)
  worker-workflow/ Temporal workflow worker (stub)
  worker-activity/ Temporal activity worker (stub)
internal/
  agent/           pure State × Event -> State; no I/O imports
  kernel/          the durability contract (Ctx, Future, StepT, sentinel errors)
    memkernel/     in-memory implementation (tests, dev, per-request runs)
    pgkernel/      Postgres-journal implementation (stub, learning path)
    temporalkernel/ Temporal adapter (stub, production path)
    conformance/   one suite every kernel must pass
  executor/        agent loop: reducer + kernel + model + tools, with guards
  model/           Nebius client: tool calling, retries, usage metering
  stepreg/         name -> step function registry (for Temporal activities)
  tool/ runtime/ memory/   stubs for roadmap Steps 2-5
db/migrations/     goose SQL migrations (users/sessions, pgkernel tables)
web/               Vite + React frontend (see "Frontend" below)
skills/ sandbox/   future skills and tool sandbox images
.junie/plans/      the roadmap
```

## Durability rules (read before touching `internal/`)

Everything durable goes through `kernel.Ctx` (`internal/kernel/kernel.go`).
A run is replayed from its journal after a crash: every journaled call
returns its recorded result instead of running again. That only works if the
workflow code is **deterministic**: the same journal must produce the same
sequence of calls.

- **Every side effect is a Step.** Model calls, tool calls and anything with
  I/O go inside `c.Step` / `kernel.StepT`. Code outside a Step re-runs on every
  replay.
- **Never call `time.Now()`, `rand`, or read the environment in workflow
  code.** Use `c.Now()` and `c.Rand()`, which are journaled. Inside a Step body
  it's fine: the body runs once and its result is recorded.
- **No goroutines in workflow code.** Parallelism goes through `c.Spawn`
  (child runs), so the kernel can journal it.
- **Step names must be deterministic and unique per run.** The executor uses
  `model/<turn>` and `tool/<turn>/<callID>` (model call IDs can repeat across
  turns, so the turn is part of the name).
- **Changing code under a live run is a nondeterminism bug.** Replay that
  reaches a journal entry recorded by a different call fails with
  `kernel.ErrNondeterminism`. Fail loudly; never paper over it.
- **A cancelled run is a crash, not a failure.** If the run's context ends
  mid-Step, nothing is journaled, so the next run re-executes that Step.
  `executor.Run` returns kernel errors (cancellation, nondeterminism) as
  errors; agent-level failures (model error, guard tripped) end the run in the
  `failed` state with a nil error.
- **The journal records results, not side effects.** A crash between a side
  effect and its journal write repeats the effect. External writes need an
  idempotency key (`runID:seq`); that's the tool ledger in Step 2.
- **Temporal adds two constraints** (Step 4): Step bodies must be registered
  activities (dispatched by name through `internal/stepreg`), and long runs
  need Continue-As-New (`kernel.ContinueAsNewer`, a no-op on other kernels).

### Conformance suite

`internal/kernel/conformance` is the definition of correct kernel behaviour:
step replay, crash-mid-step resume, nondeterminism detection, stable
`Now`/`Rand`, signals (including ones sent before `Await`), timeouts, sleep
skipped on replay, and spawned children. Every kernel calls
`conformance.Run` from its own `_test.go`. A new kernel is done when this
suite passes, not before. The suite has been checked against a deliberately
broken kernel (replay disabled), which fails 5 of the 8 tests.

## The agent state machine

`internal/agent` is the pure core of the agent. A run's `State` is always
`Reduce` folded over its `Event`s, which is what makes events safe to persist,
replay, and stream to the UI.

### States and events

| Status | Meaning |
|---|---|
| `idle` | waiting for a user message |
| `thinking` | needs a model call |
| `acting` | has tool calls waiting for results |
| `done` | the model answered without tool calls |
| `failed` | ended by a `failed` event; terminal |

| Event | Carries |
|---|---|
| `user_message` | `Text` (the prompt) |
| `model_reply` | `Text` and/or `ToolCalls`, `Usage` |
| `tool_result` | `CallID`, `Text` (output), `IsError` |
| `failed` | `Err` (why the run ended) |

### Transitions

The whole machine is the `transitions` table in `internal/agent/reducer.go`:

```
idle     --user_message-->  thinking
thinking --model_reply--->  acting   (the reply has tool calls)
                            done     (it doesn't)
acting   --tool_result--->  acting   (calls still pending)
                            thinking (that was the last one)
done     --user_message-->  thinking (the next turn)
any but failed --failed-->  failed   (terminal)
```

`Reduce` checks the table before doing anything and refuses any
(status, event) pair it doesn't list. The `switch` inside `Reduce` only
decides **which** of the allowed statuses an event lands in and updates the
data; before returning, `Reduce` checks its own result against the table.
So the code can't drift from the table without an error. `Reduce` never
mutates its input.

### Explicit enums

Go has no closed enums: any `uint8` converts to a `Status`. `Status` and
`EventKind` close the set by hand, and new enums in this repo should follow
the same pattern:

1. A `uint8` type with `iota` constants and a trailing unexported count
   (`numStatuses`, `numEventKinds`).
2. A `[...]string` names array keyed by the constants, plus a compile-time
   check `var _ [numStatuses]string = statusNames`, so adding a value without
   a name fails to build.
3. `Valid()`, `String()`, `MarshalText()`, `UnmarshalText()`. JSON stays
   human-readable (`"done"`, `"tool_result"`), and unknown values are
   rejected both when reading and writing.

### Changing the machine

1. Add or change the constant (and its name) if it's a new status or event.
2. Edit the `transitions` table, including the diagram in its doc comment.
3. Update the matching `case` in `Reduce`.
4. Run `go test ./internal/agent`. `machine_test.go` tries every status ×
   every event and fails if Reduce and the table disagree, if a table entry is
   unreachable, if a non-terminal status can't fail, or if `failed` accepts
   anything. Add a representative state there for any new status.
5. If the serialized state legitimately changed, regenerate goldens with
   `go test ./internal/agent -update` and review the diff.

Events will be persisted (`run_events`, Step 3), so renaming an event's or a
status's string is a data migration, not a refactor.

## Executor loop and guards

`internal/executor` drives one turn: model → tool calls → results → model …
until the model answers without tools or a guard trips. It is the only
package that imports both `agent` and `kernel`.

Guards (`executor.Limits`, defaults in `DefaultLimits`): max model turns, a
token budget, max tool calls per model reply, a cap on tool output kept in
context, and a timeout per Step. A tripped guard ends the run in `failed`
with a reason; it is never a Go error.

Tool failures (unknown tool, malformed arguments, handler error, timeout) go
back to the model as an error result so it can recover; they don't end the
run.

## Model gateway

`internal/model/nebius.go` speaks the OpenAI-compatible chat completions API
at Nebius Token Factory. `Complete` supports tool calling, retries network
errors, 429s and 5xx with exponential backoff and full jitter (honouring
`Retry-After`), and calls `OnUsage` once per successful call for metering.
`Chat` is `Complete` without tools. The default model is
`deepseek-ai/DeepSeek-V4.1-Flash-1017`; override with `NEBIUS_MODEL`.

## Commands

```bash
make up                 # Postgres (pgvector) + Temporal + Temporal UI
make down
make migrate            # run pending migrations
make migration name=x   # new migration file in db/migrations
make rollback / status
make goose cmd=reset    # any goose command (loads .env; plain `go run` doesn't)
make seed               # test@example.com / password
make fresh              # reset + migrate + seed (wipes the dev DB)
make dev                # Vite + Go server with hot reload (air) → localhost:8080
make build              # build the frontend into public/build
make serve              # Go server only, built assets
make test               # go test -race ./...
make smoke              # live Nebius call (chat + tool call); needs NEBIUS_TOKEN_FACTORY

go test ./internal/agent -update    # regenerate reducer goldens
cd web && npm run typecheck && npm run lint
```

## Environment and ports

`.env` (copy `.env.example`): `POSTGRES_*`, `DATABASE_URL`,
`NEBIUS_TOKEN_FACTORY` (required for the model), optional `NEBIUS_MODEL` and
`NEBIUS_BASE_URL`. The server also loads `.env` itself when started without
`make` (`cmd/server/dotenv.go`); `go test` does not.

| Port | What |
|---|---|
| 8080 | Go server (`make dev` / `make serve`) |
| 127.0.0.1:5174 | Vite dev server (not 5173: DDEV's router holds 127.0.0.1:5173 on this machine) |
| 5432 | Postgres |
| 7233 | Temporal frontend (gRPC) |
| 8088 | Temporal UI (not 8080, which is the Go server) |

## Frontend

- **Inertia v3 client on a v2 server.** gonertia renders the v2
  `<div data-page>` markup; `web/src/app.tsx` reads that JSON and passes it to
  `createInertiaApp` as `page`. Request and response headers are compatible.
- **Pages** live in `web/src/pages`, resolved by name (`app.Render(w, r,
  "agent")` → `pages/agent.tsx`). Layouts are chosen by page name in `app.tsx`:
  `auth/*` → auth layout, `settings/*` → agent shell + settings layout,
  everything else → `layouts/agent-layout.tsx` (beUI `ChatApp` +
  `AnimatedSidebar` + `AISidebar`).
- **Routes**: `web/src/routes/` is a hand-written stand-in for Laravel
  Wayfinder (`agent()`, `store.form()`, …). Add a route there whenever you add
  one in `cmd/server/main.go`.
- **Dev assets**: Vite writes `public/hot`; the server checks for it on every
  render (`reactRefresh` in `cmd/server/main.go`), so start order doesn't
  matter.
- **UI components**: `web/src/components/agents` and `components/motion` are
  vendored from beui.dev (install with `npx shadcn@latest add @beui/<name>`);
  `components/ui` is shadcn. Vendored folders are excluded from ESLint.

## Conventions

- **Lean code.** No interface with a single implementation (test fakes
  count as a second), no config for values that never change, stdlib before
  dependencies. A deliberate shortcut with a known ceiling gets a
  `ponytail:` comment naming the ceiling and the upgrade path.
- **Package boundaries.** `internal/agent` imports nothing effectful. Only
  `internal/executor` joins agent, kernel, model and tools. Nothing outside
  `internal/kernel` imports an engine (Temporal SDK, pgx journal code)
  directly.
- **Tests that prove something.** The reducer has goldens and an exhaustive
  state-machine test; kernels have the conformance suite; the executor has a
  crash-resume test (the model is not called again for a completed turn).
  When adding a safety test, make it fail once against a deliberately broken
  implementation before trusting it.
- **Migrations** are goose SQL files with `-- +goose Up` / `-- +goose Down`.
  Create them with `make migration name=...`; never edit one that has been
  applied anywhere shared.
- **Comments** explain why, not what; match the density of the surrounding
  code.

## Gotchas

- **Re-installing beUI components** can revert
  `web/src/components/motion/text-shimmer.tsx` to an upstream bug (it imports
  from itself). The fixed line imports from `@/lib/text-shimmer` and carries a
  comment saying so.
- **`go run ./cmd/migrate`** without `make` doesn't load `.env`; use
  `make goose cmd=...`.
- **Model replies render raw markdown** in the chat UI for now (`**bold**`,
  `-` lists).
- **Run test servers on another port** (e.g. `PORT=8091`) when `make dev` is
  already running; don't kill processes by name.
