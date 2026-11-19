import {
    animate,
    motion,
    useMotionTemplate,
    useMotionValue,
    useReducedMotion,
} from 'motion/react';
import { useEffect } from 'react';
import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';

const COLORS = '#ff7a1a, #ff2d55, #a23cff, #3b6bff, #ff2d55, #ff7a1a';

interface GlowFrameProps {
    children: ReactNode;
    /** Speeds the glow up and brightens it, e.g. while the agent works. */
    active?: boolean;
    className?: string;
}

/** Soft, slowly rotating gradient glow behind a simple bordered box. */
export function GlowFrame({ children, active = false, className }: GlowFrameProps) {
    const reduce = useReducedMotion() ?? false;
    const angle = useMotionValue(0);
    const background = useMotionTemplate`conic-gradient(from ${angle}deg at 50% 50%, ${COLORS})`;

    useEffect(() => {
        if (reduce) return;
        const controls = animate(angle, angle.get() + 360, {
            duration: active ? 4 : 12,
            ease: 'linear',
            repeat: Infinity,
        });
        return () => controls.stop();
    }, [active, reduce, angle]);

    return (
        <div className={cn('relative isolate rounded-2xl', className)}>
            <motion.div
                aria-hidden
                style={{ background }}
                animate={{ opacity: active ? 0.6 : 0.4 }}
                transition={{ duration: 0.6 }}
                className="pointer-events-none absolute -inset-x-16 -inset-y-10 -z-10 rounded-[3rem] blur-3xl"
            />
            <div className="rounded-2xl border border-white/20 bg-background [&_*]:!shadow-none">
                {children}
            </div>
        </div>
    );
}
