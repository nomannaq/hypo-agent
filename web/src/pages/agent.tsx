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
import { GlowFrame } from '@/components/agents/glow-frame';
import { MessageScroller } from '@/components/agents/message-scroller';
import { PromptInput } from '@/components/agents/prompt-input';
import { VoiceOrb } from '@/components/agents/voice-orb';
import { SPRING_LAYOUT } from '@/lib/ease';
import { agent } from '@/routes';

// One agent.Event from the run's stream (internal/agent/events.go).
type AgentEvent = {
    kind: 'user_message' | 'model_reply' | 'tool_result' | 'failed';
    text?: string;
    tool_calls?: { id: string; name: string }[];
    call_id?: string;
    is_error?: boolean;
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
            items.push({
                id: `tool-${i}-${call.id}`,
                type: 'step',
                label: result ? `Ran ${call.name}` : `Running ${call.name}`,
                status: result ? 'complete' : 'active',
                meta: result?.is_error ? 'failed' : undefined,
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
                                {loading ? 'Working…' : 'Ready'}
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
