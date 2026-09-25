import { api } from './api';
import { inProgress, reload as reloadList, type AppDetail } from './apps.svelte';

// The app open in the app pages. The layout loads it; the tabs read it.
export const current = $state<{ app: AppDetail | null; missing: boolean }>({ app: null, missing: false });

export async function load(id: string): Promise<void> {
	try {
		current.app = await api<AppDetail>('GET', `/apps/${encodeURIComponent(id)}`);
		current.missing = false;
	} catch {
		current.missing = true;
	}
}

// deploy starts a deployment and refreshes what shows it.
export async function deploy(id: string): Promise<void> {
	await api('POST', `/apps/${encodeURIComponent(id)}/deployments`);
	await Promise.all([load(id), reloadList()]);
}

export function busy(): boolean {
	return inProgress(current.app?.deployments[0]);
}
