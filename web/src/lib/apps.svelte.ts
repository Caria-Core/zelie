import { api } from './api';

export type Deployment = {
	id: number;
	version: string;
	image?: string;
	state: 'queued' | 'building' | 'starting' | 'live' | 'failed' | 'replaced';
	error?: string;
	created_at: string;
	finished_at?: string;
};

export type App = {
	id: string;
	source: 'github' | 'image';
	image?: string;
	repo?: string;
	branch?: string;
	port: number;
	domain?: string;
	memory_mb: number;
	cpus: number;
	// The live container's state, or none before anything went live.
	state: string;
	latest?: Deployment;
};

export type EnvVar = { name: string; value: string; secret: boolean };

export type AppDetail = App & { env: EnvVar[]; deployments: Deployment[] };

export const apps = $state<{ list: App[]; loaded: boolean }>({ list: [], loaded: false });

export async function reload(): Promise<void> {
	apps.list = await api<App[]>('GET', '/apps');
	apps.loaded = true;
}

export function inProgress(d?: Deployment): boolean {
	return !!d && (d.state === 'queued' || d.state === 'building' || d.state === 'starting');
}

// shortVersion shows a commit the way git does and an image as it is.
export function shortVersion(v: string): string {
	return /^[0-9a-f]{40}$/.test(v) ? v.slice(0, 7) : v;
}
