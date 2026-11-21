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
| `internal/kernel/temporalkernel` | **Done**: passes the same suite on a Temporal dev server, plus a worker-loss chaos test |
| `internal/kernel/conformance` | **Done**: 8 tests, run against both kernels, mutation-checked |
| `internal/model`: Nebius client | **Done**: tool calling, retry/backoff, `OnUsage` metering, live smoke test |
| `internal/executor`: agent loop | **Done**: guards, crash-resume test |
| `cmd/server`: auth, sessions, runs | **Done**: `POST /api/runs` starts a durable run; `GET /api/runs/{id}/events` streams its events (SSE) |
| `cmd/worker` | **Done**: runs agent workflows and activities, persists run events; survives being killed mid-run |
| Web UI: beUI chat shell, orb, messages, activity | **Done**: follows runs step by step, reattaches after a reload |
| `internal/tool` | **Registry, effects, tools**: `web_search` (SearXNG); `browse`, `inspect_forms`, `submit_form` (Obscura headless browser). `submit_form` is a Write tool: approved by the user, run once via the ledger. Sandbox is a stub |
| Approvals, idempotency ledger | **Done**: `waiting` state, approval card in the UI, `POST /api/runs/{id}/approvals`, `tool_ledger` table; tested on memkernel and Temporal |
| `internal/runtime`, `internal/memory` | Stubs (roadmap Steps 4–5) |
| `pgkernel` | Stub (learning path, time-boxed) |
| Run scheduler, supervisor (rest of Step 4) | Stubs in `internal/runtime` |
| `skills/`, `sandbox/images/` | Empty (Step 6, and the sandbox runner) |

Roadmap Steps 1 (agent core), 2 (tool registry, web tools, approvals and the
idempotency ledger) and 3 (streaming runs to the UI), and the durable-execution
part of Step 4, are complete (see `.junie/plans/durability.md`). Next: the rest
of Step 4 (scheduler, supervisor) or Step 5 (memory).

## Architecture

```
web (React/Inertia) ──POST /api/runs──▶ cmd/server ──start workflow──▶ Temporal
        ▲                                   ▲                          │
        └──SSE /api/runs/{id}/events────────┤ LISTEN run_events        │
                                                         cmd/worker (polls hypo-agent)
                                                                       │
                                               kernel.Ctx = temporalkernel
                                                                       │
                                   internal/executor  ◀── the only package that
                                   │     │      │          wires everything
                     internal/agent   model   internal/tool ──▶ SearXNG (web_search)
                     (pure reducer)  (Nebius)  (registry)    ──▶ Obscura (browse, CDP)
```

Each run's events land in `run_events` (written by the worker's
`events.append` activity) and reach the UI over SSE. Target (from the roadmap):
runs go through the runtime scheduler; memory and skills plug in as tools.

### Request flow today

1. `web/src/pages/agent.tsx` POSTs the whole conversation to `/api/runs`.
2. `cmd/server/runs.go` validates it, records the run in `agent_runs` (owned
   by the user), starts the `agent` workflow on Temporal (task queue
   `hypo-agent`, workflow ID = run ID `run-<random>`), and returns
   `{"id": ...}` without waiting.
3. The page opens `GET /api/runs/{id}/events` (an `EventSource`).
4. `cmd/worker` runs it: `executor.Run` folds events through `agent.Reduce`,
   calling the model (and tools, once there are any) as activities until the
   state is `done` or `failed`. After every event it calls the
   `events.append` activity, which inserts `(run_id, seq, event)` into
   `run_events`; an insert trigger sends `NOTIFY run_events`.
5. The server holds one `LISTEN run_events` connection (`hub` in `runs.go`)
   and wakes the run's streams, which re-read the table and send new events as
   `id: <seq>` / `data: <agent.Event>`. Each stream folds what it sends
   through `agent.Reduce` and closes with `event: end` once the run is `done`
   or `failed`.

The run lives on the worker, not in the request. If the worker dies mid-call,
the activity's heartbeat stops, Temporal notices within the heartbeat timeout
(20s) and retries the call on another worker; completed calls are replayed
from history, not repeated. If the browser disconnects, `EventSource`
reconnects with `Last-Event-ID` and gets only what it missed; after a reload
the page restores the conversation from `sessionStorage` and reattaches to
runs still in progress. If a run ends without a final event (terminated,
nondeterminism, Postgres down past the append retries), the stream notices on
its next 15s ping (it asks Temporal) and sends `event: error`. `memkernel` is
now used only in tests.

## Repo map

```
cmd/
  server/          HTTP + Inertia app: auth, sessions, settings, runs + SSE, `seed` subcommand
  migrate/         goose CLI (migrations embedded from db/)
  worker/          Temporal worker: the agent workflow + its activities
internal/
  agent/           pure State × Event -> State; no I/O imports
  kernel/          the durability contract (Ctx, Activity, Call, sentinel errors)
    memkernel/     in-memory implementation (tests)
    pgkernel/      Postgres-journal implementation (stub, learning path)
    temporalkernel/ Temporal adapter (production): conformance + chaos tests
    conformance/   one suite every kernel must pass
  executor/        agent loop: reducer + kernel + model + tools, with guards
  model/           Nebius client: tool calling, retries, usage metering
  dotenv/          .env loader for binaries started without make
  tool/            tool registry + read-only tools (web_search, browse)
  runtime/ memory/ stubs for roadmap Steps 4-5
db/migrations/     goose SQL migrations (users/sessions, pgkernel tables, agent_runs/run_events)
web/               Vite + React frontend (see "Frontend" below)
docker/            config for compose services (searxng/settings.yml)
skills/ sandbox/   future skills and tool sandbox images
.junie/plans/      the roadmap
```

## Durability rules (read before touching `internal/`)

Everything durable goes through `kernel.Ctx` (`internal/kernel/kernel.go`).
A run is replayed from its journal after a crash: every journaled call
returns its recorded result instead of running again. That only works if the
workflow code is **deterministic**: the same journal must produce the same
sequence of calls.

- **Every side effect is an activity.** Model calls, tool calls and anything
  with I/O are registered activities, called from workflow code with
  `kernel.Call(c, step, activity, input)`: a named
  `kernel.Activity[In, Out]` with explicit, JSON-serializable input. Closures
  are not steps: Temporal can only run registered activities, and a closure
  can't carry its input across processes. Code outside an activity re-runs on
  every replay.
- **Register what the workflow calls.** Whoever runs a workflow (a kernel in
  tests, the worker in production) registers its activities too: the executor
  exposes them as `Runner.Activities()`.
- **Never call `time.Now()`, `rand`, or read the environment in workflow
  code.** Use `c.Now()` and `c.Rand()`, which are journaled. Inside an activity
  body it's fine: the body runs once and its result is recorded.
- **`kernel.Ctx` is not a `context.Context`.** Workflow code can't use Go
  context semantics (they don't replay, and Temporal's workflow context isn't
  one). Activity bodies get a real `context.Context`.
- **No goroutines in workflow code.** Parallelism goes through `c.Spawn`
  (child runs), so the kernel can journal it.
- **Step names must be deterministic and unique per run.** The executor uses
  `model/<turn>`, `tool/<turn>/<callID>` and `event/<seq>` (model call IDs can repeat across
  turns, so the turn is part of the name).
- **Changing code under a live run is a nondeterminism bug.** Replay that
  reaches a journal entry recorded by a different call fails with
  `kernel.ErrNondeterminism`. Fail loudly; never paper over it.
- **A cancelled run is a crash, not a failure.** If the run stops mid-call
  (worker lost, shutdown), nothing is journaled, the call returns an error
  wrapping `kernel.ErrCanceled`, and the next run re-executes it.
  `executor.Run` returns kernel errors (`ErrCanceled`, `ErrNondeterminism`) as
  errors; agent-level failures (model error, guard tripped) end the run in the
  `failed` state with a nil error.
- **Activity names are part of the journal.** Renaming an activity (or a step
  name pattern) breaks replay of runs recorded under the old name.
- **The journal records results, not side effects.** A crash between a side
  effect and its journal write repeats the effect. External writes need an
  idempotency key (`runID:seq`): `events.append` writes `(run_id, seq)` with
  `ON CONFLICT DO NOTHING`; Write tools go through `Runner.Ledger` (the
  `tool_ledger` table, keyed `<runID>/tool/<turn>/<callID>`), so a retried
  activity whose effect already happened returns the recorded output. The
  window that remains is a crash between the effect and the ledger insert;
  only an idempotency key the external system honours closes it.
- **Long runs on Temporal need Continue-As-New** (`kernel.ContinueAsNewer`, a
  no-op on other kernels). Deferred while a run is a single turn; see
  `.junie/plans/durability.md`.

### Conformance suite

`internal/kernel/conformance` is the definition of correct kernel behaviour:
call replay, resume after losing the worker mid-call (`Harness.Restart`),
nondeterminism detection, stable
`Now`/`Rand`, signals (including ones sent before `Await`), timeouts, sleep
skipped on replay, and spawned children. Every kernel calls
`conformance.Run` from its own `_test.go`. A new kernel is done when this
suite passes, not before.

It has been checked against deliberately broken kernels: memkernel with replay
disabled fails 5 of the 8 tests; temporalkernel with an unjournaled `Now`, an
unjournaled `Rand` seed, or no step-name activity ID each fails the intended
test.

On Temporal the suite runs on a hermetic dev server
(`testsuite.StartDevServer`; the CLI is downloaded on first run, and the tests
skip with a message if it can't start). Re-running a finished run replays its
history against the current code and returns that code's own result.

`internal/kernel/temporalkernel/chaos_test.go` runs the real executor on
Temporal and stops the worker mid-tool-call; a new worker must finish the run
without calling the model again for the completed turn.

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
| `waiting` | a Write tool call waits for the user's approval (`Awaiting` = its call ID) |
| `done` | the model answered without tool calls |
| `failed` | ended by a `failed` event; terminal |

| Event | Carries |
|---|---|
| `user_message` | `Text` (the prompt) |
| `model_reply` | `Text` and/or `ToolCalls`, `Usage` |
| `tool_result` | `CallID`, `Text` (output), `IsError` |
| `failed` | `Err` (why the run ended) |
| `approval_requested` | `CallID` (a pending Write call) |
| `approval_decided` | `CallID`, `Approved`, `Text` (why not, if declined) |

### Transitions

The whole machine is the `transitions` table in `internal/agent/reducer.go`:

```
idle     --user_message-->  thinking
thinking --model_reply--->  acting   (the reply has tool calls)
                            done     (it doesn't)
acting   --tool_result--->  acting   (calls still pending)
                            thinking (that was the last one)
acting   --approval_requested--> waiting (a call with side effects)
waiting  --approval_decided--->  acting  (approved or not; then its tool_result)
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

Events are persisted (`run_events`) and replayed by the stream, so renaming
an event's or a status's string is a data migration, not a refactor.

## Executor loop and guards

`internal/executor` drives one turn: model → tool calls → results → model …
until the model answers without tools or a guard trips. It is the only
package that imports both `agent` and `kernel`.

Guards (`executor.Limits`, defaults in `DefaultLimits`): max model turns, a
token budget, max tool calls per model reply, a cap on tool output kept in
context, and a timeout per model or tool call. A tripped guard ends the run in `failed`
with a reason; it is never a Go error.

Tool failures (unknown tool, malformed arguments, handler error, timeout) go
back to the model as an error result so it can recover; they don't end the
run.

## Tools

`internal/tool.Registry` is a slice of `tool.Tool` (name, description, JSON
Schema, handler) and implements `executor.Tools`; `tool.Typed` builds a tool
whose handler gets its arguments decoded. The worker builds the registry
(`cmd/worker/main.go`). The executor reads `Specs()` in workflow code, so the
registry must be the same on every worker and must not change under live runs.

| Tool | Backend | Notes |
|---|---|---|
| `web_search` | SearXNG JSON API (`SEARXNG_URL`) | `{query, limit}`; limit defaults to 5, capped at 10 |
| `browse` | Obscura over CDP (`OBSCURA_URL`, `OBSCURA_CDP_TOKEN`) | `{url}`, http(s) only; fresh browser context per call; returns title, final URL, Markdown |
| `inspect_forms` | Obscura | `{url}`; lists each form's method, action and fields (name, type, label, options) |
| `submit_form` | Obscura | **Write.** `{url, form, fields}`; fills the fields, calls `requestSubmit()`, returns the resulting page |

Each tool has an `Effect` (`tool.Read` or `tool.Write`, an explicit enum in
`internal/tool/effects.go`). Read tools are safe to repeat. Every browser
call opens a fresh browser context, so calls share no state. Obscura's SSRF
guard refuses private and loopback addresses, which keeps pages away from the
database and Temporal on the compose network. Tool output is untrusted (a
page can contain instructions); the system prompt tells the model to treat it
as data. Outputs over `MaxToolOutput` (16 KB) are truncated by the executor.

To add a tool: write a `tool.Typed(...)` constructor in `internal/tool`, add a
test (an `httptest` fake for HTTP backends, a gated `TestSmoke*` for the live
service), and add it to the registry in `cmd/worker`. If it changes anything
outside the run, set `Effect = tool.Write`; the approval and ledger come with it.

### Approvals

A Write call never runs without the user's answer
(`internal/executor/executor.go`, the `Acting` case):

1. The executor appends `approval_requested` (event seq N); the run is
   `waiting`.
2. It calls `c.Await(executor.ApprovalSignal(N), ApprovalTimeout)` (10 min).
   On Temporal that's a durable wait: no worker is busy, and it survives
   restarts.
3. The UI renders a `ToolApproval` card (beUI) with the call's URL and fields.
   Allow/Deny POSTs `/api/runs/{id}/approvals {call_id, approved}`. The server
   checks that the run is the user's and that its latest event is that
   `approval_requested` (otherwise 409), then signals `approval/N` with an
   `executor.Approval` JSON payload.
4. The executor appends `approval_decided`. If approved, the tool runs through
   the ledger. If declined, it times out, or the payload is unreadable, the
   model gets an error result ("Not performed: …") and the tool never runs.

Tests: `TestWriteToolRunsOnlyWhenApproved` (approve, decline, timeout),
`TestLedgerStopsARetriedWriteFromRepeating`, and
`TestApprovalSignalOnTemporal`, which signals the way the server does. All
were checked against a deliberately removed gate or ledger.

## Model gateway

`internal/model/nebius.go` speaks the OpenAI-compatible chat completions API
at Nebius Token Factory. `Complete` supports tool calling, retries network
errors, 429s and 5xx with exponential backoff and full jitter (honouring
`Retry-After`), and calls `OnUsage` once per successful call for metering.
`Chat` is `Complete` without tools. The default model is
`deepseek-ai/DeepSeek-V4.1-Flash-1017`; override with `NEBIUS_MODEL`.

## Commands

```bash
make up                 # Postgres (pgvector), Temporal + UI, Obscura, SearXNG
make down
make migrate            # run pending migrations
make migration name=x   # new migration file in db/migrations
make rollback / status
make goose cmd=reset    # any goose command (loads .env; plain `go run` doesn't)
make seed               # test@example.com / password
make fresh              # reset + migrate + seed (wipes the dev DB)
make dev                # Vite + worker + Go server, all hot-reloading → localhost:8080 (needs make up)
make build              # build the frontend into public/build
make serve              # Go server only, built assets
make worker             # agent worker only (needs Temporal)
make test               # go test -race ./...
make smoke              # live Nebius call + web_search/browse on the compose services

go test ./internal/agent -update    # regenerate reducer goldens
cd web && npm run typecheck && npm run lint
```

## Environment and ports

`.env` (copy `.env.example`): `POSTGRES_*`, `DATABASE_URL`,
`NEBIUS_TOKEN_FACTORY` (required by the worker, which makes the model calls),
`OBSCURA_CDP_TOKEN` (required by `make up` and the worker; `openssl rand -hex
32`), optional `NEBIUS_MODEL`, `NEBIUS_BASE_URL`, `TEMPORAL_ADDRESS` (default
`localhost:7233`), `OBSCURA_URL` (default `http://localhost:9222`) and
`SEARXNG_URL` (default `http://localhost:8888`). The server and worker also load `.env` themselves when
started without `make` (`internal/dotenv`); `go test` does not.

| Port | What |
|---|---|
| 8080 | Go server (`make dev` / `make serve`) |
| 127.0.0.1:5174 | Vite dev server (not 5173: DDEV's router holds 127.0.0.1:5173 on this machine) |
| 5432 | Postgres |
| 7233 | Temporal frontend (gRPC) |
| 8088 | Temporal UI (not 8080, which is the Go server) |
| 127.0.0.1:9222 | Obscura CDP (browse tool; needs the bearer token) |
| 127.0.0.1:8888 | SearXNG (web_search tool) |

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
- **Replies arrive per step, not per token.** An activity only returns its
  final result, so the reply appears when the model call completes. Token
  streaming would be a separate, non-durable channel.
- **Runs started before `events.append` existed** fail replay with a
  nondeterminism error (their histories have no event activities). Dev only;
  terminate them in the Temporal UI.
- **Some sites serve bots an error page** (nature.com returned "Server
  Error" to Obscura). It comes back as page content, not a tool error; the
  model usually moves on to another source.
- **Keep one `OBSCURA_CDP_TOKEN` line in `.env`.** With two, Compose gives
  Obscura the last one, but a `make dev` started before the second line keeps
  the first in its environment (the environment beats `.env`), and every
  browser call gets a 401. After changing the token, run `make up` and
  restart `make dev`.
- **Docker Compose needs `OBSCURA_CDP_TOKEN`** for every command (even
  `docker compose exec db ...`), since compose.yml interpolates it. Keep it in
  `.env`.
- **Run test servers on another port** (e.g. `PORT=8091`) when `make dev` is
  already running; don't kill processes by name.
