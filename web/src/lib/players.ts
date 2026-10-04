import { t } from './i18n';

export type SteamInfo = {
	avatar: string;
	profile_url: string;
	account_created: string | null;
	vac_bans: number;
	game_bans: number;
	days_since_last_ban: number | null;
	community_banned: boolean;
};

export type OnlinePlayer = {
	id: string;
	name: string;
	ip: string;
	ping: number | null;
	connected_seconds: number | null;
	banned: boolean;
	notes: number;
	steam: SteamInfo | null;
};

export type Online = {
	running: boolean;
	source: 'rcon' | 'console' | 'query' | '';
	players: OnlinePlayer[];
	error: string;
	error_code?: string;
};

export type Player = {
	id: string;
	name: string;
	first_seen: string;
	last_seen: string;
	last_ip: string;
	play_seconds: number;
	online: boolean;
	banned: boolean;
	notes: number;
	steam: SteamInfo | null;
};

export type PlayerList = { total: number; players: Player[] };

export type Ban = {
	id: number;
	player_id: string;
	name: string;
	reason: string;
	by: string;
	created_at: string;
	expires_at: string | null;
	lifted_at: string | null;
	lifted_by: string | null;
	active: boolean;
};

export type Report = { reporter_id: string; reporter_name: string; subject: string; message: string; at: string };

export type Note = { id: number; tag: string; note: string; by: string; at: string };

export type PlayerDetail = {
	player: Player;
	sessions: { joined_at: string; left_at: string | null; ip: string; reason: string }[];
	shared_ip: { id: string; name: string; ip: string; last_seen: string }[];
	notes: Note[];
	bans: Ban[];
	reports: Report[];
};

export type ChatMessage = { id: number; player_id: string; name: string; channel: 'global' | 'team' | string; text: string; at: string };

export type ReportTarget = {
	target_id: string;
	target_name: string;
	count: number;
	reporters: number;
	last_at: string;
	items: Report[];
};

export type AuditEntry = { at: string; by: string; action: string; target: string; detail: string };

export const noteTags = ['', 'watch', 'suspect', 'alt', 'vip', 'other'] as const;

// Minutes; 0 is permanent.
export const banDurations = [60, 1440, 10080, 43200, 0] as const;

export const isSteamId = (id: string) => /^\d{17}$/.test(id);

// IPs are personal data, so they show masked until someone asks.
export function maskIP(ip: string): string {
	if (!ip) return '';
	const host = ip.replace(/:\d+$/, '');
	if (host.includes(':')) {
		const parts = host.split(':');
		return parts.slice(0, 2).join(':') + ':•';
	}
	const parts = host.split('.');
	if (parts.length !== 4) return '•';
	return `${parts[0]}.${parts[1]}.•.•`;
}

const stampFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });
const relative = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

export const stamp = (iso: string) => stampFormat.format(new Date(iso));

// until says how long from now iso is, in the largest unit that fits.
export function until(iso: string): string {
	const s = (new Date(iso).getTime() - Date.now()) / 1000;
	for (const [unit, size] of [
		['day', 86400],
		['hour', 3600],
		['minute', 60]
	] as const) {
		if (s >= size) return relative.format(Math.floor(s / size), unit);
	}
	return t('players.soon');
}

export function span(seconds: number): string {
	const s = Math.max(0, Math.floor(seconds));
	const d = Math.floor(s / 86400);
	const h = Math.floor((s % 86400) / 3600);
	const m = Math.floor((s % 3600) / 60);
	if (d > 0) return t('players.durDays', { d, h });
	if (h > 0) return t('players.durHours', { h, m });
	return t('players.durMinutes', { m });
}

// poll runs fn now and every ms while the page is visible, until the
// returned function is called.
export function poll(fn: () => Promise<void> | void, ms: number): () => void {
	let stopped = false;
	let timer: ReturnType<typeof setTimeout>;
	const tick = async () => {
		if (stopped) return;
		if (document.visibilityState === 'visible') await fn();
		if (!stopped) timer = setTimeout(tick, ms);
	};
	tick();
	return () => {
		stopped = true;
		clearTimeout(timer);
	};
}
