import { Head } from '@inertiajs/react';
import { motion, useAnimationFrame, useMotionValue } from 'motion/react';
import { useEffect, useRef, useState } from 'react';
import {
    Message,
    MessageContent,
    MessageGroup,
} from '@/components/agents/message';
import {
    MessageBubble,
    MessageBubbleContent,
} from '@/components/agents/message-bubble';
import {
    AgentActivity,
    type AgentActivityItem,
} from '@/components/agents/agent-activity';
import {
    ToolApproval,
    type ToolApprovalStatus,
} from '@/components/agents/tool-approval';
import { GlowFrame } from '@/components/agents/glow-frame';
import { MessageScroller } from '@/components/agents/message-scroller';
import { PromptInput } from '@/components/agents/prompt-input';
import { VoiceOrb } from '@/components/agents/voice-orb';
import { SPRING_LAYOUT } from '@/lib/ease';
import { agent } from '@/routes';

type ToolCall = { id: string; name: string; args?: unknown };

const host = (url: string) => {
    try {
        return new URL(url).host;
    } catch {
        return url; // not a URL: show it as given
    }
};

// A step label for a tool call, from its arguments when we know the tool.
const toolLabel = (call: ToolCall, done: boolean) => {
    const args = (call.args ?? {}) as { query?: unknown; url?: unknown };
    const url = typeof args.url === 'string' ? host(args.url) : undefined;
    if (call.name === 'web_search' && typeof args.query === 'string')
        return `${done ? 'Searched' : 'Searching'} “${args.query}”`;
    if (call.name === 'browse' && url)
        return `${done ? 'Read' : 'Reading'} ${url}`;
    if (call.name === 'inspect_forms' && url)
        return `${done ? 'Looked at the forms on' : 'Looking at the forms on'} ${url}`;
    if (call.name === 'submit_form' && url)
        return `${done ? 'Submitted a form on' : 'Submitting a form on'} ${url}`;
    return `${done ? 'Ran' : 'Running'} ${call.name}`;
};

// One agent.Event from the run's stream (internal/agent/events.go).
type AgentEvent = {
    kind:
        | 'user_message'
        | 'model_reply'
        | 'tool_result'
        | 'failed'
        | 'approval_requested'
        | 'approval_decided';
    text?: string;
    tool_calls?: ToolCall[];
    call_id?: string;
    is_error?: boolean;
    approved?: boolean;
    err?: string;
};

type Turn = {
    id: number;
    from: 'user' | 'assistant';
    text: string; // the prompt, or the final reply once the run ends
    runId?: string;
    events?: AgentEvent[];
    status?: 'working' | 'done' | 'error';
    seconds?: number; // how long the run took
};

// The conversation lives in sessionStorage so a reload restores it and
// reattaches to a run still in progress.
// ponytail: per-tab and client-side only; a conversations table replaces it
// once chats are listed in the sidebar.
const STORAGE_KEY = 'hypo.agent.turns';
const loadTurns = (): Turn[] => {
    try {
        const turns: Turn[] = JSON.parse(
            sessionStorage.getItem(STORAGE_KEY) ?? '[]',
        );
        // Reloaded before the run was started: there is nothing to reattach to.
        return turns.map((t) =>
            t.status === 'working' && !t.runId
                ? {
                      ...t,
                      status: 'error',
                      text: 'The page reloaded before the run started.',
                  }
                : t,
        );
    } catch {
        return [];
    }
};

const startRun = async (
    messages: { role: 'user' | 'assistant'; content: string }[],
) => {
    const res = await fetch('/api/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ messages }),
    });
    if (!res.ok) throw new Error('request failed');
    return ((await res.json()) as { id: string }).id;
};

// The run's approval requests, each with the call it's about and where it
// stands (ToolApproval's statuses).
type ApprovalItem = { call: ToolCall; status: ToolApprovalStatus };
const approvals = (events: AgentEvent[]): ApprovalItem[] => {
    const calls = new Map<string, ToolCall>();
    for (const e of events)
        for (const c of e.tool_calls ?? []) calls.set(c.id, c);
    const find = (kind: AgentEvent['kind'], id: string) =>
        events.find((e) => e.kind === kind && e.call_id === id);
    return events
        .filter((e) => e.kind === 'approval_requested' && e.call_id)
        .map((e) => {
            const call = calls.get(e.call_id!) ?? { id: e.call_id!, name: '?' };
            const decided = find('approval_decided', call.id);
            const result = find('tool_result', call.id);
            const status: ToolApprovalStatus = !decided
                ? 'pending'
                : !decided.approved
                  ? 'denied'
                  : !result
                    ? 'running'
                    : result.is_error
                      ? 'error'
                      : 'complete';
            return { call, status };
        });
};

// What the user is asked to approve, as ToolApproval parameters: the URL,
// then each form field.
const approvalParameters = (call: ToolCall) => {
    const args = (call.args ?? {}) as {
        url?: unknown;
        form?: unknown;
        fields?: Record<string, unknown>;
    };
    const params = [];
    if (typeof args.url === 'string')
        params.push({ id: 'url', label: 'Page', value: args.url });
    if (typeof args.form === 'number' && args.form > 0)
        params.push({ id: 'form', label: 'Form', value: `#${args.form}` });
    for (const [name, value] of Object.entries(args.fields ?? {}))
        params.push({ id: `field-${name}`, label: name, value: String(value) });
    return params;
};

// The run's steps so far, as AgentActivity items.
const activityItems = (
    events: AgentEvent[],
    working: boolean,
): AgentActivityItem[] => {
    const items: AgentActivityItem[] = [];
    const done = new Map<string, AgentEvent>();
    for (const e of events)
        if (e.kind === 'tool_result' && e.call_id) done.set(e.call_id, e);
    events.forEach((e, i) => {
        if (e.kind !== 'model_reply') return;
        items.push({
            id: `model-${i}`,
            type: 'step',
            label: 'Asked the model',
            status: 'complete',
        });
        for (const call of e.tool_calls ?? []) {
            const result = done.get(call.id);
            const asked = events.some(
                (e) => e.kind === 'approval_requested' && e.call_id === call.id,
            );
            const answered = events.some(
                (e) => e.kind === 'approval_decided' && e.call_id === call.id,
            );
            items.push({
                id: `tool-${i}-${call.id}`,
                type: 'step',
                label: toolLabel(call, !!result),
                status: result ? 'complete' : 'active',
                meta: result?.is_error
                    ? 'failed'
                    : asked && !answered
                      ? 'needs your approval'
                      : undefined,
            });
        }
    });
    const waitingOnTool = items.some(
        (it) => it.type === 'step' && it.status === 'active',
    );
    if (working && !waitingOnTool)
        items.push({
            id: 'model-next',
            type: 'step',
            label: 'Asking the model',
            status: 'active',
        });
    return items;
};

export default function Agent() {
    const [turns, setTurns] = useState<Turn[]>(loadTurns);
    const nextId = useRef(Math.max(0, ...turns.map((t) => t.id + 1)));
    const loading = turns.some((t) => t.status === 'working');
    const streams = useRef(new Set<EventSource>());

    useEffect(() => {
        sessionStorage.setItem(STORAGE_KEY, JSON.stringify(turns));
    }, [turns]);

    // The orb breathes quietly at rest and pulses while the agent works.
    const activity = useMotionValue(0.06);
    useAnimationFrame((t) => {
        activity.set(loading ? 0.35 + 0.4 * Math.sin(t / 240) ** 2 : 0.06);
    });

    // Approvals the user has answered whose decision hasn't come back in the
    // stream yet, keyed run ID + call ID.
    const [deciding, setDeciding] = useState(new Set<string>());
    const waitingForUser = turns.some(
        (t) =>
            t.status === 'working' &&
            approvals(t.events ?? []).some((a) => a.status === 'pending'),
    );

    const decide = async (runId: string, callId: string, approved: boolean) => {
        const key = `${runId}/${callId}`;
        setDeciding((d) => new Set(d).add(key));
        const res = await fetch(
            `/api/runs/${encodeURIComponent(runId)}/approvals`,
            {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ call_id: callId, approved }),
            },
        ).catch(() => null);
        if (!res?.ok) {
            setDeciding((d) => {
                const next = new Set(d);
                next.delete(key);
                return next;
            });
            alert('Your answer could not be sent. Please try again.');
        }
    };

    const update = (id: number, f: (t: Turn) => Turn) =>
        setTurns((ts) => ts.map((t) => (t.id === id ? f(t) : t)));

    // follow streams a run's events into the reply turn until the run ends.
    // The server replays the run from its first event, so a reattach starts
    // from an empty list; EventSource's own reconnects resume via Last-Event-ID.
    const follow = (replyId: number, runId: string) => {
        const started = performance.now();
        const seconds = () => Math.round((performance.now() - started) / 1000);
        const es = new EventSource(
            `/api/runs/${encodeURIComponent(runId)}/events`,
        );
        streams.current.add(es);
        update(replyId, (t) => ({ ...t, events: [] }));
        es.onmessage = (m) => {
            const e = JSON.parse(m.data) as AgentEvent;
            update(replyId, (t) => ({
                ...t,
                events: [...(t.events ?? []), e],
                text:
                    e.kind === 'model_reply' && !e.tool_calls?.length
                        ? (e.text ?? '')
                        : e.kind === 'failed'
                          ? `The run stopped: ${e.err}`
                          : t.text,
            }));
        };
        es.addEventListener('end', () => {
            es.close();
            update(replyId, (t) => ({
                ...t,
                status: 'done',
                seconds: seconds(),
            }));
        });
        // Fired for the server's `event: error` and for a connection that
        // can't recover (404, logged out). Transient drops reconnect on their own.
        es.addEventListener('error', (e) => {
            if (
                !(e instanceof MessageEvent) &&
                es.readyState !== EventSource.CLOSED
            )
                return;
            es.close();
            update(replyId, (t) => ({
                ...t,
                status: 'error',
                text: 'Sorry, the run stopped before it finished. Please try again.',
                seconds: seconds(),
            }));
        });
        return es;
    };

    // Reattach to runs still in progress after a reload; close every stream
    // when the page goes away.
    useEffect(() => {
        const open = streams.current;
        turns
            .filter((t) => t.status === 'working' && t.runId)
            .forEach((t) => follow(t.id, t.runId!));
        return () => {
            open.forEach((es) => es.close());
            open.clear();
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    const submit = async (prompt: string) => {
        const text = prompt.trim();
        if (!text || loading) return;
        const userId = nextId.current++;
        const replyId = nextId.current++;
        const history = [
            ...turns
                .filter((t) => t.from === 'user' || t.status === 'done')
                .filter((t) => t.text)
                .map((t) => ({ role: t.from, content: t.text })),
            { role: 'user' as const, content: text },
        ];
        setTurns((t) => [
            ...t,
            { id: userId, from: 'user', text },
            { id: replyId, from: 'assistant', text: '', status: 'working' },
        ]);
        try {
            const runId = await startRun(history);
            update(replyId, (t) => ({ ...t, runId }));
            follow(replyId, runId);
        } catch {
            update(replyId, (t) => ({
                ...t,
                status: 'error',
                text: 'Sorry, something went wrong starting the agent. Please try again.',
            }));
        }
    };

    const started = turns.length > 0;

    return (
        <>
            <Head title="Agent" />
            <div className="flex h-full w-full flex-col px-4 pb-4">
                {started ? (
                    <>
                        <div className="mx-auto flex w-full max-w-3xl items-center gap-3 py-3">
                            <motion.div
                                layoutId="agent-orb"
                                transition={SPRING_LAYOUT}
                            >
                                <VoiceOrb
                                    activity={activity}
                                    className="w-10"
                                    aria-label="Agent activity"
                                />
                            </motion.div>
                            <span className="text-sm text-muted-foreground">
                                {waitingForUser
                                    ? 'Waiting for your approval'
                                    : loading
                                      ? 'Working…'
                                      : 'Ready'}
                            </span>
                        </div>
                        <MessageScroller
                            navigation="rail"
                            busy={loading}
                            className="min-h-0 flex-1"
                            viewportClassName="px-1 py-4"
                            contentClassName="mx-auto min-h-full w-full max-w-3xl"
                        >
                            <MessageGroup spacing="default">
                                {turns.map((turn) => (
                                    <Message
                                        key={turn.id}
                                        from={turn.from}
                                        animateIn
                                    >
                                        {turn.from === 'user' ? (
                                            <MessageBubble variant="solid">
                                                <MessageBubbleContent>
                                                    {turn.text}
                                                </MessageBubbleContent>
                                            </MessageBubble>
                                        ) : (
                                            <MessageContent className="flex flex-col gap-2">
                                                <AgentActivity
                                                    items={activityItems(
                                                        turn.events ?? [],
                                                        turn.status ===
                                                            'working',
                                                    )}
                                                    status={
                                                        turn.status ===
                                                        'working'
                                                            ? 'working'
                                                            : 'complete'
                                                    }
                                                    duration={turn.seconds ?? 0}
                                                />
                                                {approvals(
                                                    turn.events ?? [],
                                                ).map(({ call, status }) => {
                                                    const key = `${turn.runId}/${call.id}`;
                                                    return (
                                                        <ToolApproval
                                                            key={key}
                                                            tool={call.name}
                                                            description="The agent wants to do this on your behalf. Nothing is sent until you allow it."
                                                            parameters={approvalParameters(
                                                                call,
                                                            )}
                                                            defaultOpen
                                                            status={
                                                                status ===
                                                                    'pending' &&
                                                                deciding.has(
                                                                    key,
                                                                )
                                                                    ? 'approving'
                                                                    : status
                                                            }
                                                            onApprove={() =>
                                                                decide(
                                                                    turn.runId!,
                                                                    call.id,
                                                                    true,
                                                                )
                                                            }
                                                            onDeny={() =>
                                                                decide(
                                                                    turn.runId!,
                                                                    call.id,
                                                                    false,
                                                                )
                                                            }
                                                        />
                                                    );
                                                })}
                                                {turn.status !== 'working' &&
                                                    turn.text}
                                            </MessageContent>
                                        )}
                                    </Message>
                                ))}
                            </MessageGroup>
                        </MessageScroller>
                    </>
                ) : (
                    <div className="mx-auto flex w-full max-w-3xl flex-1 flex-col items-center justify-center gap-6 text-center">
                        <motion.div
                            layoutId="agent-orb"
                            transition={SPRING_LAYOUT}
                        >
                            <VoiceOrb
                                activity={activity}
                                className="w-48 sm:w-56"
                                aria-label="Agent activity"
                            />
                        </motion.div>
                        <div className="space-y-1">
                            <h1 className="text-2xl font-semibold tracking-tight">
                                What should we investigate?
                            </h1>
                            <p className="text-sm text-muted-foreground">
                                State a hypothesis and the agent will research
                                and test it.
                            </p>
                        </div>
                    </div>
                )}
                <GlowFrame
                    active={loading}
                    className="mx-auto mt-2 w-full max-w-3xl"
                >
                    <PromptInput
                        loading={loading}
                        onSubmit={submit}
                        placeholder="Describe a hypothesis to research…"
                    />
                </GlowFrame>
            </div>
        </>
    );
}

Agent.layout = {
    breadcrumbs: [{ title: 'Agent', href: agent() }],
};
