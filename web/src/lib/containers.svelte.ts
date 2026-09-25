import { api } from './api';

export type Container = {
	id: string;
	image: string;
	state: string;
	network?: string;
	ip?: string;
};

export const containers = $state<{ list: Container[]; loaded: boolean }>({ list: [], loaded: false });

export async function reload(): Promise<void> {
	const list = await api<Container[]>('GET', '/containers');
	containers.list = list.sort((a, b) => a.id.localeCompare(b.id));
	containers.loaded = true;
}
