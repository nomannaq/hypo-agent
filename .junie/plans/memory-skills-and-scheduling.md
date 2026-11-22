# Memory, skills, and scheduling

**Status** Durable admission, memory, skills, and scheduling are implemented on `feat/memory-skills-and-scheduling`. Interactive runs use `agent-recurring-v1`. Unattended runs use `agent-scheduled-v1` and the limited research tool set.

## What to borrow from go-bot

| Capability | Verified upstream behavior | Hypo design |
|---|---|---|
| Memory | Account-scoped preferences and notes, with past chats searchable separately. [go-bot README](https://github.com/xyc999-ai/go-bot#readme) | Use Postgres for owner-scoped memories and transcript search. Return references to the source run so users can check recalled claims. |
| Skills | The model sees skill names and descriptions and loads instructions on demand. [go-bot skill discovery](https://github.com/xyc999-ai/go-bot#what-runs-where) | Ship a repository-owned `hypothesis-research` skill that uses the existing browser, search, and research tools. |
| Scheduling | A confirmed one-time reminder can deliver a result in the same chat. Sessions have a durable inbox and execute one turn at a time. [go-bot demo](https://github.com/xyc999-ai/go-bot#demos), [API docs](https://github.com/xyc999-ai/go-bot/blob/main/docs/api.md#files-and-streams) | Store explicit user schedules and a durable run queue in Postgres. Use the normal Temporal workflow and SSE for delivery. |

Upstream source inspection confirmed that go-bot can correct and forget memory entries through its tool and supports context windows around transcript matches. Hypo uses an approval-backed save tool, owner-managed deletion, and excerpts that link to the original chat. The upstream skill tool loads by exact name. Its schedule tool exposes separate create, list, view, and edit operations. [Memory](https://github.com/xyc999-ai/go-bot/blob/main/harness/tools/native_memory.go), [session search](https://github.com/xyc999-ai/go-bot/blob/main/harness/tools/native_session_search.go), [skills](https://github.com/xyc999-ai/go-bot/blob/main/harness/tools/skill.go), [schedules](https://github.com/xyc999-ai/go-bot/blob/main/harness/tools/native_schedule.go)

## Domain rules

- Give each lifecycle its own `Status` and `EventKind` in its owning package. Follow `internal/agent` with explicit `uint8` constants, a trailing count, a keyed names array, and `Valid`, `String`, `MarshalText`, and `UnmarshalText`. Reserve zero as invalid for new persisted state types. Validate required fields at the boundary, including absent and `null` fields that can bypass text decoding. Keep the definitions beside their state machines.
- Use a pure transition function and one table per lifecycle. Reject illegal edges before mutation, validate payloads for legal edges, and verify the resulting state against the table. Persist stable text values and mirror the allowed status names with SQL `CHECK` constraints. Apply a transition under a row lock or an expected-state/version predicate and make duplicate delivery a no-op at the persistence boundary.
- Keep the existing reducer responsible for what the model needs next. Runtime run status represents admission and execution across HTTP, Postgres, and Temporal. An agent waiting for `ask_user` still holds its runtime slot. `agent.Done` and `agent.Stopped` become runtime completion outcomes.
- Temporal workflow changes ship under a new workflow name with the legacy handler and its frozen specs available while existing executions drain. Preserve the existing event names, step IDs, and activity names for legacy runs. Persist versioned input at admission. Use named activities for database, filesystem, and embedding calls. Journal bounded outputs used to construct prompts or choose later calls. Child workflows inherit the workflow version and trusted run policy.

## 1. Persist conversations and admit runs durably

Add owner-scoped `conversations` and associate new `agent_runs` with them. The browser creates or selects a conversation and sends the next prompt with a client request ID. The server assembles context from ordered user and final assistant messages. Keep legacy `/api/runs` request support during the UI rollout and keep old run IDs streamable. Old runs have no reliable chat grouping. Their run URLs remain streamable, and the existing tab-local chat is preserved while newly created chats use server-backed conversations.

`internal/runtime` owns admission, dispatch, and reconciliation. In one Postgres transaction, validate ownership and insert a `queued` run with its versioned request. Enforce a bounded per-user queue and deduplicate client retries by `(owner, request_id)`. Any server replica can dispatch. Serialize admission per user and serialize capacity reservation across dispatchers. Claim the oldest queued turn in each eligible conversation. Enforce one active run per conversation with a partial unique index over `starting` and `running`. Acquire a bounded claim before calling Temporal outside the transaction. Assemble and persist the exact workflow input at dispatch so a later queued turn sees the preceding turn's answer and all retries use identical input.

| Runtime run state | Event | Next state |
|---|---|---|
| `queued` | `dispatch_claimed` | `starting` |
| `starting` | `workflow_started` | `running` |
| `starting` | `agent_succeeded` / `agent_failed` | `succeeded` / `failed` |
| `starting` | `start_failed` or `workflow_closed_without_final` | `failed` |
| `running` | `agent_succeeded` / `agent_failed` | `succeeded` / `failed` |
| `running` | `workflow_closed_without_final` | `failed` |

Terminal states have no outgoing edges. A successful final `model_reply` with no tool calls or an agent `failed` event updates the runtime state in the same transaction as `events.append` and wakes dispatch. A workflow can finish before its start acknowledgement arrives. Permit terminal events in `starting` and make the acknowledgement conditional. Count `starting` and `running` against capacity. Temporary start failures leave the run `starting` for a bounded retry on the same reserved ID. Distinguish a confirmed rejection from a request with an unknown outcome.

Use the application run ID as the Temporal workflow ID with reject-duplicate reuse. Reconcile an ambiguous start by describing that ID and retrying within a window shorter than namespace retention. After that window, reconcile available evidence and surface an unresolved start as a failure without automatically starting a new execution. Preserve the row after any uncertain start. A periodic supervisor, independent of SSE subscribers, reconciles executions that close without a final event and wakes their streams. Use Temporal's execution timeout and closure information for liveness. During an outage keep capacity reserved for unresolved active executions. [Temporal workflow ID semantics](https://docs.temporal.io/workflow-execution/workflowid-runid#what-is-a-workflow-id-reuse-policy)

**Acceptance** Concurrent submissions respect both caps. A server crash before dispatch, lost response after Temporal accepted the start, worker loss, and completion before start acknowledgement each yield one visible run. Reloading from a new tab restores the conversation and its active stream. A run closed without a final event releases its slot even with no browser connected. The existing executor guards cover per-run turns, tokens, and tools. Account-level spending controls can follow reliable usage accounting, including child work.

## 2. Add memory and session search

- Add a `memories` table with owner, kind (`preference`, `note`, `run_summary`), text, source run, timestamps, and embedding model/vector metadata. Source ownership is enforced on writes and in recall queries. Derive owner from the trusted run record or its validated parent. The model supplies text and queries. It never supplies an owner or arbitrary filesystem path.
- `remember` is a Write tool using the existing approval pause. Store its result and the note atomically under the tool call key so simultaneous retries create one memory. Add authenticated list/delete so users can review what has been saved. Successful run summaries are a separate, opt-in background projection keyed by run ID. Produce them from bounded user/assistant text after the final event commits. Exclude raw browser output, approval answers, and tool arguments from the summary input. Preserve provenance and keep summaries separate from explicit preferences. Keep deletion durable against a retried summary job.
- `recall` searches only this owner's saved memories. A bounded recall at turn start supplies relevant facts and provenance as data, with a timeout and empty-context fallback. Provide Postgres text search as the baseline and fallback. Add a configurable embedding adapter with fixed model/dimension for its index, and write embeddings asynchronously outside workflow code. Label the model for reindexing and filter vector candidates by owner before ranking. Start with a fake embedding provider in tests and verify the selected deployment provider before claiming semantic recall works live.
- `session_search` queries the owner's persisted user messages and final assistant replies, returns short excerpts and conversation/run links, and uses Postgres full-text search. Use only public transcript fields. Bound history sent to the model by dropping older whole turns and retrieving summaries when needed. The persisted transcript stays available for explicit lookup.

**Acceptance** A saved preference is recalled in a new conversation after a restart. A prior answer is discoverable without the original tab. Cross-account queries and forged source IDs cannot read or write another owner's data. Deletion stops future recall, including after retries. Embedding outages leave chat and keyword recall usable. Replay uses the recorded context snapshot.

## 3. Load skills on demand

- Load repository-owned `skills/<name>/SKILL.md` files into a release catalog with validated, unique `name` and `description` frontmatter and a digest. Sort by name. Embed the repository files at build time. Persist the bounded catalog, including the digest and bodies, directly in run input. This gives each run and its research children access to the exact bundle they started with after a worker restart or deploy.
- Add a compact skill index to the system prompt and `read_skill {name}` to the existing registry. Resolve an exact catalog name inside the tool activity and return bounded Markdown. Skills guide existing tools within the run's permissions. A web page or recalled note cannot install a skill or grant tools. Keep similarity-based `search_tools` separate from this release.
- Ship `hypothesis-research`. It should define the hypothesis and falsification criteria, look for supporting and contradictory evidence, ask subagents to investigate independent questions when useful, and end with a verdict, uncertainty, and source links.

**Acceptance** The model sees only metadata before loading the selected body. Unknown names and traversal strings fail. A run started on catalog A keeps using A after catalog B deploys, including after worker restart. The bundled skill can complete a research task using the existing registry.

## 4. Schedule browser research and reminders

Start with one-shot schedules. The `schedule` Write tool proposes a task in the current saved conversation, with an RFC3339 local timestamp and IANA timezone. The approval card shows the task, local time, and explicit offset. Validate the offset against the timezone at that instant, including DST gaps and overlaps. Normalize to UTC and store the original timezone for display. Authenticated UI operations list and cancel owned schedules. Reuse the existing approval endpoint for tool proposals and the transactional idempotency key for creation. An unattended run uses a server-selected research profile with `web_search`, stateless `browse`, `read_skill`, and `research` whose children inherit that profile. This permits deep research while the user is away. It has no approval-dependent tools or interactive site actions.

| Schedule state | Event | Next state |
|---|---|---|
| `pending` | `occurrence_enqueued` | `fired` |
| `pending` | `cancelled` | `cancelled` |
| `pending` | `delivery_window_elapsed` | `missed` |

Creation writes `pending` after approval. `fired`, `cancelled`, and `missed` are terminal for the schedule. The resulting run follows the independent runtime machine and can still be queued, active, or failed.

Use a Postgres due scan with row locks and the database clock. In one transaction write the occurrence's queued run and advance the schedule. The one-shot row owns a deterministic run ID. The run primary key and atomic schedule transition prevent duplicate admission. Cancellation and firing serialize on the same schedule row. Cancellation after enqueue returns a conflict with the run link. If a slot is busy, the queued run waits its turn. When the queue is full, retry admission until one hour after the due time, then mark the schedule `missed`. A dispatcher starting after this window also marks the overdue schedule `missed`. Once admitted, the occurrence follows the normal run dispatcher and supervisor. The chat lists scheduled runs from Postgres when the user returns and uses the existing SSE endpoint when they open a run. Daily and weekly series now use the same approval, admission, and cancellation paths. Each occurrence records `fired` or `missed` under a unique `(schedule_id, scheduled_for)` key. Recording an occurrence leaves the series `pending` and advances its next due time in the same transaction.

Future occurrences preserve the approved local clock time and, for weekly schedules, weekday. Skip a DST gap and choose the earlier instant in a DST overlap. Skip an occurrence when the prior run is queued or active. Apply the one-hour admission window, then advance beyond expired dates. Cancelling a series stops future occurrences while admitted runs continue. The Schedules page shows the cadence, next due time, latest run, and reason when the most recent recorded occurrence was missed.

**Acceptance** A schedule fires with the browser closed. Two dispatchers and a restart cannot create duplicate occurrences. A cancellation racing a due scan has one winner. The result appears in the original conversation after login, and a scheduled research run cannot accidentally initiate another schedule or an interactive site action.

## Verification and rollout

- For every new state machine, test the entire status × event matrix, exercise every declared target, reject invalid encoded states and payloads, and verify persisted names. Add a real Postgres test for locking and idempotency so the pure transition tests cover only behavior they can prove.
- Keep a pre-change Temporal history fixture and replay it during rollout testing. Run memkernel and Temporal conformance, plus targeted Postgres/Temporal tests that kill and restart the dispatcher or worker at the failure windows above. Keep the previously recorded browser tools and event streaming behavior in regression coverage.
- Use additive goose migrations and ship each milestone behind its normal entry point with API and UI verification. Test against old run rows and a fresh database. Before releasing scheduling, run a one-shot research task with a closed browser and verify its result from a second authenticated session for the same owner.
