import { useState } from 'react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

export type QuestionStatus = 'pending' | 'sending' | 'answered' | 'skipped';

// The agent's ask_user question: suggested answers as buttons, a free-text
// answer, or skip. Once answered it shows the answer the agent got.
export function QuestionCard({
    question,
    options = [],
    status,
    answer,
    onAnswer,
    onSkip,
}: {
    question: string;
    options?: string[];
    status: QuestionStatus;
    answer?: string;
    onAnswer: (answer: string) => void;
    onSkip: () => void;
}) {
    const [text, setText] = useState('');
    const open = status === 'pending';

    return (
        <div className="rounded-xl border bg-card p-4 text-card-foreground shadow-xs">
            <p className="text-xs font-medium tracking-wide text-muted-foreground uppercase">
                {open || status === 'sending'
                    ? 'The agent is asking'
                    : 'The agent asked'}
            </p>
            <p className="mt-1 font-medium">{question}</p>
            {status === 'answered' && (
                <p className="mt-2 text-sm text-muted-foreground">
                    You answered:{' '}
                    <span className="text-foreground">{answer}</span>
                </p>
            )}
            {status === 'skipped' && (
                <p className="mt-2 text-sm text-muted-foreground">
                    Not answered{answer ? ` (${answer})` : ''}.
                </p>
            )}
            {(open || status === 'sending') && (
                <form
                    className="mt-3 flex flex-col gap-2"
                    onSubmit={(e) => {
                        e.preventDefault();
                        if (text.trim()) onAnswer(text.trim());
                    }}
                >
                    {options.length > 0 && (
                        <div className="flex flex-wrap gap-2">
                            {options.map((o) => (
                                <Button
                                    key={o}
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    disabled={!open}
                                    onClick={() => onAnswer(o)}
                                >
                                    {o}
                                </Button>
                            ))}
                        </div>
                    )}
                    <div className="flex gap-2">
                        <Input
                            value={text}
                            onChange={(e) => setText(e.target.value)}
                            placeholder={
                                options.length
                                    ? 'Or write your own answer…'
                                    : 'Your answer…'
                            }
                            disabled={!open}
                            aria-label="Your answer"
                        />
                        <Button type="submit" disabled={!open || !text.trim()}>
                            Send
                        </Button>
                        <Button
                            type="button"
                            variant="ghost"
                            disabled={!open}
                            onClick={onSkip}
                        >
                            Skip
                        </Button>
                    </div>
                </form>
            )}
        </div>
    );
}
