// Hand-written stand-in for Laravel Wayfinder: same call shape (`agent()`,
// `.url()`, `.form()`) so the starter kit files work unchanged.
// Keep in sync with the routes in cmd/server/main.go.
type Method = 'get' | 'post' | 'put' | 'patch' | 'delete';

export const route = (url: string, method: Method = 'get') =>
    Object.assign(() => ({ url, method }), {
        url: () => url,
        form: () => ({ action: url, method }),
    });

export const home = route('/');
export const agent = route('/agent');
export const agentChat = (id: string) =>
    `${agent.url()}?chat=${encodeURIComponent(id)}`;
export const newAgentChat = () => `${agent.url()}?new=1`;
export const schedules = route('/schedules');
export const cancelSchedule = (id: string) =>
    route(`/api/schedules/${encodeURIComponent(id)}`, 'delete')();
export const login = route('/login');
export const register = route('/register');
export const logout = route('/logout', 'post');

export const cancelRun = (id: string) =>
    route(`/api/runs/${encodeURIComponent(id)}/cancel`, 'post')();
