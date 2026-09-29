import { api } from './api';
import { reload as reloadList } from './apps.svelte';
import type { Msg } from './i18n';

export type GameVariable = {
	env: string;
	name: string;
	description?: string;
	value: string;
	default: string;
	editable: boolean;
	rules?: string[];
};

export type GamePort = { id: number; ip: string; port: number; address: string; default: boolean };

export type Game = {
	id: string;
	egg: string;
	description?: string;
	image: string;
	images: { label: string; ref: string }[];
	startup: string;
	// The startup command with the saved values filled in.
	startup_preview: string;
	memory_mb: number;
	cpus: number;
	disk_mb: number;
	ports: GamePort[];
	variables: GameVariable[];
	// What the egg says the game needs, such as 'eula'.
	features: string[];
	// The egg asks for a EULA nobody has accepted, so the server will not start.
	eula_needed?: boolean;
	install: { state: 'installing' | 'installed' | 'failed'; deployment?: number; installed_at?: string };
	// stopped, starting, running, stopping or crashed.
	state: string;
	crashing?: Msg;
};

export type EggPreview = {
	name: string;
	description?: string;
	images: { label: string; ref: string }[];
	startup: string;
	variables: GameVariable[];
	features: string[];
};

export type Allocation = { id: number; ip: string; port: number; app?: string };
export type PortRange = { ip: string; ports: string; first: number; last: number };

// The game server open in its pages. The layout loads it and the console
// reports what the socket says the state is, which is faster than a poll.
export const game = $state<{ info: Game | null; missing: boolean; live: string }>({ info: null, missing: false, live: '' });

export async function loadGame(id: string): Promise<void> {
	try {
		game.info = await api<Game>('GET', `/games/${encodeURIComponent(id)}`);
		game.missing = false;
	} catch {
		game.missing = true;
	}
}

// What the server is doing, for the header and the console.
export function gameState(g: Game): string {
	if (g.install.state === 'installing') return 'installing';
	return game.live && game.live !== 'unknown' ? game.live : g.state;
}

export const eulaLink = 'https://aka.ms/MinecraftEULA';

// Records the acceptance of the game's EULA, which is what lets it start.
export async function acceptEula(id: string): Promise<void> {
	await api('POST', `/games/${encodeURIComponent(id)}/eula`);
}

// Saves the image and the editable variables the server starts with next.
// Only the variables given change.
export async function saveSettings(id: string, settings: { image?: string; variables: Record<string, string> }): Promise<Game> {
	const out = await api<Game>('PUT', `/games/${encodeURIComponent(id)}/variables`, settings);
	game.info = out;
	return out;
}

export async function power(id: string, action: 'start' | 'stop' | 'restart' | 'kill'): Promise<void> {
	await api('POST', `/games/${encodeURIComponent(id)}/power`, { action });
	await Promise.all([loadGame(id), reloadList()]);
}

// Players connect straight to the machine, not through the panel's address,
// which may be a tunnel. The panel says which host that is; the name the panel
// was opened with is only the last resort, when it found none.
export function address(p: GamePort): string {
	const host = p.address || location.hostname;
	return `${host.includes(':') ? `[${host}]` : host}:${p.port}`;
}

export const defaultPort = (g: Game): GamePort | undefined => g.ports.find((p) => p.default) ?? g.ports[0];
