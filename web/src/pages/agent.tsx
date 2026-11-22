import { Head, router } from '@inertiajs/react';
import { Telescope } from 'lucide-react';
import { motion, useAnimationFrame, useMotionValue } from 'motion/react';
import { Fragment, useEffect, useRef, useState } from 'react';
import Markdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
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
import { TodoList, type TodoItem } from '@/components/agents/todo-list';
import { QuestionCard } from '@/components/question-card';
import { GlowFrame } from '@/components/agents/glow-frame';
import { MessageScroller } from '@/components/agents/message-scroller';
import { PromptInput } from '@/components/agents/prompt-input';
import { VoiceOrb } from '@/components/agents/voice-orb';
import { SPRING_LAYOUT } from '@/lib/ease';
import { agent, agentChat } from '@/routes';

type ToolCall = { id: string; name: string; args?: unknown };

const host = (url: string) => {
    try {
        return new URL(url).host;
    } catch {
        return url; // not a URL: show it as given
    }
};

// A step label for a tool call, from its arguments when we know the tool:
// [while running, once done].
const toolLabel = (call: ToolCall, done: boolean) => {
    const a = (call.args ?? {}) as {
        query?: string;
        url?: string;
        element_index?: number;
        text?: string;
        option?: string;
        key?: string;
        direction?: string;
        tasks?: { title?: string }[];
    };
    const el = `element ${a.element_index ?? '?'}`;
    const labels: Record<string, [string, string]> = {
        deep_research_plan: ['Planning the research', 'Planned the research'],
        deep_research_collect: [
            'Searching for sources in parallel',
            'Collected initial evidence',
        ],
        deep_research_investigate: [
            'Investigating findings and gaps',
            'Investigated findings and gaps',
        ],
        deep_research_draft: ['Drafting the report', 'Drafted the report'],
        deep_research_review: [
            'Reviewing citations and conclusions',
            'Reviewed citations and conclusions',
        ],
        deep_research_report: ['Finalizing the report', 'Finalized the report'],
        web_search: [`Searching “${a.query}”`, `Searched “${a.query}”`],
        browse: [`Reading ${host(a.url ?? '')}`, `Read ${host(a.url ?? '')}`],
        browser_open: [
            `Opening ${host(a.url ?? '')}`,
            `Opened ${host(a.url ?? '')}`,
        ],
        browser_snapshot: ['Looking at the page', 'Looked at the page'],
        browser_click: [`Clicking ${el}`, `Clicked ${el}`],
        browser_type: [`Typing “${a.text}”`, `Typed “${a.text}”`],
        browser_select: [`Choosing “${a.option}”`, `Chose “${a.option}”`],
        browser_press: [`Pressing ${a.key}`, `Pressed ${a.key}`],
        browser_scroll: [`Scrolling ${a.direction}`, `Scrolled ${a.direction}`],
        browser_back: ['Going back', 'Went back'],
        browser_wait: [`Waiting for “${a.text}”`, `Waited for “${a.text}”`],
        browser_screenshot: ['Taking a screenshot', 'Took a screenshot'],
        todo_write: ['Updating the plan', 'Updated the plan'],
        send_message: ['Sending a message', 'Sent a message'],
        search_tools: ['Looking up tools', 'Looked up tools'],
        ask_user: ['Asking you', 'Asked you'],
        read_skill: ['Loading a skill', 'Loaded a skill'],
        schedule: ['Scheduling a task', 'Scheduled a task'],
        remember: ['Saving a memory', 'Saved a memory'],
        recall: ['Searching saved memories', 'Searched saved memories'],
        session_search: ['Searching past chats', 'Searched past chats'],
        research: [
            `Researching ${a.tasks?.length ?? ''} topics in parallel`,
            `Researched ${(a.tasks ?? []).map((t) => t.title).join(', ')}`,
        ],
    };
    const [running, finished] = labels[call.name] ?? [
        `Running ${call.name}`,
        `Ran ${call.name}`,
    ];
    return done ? finished : running;
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
    assistant_message?: string;
    tool_calls?: ToolCall[];
    call_id?: string;
    is_error?: boolean;
    approved?: boolean;
    err?: string;
};

type Turn = {
    id: number;
    from: 'user' | 'assistant';
    text: string; // prompt, error, or a legacy saved reply
    messages?: string[];
    runId?: string;
    events?: AgentEvent[];
    status?: 'queued' | 'working' | 'done' | 'error';
    seconds?: number; // how long the run took
};

// Preserve the original tab-local chat while older sessions are still open.
const STORAGE_KEY = 'hypo.agent.turns';
const loadTurns = (): Turn[] => {
    try {
        const turns: Turn[] = JSON.parse(
            sessionStorage.getItem(STORAGE_KEY) ?? '[]',
        );
        // Reloaded before the run was started: there is nothing to reattach to.
        return turns.map((t) =>
            (t.status === 'working' || t.status === 'queued') && !t.runId
                ? {
                      ...t,
                      status: 'error',
                      text: 'The page reloaded before the run started.',
                  }
                : t.status === 'working' || t.status === 'queued'
                  ? { ...t, events: [], messages: [] }
                  : t,
        );
    } catch {
        return [];
    }
};

type StoredRun = {
    id: string;
    prompt: string;
    messages: string[];
    failure: string;
    status: 'queued' | 'starting' | 'running' | 'succeeded' | 'failed';
    scheduled?: boolean;
};

type AgentProps = {
    conversationID: string;
    runs: StoredRun[];
    newChat: boolean;
};

const restoreRuns = (runs: StoredRun[]): Turn[] =>
    runs.flatMap((run, i): Turn[] => [
        {
            id: i * 2,
            from: 'user',
            text: run.scheduled ? `Scheduled task: ${run.prompt}` : run.prompt,
        },
        {
            id: i * 2 + 1,
            from: 'assistant',
            text: run.failure,
            messages: run.messages,
            runId: run.id,
            status:
                run.status === 'succeeded'
                    ? 'done'
                    : run.status === 'failed'
                      ? 'error'
                      : run.status === 'queued' || run.status === 'starting'
                        ? 'queued'
                        : 'working',
        },
    ]);

const assistantMessages = (turn: Turn) =>
    turn.messages ?? (turn.status === 'done' && turn.text ? [turn.text] : []);

function AssistantMarkdown({ text }: { text: string }) {
    return (
        <div className="w-full min-w-0 text-sm leading-7 wrap-anywhere [&_:is(h1,h2,h3,h4,h5,h6)]:font-semibold [&_a]:underline [&_a]:underline-offset-4 [&_blockquote]:border-s-2 [&_blockquote]:ps-4 [&_blockquote]:text-muted-foreground [&_code]:rounded [&_code]:bg-muted [&_code]:px-1 [&_h1]:text-xl [&_h2]:text-lg [&_h3]:text-base [&_ol]:list-decimal [&_ol]:ps-6 [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-muted [&_pre]:p-3 [&_pre_code]:p-0 [&_td]:border [&_td]:px-3 [&_th]:border [&_th]:bg-muted [&_th]:px-3 [&_ul]:list-disc [&_ul]:ps-6 [&>*+*]:mt-3">
            <Markdown
                remarkPlugins={[remarkGfm]}
                skipHtml
                disallowedElements={['img']}
                components={{
                    a: ({ href, children }) =>
                        href ? (
                            <a
                                href={href}
                                target={
                                    /^https?:\/\/|^\/\//i.test(href)
                                        ? '_blank'
                                        : undefined
                                }
                                rel="noopener noreferrer"
                            >
                                {children}
                            </a>
                        ) : (
                            children
                        ),
                    table: ({ children }) => (
                        <div className="overflow-x-auto">
                            <table className="border-collapse">
                                {children}
                            </table>
                        </div>
                    ),
                }}
            >
                {text}
            </Markdown>
        </div>
    );
}

type ChatFrame =
    | {
          type: 'accepted';
          request_id: string;
          run_id: string;
          conversation_id: string;
      }
    | { type: 'rejected'; request_id: string; error: string }
    | { type: 'event'; run_id: string; seq: number; event: AgentEvent }
    | { type: 'end'; run_id: string }
    | { type: 'error'; run_id: string; error?: string }
    | { type: 'ping' | 'pong' };

type QueuedPrompt = {
    replyId: number;
    prompt: string;
    deepResearch: boolean;
    sentOn?: WebSocket;
    initialHistory?: { role: Turn['from']; content: string }[];
};

// The run's requests to the user (approval_requested): Write calls to
// approve, and ask_user questions. Each comes with its call, the user's
// decision once made, and the call's result once it has one.
type UserRequest = {
    call: ToolCall;
    decided?: AgentEvent;
    result?: AgentEvent;
};
const userRequests = (events: AgentEvent[]): UserRequest[] => {
    const calls = new Map<string, ToolCall>();
    for (const e of events)
        for (const c of e.tool_calls ?? []) calls.set(c.id, c);
    const find = (kind: AgentEvent['kind'], id: string) =>
        events.find((e) => e.kind === kind && e.call_id === id);
    return events
        .filter((e) => e.kind === 'approval_requested' && e.call_id)
        .map((e) => {
            const call = calls.get(e.call_id!) ?? { id: e.call_id!, name: '?' };
            return {
                call,
                decided: find('approval_decided', call.id),
                result: find('tool_result', call.id),
            };
        });
};

const approvalStatus = ({
    decided,
    result,
}: UserRequest): ToolApprovalStatus =>
    !decided
        ? 'pending'
        : !decided.approved
          ? 'denied'
          : !result
            ? 'running'
            : result.is_error
              ? 'error'
              : 'complete';

// The latest plan the agent wrote with todo_write.
const latestTodos = (events: AgentEvent[]): TodoItem[] => {
    const calls = events.flatMap((e) =>
        (e.tool_calls ?? []).filter((c) => c.name === 'todo_write'),
    );
    const args = calls.at(-1)?.args as
        { todos?: { content: string; status: string }[] } | undefined;
    return (args?.todos ?? []).map((t, i) => ({
        id: String(i),
        title: t.content,
        status:
            t.status === 'in_progress'
                ? 'in-progress'
                : (t.status as TodoItem['status']),
    }));
};

// Screenshots the run took: browser_screenshot results carry [artifact:ID].
const screenshots = (events: AgentEvent[]) =>
    events.flatMap((e) =>
        e.kind === 'tool_result' && !e.is_error
            ? [...(e.text ?? '').matchAll(/\[artifact:([\w-]+)\]/g)].map(
                  (m) => m[1],
              )
            : [],
    );

// What the user is asked to approve, as ToolApproval parameters: the URL,
// then each form field.
const approvalParameters = (call: ToolCall) => {
    const args = (call.args ?? {}) as {
        url?: unknown;
        form?: unknown;
        fields?: Record<string, unknown>;
        kind?: unknown;
        content?: unknown;
        when?: unknown;
        timezone?: unknown;
        task?: unknown;
        repeat?: unknown;
    };
    if (call.name === 'schedule') {
        return [
            { id: 'task', label: 'Task', value: String(args.task ?? '') },
            {
                id: 'repeat',
                label: 'Repeats',
                value: String(args.repeat ?? 'once'),
            },
            { id: 'when', label: 'First run', value: String(args.when ?? '') },
            {
                id: 'timezone',
                label: 'Timezone',
                value: String(args.timezone ?? ''),
            },
        ];
    }
    if (call.name === 'remember') {
        return [
            { id: 'kind', label: 'Type', value: String(args.kind ?? '') },
            {
                id: 'content',
                label: 'Memory',
                value: String(args.content ?? ''),
            },
        ];
    }
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
        if (
            !e.tool_calls?.some((call) =>
                call.name.startsWith('deep_research_'),
            )
        ) {
            items.push({
                id: `model-${i}`,
                type: 'step',
                label: 'Asked the model',
                status: 'complete',
            });
        }
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
                      ? call.name === 'ask_user'
                          ? 'waiting for your answer'
                          : 'needs your approval'
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

export default function Agent(props: AgentProps) {
    const [deepResearch, setDeepResearch] = useState(false);
    return (
        <Chat
            key={props.conversationID || (props.newChat ? 'new' : 'legacy')}
            {...props}
            deepResearch={deepResearch}
            toggleDeepResearch={() => setDeepResearch((active) => !active)}
        />
    );
}

function RunActivity({ turn, connected }: { turn: Turn; connected: boolean }) {
    if (turn.status === 'queued') {
        return (
            <p role="status" className="text-sm text-muted-foreground">
                {turn.runId
                    ? 'Queued'
                    : connected
                      ? 'Sending…'
                      : 'Waiting for connection…'}
            </p>
        );
    }
    const working = turn.status === 'working';
    const items = activityItems(turn.events ?? [], working);
    const activeTool = items.findLast(
        (item) =>
            item.type === 'step' &&
            item.status === 'active' &&
            item.id.startsWith('tool-'),
    );
    return (
        <AgentActivity
            items={items}
            activeLabel={
                activeTool?.type === 'step' ? activeTool.label : undefined
            }
            status={working ? 'working' : 'complete'}
            duration={turn.seconds ?? 0}
        />
    );
}

function Chat({
    conversationID,
    runs,
    newChat,
    deepResearch,
    toggleDeepResearch,
}: AgentProps & {
    deepResearch: boolean;
    toggleDeepResearch: () => void;
}) {
    const [legacy] = useState(
        () => !conversationID && !newChat && loadTurns().length > 0,
    );
    const [turns, setTurns] = useState<Turn[]>(() =>
        legacy ? loadTurns() : restoreRuns(runs),
    );
    const nextId = useRef(Math.max(0, ...turns.map((t) => t.id + 1)));
    const [legacyLimit] = useState(() =>
        Math.max(0, ...turns.map((t) => t.id + 1)),
    );
    const loading = turns.some(
        (t) => t.status === 'working' || t.status === 'queued',
    );
    const socket = useRef<WebSocket | null>(null);
    const chat = useRef(conversationID);
    const pending = useRef(new Map<string, QueuedPrompt>());
    const watches = useRef(
        new Map<string, { replyId: number; after: number; started?: number }>(),
    );
    const navigating = useRef(false);
    const [changingChat, setChangingChat] = useState(false);
    const [connected, setConnected] = useState(false);

    useEffect(() => {
        if (legacy)
            sessionStorage.setItem(
                STORAGE_KEY,
                JSON.stringify(turns.filter((turn) => turn.id < legacyLimit)),
            );
    }, [legacy, legacyLimit, turns]);

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
            userRequests(t.events ?? []).some((r) => !r.decided),
    );

    const decide = async (
        runId: string,
        callId: string,
        approved: boolean,
        answer?: string,
    ) => {
        const key = `${runId}/${callId}`;
        setDeciding((d) => new Set(d).add(key));
        const res = await fetch(
            `/api/runs/${encodeURIComponent(runId)}/approvals`,
            {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ call_id: callId, approved, answer }),
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

    const sendPending = (ws: WebSocket) => {
        if (ws.readyState !== WebSocket.OPEN) return;
        for (const [requestId, task] of pending.current) {
            if (task.sentOn !== ws) {
                ws.send(
                    JSON.stringify({
                        type: 'submit',
                        request_id: requestId,
                        conversation_id: chat.current,
                        prompt: task.prompt,
                        deep_research: task.deepResearch,
                        initial_history: chat.current
                            ? undefined
                            : task.initialHistory,
                    }),
                );
                task.sentOn = ws;
            }
            // The first acknowledgement gives every subsequent task the
            // same saved conversation, including tasks entered while it is created.
            if (!chat.current) break;
        }
    };

    useEffect(() => {
        let closed = false;
        let timer: ReturnType<typeof setTimeout>;
        let heartbeat: ReturnType<typeof setInterval>;
        let delay = 500;
        let lastFrame = 0;
        const anchor = window.location.hash.slice(1);
        if (anchor) document.getElementById(anchor)?.scrollIntoView();
        const subscriptions = watches.current;
        for (const turn of turns) {
            if (
                turn.runId &&
                (turn.status === 'working' || turn.status === 'queued')
            ) {
                watches.current.set(turn.runId, {
                    replyId: turn.id,
                    after: -1,
                });
            }
        }

        const connect = () => {
            const url = new URL('/api/chat/socket', window.location.href);
            url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
            const ws = new WebSocket(url);
            socket.current = ws;
            const watch = (runId: string, after: number) =>
                ws.send(
                    JSON.stringify({ type: 'watch', run_id: runId, after }),
                );
            ws.onopen = () => {
                if (closed) return;
                setConnected(true);
                lastFrame = Date.now();
                delay = 500;
                for (const [runId, subscription] of watches.current)
                    watch(runId, subscription.after);
                sendPending(ws);
                heartbeat = setInterval(() => {
                    if (Date.now() - lastFrame > 45000) ws.close();
                    else if (ws.readyState === WebSocket.OPEN)
                        ws.send(JSON.stringify({ type: 'ping' }));
                }, 15000);
            };
            ws.onmessage = (message) => {
                if (closed) return;
                lastFrame = Date.now();
                const frame = JSON.parse(message.data) as ChatFrame;
                if (frame.type === 'accepted' || frame.type === 'rejected') {
                    const task = pending.current.get(frame.request_id);
                    if (!task) return;
                    pending.current.delete(frame.request_id);
                    if (frame.type === 'accepted') {
                        chat.current = frame.conversation_id;
                        update(task.replyId, (t) => ({
                            ...t,
                            runId: frame.run_id,
                        }));
                        watches.current.set(frame.run_id, {
                            replyId: task.replyId,
                            after: -1,
                        });
                        watch(frame.run_id, -1);
                    } else {
                        update(task.replyId, (t) => ({
                            ...t,
                            status: 'error',
                            text: frame.error,
                        }));
                    }
                    sendPending(ws);
                    if (
                        !conversationID &&
                        chat.current &&
                        pending.current.size === 0 &&
                        !navigating.current
                    ) {
                        navigating.current = true;
                        setChangingChat(true);
                        router.visit(agentChat(chat.current), {
                            replace: true,
                            preserveState: true,
                            onFinish: () => {
                                navigating.current = false;
                                setChangingChat(false);
                            },
                        });
                    }
                    return;
                }
                if (!('run_id' in frame)) return;
                const subscription = watches.current.get(frame.run_id);
                if (!subscription) return;
                if (frame.type === 'event') {
                    if (frame.seq <= subscription.after) return;
                    subscription.after = frame.seq;
                    subscription.started ??= performance.now();
                    const e = frame.event;
                    update(subscription.replyId, (t) => ({
                        ...t,
                        status: 'working',
                        events: [...(t.events ?? []), e],
                        messages: e.assistant_message
                            ? [...(t.messages ?? []), e.assistant_message]
                            : t.messages,
                        text:
                            e.kind === 'failed'
                                ? `The run stopped: ${e.err}`
                                : t.text,
                    }));
                } else {
                    watches.current.delete(frame.run_id);
                    const seconds =
                        subscription.started === undefined
                            ? 0
                            : Math.round(
                                  (performance.now() - subscription.started) /
                                      1000,
                              );
                    update(subscription.replyId, (t) => ({
                        ...t,
                        status:
                            frame.type === 'error' ||
                            t.events?.at(-1)?.kind === 'failed'
                                ? 'error'
                                : 'done',
                        text:
                            frame.type === 'error'
                                ? frame.error ||
                                  'The run stopped before it finished.'
                                : t.text,
                        seconds,
                    }));
                }
            };
            ws.onclose = () => {
                clearInterval(heartbeat);
                if (closed) return;
                setConnected(false);
                timer = setTimeout(connect, delay);
                delay = Math.min(delay * 2, 8000);
            };
        };
        connect();
        return () => {
            closed = true;
            clearTimeout(timer);
            clearInterval(heartbeat);
            socket.current?.close();
            socket.current = null;
            for (const subscription of subscriptions.values())
                subscription.after = -1;
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    const submit = (prompt: string) => {
        const text = prompt.trim();
        if (!text || navigating.current) return;
        const userId = nextId.current++;
        const replyId = nextId.current++;
        const initialHistory =
            legacy && !chat.current
                ? turns
                      .filter((turn) => turn.id < legacyLimit)
                      .flatMap((turn) =>
                          (turn.from === 'user'
                              ? [turn.text]
                              : turn.status === 'done'
                                ? assistantMessages(turn)
                                : []
                          )
                              .filter((content) => content.trim())
                              .map((content) => ({ role: turn.from, content })),
                      )
                      .slice(-99)
                : undefined;
        pending.current.set(crypto.randomUUID(), {
            replyId,
            prompt: text,
            deepResearch,
            initialHistory,
        });
        setTurns((t) => [
            ...t,
            { id: userId, from: 'user', text },
            { id: replyId, from: 'assistant', text: '', status: 'queued' },
        ]);
        if (socket.current) sendPending(socket.current);
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
                                    ? 'Waiting for you'
                                    : loading
                                      ? connected
                                          ? 'Working…'
                                          : 'Reconnecting…'
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
                                    <Fragment key={turn.id}>
                                        {turn.from === 'user' ||
                                        turn.events?.length ||
                                        turn.status !== 'done' ||
                                        !assistantMessages(turn).length ? (
                                            <Message
                                                id={turn.runId}
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
                                                        <RunActivity
                                                            turn={turn}
                                                            connected={
                                                                connected
                                                            }
                                                        />
                                                        {latestTodos(
                                                            turn.events ?? [],
                                                        ).length > 0 && (
                                                            <TodoList
                                                                items={latestTodos(
                                                                    turn.events ??
                                                                        [],
                                                                )}
                                                                title="Plan"
                                                                defaultOpen
                                                            />
                                                        )}
                                                        {userRequests(
                                                            turn.events ?? [],
                                                        ).map((req) => {
                                                            const {
                                                                call,
                                                                decided,
                                                            } = req;
                                                            const key = `${turn.runId}/${call.id}`;
                                                            const sending =
                                                                !decided &&
                                                                deciding.has(
                                                                    key,
                                                                );
                                                            if (
                                                                call.name ===
                                                                'ask_user'
                                                            ) {
                                                                const a =
                                                                    (call.args ??
                                                                        {}) as {
                                                                        question?: string;
                                                                        options?: string[];
                                                                    };
                                                                return (
                                                                    <QuestionCard
                                                                        key={
                                                                            key
                                                                        }
                                                                        question={
                                                                            a.question ??
                                                                            ''
                                                                        }
                                                                        options={
                                                                            a.options
                                                                        }
                                                                        status={
                                                                            !decided
                                                                                ? sending
                                                                                    ? 'sending'
                                                                                    : 'pending'
                                                                                : decided.approved
                                                                                  ? 'answered'
                                                                                  : 'skipped'
                                                                        }
                                                                        answer={
                                                                            decided?.text
                                                                        }
                                                                        onAnswer={(
                                                                            answer,
                                                                        ) =>
                                                                            decide(
                                                                                turn.runId!,
                                                                                call.id,
                                                                                true,
                                                                                answer,
                                                                            )
                                                                        }
                                                                        onSkip={() =>
                                                                            decide(
                                                                                turn.runId!,
                                                                                call.id,
                                                                                false,
                                                                            )
                                                                        }
                                                                    />
                                                                );
                                                            }
                                                            const status =
                                                                approvalStatus(
                                                                    req,
                                                                );
                                                            return (
                                                                <ToolApproval
                                                                    key={key}
                                                                    tool={
                                                                        call.name
                                                                    }
                                                                    description={
                                                                        call.name ===
                                                                        'remember'
                                                                            ? 'Save this information for future chats.'
                                                                            : call.name ===
                                                                                'schedule'
                                                                              ? 'Run this task on the approved schedule and deliver results in this chat.'
                                                                              : 'Allow the agent to perform this action.'
                                                                    }
                                                                    parameters={approvalParameters(
                                                                        call,
                                                                    )}
                                                                    defaultOpen
                                                                    status={
                                                                        status ===
                                                                            'pending' &&
                                                                        sending
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
                                                        {screenshots(
                                                            turn.events ?? [],
                                                        ).map((id) => (
                                                            <a
                                                                key={id}
                                                                href={`/api/runs/${encodeURIComponent(turn.runId!)}/artifacts/${id}`}
                                                                target="_blank"
                                                                rel="noreferrer"
                                                            >
                                                                <img
                                                                    src={`/api/runs/${encodeURIComponent(turn.runId!)}/artifacts/${id}`}
                                                                    alt="Screenshot of the page the agent was on"
                                                                    className="max-h-96 rounded-lg border"
                                                                    loading="lazy"
                                                                />
                                                            </a>
                                                        ))}
                                                        {turn.status ===
                                                            'error' &&
                                                            turn.text}
                                                    </MessageContent>
                                                )}
                                            </Message>
                                        ) : null}
                                        {turn.from === 'assistant' &&
                                            assistantMessages(turn).map(
                                                (text, index) => (
                                                    <Message
                                                        key={index}
                                                        id={
                                                            index === 0 &&
                                                            turn.status ===
                                                                'done' &&
                                                            !turn.events?.length
                                                                ? turn.runId
                                                                : undefined
                                                        }
                                                        from="assistant"
                                                        animateIn
                                                    >
                                                        <AssistantMarkdown
                                                            text={text}
                                                        />
                                                    </Message>
                                                ),
                                            )}
                                    </Fragment>
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
                        disabled={changingChat}
                        onSubmit={submit}
                        leadingAction={
                            <button
                                type="button"
                                aria-pressed={deepResearch}
                                disabled={changingChat}
                                onClick={toggleDeepResearch}
                                title="Search multiple sources and review the evidence before answering"
                                className="inline-flex h-8 items-center gap-1.5 rounded-full border border-transparent px-2.5 text-xs font-medium text-muted-foreground hover:bg-muted focus-visible:outline-2 focus-visible:outline-ring disabled:opacity-50 aria-pressed:border-primary/25 aria-pressed:bg-primary/10 aria-pressed:text-primary"
                            >
                                <Telescope
                                    aria-hidden="true"
                                    className="size-3.5"
                                />
                                Deep research
                            </button>
                        }
                        placeholder={
                            deepResearch
                                ? 'What should we research in depth?'
                                : 'Describe a hypothesis to research…'
                        }
                    />
                </GlowFrame>
            </div>
        </>
    );
}

Agent.layout = {
    breadcrumbs: [{ title: 'Agent', href: agent() }],
};
