import { api, type Me } from './api';

export const session = $state<{ me: Me | null }>({ me: null });

export async function refresh(): Promise<Me> {
	session.me = await api<Me>('GET', '/me');
	return session.me;
}
