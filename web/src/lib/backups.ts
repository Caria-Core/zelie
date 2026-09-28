import { errorFrom } from './api';
import type { Msg } from './i18n';
import type { Engine } from './apps.svelte';
import { sensitive } from './confirm.svelte';

export type Backup = {
	id: number;
	app: string;
	// Empty for a backup of an app's volumes.
	engine: Engine | '';
	reason: 'scheduled' | 'manual' | 'restore';
	state: 'running' | 'done' | 'failed';
	bytes: number;
	error?: Msg;
	created_at: string;
	finished_at?: string;
	keep_until: string;
	restored_at?: string;
	// For a backup of volumes: the folders in it (where the app saw each
	// volume, without the leading slash), how much its files take and how
	// many changed while they were copied.
	volumes?: string[];
	size?: number;
	changed?: number;
};

export type Plan = { enabled: boolean; minute: number; keep_days: number; stop: boolean };

export type Restore = {
	backup: number;
	state: 'running' | 'done' | 'failed';
	error?: Msg;
	safety?: string;
	// Set when the backup of what was there failed, so nothing changed.
	safety_failed?: boolean;
	restarted: string[];
	at: string;
};

export type Backups = {
	plan: Plan;
	backups: Backup[];
	// Where an app that is not a database sees its volumes now.
	volumes: string[];
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
			throw errorFrom(res.status, res.statusText, data);
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
