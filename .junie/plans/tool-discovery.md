# Tool discovery

## Goal

New interactive runs start with a small tool list. When the task calls for a
specialized capability, the model can find it with `search_tools` and use it
from the next model call. A greeting still takes one model call. An
interactive browser task needs at most one lookup for its browser tools.

Today `cmd/worker` builds fixed tool lists per workflow version.
`internal/executor` sends the whole list on every model call and checks tool
calls against that list. Reuse the existing registry, reducer, event stream,
and tool execution path.

## What the model sees

The initial interactive list contains `web_search`, `browse`, `ask_user`,
`todo_write`, `read_skill`, `send_message`, and `search_tools`. Searching
for sources, reading one page, and loading a skill remain direct calls.
The system prompt describes the discoverable categories in one sentence.

| Search intent | Tools made available |
|---|---|
| Interact with a page, fill a form, or take a screenshot | The browser session family, from `browser_open` and `browser_snapshot` through the input, navigation, and screenshot tools |
| Investigate independent questions in parallel | `research` |
| Save a preference, recall a note, or find a past chat | `remember`, `recall`, `session_search` |
| Run a task later or on a recurring schedule | `schedule` |

`search_tools {query}` accepts a short description or an exact tool name.
Exact names take precedence. Otherwise match tokens against curated
category terms and tool names, with a fixed tie order. Return at most one
matching category per lookup. Results contain the selected tool names and
short descriptions. The next model request receives their actual schemas.
When nothing matches, the tool returns a short hint with the available
categories. Repeated searches deduplicate tools and consume the lookup
budget. Start with four attempts per run, including misses.

Keep discovered tools available for the rest of the run so a browser
session can continue across interruptions and model turns. Each new run
starts with the base list. A loaded skill can ask the model to discover
capabilities it needs, using the same lookup and permissions as chat.

Use deterministic keyword matching for the current registry. The roadmap
proposes embeddings. Revisit them if representative queries show that
aliases and names miss relevant groups often enough to warrant an index or
provider call.

## Execution and replay

- Add the search spec and the static category matcher in `internal/tool`.
  The executor handles the search in workflow code, like its other
  workflow-native tools. The matcher requires no I/O. Record its result as
  the existing `tool_result` event. The UI can label that activity
  “Looked up tools.”
- Keep the workflow profile's authorized tool list separate from the
  schemas offered to a model call. Search only within the authorized list.
  Capture the exact offered list when sending a model request and use it
  to validate every tool call in that reply. A discovery call and a newly
  discovered call in the same batch cannot execute the latter. The model
  receives an error explaining that only the tools offered for this
  reply can run, and that discovered tools become available next turn.
- Keep the existing dispatch paths for discovered tools. In particular,
  `remember` and `schedule` still need approval and use the ledger.
  Browser actions keep their existing policy. The user-facing chat shows
  assistant messages and the existing activity stream.
- Register `agent-discovery-v1` from the messaging profile, and route new
  interactive admissions to it once workers have the registration. Keep
  the older workflow names and their tool lists for replay. Freeze the
  discovery profile and ranking for this version. Rebuild its per-run tool
  list deterministically from the journaled model replies on replay.
- Keep research children, Deep Research, and scheduled runs on their
  current fixed tool lists for this release. Ordinary research children
  inherit their parent's workflow name and still use the existing child
  filters. Apply discovery only to top-level runs of the new version.
  Include the new interactive version in recurring schedule
  authorization and keep the existing clock and skill snapshot behavior.

This design uses the current database schema and reducer event kinds.

## Delivery and verification

1. Implement the matcher and the versioned interactive profile. Test
   exact names, natural queries, empty input, misses, ties, repeated
   lookups, and stable result order.
2. Wire schema offering and validation into the executor. Use a fake
   model to assert the exact schemas in each request, including the
   same-batch rejection and the next-turn call. Test a discovered Write
   tool through approval and decline, and verify restricted profiles
   never receive its schema. Check that discovery results stay out of
   public assistant messages.
3. Replay a Temporal run interrupted after discovery. Verify the next
   request has the same ordered schemas and recorded activity calls are
   reused. Run the Go race suite and frontend checks.
4. Compare a greeting, a read-only lookup, a multi-step browser flow, a
   parallel research request, a saved preference, and a schedule with the
   full-registry profile. Record first-request tool-schema bytes, model
   calls, and successful tool selection. Aim for at least 35% fewer
   first-request tool-schema bytes. Verify greetings and read-only
   lookups need no discovery call, and browser workflows need at most
   one lookup before beginning.

## Verification results

The comparison used the production HTTP server, Postgres, and Temporal
worker, with a scripted model and search and browser fixtures. Every
scenario selected and successfully executed the intended tools. The
first-request tool list fell from 8,788 to 2,996 bytes of compact JSON,
a 65.9% reduction. These counts exclude messages and provider framing.

| Scenario | Full-registry model calls | Discovery model calls |
|---|---:|---:|
| Greeting | 1 | 1 |
| Read-only lookup | 2 | 2 |
| Open and inspect a browser page | 3 | 4 |
| Parallel research | 2 | 3 |
| Save a preference | 2 | 3 |
| Create a recurring schedule | 2 | 3 |

The call counts are for the parent. Each parallel research run also
started two child runs. The browser session used one lookup. The memory
and schedule runs each paused for approval. A Temporal worker restart
test checked that the follow-up model request received the same ordered
schemas and that recorded work was reused.

A live local run with the configured model answered a greeting without
tools. A second run used `search_tools`, `browser_open`, and
`browser_snapshot` to read the title of the example domain.
