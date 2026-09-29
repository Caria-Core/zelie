import { api } from './api';

export type Host = { cpus: number; memory_bytes: number; disk_bytes: number; disk_free_bytes: number };

// What the server has, fetched once: it does not change while the panel is
// open.
export const host = $state<{ info: Host | null }>({ info: null });

export async function loadHost(): Promise<void> {
	if (host.info) return;
	host.info = await api<Host>('GET', '/host').catch(() => null);
}

export type Usage = {
	running: boolean;
	memory_bytes?: number;
	cpu?: number;
	// Over the last few minutes, for an app with a domain.
	requests_per_min?: number;
	error_rate?: number;
	// Times it stopped by itself in the last day.
	crashes: number;
};

// megabytes shows a size in MB the way people say it.
export function megabytes(mb: number): string {
	if (mb < 1024) return `${Math.round(mb)} MB`;
	const gb = mb / 1024;
	return `${Number.isInteger(gb) ? gb : gb.toFixed(1)} GB`;
}
