import { api } from './api';
import { inProgress, reload as reloadList, type AppDetail } from './apps.svelte';
import type { Usage } from './host.svelte';

// The app open in the app pages. The layout loads it, and what it uses now;
// the tabs read both.
export const current = $state<{ app: AppDetail | null; missing: boolean; usage: Usage | null }>({
	app: null,
	missing: false,
	usage: null
});

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

// restart runs the live version again, and rollback an earlier one, both
// without building.
export async function restart(id: string): Promise<void> {
	await api('POST', `/apps/${encodeURIComponent(id)}/restart`);
	await Promise.all([load(id), reloadList()]);
}

// update deploys what the image's tag points at now; a database is backed
// up first.
export async function update(id: string): Promise<void> {
	await api('POST', `/apps/${encodeURIComponent(id)}/update`);
	await Promise.all([load(id), reloadList()]);
}

export async function stop(id: string): Promise<void> {
	await api('POST', `/apps/${encodeURIComponent(id)}/stop`);
	await Promise.all([load(id), reloadList()]);
}

export async function start(id: string): Promise<void> {
	await api('POST', `/apps/${encodeURIComponent(id)}/start`);
	await Promise.all([load(id), reloadList()]);
}

export async function rollback(id: string, deployment: number): Promise<void> {
	await api('POST', `/apps/${encodeURIComponent(id)}/deployments/${deployment}/rollback`);
	await Promise.all([load(id), reloadList()]);
}

export function busy(): boolean {
	return inProgress(current.app?.deployments[0]);
}
