import { route } from '@/routes';

export const edit = route('/settings/profile');
export const update = route('/settings/profile', 'patch');
export const destroy = route('/settings/profile', 'delete');
