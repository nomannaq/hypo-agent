import { route } from '@/routes';

export const edit = route('/settings/memory');
export const update = route('/settings/memory', 'put');
export const destroy = (id: string) =>
    route(`/settings/memory/${encodeURIComponent(id)}`, 'delete')();
