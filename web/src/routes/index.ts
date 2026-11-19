// Hand-written stand-in for Laravel Wayfinder: same call shape (`dashboard()`,
// `.url()`, `.form()`) so the starter kit files work unchanged.
// Keep in sync with the routes in cmd/server/main.go.
type Method = 'get' | 'post' | 'put' | 'patch' | 'delete';

export const route = (url: string, method: Method = 'get') =>
    Object.assign(() => ({ url, method }), {
        url: () => url,
        form: () => ({ action: url, method }),
    });

export const home = route('/');
export const dashboard = route('/dashboard');
export const login = route('/login');
export const register = route('/register');
export const logout = route('/logout', 'post');
