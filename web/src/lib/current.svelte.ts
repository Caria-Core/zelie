import { api, ApiError } from './api';
import { say } from './i18n';
import { inProgress, reload as reloadList, type AppDetail } from './apps.svelte';
import type { Usage } from './host.svelte';
import { leaveIfSignedOut } from './session.svelte';

// The app open in the app pages. The layout loads it, and what it uses now;
// the tabs read both.
// offline is set while the panel cannot be reached; what was loaded stays.
export const current = $state<{ app: AppDetail | null; missing: boolean; gone: string; usage: Usage | null; offline: boolean }>({
	app: null,
	missing: false,
	gone: '',
	usage: null,
	offline: false
});

// The app asked for last, so a slower answer about another one is dropped.
let wanted = '';

export async function load(id: string): Promise<void> {
	wanted = id;
	try {
		const app = await api<AppDetail>('GET', `/apps/${encodeURIComponent(id)}`);
		if (wanted !== id) return;
		current.app = app;
		current.missing = false;
		current.gone = '';
		current.offline = false;
	} catch (err) {
		if (wanted !== id || leaveIfSignedOut(err)) return;
		if (err instanceof ApiError && (err.status === 404 || err.status === 403)) {
			current.missing = true;
			current.gone = err.msg.code === 'game.clone_failed' ? say(err.msg) : '';
			current.offline = false;
		} else {
			// See loadGame.
			current.offline = true;
		}
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
