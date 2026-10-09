import { goto } from '$app/navigation';
import { api, ApiError, type Me } from './api';

export const session = $state<{ me: Me | null }>({ me: null });

export async function refresh(): Promise<Me> {
	session.me = await api<Me>('GET', '/me');
	return session.me;
}

// A 401 in the middle of a page means the session is over: it ran out, or
// was ended from another device. Sends the user to log in again, and says
// whether that is what the error was.
export function leaveIfSignedOut(err: unknown): boolean {
	if (!(err instanceof ApiError) || err.status !== 401) return false;
	goto('/login');
	return true;
}
