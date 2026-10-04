import { api } from './api';
import { ask } from './ask.svelte';
import { messageOf } from './errors';
import { t } from './i18n';
import type { Ban, PlayerDetail } from './players';

export type FormKind = 'kick' | 'ban' | 'note';

// What the Players tab shares between its sections, the player drawer and
// the action dialogs.
export const view = $state<{
	reveal: boolean;
	// The player whose drawer is open.
	detail: string;
	form: { kind: FormKind; id: string; name: string } | null;
	// Counts up after any change, so lists load again.
	tick: number;
	error: string;
}>({ reveal: false, detail: '', form: null, tick: 0, error: '' });

export const changed = () => view.tick++;

const base = (app: string) => `/games/${encodeURIComponent(app)}`;

export async function unban(app: string, ban: Pick<Ban, 'id' | 'name'>) {
	const ok = await ask({ title: t('players.unbanConfirm', { name: ban.name }), text: t('players.unbanConfirmText'), action: t('players.unban'), danger: false });
	if (!ok) return;
	view.error = '';
	try {
		await api('DELETE', `${base(app)}/bans/${ban.id}`);
		changed();
	} catch (err) {
		view.error = messageOf(err);
	}
}

// unbanPlayer finds the player's active ban first, for lists that only know
// the player is banned.
export async function unbanPlayer(app: string, id: string, name: string) {
	view.error = '';
	try {
		const d = await api<PlayerDetail>('GET', `${base(app)}/players/${encodeURIComponent(id)}`);
		const ban = d.bans.find((b) => b.active);
		if (ban) await unban(app, { id: ban.id, name });
	} catch (err) {
		view.error = messageOf(err);
	}
}

export async function toggle(app: string, id: string, what: 'op' | 'whitelist', on: boolean) {
	view.error = '';
	try {
		await api('POST', `${base(app)}/players/${encodeURIComponent(id)}/${what}`, { on });
		changed();
	} catch (err) {
		view.error = messageOf(err);
	}
}
