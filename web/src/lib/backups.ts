import { ApiError } from './api';
import type { Engine } from './apps.svelte';
import { sensitive } from './confirm.svelte';

export type Backup = {
	id: number;
	app: string;
	engine: Engine;
	reason: 'scheduled' | 'manual' | 'restore';
	state: 'running' | 'done' | 'failed';
	bytes: number;
	error?: string;
	created_at: string;
	finished_at?: string;
	keep_until: string;
	restored_at?: string;
};

export type Plan = { enabled: boolean; minute: number; keep_days: number };

export type Restore = {
	backup: number;
	state: 'running' | 'done' | 'failed';
	error?: string;
	safety?: string;
	restarted: string[];
	at: string;
};

export type Backups = {
	plan: Plan;
	backups: Backup[];
	running: boolean;
	recovery_saved_at: string | null;
	time_zone: string;
	restore?: Restore;
};

export function bytes(n: number): string {
	if (n < 1024) return `${n} B`;
	if (n < 1024 ** 2) return `${Math.round(n / 1024)} KB`;
	if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MB`;
	return `${(n / 1024 ** 3).toFixed(1)} GB`;
}

// clock turns a minute of the day into 03:00.
export function clock(minute: number): string {
	return `${String(Math.floor(minute / 60)).padStart(2, '0')}:${String(minute % 60).padStart(2, '0')}`;
}

// saveRecovery downloads the recovery file. The server wants a recent
// confirmation first, so this is a fetch rather than a link.
export async function saveRecovery(): Promise<void> {
	const blob = await sensitive(async () => {
		const res = await fetch('/api/backups/recovery');
		if (!res.ok) {
			const data = await res.json().catch(() => ({}));
			throw new ApiError(res.status, data.error ?? res.statusText, data.confirm === true);
		}
		return res.blob();
	});
	const url = URL.createObjectURL(blob);
	const a = document.createElement('a');
	a.href = url;
	a.download = 'zelie-recovery.txt';
	a.click();
	URL.revokeObjectURL(url);
}
