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
| `internal/tool` | **Registry and effects**: web (`web_search`, `browse`), a per-run browser session (`browser_open`, `_snapshot`, `_click`, `_type`, `_select`, `_press`, `_scroll`, `_back`, `_wait`, `_screenshot`), run tools (`todo_write`, `ask_user`, `research` subagents), and memory tools. Sandbox is a stub |
| Approvals, questions, ledger | **Done**: `waiting` state, approval card and question card in the UI, `POST /api/runs/{id}/approvals`, `tool_ledger` table; tested on memkernel and Temporal. `remember` and `schedule` are Write. Browser actions are autonomous by choice |
| Subagents, budgets, artifacts | **Done**: `research` spawns child runs (Temporal child workflows); per-run tool budgets; screenshots in `run_artifacts`, served by `GET /api/runs/{id}/artifacts/{artifact}` |
| `internal/runtime` | Durable run admission, per-user caps, reconciliation, and one-shot and recurring schedules |
| `internal/memory` | Owner-scoped notes, transcript search, opt-in summaries, and optional pgvector recall |
| `pgkernel` | Stub (learning path, time-boxed) |
| `skills/` | Embedded catalog and `hypothesis-research`, loaded by `read_skill` |
| `sandbox/images/` | Empty (sandbox runner) |

Roadmap Steps 1 (agent core), 2 (tool registry, web and browser tools,
approvals, questions, subagents, budgets and the idempotency ledger) and 3
(streaming runs to the UI), and the durable-execution part of Step 4, are
complete (see `.junie/plans/durability.md`). Next: the rest
of the [memory, skills, and scheduling plan](.junie/plans/memory-skills-and-scheduling.md).

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
`events.append` activity) and reach the UI over SSE. The runtime scheduler
admits and supervises runs. The memory tools are available to new chat runs.

### Request flow today

1. `web/src/pages/agent.tsx` posts a prompt, a request ID, and the selected
   conversation to `POST /api/runs`. The first turn creates the conversation.
   Legacy clients can send their message history.
2. `internal/runtime` commits the queued run and returns its ID. The server's
   dispatcher claims it in Postgres, saves the workflow input assembled from
   completed turns, and starts its registered workflow on Temporal (task queue
   `hypo-agent`, workflow ID = application run ID). Admission bounds each user
   to 10 queued and two active runs, with one active run per conversation.
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
the page restores the conversation from Postgres and reattaches to queued
and active runs. Old tab-local chats can still be opened from `/agent`.
The supervisor checks Temporal independently of SSE. A final event and its
terminal runtime state commit together. When a workflow closes without a
final event, the supervisor releases its slot and the stream sends
`event: error`. `memkernel` is used only in tests.

## Repo map

```
cmd/
  server/          HTTP + Inertia app: auth, sessions, settings, runs + SSE, `seed` subcommand
  migrate/         goose CLI (migrations embedded from db/)
  worker/          Temporal worker: the agent workflow + its activities
  browser-sessions/ Local service that keeps live tabs across worker restarts
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
  tool/            tool registry, web + browser-session tools, run tools (todo, ask_user, research)
  runtime/         run admission, lifecycle, and supervisor
  memory/          owner-scoped memory, transcript search, and background projections
db/migrations/     goose SQL migrations (users/sessions, pgkernel, agent_runs/run_events, tool_ledger, run_artifacts)
web/               Vite + React frontend (see "Frontend" below)
docker/            config for compose services (searxng/settings.yml)
skills/           repository skill files, embedded catalog, and read_skill
sandbox/          future tool sandbox images
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
| `approval_decided` | `CallID`, `Approved`, `Text` (why not, if declined; the answer, for an `ask_user` question) |

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
| `browse` | Obscura over CDP (`OBSCURA_URL`, `OBSCURA_CDP_TOKEN`) | `{url}`; reads a page in a fresh browser context: title, URL, Markdown. Stateless, so subagents use it freely |
| `browser_open` | Obscura | `{url}`; opens the run's session page (replacing any open one) |
| `browser_snapshot` | Obscura | numbered interactive elements (`[3] button "Send"`, current values), then up to 8 KB of page Markdown |
| `browser_click` / `_type` / `_select` / `_press` | Obscura | act on an element number; `_type` uses CDP `Input.insertText` (+ `submit` presses Enter); `_press` sends real CDP key events |
| `browser_scroll` / `_back` / `_wait` | Obscura | down/up/top/bottom; history back; wait (≤20 s) for text to appear |
| `browser_screenshot` | Obscura + `run_artifacts` | PNG stored per run, shown in the chat; the model only gets `[artifact:<id>]` |
| `todo_write` | none | the agent's plan; the chat renders the latest list (beUI `TodoList`) |
| `ask_user` | executor | pauses the run for the user's answer (see Approvals) |
| `research` | executor | up to 4 parallel subagents (see Subagents) |

Every action tool reports where the page is afterwards, following any
navigation it started (`page.settle`: a `frameNavigated` within a second,
then its `loadEventFired`). Obscura's SSRF guard refuses private and loopback
addresses, which keeps pages away from the database and Temporal on the
compose network. Tool output is untrusted (a page can contain instructions);
the system prompt tells the model to treat it as data. Outputs over
`MaxToolOutput` (16 KB) are truncated by the executor.

**Browser sessions** (`internal/tool/session.go`): the `browser-sessions`
service holds one live page per run and keeps the original CDP connection
open when an agent worker restarts. All workers for the same environment
use the same service endpoint. Run IDs and call keys come from the executor.
The service serializes calls per run and keeps receipts so an activity
retry can retrieve the result of an accepted action after losing its worker.
Idle tabs and receipts expire after about 10 minutes. Restarting the
session service or Obscura closes the affected tabs. The agent can then
open the page again. Browser commands run within the tool activity and
session service, outside workflow replay.

Each tool has an `Effect` (`tool.Read` or `tool.Write`, an explicit enum in
`internal/tool/effects.go`). Browser actions are Read by choice: the agent acts
on sites without asking. Set `Effect = tool.Write` on a tool to make every call
wait for approval and go through the ledger.

**Budgets** (`Limits.Budgets`, by tool name): `web_search` 20, `browse` 25,
`browser_open` 25, `research` 2 per run. Counted from the run's own messages,
so they are durable and need no worker state; a call over budget gets an error
result telling the model to work with what it has.

To add a tool: write a `tool.Typed(...)` constructor in `internal/tool`, add a
test (an `httptest` fake for HTTP backends, a gated `TestSmoke*` for the live
service), and add it to the registry in `cmd/worker`.

### Subagents

`research {tasks: [{title, prompt}]}` is carried out in workflow code
(`Runner.research`): one `c.Spawn` per task (Temporal child workflows
`<run>/<n>` on the same queue, so they run in parallel), then each child's
final answer under its title. A child is a full agent run with
`Input.Subagent` set: it is offered no `ask_user`, `research` or Write tools,
its events aren't streamed (the parent's tool result reports it), and its
screenshots and ledger entries belong to the top-level run. Each child has its
own turn limit and budgets.

### Approvals and questions

A Write call never runs without the user's answer, and an `ask_user` question
waits for one; both use the same pause (`internal/executor/executor.go`, the
`Acting` case):

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
5. For `ask_user` the UI shows a question card (options as buttons, a free
   answer, Skip) and POSTs `{call_id, approved: true, answer}`; the answer is
   `approval_decided.Text` and the tool result. Skipped or timed out, the
   model is told to continue without it.

Tests: `TestWriteToolRunsOnlyWhenApproved` (approve, decline, timeout),
`TestAskUserWaitsForTheAnswer`, `TestLedgerStopsARetriedWriteFromRepeating`,
`TestBudgetStopsRepeatedCalls`, `TestResearchRunsSubagents` (and
`TestResearchSubagentsOnTemporal`), and `TestApprovalSignalOnTemporal`, which
signals the way the server does. Each was checked against a deliberately
removed gate, ledger, budget or subagent filter. `TestSmokeBrowserSession`
(`make smoke`) walks a real form: open, snapshot, type, click submit, back,
scroll, wait, screenshot.

## Persistent memory

New interactive runs use `agent-discovery-v1`. The model can look up the
memory tools with `search_tools`.
The worker retains prior workflow registrations for replay.
Roll out the migration, then the new worker and server. Version information
travels with the persisted run input, and child runs use the parent's workflow
name.

- `remember` saves an approved preference or note. The store derives the owner
  from the run and records a receipt under the activity call key. Deletion
  clears text and vectors and retains the receipt across activity retries.
- `recall` searches saved memories and returns provenance. The first model call
  also gets a bounded recall result through a journaled activity. Only the
  top-level run gets private memories and conversation search.
- `session_search` searches the owner's prior prompts and final assistant
  replies and returns excerpts with links. Search excludes raw tool events.
- **Settings → Memory** shows saved entries and lets the user delete them or
  enable summaries for future completed turns. The worker creates summaries
  asynchronously using the configured chat model.
- Text search works with Postgres alone. To enable semantic memory, configure
  `MEMORY_EMBEDDING_BASE_URL`, `MEMORY_EMBEDDING_MODEL`, and an optional
  `MEMORY_EMBEDDING_API_KEY` on workers. Use the same 1024-dimensional model
  configuration on every worker. The provider receives saved memory text for
  indexing and sends back vectors for recall queries. Search falls back to text
  if the provider fails.

## Repository skills

Add `skills/<name>/SKILL.md` with YAML `name` and `description` frontmatter.
The name must match the directory. The catalog accepts up to eight files, each
at most 8 KiB. Build the server to include changes to bundled files.

The server embeds the catalog and persists its sorted metadata, bodies, and
SHA-256 digest in each admitted run's input. Research children inherit that
snapshot. The model sees the name and description and can load the full body
through `read_skill {name}`. The activity resolves the name in the persisted
snapshot, so adding or editing a skill changes the catalog for new runs.
Workers serve the snapshot carried in each skills-enabled run. Skills use
the tools already offered to that run.

## Schedules

The `schedule` Write tool uses the approval flow to show the exact task,
first local RFC3339 timestamp with UTC offset, IANA timezone, and cadence. The offset must
match the timezone at that instant. Schedules target the saved conversation
where the user requested them. The Schedules page lists owned schedules,
cancels pending ones, and links fired schedules to their run.

The server uses the Postgres clock, and creation deduplicates on the tool call
key. One-shot schedules transition to `fired`, `cancelled`, or `missed`.
A daily or weekly series stays `pending` until cancelled. The queued run,
recorded outcome, and next due time commit together under the schedule lock.
A unique key on `(schedule_id, scheduled_for)` prevents duplicate occurrences.

Daily and weekly schedules preserve the original local clock time, and weekly
schedules preserve the weekday. Future DST gaps skip the date. Future overlaps
use the earlier instant. If a prior occurrence is queued or active, the due
occurrence is skipped. A full queue is retried for up to one hour after the
due time. After longer downtime, the series records the expired due occurrence
and advances to the first eligible date. Cancelling a series stops future
occurrences. Runs already admitted follow the normal run lifecycle.

Scheduled runs use `agent-scheduled-v1` with the approved prompt and pinned
skill catalog. The profile offers web search, stateless browsing, skills,
and research subagents. The task result streams to the original chat.

## Deep research mode

The prompt composer sends `deep_research: true` to `POST /api/runs`. Admission
pins `agent-deep-research-v2` in the saved input. The worker retains v1 for replay. The workflow checkpoints
planning, parallel collection, targeted follow-up, drafting, independent
review, and the revised report. Progress reaches the existing SSE stream
through named research-stage events represented as tool calls and results.

Investigation and citation-review workers use read-only browser tools and
return evidence with direct URLs. The parent reads source provenance from
successful browser tool results. Child runs receive their assigned task and
the pinned catalog. The final stage weighs reviewer findings with the source
notes, including gaps and unavailable investigations. On a blank model response,
the workflow journals one shorter retry for that stage. If drafting still
returns no text, reviewers work from the source notes. If final writing still
returns no text, the chat receives the findings and review notes.

The staged approach is adapted from the
[HyperResearch workflow](https://github.com/jordan-gibbs/hyperresearch/blob/8f192b88bf777e2ad6da5e07e4b2ad7af40e2d7c/src/hyperresearch/skills/hyperresearch.md).

## Assistant messages

An interactive run can send a standalone chat message with
`send_message {text}` and continue working. The worker journals it as an
ordinary tool result. Text in a model reply also appears in chat, and a reply
without tool calls completes the run. Research reports and scheduled replies
use the same Markdown renderer.

The Postgres function `run_assistant_messages` projects these messages from
the event journal in sequence order. SSE includes the projected text as
`assistant_message`. Reloaded chats and follow-up prompts use the same
projection. Keep tool errors and other tool results out of the projection.
Assistant Markdown supports tables, lists, and source links. The renderer
drops raw HTML and remote images.

Apply the assistant messages migration before deploying the server and
worker. The worker keeps the prior workflow registrations for replay.

## Tool discovery

The top-level `agent-discovery-v1` workflow starts with seven tools.
The model calls `search_tools {query}` for browser sessions, parallel
research, memory, or schedules. The matcher searches the workflow's
authorized catalog. It returns one group, whose schemas become available
on the next model call and stay available for the rest of the run.
The executor checks every call against the schemas offered to that reply.
Discovered Write tools use the usual approval and ledger path.

The workflow computes discovery from journaled model replies and
persists the result as a tool event. Keep the starting list, catalog
order, ranking, and four-attempt budget stable for this workflow version.
Research children, Deep Research, and scheduled runs have fixed lists.
Deploy workers with the new registration before routing new chat runs
to it. See [tool discovery](.junie/plans/tool-discovery.md).

## Model gateway

`internal/model/nebius.go` speaks the OpenAI-compatible chat completions API
at Nebius Token Factory. `Complete` supports tool calling, retries network
errors, 429s and 5xx with exponential backoff and full jitter (honouring
`Retry-After`), and calls `OnUsage` once per successful call for metering.
`Chat` is `Complete` without tools. The default model is
`deepseek-ai/DeepSeek-V4.1-Flash-1017`; override with `NEBIUS_MODEL`.

## Commands

```bash
make up                 # Postgres, Temporal + UI, Obscura, browser sessions, SearXNG
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
`localhost:7233`), `OBSCURA_URL` (default `http://localhost:9222`),
`BROWSER_SESSION_URL` (default `http://127.0.0.1:9233`), and
`SEARXNG_URL` (default `http://localhost:8888`). Start the session service
before rolling out workers that use it. Tabs opened by the old in-process
worker need to be reopened during this transition. The server and worker also load `.env` themselves when
started without `make` (`internal/dotenv`); `go test` does not.

| Port | What |
|---|---|
| 8080 | Go server (`make dev` / `make serve`) |
| 127.0.0.1:5174 | Vite dev server (not 5173: DDEV's router holds 127.0.0.1:5173 on this machine) |
| 5432 | Postgres |
| 7233 | Temporal frontend (gRPC) |
| 8088 | Temporal UI (not 8080, which is the Go server) |
| 127.0.0.1:9222 | Obscura CDP (browse tool; needs the bearer token) |
| 127.0.0.1:9233 | Browser session service (uses the Obscura bearer token) |
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
