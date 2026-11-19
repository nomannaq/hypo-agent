import { FlaskConical } from 'lucide-react';
import type { SVGAttributes } from 'react';
import { cn } from '@/lib/utils';

export default function AppLogoIcon({ className }: SVGAttributes<SVGElement>) {
    // The kit styles the Laravel mark with fill-current; a stroke icon needs fill-none.
    return <FlaskConical className={cn(className, 'fill-none')} />;
}
