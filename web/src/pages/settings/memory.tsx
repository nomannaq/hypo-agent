import { Head, Link, router } from '@inertiajs/react';
import { useState } from 'react';
import Heading from '@/components/heading';
import { Button } from '@/components/ui/button';
import { agentChat } from '@/routes';
import { destroy, edit, update } from '@/routes/memory';

type Entry = {
    id: string;
    kind: 'preference' | 'note' | 'run_summary';
    content: string;
    source_run: string;
    conversation_id?: string;
};

const labels: Record<Entry['kind'], string> = {
    preference: 'Preference',
    note: 'Note',
    run_summary: 'Run summary',
};

export default function Memory({
    entries,
    summariesEnabled,
}: {
    entries: Entry[];
    summariesEnabled: boolean;
}) {
    const [busy, setBusy] = useState(false);
    const options = {
        preserveScroll: true,
        onStart: () => setBusy(true),
        onFinish: () => setBusy(false),
    };

    return (
        <div className="space-y-6">
            <Head title="Memory settings" />
            <Heading variant="small" title="Memory" />
            <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="space-y-1">
                    <p className="text-sm font-medium">Save run summaries</p>
                    <p className="text-sm text-muted-foreground">
                        Save a short summary of future completed turns for
                        recall.
                    </p>
                </div>
                <Button
                    variant="outline"
                    disabled={busy}
                    onClick={() =>
                        router.put(
                            update.url(),
                            { enabled: !summariesEnabled },
                            options,
                        )
                    }
                >
                    {summariesEnabled
                        ? 'Turn off summaries'
                        : 'Turn on summaries'}
                </Button>
            </div>
            {entries.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                    No saved memories.
                </p>
            ) : (
                <ul className="divide-y">
                    {entries.map((entry) => (
                        <li
                            key={entry.id}
                            className="flex items-start justify-between gap-4 py-4"
                        >
                            <div className="min-w-0 space-y-1 text-sm">
                                <p className="text-muted-foreground">
                                    {labels[entry.kind]}
                                </p>
                                <p className="break-words whitespace-pre-wrap">
                                    {entry.content}
                                </p>
                                {entry.conversation_id && (
                                    <Link
                                        href={`${agentChat(entry.conversation_id)}#${entry.source_run}`}
                                        className="underline underline-offset-4"
                                    >
                                        View source chat
                                    </Link>
                                )}
                            </div>
                            <Button
                                variant="ghost"
                                size="sm"
                                disabled={busy}
                                aria-label={`Delete ${labels[entry.kind].toLowerCase()}: ${entry.content.slice(0, 50)}`}
                                onClick={() =>
                                    router.delete(
                                        destroy(entry.id).url,
                                        options,
                                    )
                                }
                            >
                                Delete
                            </Button>
                        </li>
                    ))}
                </ul>
            )}
        </div>
    );
}

Memory.layout = { breadcrumbs: [{ title: 'Memory', href: edit() }] };
