import { Head } from '@inertiajs/react';
import { motion, useAnimationFrame, useMotionValue } from 'motion/react';
import { useRef, useState } from 'react';
import {
    Message,
    MessageContent,
    MessageGroup,
    MessageTyping,
} from '@/components/agents/message';
import {
    MessageBubble,
    MessageBubbleContent,
} from '@/components/agents/message-bubble';
import { GlowFrame } from '@/components/agents/glow-frame';
import { MessageScroller } from '@/components/agents/message-scroller';
import { PromptInput } from '@/components/agents/prompt-input';
import { VoiceOrb } from '@/components/agents/voice-orb';
import { SPRING_LAYOUT } from '@/lib/ease';
import { agent } from '@/routes';

type Turn = {
    id: number;
    from: 'user' | 'assistant';
    text: string;
    pending?: boolean;
};

// TODO: start a durable agent run (Temporal) and stream its events over SSE.
// Until the runtime is wired, the reply says so instead of faking an answer.
const runAgent = () =>
    new Promise<string>((resolve) =>
        setTimeout(
            () =>
                resolve(
                    "The agent runtime isn't connected yet. Once it is, this reply will stream from a durable run.",
                ),
            1200,
        ),
    );

export default function Agent() {
    const [turns, setTurns] = useState<Turn[]>([]);
    const [loading, setLoading] = useState(false);
    const nextId = useRef(0);

    // The orb breathes quietly at rest and pulses while the agent works.
    const activity = useMotionValue(0.06);
    useAnimationFrame((t) => {
        activity.set(loading ? 0.35 + 0.4 * Math.sin(t / 240) ** 2 : 0.06);
    });

    const submit = async (prompt: string) => {
        const text = prompt.trim();
        if (!text || loading) return;
        const userId = nextId.current++;
        const replyId = nextId.current++;
        setTurns((t) => [
            ...t,
            { id: userId, from: 'user', text },
            { id: replyId, from: 'assistant', text: '', pending: true },
        ]);
        setLoading(true);
        try {
            const reply = await runAgent();
            setTurns((t) =>
                t.map((turn) =>
                    turn.id === replyId
                        ? { ...turn, text: reply, pending: false }
                        : turn,
                ),
            );
        } finally {
            setLoading(false);
        }
    };

    const started = turns.length > 0;

    return (
        <>
            <Head title="Agent" />
            <div className="flex h-[calc(100svh-5rem)] w-full flex-col px-4 pb-4">
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
                                            <MessageContent>
                                                {turn.pending ? (
                                                    <MessageTyping />
                                                ) : (
                                                    turn.text
                                                )}
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
