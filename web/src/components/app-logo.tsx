import { usePage } from '@inertiajs/react';

import { VoiceOrb } from '@/components/agents/voice-orb';

export default function AppLogo() {
    const { name } = usePage().props;

    return (
        <>
            <div className="flex aspect-square size-8 items-center justify-center">
                <VoiceOrb
                    active={false}
                    activity={0.15}
                    className="w-8"
                    aria-label="Agent logo"
                />
            </div>
            <div className="ml-1 grid flex-1 text-left text-sm">
                <span className="mb-0.5 truncate leading-tight font-semibold">
                    {name}
                </span>
            </div>
        </>
    );
}
