import { Head, Link, router } from '@inertiajs/react';
import { useState } from 'react';
import Heading from '@/components/heading';
import { Button } from '@/components/ui/button';
import {
    agentChat,
    cancelSchedule,
    schedules as schedulesRoute,
} from '@/routes';

type Schedule = {
    id: string;
    conversation_id: string;
    prompt: string;
    fire_at: string;
    timezone: string;
    status: 'pending' | 'fired' | 'missed' | 'cancelled';
    run_id?: string;
    repeat: 'once' | 'daily' | 'weekly';
    last_occurrence?: {
        status: 'fired' | 'missed';
        reason?: 'expired' | 'overlap';
    };
};

const labels: Record<Schedule['status'], string> = {
    pending: 'Pending',
    fired: 'Run created',
    missed: 'Missed',
    cancelled: 'Cancelled',
};

const frequencies: Record<Schedule['repeat'], string> = {
    once: 'One-time',
    daily: 'Daily',
    weekly: 'Weekly',
};

const chatLink = (entry: Schedule) =>
    agentChat(entry.conversation_id) + (entry.run_id ? `#${entry.run_id}` : '');

export default function Schedules({ schedules }: { schedules: Schedule[] }) {
    const [cancelling, setCancelling] = useState<string>();
    const [message, setMessage] = useState('');
    const [conflict, setConflict] = useState<Schedule>();
    const cancel = async (entry: Schedule) => {
        setCancelling(entry.id);
        setMessage('');
        setConflict(undefined);
        try {
            const res = await fetch(cancelSchedule(entry.id).url, {
                method: 'DELETE',
            });
            if (res.status === 409) {
                const current: Schedule = await res.json();
                setMessage(
                    current.status === 'fired'
                        ? 'This task has already been queued.'
                        : 'This schedule has already ended.',
                );
                setConflict(current);
            } else if (!res.ok) {
                throw new Error();
            }
            router.reload({ only: ['schedules'] });
        } catch {
            setMessage('Could not cancel this schedule. Try again.');
        } finally {
            setCancelling(undefined);
        }
    };

    return (
        <section className="mx-auto w-full max-w-3xl space-y-6 overflow-auto px-6 py-8">
            <Head title="Schedules" />
            <Heading
                title="Schedules"
                description="Ask in a chat to schedule a reminder or research task."
            />
            {message && (
                <p role="status" className="text-sm">
                    {message}{' '}
                    {conflict?.run_id && (
                        <Link className="underline" href={chatLink(conflict)}>
                            View run
                        </Link>
                    )}
                </p>
            )}
            {schedules.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                    No schedules yet.
                </p>
            ) : (
                <ul className="divide-y">
                    {schedules.map((entry) => (
                        <li
                            key={entry.id}
                            className="flex items-start justify-between gap-4 py-4"
                        >
                            <div className="min-w-0 space-y-1">
                                <p className="break-words whitespace-pre-wrap">
                                    {entry.prompt}
                                </p>
                                <p className="text-sm text-muted-foreground">
                                    {entry.repeat !== 'once' &&
                                        entry.status === 'pending' &&
                                        'Next: '}
                                    <time dateTime={entry.fire_at}>
                                        {new Intl.DateTimeFormat(undefined, {
                                            dateStyle: 'medium',
                                            timeStyle: 'short',
                                            timeZone: entry.timezone,
                                        }).format(new Date(entry.fire_at))}
                                    </time>{' '}
                                    {entry.timezone} ·{' '}
                                    {frequencies[entry.repeat]} ·{' '}
                                    {labels[entry.status]}
                                </p>
                                {entry.last_occurrence?.status === 'missed' && (
                                    <p className="text-sm text-muted-foreground">
                                        {entry.last_occurrence.reason ===
                                        'overlap'
                                            ? 'Last occurrence skipped while its previous run was active.'
                                            : 'Last occurrence missed its delivery window.'}
                                    </p>
                                )}
                                <Link
                                    href={chatLink(entry)}
                                    className="text-sm underline"
                                >
                                    {entry.run_id
                                        ? entry.repeat === 'once'
                                            ? 'View run'
                                            : 'View latest run'
                                        : 'Open chat'}
                                </Link>
                            </div>
                            {entry.status === 'pending' && (
                                <Button
                                    variant="outline"
                                    size="sm"
                                    disabled={!!cancelling}
                                    onClick={() => cancel(entry)}
                                >
                                    Cancel
                                </Button>
                            )}
                        </li>
                    ))}
                </ul>
            )}
        </section>
    );
}

Schedules.layout = {
    breadcrumbs: [{ title: 'Schedules', href: schedulesRoute() }],
};
