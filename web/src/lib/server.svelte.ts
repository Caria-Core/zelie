import { api } from './api';
import type { Msg } from './i18n';

export type Release = { version: string; notes: string; published: string; url: string };
// reason and back say why an update failed and what became of the old version.
// error is the same in English, and all an update from an earlier version saved.
export type UpdateResult = { from: string; to: string; running?: boolean; ok: boolean; reason?: Msg; back?: Msg; error?: string; at: string };
export type ServerInfo = {
	version: string;
	available?: Release;
	checked_at?: string;
	check_error?: string;
	last_update?: UpdateResult;
	host?: { cpus: number; memory_bytes: number; disk_bytes: number; disk_free_bytes: number };
	access?: 'acme' | 'self-signed' | 'tunnel';
	address?: string;
	steam_key_set?: boolean;
};

// The sidebar and the server page share what the panel knows about the
// server and its updates.
export const server = $state<{ info: ServerInfo | null }>({ info: null });

export async function loadServer(): Promise<ServerInfo> {
	server.info = await api<ServerInfo>('GET', '/server');
	return server.info;
}

// Where players reach this machine's game servers.
export type NodeInfo = {
	id: number;
	name: string;
	address: string;
	detected: string;
	// The address is one found on a network card that is not public.
	private: boolean;
	override: string;
};
