import { api, ApiError } from './api';
import { reload as reloadList } from './apps.svelte';
import { say, type Msg } from './i18n';
import { leaveIfSignedOut } from './session.svelte';

export type GameVariable = {
	env: string;
	name: string;
	description?: string;
	value: string;
	default: string;
	editable: boolean;
	// The egg keeps non-admin users from changing it; administrators can.
	locked?: boolean;
	// It holds one of the server's ports, which are chosen on the Network tab.
	port?: boolean;
	rules?: string[];
};

// A variable of the egg that holds one of the server's ports, such as the query port.
export type PortUse = { env: string; name: string };

export type GamePort = { id: number; ip: string; port: number; address: string; default: boolean; used_by: PortUse[]; offset?: number };

// Set for games installed with SteamCMD. A build is Steam's number for one
// version of the game; either may be empty when it is not known yet.
export type GameSteam = {
	app_id: number;
	installed_build: string;
	latest_build: string;
	update_available: boolean;
	checked_at: string | null;
	auto_update: boolean;
};

export type Game = {
	id: string;
	egg: string;
	description?: string;
	image: string;
	images: { label: string; ref: string }[];
	startup: string;
	// The egg's own command, which resetting goes back to.
	egg_startup: string;
	// The startup command with the saved values filled in.
	startup_preview: string;
	memory_mb: number;
	cpus: number;
	disk_mb: number;
	ports: GamePort[];
	// How many ports in a row from the game port the game needs, when more than one.
	// Those after the game port have an offset and are not chosen on their own.
	block?: number;
	block_broken?: boolean;
	variables: GameVariable[];
	// What the egg says the game needs, such as 'eula'.
	features: string[];
	// The egg asks for a EULA nobody has accepted, so the server will not start.
	eula_needed?: boolean;
	// clone_of names the server being copied while the install is a copy.
	install: { state: 'installing' | 'installed' | 'failed'; deployment?: number; installed_at?: string; clone_of?: string };
	// stopped, starting, running, stopping or crashed.
	state: string;
	crashing?: Msg;
	steam?: GameSteam;
	// What the Players tab can do: everything, only the online list, or nothing.
	players?: 'full' | 'list' | '';
	// Which game's commands the Players tab can use.
	players_game?: 'rust' | 'minecraft' | '';
};

export type EggPreview = {
	name: string;
	description?: string;
	images: { label: string; ref: string }[];
	startup: string;
	variables: GameVariable[];
	features: string[];
	// The variables that take an extra port each, in the order they get them.
	port_variables: PortUse[];
};

export type Allocation = { id: number; ip: string; port: number; app?: string };
export type PortRange = { ip: string; ports: string; first: number; last: number };

// The game server open in its pages. The layout loads it and the console
// reports what the socket says the state is, which is faster than a poll.
// gone says why the server is missing, when it was a copy that failed and was removed.
// offline is set while the panel cannot be reached; what was loaded stays.
export const game = $state<{ info: Game | null; missing: boolean; gone: string; live: string; offline: boolean }>({
	info: null,
	missing: false,
	gone: '',
	live: '',
	offline: false
});

// The server asked for last, so a slower answer about another one is dropped.
let wanted = '';

export async function loadGame(id: string): Promise<void> {
	wanted = id;
	try {
		const info = await api<Game>('GET', `/games/${encodeURIComponent(id)}`);
		if (wanted !== id) return;
		game.info = info;
		game.missing = false;
		game.gone = '';
		game.offline = false;
	} catch (err) {
		if (wanted !== id || leaveIfSignedOut(err)) return;
		if (err instanceof ApiError && (err.status === 404 || err.status === 403)) {
			game.missing = true;
			game.gone = err.msg.code === 'game.clone_failed' ? say(err.msg) : '';
			game.offline = false;
		} else {
			// A dropped connection or a panel that is restarting says nothing
			// about the server, and whatever the page holds must stay.
			game.offline = true;
		}
	}
}

// Makes a new server from this one and starts copying its files. The new
// server is there at once, installing.
export async function cloneGame(id: string, name: string): Promise<Game> {
	const made = await api<Game>('POST', `/games/${encodeURIComponent(id)}/clone`, { name });
	await reloadList();
	return made;
}

// What the server is doing, for the header and the console.
export function gameState(g: Game): string {
	if (g.install.state === 'installing') return 'installing';
	return game.live && game.live !== 'unknown' ? game.live : g.state;
}

// What the crash doctor found: the likely cause, and a fix when there is one.
// The fixes that change something are applied with applyFix; the others
// (open_network, open_backups) are links to a page.
export type DiagnosisFix = {
	kind: 'raise_memory' | 'raise_disk' | 'switch_image' | 'accept_eula' | 'reinstall' | 'open_network' | 'open_backups';
	params?: Record<string, unknown>;
};
export type Diagnosis = { cause?: Msg; fix?: DiagnosisFix };

// Asked once when a server has crashed or its install failed.
export async function loadDiagnosis(id: string): Promise<Diagnosis> {
	return api<Diagnosis>('GET', `/games/${encodeURIComponent(id)}/diagnosis`);
}

// Does what the diagnosis offers. It does not start the server.
export async function applyFix(id: string, fix: DiagnosisFix): Promise<void> {
	await api('POST', `/games/${encodeURIComponent(id)}/diagnosis/fix`, { kind: fix.kind, params: fix.params });
	await Promise.all([loadGame(id), reloadList()]);
}

export const eulaLink = 'https://aka.ms/MinecraftEULA';

// Records the acceptance of the game's EULA, which is what lets it start.
export async function acceptEula(id: string): Promise<void> {
	await api('POST', `/games/${encodeURIComponent(id)}/eula`);
}

// Saves the image, startup command and editable variables the server starts
// with next. Only what is given changes; an empty startup goes back to the
// egg's own.
export async function saveSettings(id: string, settings: { image?: string; startup?: string; variables: Record<string, string> }): Promise<Game> {
	const out = await api<Game>('PUT', `/games/${encodeURIComponent(id)}/variables`, settings);
	game.info = out;
	return out;
}

// Changes the limits. Memory and CPUs apply from the next start.
export async function saveResources(id: string, sizes: { memory_mb: number; cpus: number; disk_mb: number }): Promise<Game> {
	const out = await api<Game>('PUT', `/games/${encodeURIComponent(id)}/resources`, sizes);
	game.info = out;
	return out;
}

// Which pool allocations (by id) fill each role of the server's ports.
export type PortPlan = { primary: number; variables: Record<string, number>; extra: number[] };

// Changes the ports of a stopped server.
export async function savePorts(id: string, plan: PortPlan): Promise<Game> {
	const out = await api<Game>('PUT', `/games/${encodeURIComponent(id)}/ports`, plan);
	game.info = out;
	return out;
}

export async function power(id: string, action: 'start' | 'stop' | 'restart' | 'kill'): Promise<void> {
	await api('POST', `/games/${encodeURIComponent(id)}/power`, { action });
	await Promise.all([loadGame(id), reloadList()]);
}

// Brings a Steam game to its latest build: a running server restarts, which
// updates it as it starts, and a stopped one is installed again.
export async function updateSteam(id: string): Promise<void> {
	await api('POST', `/games/${encodeURIComponent(id)}/steam/update`);
	await Promise.all([loadGame(id), reloadList()]);
}

// Lets Zelie update the server by itself once nobody is playing.
export async function setAutoUpdate(id: string, on: boolean): Promise<void> {
	await api('PUT', `/games/${encodeURIComponent(id)}/steam`, { auto_update: on });
	await loadGame(id);
}

// Players connect straight to the machine, not through the panel's address,
// which may be a tunnel. The panel says which host that is; the name the panel
// was opened with is only the last resort, when it found none.
export function address(p: GamePort): string {
	const host = p.address || location.hostname;
	return `${host.includes(':') ? `[${host}]` : host}:${p.port}`;
}

export const defaultPort = (g: Game): GamePort | undefined => g.ports.find((p) => p.default) ?? g.ports[0];
