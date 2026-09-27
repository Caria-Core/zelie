import { api } from './api';

export type Deployment = {
	id: number;
	version: string;
	image?: string;
	state: 'queued' | 'building' | 'testing' | 'starting' | 'live' | 'failed' | 'replaced' | 'skipped';
	error?: string;
	cause: 'manual' | 'push' | 'restart' | 'rollback' | 'recover' | 'restore';
	// The image still exists, so it can be rolled back to.
	kept: boolean;
	// The first line of the pushed commit's message.
	message?: string;
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
	auto_deploy: boolean;
	health_path: string;
	// Runs the tests before a new build goes live; empty for none.
	test_command: string;
	// The user's own commands; empty means what the build chose.
	build_command: string;
	start_command: string;
	detected: { builder?: 'dockerfile' | 'railpack'; build?: string; start?: string };
	restart_pulls: boolean;
	// The live container's state, or none before anything went live.
	state: string;
	// The user stopped it; Zelie keeps it down.
	stopped: boolean;
	// Why Zelie stopped bringing it back up after crashes.
	crashing?: string;
	// Which volume has grown past its limit, keeping the app down.
	volume_full?: string;
	// Set for a database: postgres, mariadb or redis, and its major version.
	engine?: Engine;
	engine_version?: string;
	latest?: Deployment;
};

export type Engine = 'postgres' | 'mariadb' | 'redis';

export const engineLabel: Record<Engine, string> = { postgres: 'PostgreSQL', mariadb: 'MariaDB', redis: 'Redis' };

export const isDatabase = (a: App): boolean => !!a.engine;

// A link between an app and a database. On an app's page db is the
// database; on a database's page it is the app.
export type Link = { db: string; engine: Engine; prefix: string; vars: string[]; created_at: string };

export type EnvVar = { name: string; value: string; secret: boolean };

export type AppDetail = App & { env: EnvVar[]; deployments: Deployment[] };

export const apps = $state<{ list: App[]; loaded: boolean }>({ list: [], loaded: false });

export async function reload(): Promise<void> {
	apps.list = await api<App[]>('GET', '/apps');
	apps.loaded = true;
}

// Stopping waits for the app to exit, up to half a minute.
export const shownState = (a: App): string => (a.stopped && a.state === 'running' ? 'stopping' : a.state);

export function inProgress(d?: Deployment): boolean {
	return !!d && (d.state === 'queued' || d.state === 'building' || d.state === 'testing' || d.state === 'starting');
}

// shortVersion shows a commit the way git does and an image as it is.
export function shortVersion(v: string): string {
	return /^[0-9a-f]{40}$/.test(v) ? v.slice(0, 7) : v;
}
