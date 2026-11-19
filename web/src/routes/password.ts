import { route } from '@/routes';

// Not served yet (needs mail); the login page only links here when canResetPassword is set.
export const request = route('/forgot-password');
