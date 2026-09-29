import type { Allocation } from './games.svelte';

// How a port of the pool reads in a list: the number, and the address when
// the port is only open on one.
export function portLabel(a: { ip: string; port: number }): string {
	return a.ip === '0.0.0.0' ? String(a.port) : `${a.port} (${a.ip})`;
}

// The ports of the pool nobody holds, as the pool lists them.
export const freePorts = (pool: Allocation[]): Allocation[] => pool.filter((a) => !a.app);
