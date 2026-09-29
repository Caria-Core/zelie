import type { Allocation } from './games.svelte';

// How a port of the pool reads in a list: the number, and the address when
// the port is only open on one.
export function portLabel(a: { ip: string; port: number }): string {
	return a.ip === '0.0.0.0' ? String(a.port) : `${a.port} (${a.ip})`;
}

// The ports of the pool nobody holds, as the pool lists them.
export const freePorts = (pool: Allocation[]): Allocation[] => pool.filter((a) => !a.app);

type Slot = { id: number; ip: string; port: number };

// The n ports in a row that start at the given one, on one address, each
// passing usable; null when the run is broken. A game such as Valheim
// listens on the port after its own, so the two are chosen as one.
export function runFrom<T extends Slot>(list: T[], startId: number, n: number, usable: (a: T) => boolean): T[] | null {
	const start = list.find((a) => a.id === startId);
	if (!start) return null;
	const run = [start];
	for (let i = 1; i < n; i++) {
		const next = list.find((a) => a.port === start.port + i && a.ip === start.ip && usable(a));
		if (!next) return null;
		run.push(next);
	}
	return run;
}

// The ports that can start a run of n usable ports.
export const runStarts = <T extends Slot>(list: T[], n: number, usable: (a: T) => boolean): T[] => list.filter((a) => usable(a) && runFrom(list, a.id, n, usable));
